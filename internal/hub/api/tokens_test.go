package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/hub/store"
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

func TestAPITokenNameErrorMessage(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	created, err := h.admin.CreateApiToken(t.Context(), connect.NewRequest(&heronv1.CreateApiTokenRequest{Name: "kept"}))
	if err != nil {
		t.Fatal(err)
	}
	tok := created.Msg.ApiToken
	name := strings.Repeat("名", 65)
	_, createErr := h.admin.CreateApiToken(t.Context(), connect.NewRequest(&heronv1.CreateApiTokenRequest{Name: name}))
	_, updateErr := h.admin.UpdateApiToken(t.Context(), connect.NewRequest(&heronv1.UpdateApiTokenRequest{Id: tok.Id, Name: name, ExpectedVersion: tok.Version}))
	for method, err := range map[string]error{"CreateApiToken": createErr, "UpdateApiToken": updateErr} {
		const want = "invalid_argument: name must be 1–64 characters after trimming whitespace and control characters; got 65"
		if connect.CodeOf(err) != connect.CodeInvalidArgument || err.Error() != want {
			t.Errorf("%s: error = %v, want %q", method, err, want)
		}
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
	_, err = h.admin.CreateApiToken(ctx, connect.NewRequest(&heronv1.CreateApiTokenRequest{Name: "x", Grant: &heronv1.TokenGrant{NodeIds: []int64{999}}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || err.Error() != "invalid_argument: grant.node_ids: node 999 does not exist" {
		t.Fatalf("create with a missing node: %v", err)
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
	if _, err := h.store.UpdateAPIToken(ctx, id, "temp", nil, h.clk.Now(), versionOf(t, h.store, id)); err != nil {
		t.Fatal(err)
	}
	got := rawCall(t, h, "ListNodes", "{}", bearer(tok))
	want := "API token expired at 2026-01-01T00:00:00Z; an administrator can extend it with UpdateApiToken or issue a new one"
	if got.status != http.StatusUnauthorized || got.code != "unauthenticated" || got.message != want {
		t.Fatalf("expired token: %+v", got)
	}
	if _, err := h.store.UpdateAPIToken(ctx, id, "temp", nil, h.clk.Now().Add(time.Hour), versionOf(t, h.store, id)); err != nil {
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

// withBearer 让这次请求以 API token 鉴权：bearer 先于 cookie 判定，harness 客户端 jar 里的会话不参与。
func withBearer[T any](msg *T, tok string) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+tok)
	return req
}

// 编辑不换明文：同一个 bearer 继续鉴权，下一个请求即按新授权读，GetCaller 读到的是保存后的授权。
func TestUpdateApiTokenKeepsPlaintextAndAppliesNextRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	node, err := h.admin.CreateNode(ctx, connect.NewRequest(&heronv1.CreateNodeRequest{Name: "n"}))
	if err != nil {
		t.Fatal(err)
	}
	id, tok := createToken(t, h, "agent")
	expires := h.clk.Now().Add(time.Hour).Unix()
	resp, err := h.admin.UpdateApiToken(ctx, connect.NewRequest(&heronv1.UpdateApiTokenRequest{
		Id: id, Name: "renamed", Grant: &heronv1.TokenGrant{}, ExpiresAt: proto.Int64(expires), ExpectedVersion: listedVersion(t, h, id),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Msg.GetApiToken(); got.GetName() != "renamed" || got.GetGrant().GetAllNodes() || got.GetExpiresAt() != expires {
		t.Fatalf("update response: %+v", got)
	}
	caller, err := h.admin.GetCaller(ctx, withBearer(&heronv1.GetCallerRequest{}, tok))
	if err != nil {
		t.Fatal(err)
	}
	if got := caller.Msg.GetApiToken(); got.GetId() != id || got.GetName() != "renamed" || got.GetGrant().GetAllNodes() || got.GetExpiresAt() != expires {
		t.Fatalf("caller after edit: %+v", caller.Msg)
	}
	listed, err := h.admin.ListNodes(ctx, withBearer(&heronv1.ListNodesRequest{}, tok))
	if err != nil || len(listed.Msg.GetNodes()) != 0 {
		t.Fatalf("narrowed scope still lists node %d: %+v %v", node.Msg.GetNode().GetId(), listed, err)
	}
}

func TestUpdateApiTokenValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	id, _ := createToken(t, h, "agent")
	for name, tc := range map[string]struct {
		req  *heronv1.UpdateApiTokenRequest
		code connect.Code
		want string
	}{
		"missing token":      {&heronv1.UpdateApiTokenRequest{Id: id + 1, Name: "x"}, connect.CodeNotFound, "does not exist"},
		"empty name":         {&heronv1.UpdateApiTokenRequest{Id: id, Name: " "}, connect.CodeInvalidArgument, ""},
		"zero expiry":        {&heronv1.UpdateApiTokenRequest{Id: id, Name: "x", ExpiresAt: proto.Int64(0)}, connect.CodeInvalidArgument, "expires_at"},
		"expiry beyond 9999": {&heronv1.UpdateApiTokenRequest{Id: id, Name: "x", ExpiresAt: proto.Int64(253402300800)}, connect.CodeInvalidArgument, "expires_at"},
		"both scopes":        {&heronv1.UpdateApiTokenRequest{Id: id, Name: "x", Grant: &heronv1.TokenGrant{AllNodes: true, NodeIds: []int64{1}}}, connect.CodeInvalidArgument, "invalid_argument: grant.node_ids: must be empty when all_nodes is true"},
		"missing node":       {&heronv1.UpdateApiTokenRequest{Id: id, Name: "x", Grant: &heronv1.TokenGrant{NodeIds: []int64{999}}}, connect.CodeInvalidArgument, "invalid_argument: grant.node_ids: node 999 does not exist"},
		"no version":         {&heronv1.UpdateApiTokenRequest{Id: id, Name: "x"}, connect.CodeInvalidArgument, "expected_version"},
	} {
		t.Run(name, func(t *testing.T) {
			if name != "no version" {
				tc.req.ExpectedVersion = listedVersion(t, h, id)
			}
			_, err := h.admin.UpdateApiToken(ctx, connect.NewRequest(tc.req))
			if connect.CodeOf(err) != tc.code || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %v containing %q", err, tc.code, tc.want)
			}
		})
	}
	// 只改名时原样带回过去的时刻是合法的：编辑不要求有效期在未来。
	if _, err := h.admin.UpdateApiToken(ctx, connect.NewRequest(&heronv1.UpdateApiTokenRequest{Id: id, Name: "x", ExpiresAt: proto.Int64(h.clk.Now().Unix() - 60), ExpectedVersion: listedVersion(t, h, id)})); err != nil {
		t.Fatalf("past expiry on edit: %v", err)
	}
}

func TestCreateApiTokenExpiryMustBeInTheFuture(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	now := h.clk.Now().Unix()
	if _, err := h.admin.CreateApiToken(ctx, connect.NewRequest(&heronv1.CreateApiTokenRequest{Name: "x", ExpiresAt: proto.Int64(now)})); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "expires_at") {
		t.Fatalf("expiry at now: %v", err)
	}
	created, err := h.admin.CreateApiToken(ctx, connect.NewRequest(&heronv1.CreateApiTokenRequest{Name: "x", ExpiresAt: proto.Int64(now + 1)}))
	if err != nil || created.Msg.GetApiToken().GetExpiresAt() != now+1 {
		t.Fatalf("expiry in one second: %+v %v", created, err)
	}
	list, err := h.admin.ListApiTokens(ctx, connect.NewRequest(&heronv1.ListApiTokensRequest{}))
	if err != nil || list.Msg.GetNow() != now || list.Msg.GetTokens()[0].GetExpiresAt() != now+1 {
		t.Fatalf("list: %+v %v", list, err)
	}
}

// 指定节点的 token 也能读自己：GetCaller 在 scopedReadAllowed 里，不被"requires a site-wide API token"挡掉。
func TestGetCallerForSessionAndScopedToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	session, err := h.admin.GetCaller(ctx, connect.NewRequest(&heronv1.GetCallerRequest{}))
	if err != nil || session.Msg.GetSession() == nil {
		t.Fatalf("session caller: %+v %v", session, err)
	}
	created, err := h.admin.CreateApiToken(ctx, connect.NewRequest(&heronv1.CreateApiTokenRequest{Name: "scoped", Grant: &heronv1.TokenGrant{}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := rawCall(t, h, "GetCaller", "{}", bearer(created.Msg.GetToken())); got.status != http.StatusOK {
		t.Fatalf("scoped token refused: %+v", got)
	}
	caller, err := h.admin.GetCaller(ctx, withBearer(&heronv1.GetCallerRequest{}, created.Msg.GetToken()))
	if err != nil || caller.Msg.GetApiToken().GetName() != "scoped" || caller.Msg.GetApiToken().GetGrant().GetAllNodes() {
		t.Fatalf("scoped caller: %+v %v", caller, err)
	}
}

// versionOf 是 id 当前的 Version()，编辑时带回；token 不存在时为空串。
func versionOf(t *testing.T, s *store.Store, id int64) string {
	t.Helper()
	list, err := s.ListAPITokens(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range list {
		if tok.ID == id {
			return tok.Version()
		}
	}
	return ""
}

// listedVersion 是 ListApiTokens 给出的 version：面板从这里取，编辑时带回。
func listedVersion(t *testing.T, h *harness, id int64) string {
	t.Helper()
	resp, err := h.admin.ListApiTokens(context.Background(), connect.NewRequest(&heronv1.ListApiTokensRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range resp.Msg.GetTokens() {
		if tok.GetId() == id {
			return tok.GetVersion()
		}
	}
	t.Fatalf("API token %d not listed", id)
	return ""
}

// 读到之后被改过的 token 拒绝整体替换，什么都不改；按新读到的版本再提交即成功。
func TestUpdateApiTokenRejectsStaleVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	id, _ := createToken(t, h, "agent")
	read := listedVersion(t, h, id)
	if _, err := h.admin.UpdateApiToken(ctx, connect.NewRequest(&heronv1.UpdateApiTokenRequest{Id: id, Name: "first", ExpectedVersion: read})); err != nil {
		t.Fatal(err)
	}
	_, err := h.admin.UpdateApiToken(ctx, connect.NewRequest(&heronv1.UpdateApiTokenRequest{Id: id, Name: "second", ExpectedVersion: read}))
	want := fmt.Sprintf("aborted: API token %d changed since it was read (edited, or a node it created or registered joined its scope); read it again with ListApiTokens and resubmit", id)
	if connect.CodeOf(err) != connect.CodeAborted || err.Error() != want {
		t.Fatalf("stale version: %v", err)
	}
	resp, err := h.admin.UpdateApiToken(ctx, connect.NewRequest(&heronv1.UpdateApiTokenRequest{Id: id, Name: "second", ExpectedVersion: listedVersion(t, h, id)}))
	if err != nil || resp.Msg.GetApiToken().GetName() != "second" || resp.Msg.GetApiToken().GetVersion() != listedVersion(t, h, id) {
		t.Fatalf("current version: %v %v", resp, err)
	}
}
