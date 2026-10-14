package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/xjetry/heron/internal/hub/auth"
)

// 这些用例钉住会话读取的不变式：请求里多出来的 cookie（兄弟主机写进浏览器的父域同名 cookie、语法不合的邻居）
// 不能让有效会话失效；通过校验的那个 token 进 ctx，Logout 与撤销当前会话作用在它上面。

type cookieResult struct {
	status     int
	body       []byte
	setCookies []*http.Cookie
}

// cookieCall 用纯 HTTP+JSON 调一个 AdminService 方法，每个 line 作为一个 Cookie 字段行原样发出；不带 cookie jar。
func cookieCall(t *testing.T, h *harness, method, body string, lines ...string) cookieResult {
	t.Helper()
	hdr := http.Header{}
	for _, l := range lines {
		hdr.Add("Cookie", l)
	}
	return adminCall(t, h, method, body, hdr)
}

// adminCall 用纯 HTTP+JSON 调一个 AdminService 方法，header 原样加到请求上；不带 cookie jar。
func adminCall(t *testing.T, h *harness, method, body string, header http.Header) cookieResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/heron.v1.AdminService/"+method, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return cookieResult{status: resp.StatusCode, body: b, setCookies: resp.Cookies()}
}

func pair(value string) string { return SessionCookie + "=" + value }

func pairs(values ...string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = pair(v)
	}
	return strings.Join(parts, "; ")
}

// forgedToken 是 NewToken 形状、库里没有的值：它会走到查库那一步，形状过滤挡不掉它。
func forgedToken(t *testing.T) string {
	t.Helper()
	plain, _ := auth.NewToken()
	return plain
}

// twoSessions 登录两次，返回两个有效会话的 token，第一个是 harness jar 里的那个。
func twoSessions(t *testing.T, h *harness) (string, string) {
	t.Helper()
	h.login(t)
	first := strings.TrimPrefix(sessionCookieHeader(t, h), SessionCookie+"=")
	second := loginRaw(t, h, "").Cookies()[0].Value
	if first == second {
		t.Fatal("two logins issued the same token")
	}
	return first, second
}

// currentSessions 用给定的 Cookie 字段行调 ListSessions，返回标为当前的会话 id。
func currentSessions(t *testing.T, h *harness, lines ...string) []string {
	t.Helper()
	r := cookieCall(t, h, "ListSessions", "{}", lines...)
	if r.status != http.StatusOK {
		t.Fatalf("ListSessions with %q: status %d body %s", lines, r.status, r.body)
	}
	var out struct {
		Sessions []struct {
			ID      string `json:"id"`
			Current bool   `json:"current"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatal(err)
	}
	var cur []string
	for _, s := range out.Sessions {
		if s.Current {
			cur = append(cur, s.ID)
		}
	}
	return cur
}

func TestForgedSessionCookieDoesNotShadowValidOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	valid := strings.TrimPrefix(sessionCookieHeader(t, h), SessionCookie+"=")
	for _, forged := range []string{"bogus", forgedToken(t)} {
		for _, tc := range []struct {
			name  string
			lines []string
			want  int
		}{
			{"forged first", []string{pairs(forged, valid)}, 200},
			{"valid first", []string{pairs(valid, forged)}, 200},
			{"forged on an earlier line", []string{pair(forged), pair(valid)}, 200},
			{"forged only", []string{pair(forged)}, 401},
			{"two forged", []string{pairs(forged, forgedToken(t))}, 401},
		} {
			if got := cookieCall(t, h, "ListNodes", "{}", tc.lines...); got.status != tc.want {
				t.Errorf("forged %q, %s: status %d, want %d (body %s)", forged, tc.name, got.status, tc.want, got.body)
			}
		}
	}
}

// 候选数不设上限：设了上限，写 cookie 的一方用更多的值就能把有效值挤出去。
func TestManyForgedSessionCookiesDoNotCrowdOutValidOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	valid := strings.TrimPrefix(sessionCookieHeader(t, h), SessionCookie+"=")
	values := make([]string, 0, 1001)
	for range 1000 {
		values = append(values, forgedToken(t))
	}
	values = append(values, valid)
	if got := cookieCall(t, h, "ListNodes", "{}", pairs(values...)); got.status != 200 {
		t.Errorf("1000 forged values ahead of the valid one: status %d, want 200 (body %s)", got.status, got.body)
	}
}

// 同一行里别的 cookie 语法不合（无名 cookie、带引号的 JSON、非 ASCII 值、名字不是 token、未闭合的引号）或个数
// 超过 net/http 解析器的 3000 上限，都不能连带丢掉会话。前五种是 Chromium 148.0.7778.96 或 WebKit 26.4 实测会把
// 兄弟主机写的 cookie 与会话放进同一行发出的形状（WebKit 不发无名 cookie）。
//
// 切分只按分号、不认引号：按引号配对切分时，排在会话前面的未闭合引号会把会话吞进它的值里。它排在前还是后，在上面
// 两个浏览器版本上实测如下。会话是 host-only、Path=/，q="x 由兄弟主机写在父域上，会话与 q="x 的写入先后、q="x 的
// Path 为 / 或更长各取两种：
//   - WebKit：四种组合都把 q="x 排在会话之前。
//   - Chromium：q="x 的 Path 更长或比会话先写入时排在前；只有同为 Path=/ 且晚于会话写入时排在后。
//
// Path 由写 cookie 的兄弟主机决定，给更长的 Path 就能在两种浏览器里都排到会话前面。
func TestMalformedNeighbourCookiesDoNotHideSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	valid := pair(strings.TrimPrefix(sessionCookieHeader(t, h), SessionCookie+"="))
	for _, line := range []string{
		valid + "; x",
		`j={"a":1}; ` + valid,
		"u=é; " + valid,
		"a:b=1; " + valid,
		`q="x; ` + valid,
		strings.Repeat("x=1; ", 3000) + valid,
	} {
		if got := cookieCall(t, h, "ListNodes", "{}", line); got.status != 200 {
			shown := line
			if len(shown) > 80 {
				shown = shown[:80] + "..."
			}
			t.Errorf("Cookie %q: status %d, want 200 (body %s)", shown, got.status, got.body)
		}
	}
}

// cookie-value 可以整体包在一对双引号里（RFC 6265 第 4.1.1 节）。会话值与 Request.Cookies 一样去掉这对引号再校验：
// hub 签发的值不带引号，这里钉住的是切分与 net/http 对合法输入的结果一致。
func TestQuotedSessionCookieValueAuthenticates(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	valid := strings.TrimPrefix(sessionCookieHeader(t, h), SessionCookie+"=")
	for _, line := range []string{pair(`"` + valid + `"`), pairs(forgedToken(t), `"`+valid+`"`)} {
		if got := cookieCall(t, h, "ListNodes", "{}", line); got.status != 200 {
			t.Errorf("Cookie %q: status %d, want 200 (body %s)", line, got.status, got.body)
		}
	}
}

// 多个值都有效时取顺序上第一个有效的；会话列表的"当前"读的就是进 ctx 的那个。
func TestTwoValidSessionCookiesPickFirstValid(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	a, b := twoSessions(t, h)
	forged := forgedToken(t)
	for _, tc := range []struct {
		line string
		want string
	}{
		{pairs(a, b), sessionID(a)},
		{pairs(b, a), sessionID(b)},
		{pairs(forged, b, a), sessionID(b)},
	} {
		if got := currentSessions(t, h, tc.line); len(got) != 1 || got[0] != tc.want {
			t.Errorf("Cookie %q: current sessions %v, want [%s]", tc.line, got, tc.want)
		}
	}
}

func TestLogoutWithForgedCookieRevokesTheValidSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	a, b := twoSessions(t, h)
	r := cookieCall(t, h, "Logout", "{}", pairs(forgedToken(t), a))
	if r.status != 200 {
		t.Fatalf("Logout: status %d body %s", r.status, r.body)
	}
	if len(r.setCookies) != 1 || r.setCookies[0].Name != SessionCookie || r.setCookies[0].MaxAge != -1 {
		t.Errorf("Logout must clear the session cookie: %v", r.setCookies)
	}
	if got := cookieCall(t, h, "ListNodes", "{}", pair(a)); got.status != 401 {
		t.Errorf("session presented alongside the forged value survived Logout: status %d", got.status)
	}
	if got := cookieCall(t, h, "ListNodes", "{}", pair(b)); got.status != 200 {
		t.Errorf("Logout revoked an unrelated session: status %d", got.status)
	}
}

func TestRevokeCurrentSessionWithForgedCookie(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	a, b := twoSessions(t, h)
	r := cookieCall(t, h, "RevokeSession", `{"id":"`+sessionID(a)+`"}`, pairs(forgedToken(t), a))
	if r.status != 200 {
		t.Fatalf("RevokeSession: status %d body %s", r.status, r.body)
	}
	if len(r.setCookies) != 1 || r.setCookies[0].Name != SessionCookie || r.setCookies[0].MaxAge != -1 {
		t.Errorf("revoking the current session must clear its cookie: %v", r.setCookies)
	}
	if got := cookieCall(t, h, "ListNodes", "{}", pair(a)); got.status != 401 {
		t.Errorf("revoked current session still authenticates: status %d", got.status)
	}
	if got := cookieCall(t, h, "ListNodes", "{}", pair(b)); got.status != 200 {
		t.Errorf("revoking the current session revoked another one: status %d", got.status)
	}
}

// 会话校验失败不是登录失败。伪造会话 cookie 若被记成登录失败，发过它们的来源会比没发过的来源早被锁。比较的是两个
// 来源开始被锁的尝试序号，与锁定阈值的具体数值无关，伪造值哪怕只被记了一次，那个来源也会早一次被锁。两种部署各比一次：
//   - 配了可信代理：同一个 hub 上的两个来源由 X-Forwarded-For 区分，其中一个先发伪造 cookie。
//   - 不配代理：这是默认部署，来源就是 TCP 对端。起两个 hub，都从 127.0.0.1 访问，其中一个先收到伪造 cookie。
//     只比 X-Forwarded-For 来源的那一组照不到记在 TCP 对端上的计数，而默认部署下 TCP 对端就是管理员自己的地址。
func TestForgedSessionCookiesDoNotCountAsLoginFailures(t *testing.T) {
	t.Parallel()
	t.Run("behind a trusted proxy", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, "127.0.0.1/32")
		h.login(t)
		valid := strings.TrimPrefix(sessionCookieHeader(t, h), SessionCookie+"=")
		forged, control := http.Header{"X-Forwarded-For": {"203.0.113.1"}}, http.Header{"X-Forwarded-For": {"203.0.113.2"}}
		sendForgedSessionCookies(t, h, valid, forged)
		assertSameLockout(t, wrongPasswordsUntilLocked(t, h, forged), wrongPasswordsUntilLocked(t, h, control))
	})
	t.Run("without a proxy", func(t *testing.T) {
		t.Parallel()
		forgedHub, controlHub := newHarness(t, ""), newHarness(t, "")
		forgedHub.login(t)
		controlHub.login(t)
		valid := strings.TrimPrefix(sessionCookieHeader(t, forgedHub), SessionCookie+"=")
		sendForgedSessionCookies(t, forgedHub, valid, http.Header{})
		assertSameLockout(t, wrongPasswordsUntilLocked(t, forgedHub, http.Header{}), wrongPasswordsUntilLocked(t, controlHub, http.Header{}))
	})
}

// sendForgedSessionCookies 以 source 表示的来源发十次只带伪造值的请求（401）和十次伪造值夹着有效值 valid 的请求（200）。
func sendForgedSessionCookies(t *testing.T, h *harness, valid string, source http.Header) {
	t.Helper()
	for i := range 10 {
		if got := adminCall(t, h, "ListNodes", "{}", withCookie(source, pairs("bogus", forgedToken(t)))); got.status != 401 {
			t.Fatalf("forged-only request %d: status %d", i, got.status)
		}
		if got := adminCall(t, h, "ListNodes", "{}", withCookie(source, pairs(forgedToken(t), valid))); got.status != 200 {
			t.Fatalf("forged+valid request %d: status %d", i, got.status)
		}
	}
}

func withCookie(source http.Header, line string) http.Header {
	hdr := source.Clone()
	hdr.Add("Cookie", line)
	return hdr
}

// wrongPasswordsUntilLocked 以 source 表示的来源一次次输错密码，返回第一次被拒为锁定的尝试序号。
func wrongPasswordsUntilLocked(t *testing.T, h *harness, source http.Header) int {
	t.Helper()
	for attempt := 1; attempt <= 100; attempt++ {
		r := adminCall(t, h, "Login", `{"password":"incorrect"}`, source)
		switch body := string(r.body); {
		case r.status == 401 && strings.Contains(body, "too many failed logins"):
			return attempt
		case r.status == 401 && strings.Contains(body, "密码或第二认证因素无效"):
		default:
			t.Fatalf("wrong password, attempt %d: status %d body %s", attempt, r.status, body)
		}
	}
	t.Fatal("never locked out within 100 wrong passwords")
	return 0
}

func assertSameLockout(t *testing.T, forgedAt, controlAt int) {
	t.Helper()
	if forgedAt != controlAt {
		t.Errorf("forged session cookies counted as login failures: the source that sent them was locked out from attempt %d, the control source from attempt %d", forgedAt, controlAt)
	}
	t.Logf("locked out from attempt %d (source that sent forged cookies) and %d (control)", forgedAt, controlAt)
}
