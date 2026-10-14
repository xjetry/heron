// Package agentconfig 是 agent 配置文件的格式与 hub 地址规则。agent（register、run、configure）与节点上的
// root 更新器（hub 来源）读同一份文件，解析与地址规则只在这里定义一次——更新器没有自己的一套。
// 本包不依赖探测策略的解析（prober）：那会把探测代码带进 root 更新器；策略校验留在 agent 的 client 包。
package agentconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
)

// Config 的更新只覆盖命令行明确提供的字段；空列表表示显式清空，不能被解释为沿用旧配置。
// Decode 拒绝未知字段与首个 JSON 对象之后的内容；拼错的拒绝策略不能静默失效。
// Config 是 agent 的配置文件。Hub、Token、Name 由 register 写入；其余字段是宿主机的本地策略，
// 只由 configure 或手工修改，hub 改不了：agent 运行期不写配置，下行消息里也没有对应字段。
// run 加载时由 client.Validate 调 CheckHub；明文地址是否准入取决于本地 InsecureHTTP，不由 hub 下发。
type Config struct {
	Hub          string   `json:"hub"`
	Token        string   `json:"token"`
	Name         string   `json:"name"`
	InsecureHTTP bool     `json:"insecure_http,omitempty"`
	ProbeAllow   []string `json:"probe_allow,omitempty"`
	ProbeDeny    []string `json:"probe_deny,omitempty"`
}

// Decode 严格解析一份配置：未知字段与第一个对象之后的任何内容都是错误（规则见 Config）——策略字段拼错时若静默忽略，
// 宿主机以为拒绝了的地址实际放行。name 只用于报错。
func Decode(r io.Reader, name string) (Config, error) {
	var c Config
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", name, err)
	}
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return c, fmt.Errorf("%s: unexpected content after the configuration object", name)
	}
	return c, nil
}

// Read 读文件并 Decode，不校验，供 configure 修正一份当前不合规的配置（例如升级后才被拒的明文 hub 地址）。
func Read(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Decode(bytes.NewReader(b), path)
}

// CheckHub 是 hub 地址的传输规则，register 在发请求之前、run 在加载配置时、hub 来源的更新器在发
// GetRelease 之前调用。https 总是接受；http 只在主机是 loopback IP 字面量、或本地显式放行（insecure）时接受：
// 明文链路上的中间人与 hub 失守等价。localhost 这类名字要经解析，不在豁免内。
func CheckHub(raw string, insecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return fmt.Errorf("hub %q must be an absolute https:// URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if ip, err := netip.ParseAddr(u.Hostname()); err == nil && ip.Unmap().IsLoopback() {
			return nil
		}
		if insecure {
			return nil
		}
		return fmt.Errorf("hub %q uses plain http, so the node token and metrics would travel unencrypted; use https, or accept this explicitly with `heron-agent register --insecure-http` or `heron-agent configure --insecure-http=true`", raw)
	}
	return fmt.Errorf("hub %q must be an https:// URL", raw)
}

// Save 先写临时文件再 rename：token 不会以部分写入的状态落盘。
//
// 权限不变式：落盘后的配置对其他用户不可读。它由"临时文件一定是本次独占
// 创建的"承载：先删掉可能残留的同名临时文件，再以 O_EXCL 创建——WriteFile
// 的权限参数只对新建文件生效，残留的 0644 临时文件会带着旧权限被 rename 成配置。
//
// 属主不变式：目标已存在时，新文件沿用它的属主与属组。安装脚本把配置交给服务用户，root 执行 configure
// 后若换成 root 属主的 0600 文件，服务就读不到配置；改不了属主（非 root 改别人的文件）时报错而不是留下读不到的配置。
func Save(path string, c Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	owner := -1
	group := -1
	if st, err := os.Stat(path); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			owner, group = int(sys.Uid), int(sys.Gid)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp := path + ".tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if owner >= 0 {
		if err := f.Chown(owner, group); err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("keep the owner of %s: %w", path, err)
		}
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
