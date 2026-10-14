package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/xjetry/heron/internal/releasesig/sigtest"
)

// pinAssetJSON 与 pinManifest 写出的 assets 项同形。
type pinAssetJSON struct {
	Arch   string `json:"arch"`
	SHA256 string `json:"sha256"`
}

type pinJSON struct {
	Repository string         `json:"repository"`
	Tag        string         `json:"tag"`
	Assets     []pinAssetJSON `json:"assets"`
}

// pinInto 在临时目录上执行 pinManifest：出错时不得写出清单文件。
func pinInto(t *testing.T, srv *httptest.Server, version string) (string, error) {
	t.Helper()
	pub, _ := sigtest.Key()
	out := filepath.Join(t.TempDir(), "pin.json")
	err := pinManifest(context.Background(), srv.Client(), srv.URL+"/", []ed25519.PublicKey{pub}, version, out, fetchLinux, fetchDarwin)
	if err != nil {
		if _, statErr := os.Stat(out); statErr == nil {
			t.Errorf("%s: pin wrote %s despite failing", t.Name(), out)
		}
	}
	return out, err
}

func TestPinWritesBoundAgentDigests(t *testing.T) {
	installers := testInstallers()
	sums := bundleSums(t, installers, "")
	srv, _ := serveRelease(t, "v1.2.3", "v1.2.3", sums, installers)
	pub, _ := sigtest.Key()
	out := filepath.Join(t.TempDir(), "pin.json")
	if err := pinManifest(context.Background(), srv.Client(), srv.URL+"/", []ed25519.PublicKey{pub}, "v1.2.3", out, fetchLinux, fetchDarwin); err != nil {
		t.Fatalf("pinManifest: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read pin: %v", err)
	}
	var pin pinJSON
	if err := json.Unmarshal(raw, &pin); err != nil {
		t.Fatalf("pin is not JSON: %v", err)
	}
	if pin.Tag != "v1.2.3" {
		t.Errorf("tag = %q, want v1.2.3", pin.Tag)
	}
	// 清单顶层只有 repository、tag、assets 这三个键，与 scripts/pinned-agent-download.sh 的 jq -e 同一口径。
	// 多出来的键必须在这里失败，不能靠解码进结构体时被丢掉。
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("pin is not a JSON object: %v", err)
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if got, want := strings.Join(keys, ","), "assets,repository,tag"; got != want {
		t.Errorf("pin keys = %q, want %q", got, want)
	}
	if len(pin.Assets) != 2 || pin.Assets[0].Arch != "amd64" || pin.Assets[1].Arch != "arm64" {
		t.Fatalf("assets = %+v, want exactly amd64 and arm64", pin.Assets)
	}
	for _, asset := range pin.Assets {
		want, err := sumsDigest(sums, "heron-agent_linux_"+asset.Arch+".tar.gz")
		if err != nil {
			t.Fatal(err)
		}
		if asset.SHA256 != want {
			t.Errorf("assets[%s].sha256 = %s, want the SHA256SUMS digest %s", asset.Arch, asset.SHA256, want)
		}
	}
	// 与 scripts/pinned-agent-download.sh 的 jq -e 同一口径：清单不满足这些条件时下载入口直接拒绝，pin 写出的文件必须
	// 天然过得了那一关，否则指定版本的端到端根本不会跑。fetchRelease 只接受正式版，tag 也按正式版核对。
	if pin.Repository != "xjetry/heron" {
		t.Errorf("repository = %q, want xjetry/heron", pin.Repository)
	}
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(pin.Tag) {
		t.Errorf("tag %q is not a stable release tag", pin.Tag)
	}
	arches := []string{pin.Assets[0].Arch, pin.Assets[1].Arch}
	if strings.Join(arches, ",") != "amd64,arm64" {
		t.Errorf("assets arches = %v, want [amd64 arm64]", arches)
	}
	for _, asset := range pin.Assets {
		if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(asset.SHA256) {
			t.Errorf("assets[%s].sha256 = %q, want 64 lowercase hex digits", asset.Arch, asset.SHA256)
		}
	}
}

func TestPinRejectsUnverifiedSums(t *testing.T) {
	installers := testInstallers()
	// 签名对 v1.2.2 的清单有效：验签不过，摘要还没有可信来源，不得写出清单。
	srv, _ := serveRelease(t, "v1.2.3", "v1.2.2", bundleSums(t, installers, ""), installers)
	if _, err := pinInto(t, srv, "v1.2.3"); err == nil || !strings.Contains(err.Error(), "verify") {
		t.Fatalf("pinManifest with a signature for another version = %v, want a verification error", err)
	}
}

func TestPinRejectsIncompleteAgentBundle(t *testing.T) {
	installers := testInstallers()
	for _, missing := range []string{"heron-agent_linux_riscv64.tar.gz", "heron-agent_darwin_amd64.tar.gz"} {
		srv, _ := serveRelease(t, "v1.2.3", "v1.2.3", bundleSums(t, installers, missing), installers)
		_, err := pinInto(t, srv, "v1.2.3")
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("pinManifest without %s = %v, want an error naming it", missing, err)
		}
	}
}

// sumsDigest 从一份 sha256sum 清单里取某个资产的摘要（测试里清单即事实源）。
func sumsDigest(sums []byte, name string) (string, error) {
	digests, err := parseSums(sums)
	if err != nil {
		return "", err
	}
	digest, ok := digests[name]
	if !ok {
		return "", fmt.Errorf("%s is not listed", name)
	}
	return digest, nil
}

// readbackDir 按 vX 的 Release 页面回读后的样子布置 DIR：SHA256SUMS 与两个安装脚本。overrideName 与
// overrideDigest 改写 vX 清单里某一行的摘要，tamper 给某个脚本的字节加一个后缀，用来构造回读必须拦下的差异。
func readbackDir(t *testing.T, installers map[string][]byte, overrideName, overrideDigest, tamper string) string {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	for _, name := range []string{"heron-hub_linux_amd64.tar.gz", "heron-hub_linux_arm64.tar.gz", "install.sh", "install-macos.sh"} {
		digest := strings.Repeat("b", 64)
		if body, ok := installers[name]; ok {
			sum := sha256.Sum256(body)
			digest = hex.EncodeToString(sum[:])
			if name == overrideName {
				digest = overrideDigest
			}
		}
		fmt.Fprintf(&b, "%s  %s\n", digest, name)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range installers {
		if name == tamper {
			body = append(append([]byte{}, body...), '#')
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// readbackOn 在 vY=v1.2.3 的服务端上核对 vX 的回读目录。
func readbackOn(t *testing.T, srv *httptest.Server, dir, version string) error {
	t.Helper()
	pub, _ := sigtest.Key()
	return readbackInstallers(context.Background(), srv.Client(), srv.URL+"/", []ed25519.PublicKey{pub}, version, "v1.2.3", dir)
}

func TestReadbackAcceptsCopiedInstallers(t *testing.T) {
	installers := testInstallers()
	srv, _ := serveRelease(t, "v1.2.3", "v1.2.3", bundleSums(t, installers, ""), installers)
	dir := readbackDir(t, installers, "", "", "")
	if err := readbackOn(t, srv, dir, "v1.3.0"); err != nil {
		t.Fatalf("readbackInstallers: %v", err)
	}
}

func TestReadbackRejectsDifferentInstaller(t *testing.T) {
	installers := testInstallers()
	srv, _ := serveRelease(t, "v1.2.3", "v1.2.3", bundleSums(t, installers, ""), installers)
	// 回读目录里的 install.sh 多一个字节：vX 的 SHA256SUMS 行照旧，但脚本本身不是 vY 那份——回读必须读
	// 字节，不能只信清单行。
	dir := readbackDir(t, installers, "", "", "install.sh")
	err := readbackOn(t, srv, dir, "v1.3.0")
	if err == nil || !strings.Contains(err.Error(), "install.sh") {
		t.Fatalf("readbackInstallers with a tampered installer = %v, want an error naming install.sh", err)
	}
}

func TestReadbackRejectsSumsLineMismatch(t *testing.T) {
	installers := testInstallers()
	srv, _ := serveRelease(t, "v1.2.3", "v1.2.3", bundleSums(t, installers, ""), installers)
	// 脚本字节与 vY 相同，但 vX 的 SHA256SUMS 里 install-macos.sh 一行的摘要是别的值。
	dir := readbackDir(t, installers, "install-macos.sh", strings.Repeat("c", 64), "")
	err := readbackOn(t, srv, dir, "v1.3.0")
	if err == nil || !strings.Contains(err.Error(), "install-macos.sh") {
		t.Fatalf("readbackInstallers with a mismatched sums line = %v, want an error naming install-macos.sh", err)
	}
}
