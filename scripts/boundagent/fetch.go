// 取绑定版本 vY 的 release 元数据与 agent 安装脚本。接受与否只由发行签名决定：传输不参与信任，
// 所以用普通 HTTP 客户端（跟随 GitHub 下载地址的重定向），不用 githubtransport 的地址限制。
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/xjetry/heron/internal/releasesig"
	"github.com/xjetry/heron/internal/update"
)

const (
	officialDownloads = "https://github.com/xjetry/heron/releases/download/"
	// maxInstaller 是单个安装脚本的大小上限；现有脚本在 30 KiB 量级。
	maxInstaller = 1 << 20
)

// release 是已验签的一份 SHA256SUMS：digests 是资产名到小写十六进制 SHA-256。
type release struct {
	version string
	sums    []byte
	digests map[string]string
}

// fetchRelease 取回 version 的 SHA256SUMS 与签名，用 keys 验签（正式命令传 releasesig.Trusted()，测试传 sigtest
// 的公钥），解析清单。version 必须是正式版：只发 hub 只绑定正式版，预发布的签名不该被当作绑定依据。
func fetchRelease(ctx context.Context, client *http.Client, base string, keys []ed25519.PublicKey, version string) (release, error) {
	if !update.ValidVersion(version) {
		return release{}, fmt.Errorf("bound agent version %q is not a stable release", version)
	}
	sums, err := get(ctx, client, base+version+"/SHA256SUMS", releasesig.MaxSums)
	if err != nil {
		return release{}, err
	}
	sig, err := get(ctx, client, base+version+"/SHA256SUMS.sig", releasesig.MaxFile)
	if err != nil {
		return release{}, err
	}
	if err := releasesig.Verify(keys, version, sums, sig); err != nil {
		return release{}, fmt.Errorf("verify %s SHA256SUMS: %w", version, err)
	}
	digests, err := parseSums(sums)
	if err != nil {
		return release{}, fmt.Errorf("%s SHA256SUMS: %w", version, err)
	}
	return release{version: version, sums: sums, digests: digests}, nil
}

// get 取 url 的正文，至多 limit 字节；非 200 或超限即错误。
func get(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	// 多读一个字节是为了区分"恰好 limit 字节"与"超过 limit"：正文比上限还大时拒绝，不带截断的字节继续走。
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("GET %s: body exceeds %d bytes", url, limit)
	}
	return body, nil
}

// parseSums 解析 sha256sum 的输出：每行 "<64 位小写十六进制>  <名字>"（两个空格），末尾一个换行；格式不对或名字重复即错误。
func parseSums(sums []byte) (map[string]string, error) {
	if !bytesHasSuffixNewline(sums) {
		return nil, errors.New("not a sha256sum listing: must end with a newline")
	}
	digests := make(map[string]string)
	for i, line := range strings.Split(string(sums[:len(sums)-1]), "\n") {
		if len(line) < 67 || line[64] != ' ' || line[65] != ' ' || strings.ContainsAny(line[66:], " \t") {
			return nil, fmt.Errorf("line %d: want \"<64 hex>  <name>\", got %q", i+1, line)
		}
		digest := line[:64]
		if !isLowerHex(digest) {
			return nil, fmt.Errorf("line %d: digest %q is not 64 lowercase hex digits", i+1, digest)
		}
		name := line[66:]
		if _, dup := digests[name]; dup {
			return nil, fmt.Errorf("duplicate entry for %s", name)
		}
		digests[name] = digest
	}
	return digests, nil
}

func bytesHasSuffixNewline(b []byte) bool {
	return len(b) > 0 && b[len(b)-1] == '\n'
}

func isLowerHex(s string) bool {
	for _, ch := range s {
		switch {
		case ch >= '0' && ch <= '9', ch >= 'a' && ch <= 'f':
		default:
			return false
		}
	}
	return true
}

// agentBundle 返回 agent 组在 SHA256SUMS 里必须出现的资产名。
func agentBundle(linuxArches, darwinArches []string) []string {
	var names []string
	for _, a := range linuxArches {
		names = append(names, "heron-agent_linux_"+a+".tar.gz", "heron-updater_linux_"+a+".tar.gz")
	}
	for _, a := range darwinArches {
		names = append(names, "heron-agent_darwin_"+a+".tar.gz")
	}
	return append(names, "install.sh", "install-macos.sh")
}

// fetchInstallers 核对 vY 的清单含完整的 agent 组，取回两个安装脚本并按清单核对摘要，全部通过后才写进 dir
// （先写临时文件再改名）：下载与核对的任何一步失败都不写 dir。
func fetchInstallers(ctx context.Context, client *http.Client, base string, keys []ed25519.PublicKey, version, dir string, linuxArches, darwinArches []string) error {
	rel, err := fetchRelease(ctx, client, base, keys, version)
	if err != nil {
		return err
	}
	for _, name := range agentBundle(linuxArches, darwinArches) {
		if _, ok := rel.digests[name]; !ok {
			return fmt.Errorf("%s SHA256SUMS does not list %s: the release does not carry a complete agent bundle", version, name)
		}
	}
	type installer struct {
		name string
		body []byte
	}
	// 两个脚本都取回并核对通过之后才动磁盘：写一半失败时不能留下一个脚本装 vY、另一个缺失的 dist。
	var verified []installer
	for _, name := range []string{"install.sh", "install-macos.sh"} {
		body, err := get(ctx, client, base+version+"/"+name, maxInstaller)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != rel.digests[name] {
			return fmt.Errorf("%s/%s does not match its SHA256SUMS entry", version, name)
		}
		verified = append(verified, installer{name: name, body: body})
	}
	for _, ins := range verified {
		tmp := filepath.Join(dir, ins.name+".tmp")
		if err := os.WriteFile(tmp, ins.body, 0o755); err != nil {
			return err
		}
	}
	for _, ins := range verified {
		if err := os.Rename(filepath.Join(dir, ins.name+".tmp"), filepath.Join(dir, ins.name)); err != nil {
			return err
		}
	}
	return nil
}
