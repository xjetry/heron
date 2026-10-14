// 绑定版本的端到端清单与发布回读：pin 与 fetch 读同一份已验签的 SHA256SUMS，一个写兼容清单，
// 一个复制安装脚本；readback 在 release 发布之后从 Release 页面取回的文件上核对复制的脚本仍是绑定版本那两份。
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// officialRepository 从 officialDownloads 推出清单里的 repository：下载脚本按它拼下载地址，
// 两者必须指向同一个仓库，不各写一份。
func officialRepository() string {
	s := strings.TrimPrefix(officialDownloads, "https://github.com/")
	return strings.TrimSuffix(s, "/releases/download/")
}

// e2eArches 是端到端容器实际运行的 Linux 架构（下载脚本取的就是这两个包），也是清单 assets
// 的全部成员；agent 组的其余架构不进端到端，不写清单。
var e2eArches = []string{"amd64", "arm64"}

type pinAsset struct {
	Arch   string `json:"arch"`
	SHA256 string `json:"sha256"`
}

type pinFile struct {
	Repository string     `json:"repository"`
	Tag        string     `json:"tag"`
	Assets     []pinAsset `json:"assets"`
}

// pinManifest 取指定版本已验签的清单，核对 agent 组完整后，把端到端用到的两个 Linux 架构的 agent 包摘要写成
// 清单文件（先写临时文件再改名）。只发 hub 的 release 实际发出去的组合是 hub 加绑定版本的 agent，端到端按这份
// 清单下载，信任根与 fetch 相同：已验签的 SHA256SUMS，而不是下载时临时取得的校验和。
func pinManifest(ctx context.Context, client *http.Client, base string, keys []ed25519.PublicKey, version, out string, linuxArches, darwinArches []string) error {
	rel, err := fetchRelease(ctx, client, base, keys, version)
	if err != nil {
		return err
	}
	// 完整性核对的是整个 agent 组，不只是端到端用到的两个架构：被绑定的 release 必须带齐 agent 产物，只核
	// 对清单用到的两项会把"能跑端到端但产物发不齐"的版本放进绑定。
	for _, name := range agentBundle(linuxArches, darwinArches) {
		if _, ok := rel.digests[name]; !ok {
			return fmt.Errorf("%s SHA256SUMS does not list %s: the release does not carry a complete agent bundle", version, name)
		}
	}
	assets := make([]pinAsset, 0, len(e2eArches))
	for _, arch := range e2eArches {
		// 上面按 agentBundle 核对过完整组，heron-agent_linux_<e2e 架构> 必然在清单里。
		assets = append(assets, pinAsset{Arch: arch, SHA256: rel.digests["heron-agent_linux_"+arch+".tar.gz"]})
	}
	// fetchRelease 只接受正式版。清单只有 repository、tag、assets。
	body, err := json.MarshalIndent(pinFile{Repository: officialRepository(), Tag: version, Assets: assets}, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, out)
}

// readbackInstallers 核对 vX 的 release 实际可下载的 agent 安装脚本就是 vY 那两份。DIR 里是从 vX 的 Release
// 页面取回的 SHA256SUMS、install.sh 与 install-macos.sh；核对两层：脚本字节本身的 SHA-256 等于 vY 已验签清单
// 里的摘要（脚本装的是 vY），且 vX 的 SHA256SUMS 里这两行的摘要与之相同（发布流程没有在复制之后改动它们）。
// 任一层不符都意味着 latest 的安装命令装的不是 hub 绑定的 agent。
func readbackInstallers(ctx context.Context, client *http.Client, base string, keys []ed25519.PublicKey, version, bound, dir string) error {
	rel, err := fetchRelease(ctx, client, base, keys, bound)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	releaseDigests, err := parseSums(raw)
	if err != nil {
		return fmt.Errorf("%s SHA256SUMS: %w", version, err)
	}
	for _, name := range []string{"install.sh", "install-macos.sh"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		actual := hex.EncodeToString(sum[:])
		if actual != rel.digests[name] {
			return fmt.Errorf("%s/%s is not the bound agent %s installer: sha256 %s, want %s", version, name, bound, actual, rel.digests[name])
		}
		// 清单行单独核对而不是只比字节：字节相同但 vX 的 SHA256SUMS 行不同说明发布产物与回读字节不一致，
		// 依赖清单行的读者（在线更新、人工核对）拿到的是另一个值。
		listed, ok := releaseDigests[name]
		if !ok {
			return fmt.Errorf("%s SHA256SUMS does not list %s", version, name)
		}
		if listed != rel.digests[name] {
			return fmt.Errorf("%s SHA256SUMS lists %s with sha256 %s, want the bound agent %s digest %s", version, name, listed, bound, rel.digests[name])
		}
	}
	return nil
}
