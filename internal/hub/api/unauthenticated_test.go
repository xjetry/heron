package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 未认证错误是调用方唯一能看到的指路信息：AI 等程序化调用方不持有管理员密码，缺凭据时必须被指向
// API token（Bearer 头与面板上创建它的页面），而不是只被告知去 Login；只接受会话的方法要直说 token 不行。
func TestUnauthenticatedMessagesPointToTheUsableCredential(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	unknown := "heron_at_" + strings.Repeat("0", 64)
	cases := []struct {
		name, method, authorization string
		want                        []string
		notWant                     []string
	}{
		{"read without credentials", "ListNodes", "", []string{"Authorization: Bearer <API token>", "API token page", "Login"}, nil},
		{"change without credentials", "ExecuteChange", "", []string{"Authorization: Bearer <API token>", "preauthorize", "API token page", "Login"}, nil},
		{"session-only without credentials", "ListApiTokens", "", []string{"panel session", "Login", "API tokens cannot"}, []string{"Authorization: Bearer <API token>"}},
		{"non-bearer scheme", "ListNodes", "Token " + unknown, []string{"Bearer scheme", "Authorization: Bearer <API token>"}, []string{unknown}},
		{"unknown token", "ListNodes", "Bearer " + unknown, []string{"unknown or revoked", "API token page"}, []string{unknown}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+"/heron.v1.AdminService/"+c.method, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if c.authorization != "" {
				req.Header.Set("Authorization", c.authorization)
			}
			resp, err := h.srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, body %s", resp.StatusCode, body)
			}
			var e struct{ Message string }
			if err := json.Unmarshal(body, &e); err != nil {
				t.Fatalf("decode %s: %v", body, err)
			}
			for _, w := range c.want {
				if !strings.Contains(e.Message, w) {
					t.Errorf("message %q lacks %q", e.Message, w)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(e.Message, w) {
					t.Errorf("message %q contains %q", e.Message, w)
				}
			}
		})
	}
}
