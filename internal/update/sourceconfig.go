package update

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
)

// sourceConfigPath 由 root 安装器按 --update-source 写入。它不在 agent 配置里：
// agent 配置属服务用户，而从哪里取字节是 root 的决定。
const sourceConfigPath = "/etc/heron-update-agent/config.json"

// agentConfigPath 是安装器固定的 agent 配置路径；hub 来源从这里读 hub 地址与 token，Current 也按它核对 agent 的启动参数。
const agentConfigPath = "/etc/heron-agent/config.json"

// parseSourceConfig 严格解析来源配置：未知字段、第一个对象之后的内容、不认识或缺失的取值都是错误。
func parseSourceConfig(b []byte) (string, error) {
	var c struct {
		Source string `json:"source"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return "", err
	}
	if err := trailingJSON(dec); err != nil {
		return "", err
	}
	switch c.Source {
	case "github", "hub":
		return c.Source, nil
	}
	return "", errors.New("source must be github or hub")
}

// chooseSource 按本机安装参数选来源，只对 agent 角色读文件（hub 角色固定 GitHub）。文件不存在等于 github：
// 这不是放宽，两种来源的接受规则相同（Accept）。其他读取或解析错误不回退到 github——那会让一台只能连 hub 的
// 主机悄悄改走必然失败的路径——而是让更新器不支持更新，原因带上文件路径。
func chooseSource(role string, read func() ([]byte, error), github, hub source) sourceChoice {
	if role != "agent" {
		return githubChoice(github)
	}
	b, err := read()
	if errors.Is(err, fs.ErrNotExist) {
		return githubChoice(github)
	}
	if err == nil {
		var name string
		if name, err = parseSourceConfig(b); err == nil {
			if name == "hub" {
				return hubChoice(hub)
			}
			return githubChoice(github)
		}
	}
	return sourceChoice{err: fmt.Sprintf("update source config %s is unusable: %v; rerun the installer with --update-source", sourceConfigPath, err)}
}

// githubChoice 与 hubChoice 让来源名与交给来源的总上限成对出现：引擎按 limit 给 ctx 期限，limit 为零的选择会让
// 取回立刻到期。
func githubChoice(src source) sourceChoice {
	return sourceChoice{name: "github", src: src, limit: DownloadLimit}
}

func hubChoice(src source) sourceChoice {
	return sourceChoice{name: "hub", src: src, limit: hubFetchLimit}
}
