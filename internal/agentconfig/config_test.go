package agentconfig

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCheckHub(t *testing.T) {
	for _, tc := range []struct {
		hub      string
		insecure bool
		want     string // 空即接受
	}{
		{"https://heron.example.com", false, ""},
		{"https://10.0.0.1:8443/sub", false, ""},
		{"http://127.0.0.1:8080", false, ""},
		{"http://127.9.9.9", false, ""},
		{"http://[::1]:8080", false, ""},
		{"http://[::ffff:127.0.0.1]:8080", false, ""},
		{"http://localhost:8080", false, "uses plain http"},
		{"http://10.0.0.1:8080", false, "uses plain http"},
		{"http://hub.example", false, "uses plain http"},
		{"http://host.docker.internal:8080", false, "uses plain http"},
		{"http://10.0.0.1:8080", true, ""},
		{"http://localhost:8080", true, ""},
		{"ftp://hub.example", true, "must be an https:// URL"},
		{"hub.example", false, "absolute https:// URL"},
		{"", false, "absolute https:// URL"},
		{"http:hub.example", true, "absolute https:// URL"},
	} {
		err := CheckHub(tc.hub, tc.insecure)
		if tc.want == "" && err != nil {
			t.Errorf("%q insecure=%v rejected: %v", tc.hub, tc.insecure, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%q insecure=%v: err=%v, want %q", tc.hub, tc.insecure, err, tc.want)
		}
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Read 严格解析：未知字段与第一个对象之后的任何内容都是错误，报错带文件路径。
func TestReadStrictDecode(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"typo_field", `{"hub":"https://h","token":"t","probe_dney":["10.0.0.0/8"]}`, `unknown field "probe_dney"`},
		{"trailing_space", "{\"hub\":\"https://h\",\"token\":\"t\"}\n\n  ", ""},
		{"second_object", `{"hub":"https://h","token":"t"} {"probe_deny":["10.0.0.0/8"]}`, "unexpected content after the configuration object"},
		{"trailing_garbage", `{"hub":"https://h","token":"t"} trailing garbage`, "unexpected content after the configuration object"},
	} {
		_, err := Read(writeConfig(t, tc.body))
		if tc.want == "" && err != nil {
			t.Errorf("%s rejected: %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err=%v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestDecodeReportsName(t *testing.T) {
	_, err := Decode(strings.NewReader(`{"hub":"https://h","token":"t","bogus":1}`), "/etc/heron-agent/config.json")
	if err == nil || !strings.Contains(err.Error(), "/etc/heron-agent/config.json") || !strings.Contains(err.Error(), `unknown field "bogus"`) {
		t.Fatalf("err = %v", err)
	}
}

// Read 不校验传输规则：configure 要能读出一份被 run 拒绝的配置去修正它。
func TestReadSkipsValidation(t *testing.T) {
	c, err := Read(writeConfig(t, `{"hub":"http://10.0.0.1:8080","token":"t"}`))
	if err != nil || c.Hub != "http://10.0.0.1:8080" {
		t.Fatalf("%+v %v", c, err)
	}
}

// root 改写服务用户拥有的配置后，属主仍是服务用户。只有 root 能把文件交给别人，所以这条只在 root 下跑；
// 非 root 下跳过会让它恒绿，验收在容器里以 root 跑一次（见提交说明）。
func TestSaveKeepsOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to hand the file to another user")
	}
	p := filepath.Join(t.TempDir(), "config.json")
	if err := Save(p, Config{Hub: "https://h", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(p, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	if err := Save(p, Config{Hub: "https://h", Token: "t2"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	sys := st.Sys().(*syscall.Stat_t)
	if sys.Uid != 65534 || sys.Gid != 65534 || st.Mode().Perm() != 0o600 {
		t.Fatalf("owner=%d:%d perm=%o, want 65534:65534 600", sys.Uid, sys.Gid, st.Mode().Perm())
	}
}
