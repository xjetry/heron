package api

import (
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/hub/auth"
)

func sessionRequest[T any](msg *T, token string) *connect.Request[T] {
	r := connect.NewRequest(msg)
	r.Header().Set("Cookie", SessionCookie+"="+token)
	return r
}

func sessionID(token string) string {
	h := auth.HashToken(token)
	return hex.EncodeToString(h[:])
}

func listSessions(t *testing.T, h *harness, token string) []*heronv1.Session {
	t.Helper()
	client := heronv1connect.NewAdminServiceClient(h.srv.Client(), h.srv.URL)
	r, err := client.ListSessions(t.Context(), sessionRequest(&heronv1.ListSessionsRequest{}, token))
	if err != nil {
		t.Fatal(err)
	}
	return r.Msg.Sessions
}

func TestSessionsListAndRevoke(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	first := strings.TrimPrefix(sessionCookieHeader(t, h), SessionCookie+"=")
	created := h.clk.Now().Unix()
	h.clk.Advance(10 * time.Second)
	second := loginRaw(t, h, "").Cookies()[0].Value
	client := heronv1connect.NewAdminServiceClient(h.srv.Client(), h.srv.URL)
	for _, token := range []string{first, second} {
		rows := listSessions(t, h, token)
		// 签发来源随会话列出：直连登录的来源就是对端地址。
		want := []*heronv1.Session{
			{Id: sessionID(second), CreatedAt: created + 10, LastUsedAt: created + 10, Current: token == second, Source: "127.0.0.1"},
			{Id: sessionID(first), CreatedAt: created, LastUsedAt: created, Current: token == first, Source: "127.0.0.1"},
		}
		if !proto.Equal(&heronv1.ListSessionsResponse{Sessions: rows}, &heronv1.ListSessionsResponse{Sessions: want}) {
			t.Errorf("session list metadata/current/order: got %v, want %v", rows, want)
		}
	}
	before := &heronv1.ListSessionsResponse{Sessions: listSessions(t, h, first)}
	if _, err := client.RevokeSession(t.Context(), sessionRequest(&heronv1.RevokeSessionRequest{Id: strings.Repeat("f", 64)}, first)); err != nil {
		t.Fatalf("revoke unknown session must succeed: %v", err)
	}
	if after := (&heronv1.ListSessionsResponse{Sessions: listSessions(t, h, first)}); !proto.Equal(before, after) {
		t.Errorf("unknown revoke changed list: %v", after)
	}
	for range 2 {
		r, err := client.RevokeSession(t.Context(), sessionRequest(&heronv1.RevokeSessionRequest{Id: sessionID(second)}, first))
		if err != nil {
			t.Fatalf("revoke other/repeat must succeed: %v", err)
		}
		if r.Header().Get("Set-Cookie") != "" {
			t.Error("revoking another session cleared current cookie")
		}
	}
	if got := rawCall(t, h, "ListNodes", "{}", map[string][]string{"Cookie": {SessionCookie + "=" + second}}); got.status != 401 {
		t.Errorf("revoked cookie still authenticates: %+v", got)
	}
	if rows := listSessions(t, h, first); len(rows) != 1 || rows[0].Id != sessionID(first) || !rows[0].Current {
		t.Errorf("revoking other session changed survivor: %v", rows)
	}
}

func TestSessionIDsCannotAuthenticate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	current := strings.TrimPrefix(sessionCookieHeader(t, h), SessionCookie+"=")
	loginRaw(t, h, "")
	rows := listSessions(t, h, current)
	if len(rows) != 2 {
		t.Fatalf("expected two sessions for credential probe, got %d", len(rows))
	}
	for _, row := range rows {
		if got := rawCall(t, h, "ListNodes", "{}", map[string][]string{"Cookie": {SessionCookie + "=" + row.Id}}); got.status != 401 {
			t.Errorf("listed session id can impersonate its owner: status=%d", got.status)
		}
	}
}

func TestRevokeCurrentSessionClearsCookie(t *testing.T) {
	t.Parallel()
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			h := newHarness(t, "127.0.0.1/32")
			h.login(t)
			token := strings.TrimPrefix(sessionCookieHeader(t, h), SessionCookie+"=")
			req := sessionRequest(&heronv1.RevokeSessionRequest{Id: strings.ToUpper(sessionID(token))}, token)
			req.Header().Set("X-Forwarded-Proto", scheme)
			client := heronv1connect.NewAdminServiceClient(h.srv.Client(), h.srv.URL)
			r, err := client.RevokeSession(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			cookies := (&http.Response{Header: r.Header()}).Cookies()
			if len(cookies) != 1 {
				t.Errorf("current revoke must clear exactly one cookie: %v", cookies)
			} else {
				c := cookies[0]
				if c.Name != SessionCookie || c.Value != "" || c.MaxAge != -1 || c.Path != "/" || c.Domain != "" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Secure != (scheme == "https") {
					t.Errorf("current revoke clearing cookie attributes: %+v", c)
				}
			}
			if got := rawCall(t, h, "ListNodes", "{}", map[string][]string{"Cookie": {SessionCookie + "=" + token}}); got.status != 401 {
				t.Errorf("current session survived revoke: %+v", got)
			}
		})
	}
}

func TestListSessionsExpiryMatchesAuthentication(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"idle", "absolute"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, "")
			h.login(t)
			active := strings.TrimPrefix(sessionCookieHeader(t, h), SessionCookie+"=")
			old, hash := auth.NewToken()
			now := h.clk.Now()
			before, err := h.store.AdminSecurity(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			created, expires := now, now.Add(auth.SessionAbsolute)
			if kind == "absolute" {
				created = now.Add(-auth.SessionAbsolute + time.Hour)
				expires = now.Add(time.Hour)
			}
			// 种会话与登录签发同一条写路径；来源列为空串，等价于没有请求的签发路径。
			if err := h.store.CommitAdminSecurity(t.Context(), before, before.Data, false, &hash, created, expires, ""); err != nil {
				t.Fatal(err)
			}
			// 绝对过期独立于空闲过期：保持旧会话的最近使用时刻为现在。
			touched := make(chan error, 1)
			h.store.TouchSessionAsync(hash, now, func(err error) { touched <- err })
			if err := <-touched; err != nil {
				t.Fatal(err)
			}
			duration := auth.SessionIdle
			if kind == "absolute" {
				duration = time.Hour
			}
			h.clk.Advance(duration - time.Second)
			if rows := listSessions(t, h, active); len(rows) != 2 {
				t.Errorf("unexpired %s session missing: %v", kind, rows)
			}
			// write 与 writeAsync 共用 store.go 的 s.writes 单消费者 FIFO 通道：write 阻塞到自己的
			// 请求被处理完才返回，因此这次同步写返回时，前面 listSessions 鉴权触发的异步 touch
			// 已经落库。传入起始时刻 now 时它不删除任何行（本用例的会话都在 now 之后过期），这里
			// 只借它的返回时机当屏障。
			if _, err := h.store.DeleteExpiredSessions(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			h.clk.Advance(time.Second)
			rows := listSessions(t, h, active)
			if len(rows) != 1 || rows[0].Id != sessionID(active) {
				t.Errorf("expired %s session leaked into list: %v", kind, rows)
			}
			// 先观察列表再鉴权，避免鉴权删掉过期行替列表的过滤兜底。
			if got := rawCall(t, h, "ListNodes", "{}", map[string][]string{"Cookie": {SessionCookie + "=" + old}}); got.status != 401 {
				t.Errorf("expired %s cookie authenticates: %+v", kind, got)
			}
			client := heronv1connect.NewAdminServiceClient(h.srv.Client(), h.srv.URL)
			// 鉴权已清掉旧行，重新写入同一过期会话，覆盖仍在库中的过期 hash 撤销。
			before, err = h.store.AdminSecurity(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := h.store.CommitAdminSecurity(t.Context(), before, before.Data, false, &hash, created, expires, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := client.RevokeSession(t.Context(), sessionRequest(&heronv1.RevokeSessionRequest{Id: sessionID(old)}, active)); err != nil {
				t.Errorf("expired revoke must succeed: %v", err)
			}
			stored, err := h.store.Sessions(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, sess := range stored {
				if sess.TokenHash == hash {
					t.Errorf("expired session remains after revoke: %+v", sess)
				}
			}
		})
	}
}

// 签发来源与本次请求来源分列：经可信代理以 A 登录签发的会话，在 B 的读取里 source 仍是 A，
// request_source 是 B（可信代理解析后的值，不是对端原始地址）；再次以 B 读取，source 不被改写。
func TestListSessionsReportsIssuingSourceAndRequestSource(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "127.0.0.1/32")
	h.auth.SetPassword(t.Context(), password)
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/heron.v1.AdminService/Login", strings.NewReader(`{"password":"`+password+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.10")
	login, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login status %d", login.StatusCode)
	}
	var token string
	for _, c := range login.Cookies() {
		if c.Name == SessionCookie {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("no session cookie from login")
	}
	client := heronv1connect.NewAdminServiceClient(h.srv.Client(), h.srv.URL)
	read := func() *heronv1.ListSessionsResponse {
		t.Helper()
		list := connect.NewRequest(&heronv1.ListSessionsRequest{})
		list.Header().Set("Cookie", SessionCookie+"="+token)
		list.Header().Set("X-Forwarded-For", "198.51.100.9")
		resp, err := client.ListSessions(t.Context(), list)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg
	}
	id := sessionID(token)
	for attempt, msg := range []*heronv1.ListSessionsResponse{read(), read()} {
		if msg.RequestSource != "198.51.100.9" {
			t.Fatalf("read %d: request_source = %q, want 198.51.100.9 (trusted-proxy resolved), not the raw peer address", attempt+1, msg.RequestSource)
		}
		if len(msg.Sessions) != 1 {
			t.Fatalf("read %d: sessions = %+v, want the single login session", attempt+1, msg.Sessions)
		}
		if msg.Sessions[0].Id != id || !msg.Sessions[0].Current || msg.Sessions[0].Source != "203.0.113.10" {
			t.Fatalf("read %d: session = %+v, want source 203.0.113.10 (issuing address) on %s", attempt+1, msg.Sessions[0], id)
		}
	}
}

func TestSessionMethodsRejectAPITokens(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	_, token := createToken(t, h, "session-test")
	for _, method := range []string{"ListSessions", "RevokeSession"} {
		if got := rawCall(t, h, method, `{"id":"`+strings.Repeat("f", 64)+`"}`, bearer(token)); got.status != 403 || got.code != "permission_denied" {
			t.Errorf("%s token must be PermissionDenied: %+v", method, got)
		}
	}
}

func TestRevokeSessionRejectsMalformedID(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	for _, id := range []string{"", strings.Repeat("a", 62), strings.Repeat("a", 66), strings.Repeat("z", 64)} {
		_, err := h.admin.RevokeSession(t.Context(), connect.NewRequest(&heronv1.RevokeSessionRequest{Id: id}))
		if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "id must be 64 hexadecimal characters") {
			t.Errorf("malformed id %q accepted or unexplained: %v", id, err)
		}
	}
}
