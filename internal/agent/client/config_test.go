package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// LoadConfig 校验整份配置：hub 地址的传输规则与本地探测策略。解析本身的严格性（未知字段、对象后多余内容）
// 在 agentconfig 包内钉住，这里只留准入两类用例。
func TestLoadConfigValidates(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"https", `{"hub":"https://h","token":"t","name":"n"}`, ""},
		{"loopback_http", `{"hub":"http://127.0.0.1:8080","token":"t"}`, ""},
		{"plain_http", `{"hub":"http://10.0.0.1:8080","token":"t"}`, "uses plain http"},
		{"insecure_http", `{"hub":"http://10.0.0.1:8080","token":"t","insecure_http":true}`, ""},
		{"policy", `{"hub":"https://h","token":"t","probe_allow":["127.0.0.0/8"],"probe_deny":["10.0.0.0/8"]}`, ""},
		{"bad_policy", `{"hub":"https://h","token":"t","probe_deny":["10.1.2.3/8"]}`, "host bits set"},
	} {
		_, err := LoadConfig(writeConfig(t, tc.body))
		if tc.want == "" && err != nil {
			t.Errorf("%s rejected: %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err=%v, want %q", tc.name, err, tc.want)
		}
	}
}
