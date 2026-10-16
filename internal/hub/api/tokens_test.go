package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/testwait"
)

type rawResult struct {
	status  int
	code    string
	message string
}

// rawCall 用纯 HTTP+JSON 调一个 AdminService 方法，headers 原样加到请求上；不带 cookie jar。
func rawCall(t *testing.T, h *harness, method string, body string, headers map[string][]string) rawResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/heron.v1.AdminService/"+method, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var e struct{ Code, Message string }
	json.NewDecoder(resp.Body).Decode(&e)
	return rawResult{resp.StatusCode, e.Code, e.Message}
}

func bearer(tok string) map[string][]string {
	return map[string][]string{"Authorization": {"Bearer " + tok}}
}

// sessionCookieHeader 取 harness 登录后 jar 里的会话 cookie，供手工拼请求头。
// 包里已有 sessionCookie 构造 Set-Cookie，不能同名。
func sessionCookieHeader(t *testing.T, h *harness) string {
	t.Helper()
	u, _ := http.NewRequest(http.MethodGet, h.srv.URL, nil)
	for _, c := range h.http.Jar.Cookies(u.URL) {
		if c.Name == SessionCookie {
			return SessionCookie + "=" + c.Value
		}
	}
	t.Fatal("no session cookie after login")
	return ""
}

func createToken(t *testing.T, h *harness, name string) (int64, string) {
	t.Helper()
	resp, err := h.admin.CreateApiToken(context.Background(), connect.NewRequest(&heronv1.CreateApiTokenRequest{Name: name}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetApiToken().GetId(), resp.Msg.GetToken()
}

func TestAPITokenReachesExactlyTheReadMethods(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	_, tok := createToken(t, h, "matrix")
	svc := adminService()
	for i := 0; i < svc.Methods().Len(); i++ {
		name := string(svc.Methods().Get(i).Name())
		got := rawCall(t, h, name, "{}", bearer(tok))
		if h.svc.access["/heron.v1.AdminService/"+name] == heronv1.Access_ACCESS_READ {
			if got.code == "unauthenticated" || got.code == "permission_denied" {
				t.Errorf("%s: read method refused a valid token: %+v", name, got)
			}
			continue
		}
		if name == "Login" || name == "BeginPasskeyLogin" || name == "FinishPasskeyLogin" {
			if got.status != http.StatusForbidden || got.code != "permission_denied" ||
				!strings.Contains(got.message, name) || !strings.Contains(got.message, "cannot log in") {
				t.Errorf("%s: %+v, want 403 permission_denied naming the method and that tokens cannot log in", name, got)
			}
			continue
		}
		if got.status != http.StatusForbidden || got.code != "permission_denied" ||
			!strings.Contains(got.message, name) || !strings.Contains(got.message, "ExecuteChange") {
			t.Errorf("%s: %+v, want 403 permission_denied naming the method and the preauthorized write entry", name, got)
		}
	}
}

func TestBearerAndCookiePathsNeverFallBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	_, tok := createToken(t, h, "cross")
	cookie := sessionCookieHeader(t, h)
	cases := []struct {
		name    string
		method  string
		headers map[string][]string
		status  int
	}{
		{"valid cookie + invalid bearer", "ListNodes", map[string][]string{"Cookie": {cookie}, "Authorization": {"Bearer " + auth.APITokenPrefix + strings.Repeat("0", 64)}}, 401},
		{"valid bearer + invalid cookie", "ListNodes", map[string][]string{"Cookie": {SessionCookie + "=garbage"}, "Authorization": {"Bearer " + tok}}, 200},
		{"valid cookie + Basic (reverse proxy auth) on read", "ListNodes", map[string][]string{"Cookie": {cookie}, "Authorization": {"Basic dXNlcjpwYXNz"}}, 200},
		{"valid cookie + Basic (reverse proxy auth) on write", "CreateNode", map[string][]string{"Cookie": {cookie}, "Authorization": {"Basic dXNlcjpwYXNz"}}, 200},
		{"lowercase scheme is still bearer", "ListNodes", map[string][]string{"Authorization": {"bearer " + tok}}, 200},
		{"empty bearer + valid cookie", "ListNodes", map[string][]string{"Cookie": {cookie}, "Authorization": {"Bearer "}}, 401},
		{"two bearer headers", "ListNodes", map[string][]string{"Authorization": {"Bearer " + tok, "Bearer " + tok}}, 401},
		{"valid bearer cannot mint tokens", "CreateApiToken", map[string][]string{"Cookie": {cookie}, "Authorization": {"Bearer " + tok}}, 403},
	}
	for _, c := range cases {
		body := "{}"
		if c.method == "CreateNode" {
			body = `{"name":"via-basic"}`
		}
		if got := rawCall(t, h, c.method, body, c.headers); got.status != c.status {
			t.Errorf("%s: status %d (%s %q), want %d", c.name, got.status, got.code, got.message, c.status)
		}
	}
}

func TestCredentialsDoNotCrossServices(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	_, apiTok := createToken(t, h, "x")
	_, nodeTok := h.createNode(t, "n1")
	if err := h.report(t, apiTok, &heronv1.Metrics{}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("API token accepted by AgentService.Report: %v", err)
	}
	if got := rawCall(t, h, "ListNodes", "{}", bearer(nodeTok)); got.status != 401 {
		t.Errorf("node token accepted by AdminService: %+v", got)
	}
}

func TestRevocationTakesEffectOnTheNextRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, tok := createToken(t, h, "gone")
	if got := rawCall(t, h, "ListNodes", "{}", bearer(tok)); got.status != 200 {
		t.Fatalf("before revocation: %+v", got)
	}
	if _, err := h.admin.DeleteApiToken(context.Background(), connect.NewRequest(&heronv1.DeleteApiTokenRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if got := rawCall(t, h, "ListNodes", "{}", bearer(tok)); got.status != 401 {
		t.Fatalf("after DeleteApiToken: %+v", got)
	}
	// heron-hub token revoke 在另一进程里直接删行：不经 api 层，同样必须立即生效。
	id2, tok2 := createToken(t, h, "gone-offline")
	if _, err := h.store.DeleteAPIToken(context.Background(), id2); err != nil {
		t.Fatal(err)
	}
	if got := rawCall(t, h, "ListNodes", "{}", bearer(tok2)); got.status != 401 {
		t.Fatalf("after direct row delete: %+v", got)
	}
}

func TestAPITokenManagementValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	for _, name := range []string{"", "  \x01  ", strings.Repeat("字", 65)} {
		_, err := h.admin.CreateApiToken(ctx, connect.NewRequest(&heronv1.CreateApiTokenRequest{Name: name}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("name %q: %v, want InvalidArgument", name, err)
		}
	}
	resp, err := h.admin.CreateApiToken(ctx, connect.NewRequest(&heronv1.CreateApiTokenRequest{Name: "  ci\x07  "}))
	if err != nil || resp.Msg.GetApiToken().GetName() != "ci" || !strings.HasPrefix(resp.Msg.GetToken(), auth.APITokenPrefix) {
		t.Fatalf("cleaned create: %v %v", resp, err)
	}
	for i := 1; i < auth.MaxAPITokens; i++ {
		createToken(t, h, "n")
	}
	_, err = h.admin.CreateApiToken(ctx, connect.NewRequest(&heronv1.CreateApiTokenRequest{Name: "one too many"}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "100") {
		t.Fatalf("token %d: %v, want ResourceExhausted naming the limit", auth.MaxAPITokens+1, err)
	}
	_, err = h.admin.DeleteApiToken(ctx, connect.NewRequest(&heronv1.DeleteApiTokenRequest{Id: 999999}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("delete unknown: %v, want NotFound", err)
	}
}

func TestListApiTokensShowsLastUse(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	_, tok := createToken(t, h, "seen")
	createToken(t, h, "unseen")
	rawCall(t, h, "ListNodes", "{}", bearer(tok))
	testwait.Until(t, 5*time.Millisecond, func() bool {
		resp, err := h.admin.ListApiTokens(context.Background(), connect.NewRequest(&heronv1.ListApiTokensRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		list := resp.Msg.GetTokens()
		if len(list) != 2 || list[0].GetName() != "seen" || list[1].GetName() != "unseen" {
			t.Fatalf("list: %v", list)
		}
		if list[1].LastUsedAt != nil {
			t.Fatalf("unused token reports a last use: %v", list[1])
		}
		if list[0].LastUsedAt == nil {
			return false
		}
		if list[0].GetLastUsedAt() != h.clk.Now().Unix() {
			t.Fatalf("last used %d, want %d", list[0].GetLastUsedAt(), h.clk.Now().Unix())
		}
		return true
	}, "last use never recorded")
}

// 经真实 HTTP 入口。UpdateAPIToken 走同一个写协程，排在它之前入队的异步刷新之后，所以拿它当屏障断言"过期的请求没有
// 刷新使用时刻"。
func TestExpiredTokenIsRefusedAtTheEntryAndRevivable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, tok := createToken(t, h, "temp")
	ctx := context.Background()
	if _, err := h.store.UpdateAPIToken(ctx, id, "temp", nil, h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	got := rawCall(t, h, "ListNodes", "{}", bearer(tok))
	want := "API token expired at 2026-01-01T00:00:00Z; an administrator can extend it with UpdateApiToken or issue a new one"
	if got.status != http.StatusUnauthorized || got.code != "unauthenticated" || got.message != want {
		t.Fatalf("expired token: %+v", got)
	}
	if _, err := h.store.UpdateAPIToken(ctx, id, "temp", nil, h.clk.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	list, err := h.store.ListAPITokens(ctx)
	if err != nil || len(list) != 1 || !list[0].LastUsedAt.IsZero() {
		t.Fatalf("expired request counted as a use: %+v %v", list, err)
	}
	if got := rawCall(t, h, "ListNodes", "{}", bearer(tok)); got.status != http.StatusOK {
		t.Fatalf("revived token: %+v", got)
	}
}
