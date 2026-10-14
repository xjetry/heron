package releasestamp

import (
	"bytes"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const src = "#!/bin/sh\nRELEASE_VERSION=\"\"\nRELEASE_SHA256=\"\"\n" + Begin + "\n" + End + "\necho \"$RELEASE_VERSION\"\nprintf '%s\\n' \"$RELEASE_SHA256\"\n"

func asset(name, body string) Asset { return Asset{Name: name, SHA256: sha256.Sum256([]byte(body))} }

// 写入后的脚本在 sh 里求值得到的就是写进去的版本号与清单，清单按文件名排序，与传入顺序无关；
// 同一组输入写两次逐字节相同。
func TestScriptEmbedsSortedManifest(t *testing.T) {
	b := asset("heron-agent_linux_amd64.tar.gz", "b")
	a := asset("heron-agent_darwin_arm64.tar.gz", "a")
	out, err := Script([]byte(src), "v1.2.3-rc.1", []Asset{b, a})
	if err != nil {
		t.Fatal(err)
	}
	again, err := Script([]byte(src), "v1.2.3-rc.1", []Asset{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, again) {
		t.Fatalf("not deterministic:\n%s\n---\n%s", out, again)
	}
	got, err := exec.Command("sh", "-c", string(out)).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
	want := "v1.2.3-rc.1\n" +
		"ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb  heron-agent_darwin_arm64.tar.gz\n" +
		"3e23e8160039594a33894f6564e1b1348bbd7a0088d42c4acb73eeaed59c009d  heron-agent_linux_amd64.tar.gz\n"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// 写入区之外逐字节不变。
	if !strings.HasPrefix(string(out), src[:strings.Index(src, End)]) || !strings.HasSuffix(string(out), src[strings.Index(src, End):]) {
		t.Fatalf("bytes outside the region changed:\n%s", out)
	}
}

// 能改写 shell 源码的值一律拒绝：引号、换行、空白、$、非 ASCII，以及以 . 或 - 开头的写法。
func TestScriptRefusesValuesOutsideTheCharset(t *testing.T) {
	ok := asset("heron-hub_linux_amd64.tar.gz", "x")
	for _, v := range []string{"", "v1'x'", "v1\nx", "v1 x", "v1$(id)", "-v1", ".v1", "v1+meta", "v1é", strings.Repeat("a", 129)} {
		if _, err := Script([]byte(src), v, []Asset{ok}); err == nil || !strings.Contains(err.Error(), "version") {
			t.Errorf("version %q accepted (err %v)", v, err)
		}
	}
	for _, n := range []string{"a'.tar.gz", "a b.tar.gz", "a\n.tar.gz", "$x.tar.gz", "-a.tar.gz", ".a.tar.gz", "a.tar", "SHA256SUMS", "a/b.tar.gz", "é.tar.gz"} {
		if _, err := Script([]byte(src), "v1", []Asset{asset(n, "x")}); err == nil || !strings.Contains(err.Error(), "asset name") {
			t.Errorf("asset name %q accepted (err %v)", n, err)
		}
	}
	if _, err := Script([]byte(src), "v1", []Asset{ok, ok}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate asset accepted (err %v)", err)
	}
	if _, err := Script([]byte(src), "v1", nil); err == nil {
		t.Error("empty manifest accepted")
	}
	if _, err := Script([]byte(src), strings.Repeat("a", 128), []Asset{ok}); err != nil {
		t.Errorf("128-character version refused: %v", err)
	}
}

// 边界必须可机器判定：两行各一次、整行相同、相邻。已写入的脚本不再写入。
func TestScriptRegionMustBeEmptyAndUnique(t *testing.T) {
	ok := []Asset{asset("heron-hub_linux_amd64.tar.gz", "x")}
	stamped, err := Script([]byte(src), "v1", ok)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]string{
		"stamped":          string(stamped),
		"no markers":       "#!/bin/sh\n",
		"end only":         End + "\n",
		"end before begin": End + "\n" + Begin + "\n",
		"begin twice":      Begin + "\n" + End + "\n" + Begin + "\n",
		"end twice":        Begin + "\n" + End + "\n" + End + "\n",
		"indented":         "  " + Begin + "\n" + End + "\n",
		"content between":  Begin + "\n:\n" + End + "\n",
	} {
		if _, err := Script([]byte(s), "v1", ok); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// WriteDir 只收 dir 下的 *.tar.gz，输出到 dir 并保留源码的权限位；dir 里没有 tar 包即失败。
func TestWriteDir(t *testing.T) {
	d := t.TempDir()
	dist := filepath.Join(d, "dist")
	os.Mkdir(dist, 0o755)
	script := filepath.Join(d, "install.sh")
	if err := os.WriteFile(script, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteDir("v1", dist, script); err == nil || !strings.Contains(err.Error(), "no *.tar.gz") {
		t.Fatalf("empty dir: %v", err)
	}
	for name, body := range map[string]string{"heron-hub_linux_amd64.tar.gz": "h", "heron-updater_linux_amd64.tar.gz": "u", "SHA256SUMS": "ignored", "notes.txt": "ignored"} {
		os.WriteFile(filepath.Join(dist, name), []byte(body), 0o644)
	}
	if err := WriteDir("v1", dist, script); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(dist, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := Script([]byte(src), "v1", []Asset{asset("heron-hub_linux_amd64.tar.gz", "h"), asset("heron-updater_linux_amd64.tar.gz", "u")})
	if !bytes.Equal(out, want) {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	if st, _ := os.Stat(filepath.Join(dist, "install.sh")); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v, want 0755", st.Mode())
	}
}

// 三个源码安装脚本都带一个空的写入区，WriteDir 能写入。
func TestSourceScriptsHaveAnEmptyRegion(t *testing.T) {
	ok := []Asset{asset("heron-hub_linux_amd64.tar.gz", "x")}
	for _, s := range []string{"install.sh", "install-macos.sh", "install-hub.sh"} {
		b, err := os.ReadFile(filepath.Join("..", s))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Script(b, "v1", ok); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
}
