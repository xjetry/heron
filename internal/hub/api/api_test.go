package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/hub/backup"
	"github.com/xjetry/heron/internal/hub/geo"
	"github.com/xjetry/heron/internal/hub/heartbeat"
	"github.com/xjetry/heron/internal/hub/ingest"
	"github.com/xjetry/heron/internal/hub/live"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/nodeops"
	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
	"github.com/xjetry/heron/internal/testdeps"
	"github.com/xjetry/heron/internal/testwait"
)

const password = "correct horse battery staple"

type harness struct {
	dbPath string
	srv    *httptest.Server
	http   *http.Client // 带 cookie jar
	admin  heronv1connect.AdminServiceClient
	agent  heronv1connect.AgentServiceClient
	clk    *clock.Fake
	store  *store.Store
	auth   *auth.Auth
	live   *live.Live
	ingest *ingest.Service
	book   *traffic.Book
	reg    *probe.Registry
	alerts *alert.Engine
	svc    *Service
	pub    *Public
	// heartbeat 是心跳循环的替身：api 只读 Status，用例按需改它。
	heartbeat *stubHeartbeat
}

// stubHeartbeat 实现 HeartbeatStatusProvider，不跑循环；用例用 set 摆出成功/失败/从未跑过的状态。
type stubHeartbeat struct {
	mu     sync.Mutex
	status heartbeat.Status
}

func (s *stubHeartbeat) Status() heartbeat.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *stubHeartbeat) set(st heartbeat.Status) {
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
}

func TestTestClientsOwnTheirTransport(t *testing.T) {
	t.Parallel()
	testdeps.RequireOwnedTransports(t, ".")
}

// 本包的用例默认并行（t.Parallel）。一个用例能并行的前提，缺一条就不加并写明原因：
//   - 不与别的用例共享假时钟、Store 或 harness：各自经 newHarness 等夹具在自己的 t.TempDir 里装配；
//   - 不用 t.Setenv，不改进程级设置（GOMAXPROCS、slog 默认 logger）；
//   - 不读写包级可变状态；
//   - 发出的每个 HTTP 请求都走夹具拥有的 Transport，不走进程共享的 http.DefaultTransport：标准库
//     httptest.Server.Close 会对 http.DefaultTransport 调 CloseIdleConnections，并行时别的用例收尾就会关掉本用例
//     正要复用的连接（机制见 testdeps 的 OwnedTransport）。harness 的 h.http 与 h.admin 用 srv.Client().Transport，
//     不带 cookie 的裸请求用 h.srv.Client()；TestTestClientsOwnTheirTransport 经 testdeps.RequireOwnedTransports
//     在源码上守着这一条；
//   - 断言里的耗时阈值（上界或比值）远大于负载能造成的停顿：负载下一次调度停顿可达几十毫秒，阈值在这个量级的
//     不并行；阈值在百毫秒以上且比被量操作的正常耗时大两个数量级的，或只用来区分"等满了某个超时"与"没等"的
//     （如 drainTimeout，缺陷路径至少要等满它），可以并行。
//
// 不并行的用例在 go test 里先于全部并行用例串行跑完，两类不会重叠。
func newHarness(t *testing.T, trusted string, opts ...harnessOption) *harness {
	t.Helper()
	return newZonedHarness(t, trusted, time.UTC, store.DefaultRetention, opts...)
}

// harnessOption 替换 newZonedHarness 装配里的个别依赖，其余照常。
type harnessOption func(*harnessDeps)

type harnessDeps struct {
	authLog      *slog.Logger
	log          *slog.Logger
	telegramBase string
	// config 在装配前改 api.Config 里与 serve 的 flag 对应的项。
	config func(*Config)
}

// withAuthLog 给 Auth 换 logger，用例借它的日志语句位置暂停登录。
func withAuthLog(l *slog.Logger) harnessOption {
	return func(d *harnessDeps) { d.authLog = l }
}

// withLog 给 api.Service 换 logger，用例借它断言哪些路径不该记错误。
func withLog(l *slog.Logger) harnessOption {
	return func(d *harnessDeps) { d.log = l }
}

// withConfig 让用例在装配前改 api.Config 里与 serve 的 flag 对应的项。
func withConfig(edit func(*Config)) harnessOption {
	return func(d *harnessDeps) { d.config = edit }
}

// withTelegramBase 让投递队列把 Telegram 请求发到 base（测试服务器），用例看得到接收方收到的正文。
func withTelegramBase(base string) harnessOption {
	return func(d *harnessDeps) { d.telegramBase = base }
}

// newZonedHarness 的 loc 是 hub 的 --timezone：流量周期、到期扫描与 days_left 用同一个时区，与 serve 的装配一致。
// retention 与 serve 传给 api.Config 的是同一个字段：接错保留期不会影响这个夹具本身的任何行为（它不跑
// RunMaintenance），只会在存储健康的判定与 GetStorageStatsResponse.retention_s 上露出来，所以留给调用方传入，
// 默认测试用 store.DefaultRetention，需要钉住"判定用的是配置保留期"的用例可以传入不同的值。
func newZonedHarness(t *testing.T, trusted string, loc *time.Location, retention store.Retention, opts ...harnessOption) *harness {
	t.Helper()
	deps := harnessDeps{authLog: slog.Default(), log: slog.Default(), config: func(*Config) {}}
	for _, o := range opts {
		o(&deps)
	}
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	dbPath := filepath.Join(t.TempDir(), "t.db")
	st, err := store.Open(dbPath, clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	prefixes, err := auth.ParsePrefixes(trusted)
	if err != nil {
		t.Fatal(err)
	}
	reg := probe.New(st, slog.Default())
	l := live.New(clk, 30*time.Second)
	book := traffic.New(st, clk, loc, slog.Default())
	alerts := alert.New(alert.Config{TTL: 30 * time.Second, Location: loc}, st, st.Evaluation(), l, clk, slog.Default())
	alerts.SetTraffic(book)
	// 通知与国家查询共用一个出站客户端，与 serve 的装配相同。
	client := outbound.NewClient(alert.NotifyTimeout)
	notifier := alert.NewQueue(alert.QueueConfig{TelegramBase: deps.telegramBase}, alert.QueueDeps{Store: st, Channels: alerts.Channels, Client: client, Clock: clk, Log: slog.Default()})
	a := auth.New(st, reg, notifier, clk, loc, deps.authLog)
	in := ingest.New(ingest.Config{TTL: 30 * time.Second, TrustedProxies: prefixes},
		ingest.Deps{Live: l, Store: st, Auth: a, Traffic: book, Tasks: reg, Clock: clk, Log: slog.Default()})
	ctx := context.Background()
	if err := errors.Join(a.Load(ctx), in.Load(ctx), book.Load(ctx), reg.Load(ctx), alerts.Load(ctx)); err != nil {
		t.Fatal(err)
	}
	if err := alerts.SweepTraffic(ctx); err != nil {
		t.Fatal(err)
	}
	hb := &stubHeartbeat{}
	cfg := Config{Backups: backup.New(st, notifier, clk, slog.Default()), Heartbeat: hb, TTL: 30 * time.Second, ReportInterval: 10 * time.Second, TrustedProxies: prefixes, HubVersion: "test-hub-version", Location: loc, Retention: retention, Geo: geo.NewHTTP(client)}
	deps.config(&cfg)
	ops := nodeops.New(nodeops.Deps{Credentials: a, Nodes: reg, Alerts: alerts, Traffic: book, State: in, Log: slog.Default()})
	svc := New(cfg, Deps{Store: st, Auth: a, Live: l, Nodes: ops, Traffic: book, Probes: reg, Alerts: alerts, Notifier: notifier, Clock: clk, Log: deps.log})
	pub := NewPublic(PublicConfig{ReportInterval: 10 * time.Second, TrustedProxies: prefixes, Location: loc},
		PublicDeps{Store: st, Live: l, Traffic: book, Probes: reg, Clock: clk, Log: slog.Default()})
	mux := http.NewServeMux()
	mux.Handle(in.Handler())
	mux.Handle(svc.Handler())
	mux.Handle(pub.Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Transport: srv.Client().Transport}
	return &harness{dbPath: dbPath, srv: srv, http: hc, admin: heronv1connect.NewAdminServiceClient(hc, srv.URL),
		agent: heronv1connect.NewAgentServiceClient(srv.Client(), srv.URL), clk: clk, store: st, auth: a, live: l, ingest: in, book: book, reg: reg, alerts: alerts, svc: svc, pub: pub, heartbeat: hb}
}

// deps 是夹具装配 Service 时用的那套协作者；用例改其中一项再交给 New，其余与夹具相同。
func (h *harness) deps() Deps {
	return Deps{Store: h.store, Auth: h.auth, Live: h.live, Nodes: h.svc.nodes, Traffic: h.book, Probes: h.reg, Alerts: h.alerts,
		Notifier: h.svc.notifier, Clock: h.clk, Log: slog.Default()}
}

// publicDeps 是夹具装配 Public 时用的那套协作者。
func (h *harness) publicDeps() PublicDeps {
	return PublicDeps{Store: h.store, Live: h.live, Traffic: h.book, Probes: h.reg, Clock: h.clk, Log: slog.Default()}
}

func (h *harness) login(t *testing.T) {
	t.Helper()
	if err := h.auth.SetPassword(context.Background(), password); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.Login(context.Background(), connect.NewRequest(&heronv1.LoginRequest{Password: password})); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) createNode(t *testing.T, name string) (int64, string) {
	t.Helper()
	resp, err := h.admin.CreateNode(context.Background(), connect.NewRequest(&heronv1.CreateNodeRequest{Name: name}))
	if err != nil {
		t.Fatal(err)
	}
	// 通常用例需要已安装节点；安装凭据本身的边界由直接调用 CreateNode 的用例验证。
	id, token, err := h.auth.Register(t.Context(), resp.Msg.Token, name, netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	return id, token
}

func (h *harness) report(t *testing.T, tok string, m *heronv1.Metrics) error {
	t.Helper()
	req := connect.NewRequest(&heronv1.ReportRequest{Metrics: m})
	req.Header().Set("Authorization", "Bearer "+tok)
	_, err := h.agent.Report(context.Background(), req)
	return err
}

// publicClient 是不带任何凭据的公开服务客户端：harness.http 带着会话 cookie jar，这里用裸客户端。
func (h *harness) publicClient(opts ...connect.ClientOption) heronv1connect.PublicServiceClient {
	return heronv1connect.NewPublicServiceClient(h.srv.Client(), h.srv.URL, opts...)
}

// setPublic 只改公开与否；UpdateNode 整体替换可编辑字段，其余取建节点时的默认值（重置日 1、宽限期取 TTL）。
func (h *harness) setPublic(t *testing.T, id int64, name string, public bool) {
	t.Helper()
	req := &heronv1.UpdateNodeRequest{Id: id, Name: name, Public: public, TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0)}
	if _, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(req)); err != nil {
		t.Fatal(err)
	}
}

func codeOf(err error) connect.Code { return connect.CodeOf(err) }

// 写入前后断言需要即时行数，不能复用 StorageStats 的旧快照；独立连接直接计数也作为 API 统计的对照。
func rowCounts(t *testing.T, path string) map[string]int64 {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(t.Context(), "SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(name, "sqlite_") {
			names = append(names, name)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, name := range names {
		var count int64
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM "`+strings.ReplaceAll(name, `"`, `""`)+`"`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		out[name] = count
	}
	return out
}

func TestEveryAdminProcedureRejectsAnonymousCalls(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	services := heronv1.File_heron_v1_admin_proto.Services()
	count := 0
	for i := 0; i < services.Len(); i++ {
		svc := services.Get(i)
		for j := 0; j < svc.Methods().Len(); j++ {
			method := svc.Methods().Get(j)
			path := "/" + string(svc.FullName()) + "/" + string(method.Name())
			// LOGIN 方法的凭据在请求体里，匿名白名单由 cmd/hub/mux_test 守；无管理员的 Login 失败不能证明拦截器存在。
			if h.svc.access[path] == heronv1.Access_ACCESS_LOGIN {
				continue
			}
			count++
			resp, err := h.srv.Client().Post(h.srv.URL+path, "application/json", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Code string `json:"code"`
			}
			json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized || body.Code != "unauthenticated" {
				t.Errorf("%s: status %d code %q, want 401 unauthenticated", path, resp.StatusCode, body.Code)
			}
			if resp.Header.Get("Access-Control-Allow-Origin") != "" {
				t.Errorf("%s: hub must not emit CORS allow headers", path)
			}
		}
	}
	if count == 0 {
		t.Fatal("enumerated no procedures")
	}
}

func TestLoginRequiresAdminAndRightPassword(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	ctx := context.Background()
	_, err := h.admin.Login(ctx, connect.NewRequest(&heronv1.LoginRequest{Password: password}))
	if codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("login with empty admin table: %v", err)
	}
	if err := h.auth.SetPassword(ctx, password); err != nil {
		t.Fatal(err)
	}
	_, err = h.admin.Login(ctx, connect.NewRequest(&heronv1.LoginRequest{Password: "not it, definitely"}))
	if codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := h.admin.ListNodes(ctx, connect.NewRequest(&heronv1.ListNodesRequest{})); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("ListNodes before login: %v", err)
	}
	if _, err := h.admin.Login(ctx, connect.NewRequest(&heronv1.LoginRequest{Password: password})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.ListNodes(ctx, connect.NewRequest(&heronv1.ListNodesRequest{})); err != nil {
		t.Fatalf("ListNodes after login: %v", err)
	}
	if _, err := h.admin.Logout(ctx, connect.NewRequest(&heronv1.LogoutRequest{})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.ListNodes(ctx, connect.NewRequest(&heronv1.ListNodesRequest{})); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("ListNodes after logout: %v", err)
	}
}

// 另一来源的错误密码登录停在它的失败日志上（已记账、未放门）时，管理员经 HTTP 登录得到
// ResourceExhausted 与稍后重试的正文，不是密码错误。暂停点若不在门内，这次登录会成功而让用例变红。
func TestLoginBusyReturnsResourceExhausted(t *testing.T) {
	t.Parallel()
	pause, entered, release := testwait.PauseAtLog(slog.Default().Handler(), "login failed")
	h := newHarness(t, "", withAuthLog(slog.New(pause)))
	ctx := context.Background()
	if err := h.auth.SetPassword(ctx, password); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.auth.Login(ctx, "wrong password here", netip.MustParseAddr("198.51.100.7"))
		done <- err
	}()
	defer func() {
		release()
		if err := <-done; !errors.Is(err, auth.ErrBadPassword) {
			t.Errorf("paused login = %v, want ErrBadPassword", err)
		}
	}()
	select {
	case <-entered:
	case <-time.After(testwait.Bound):
		t.Fatal("wrong-password login did not reach its failure log")
	}
	requestCtx, cancel := context.WithTimeout(ctx, testwait.Bound)
	defer cancel()
	_, err := h.admin.Login(requestCtx, connect.NewRequest(&heronv1.LoginRequest{Password: password}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeResourceExhausted || ce.Message() != "password verification is busy; please try again later" {
		t.Fatalf("busy login = %v, want ResourceExhausted with retry message", err)
	}
}

func loginRaw(t *testing.T, h *harness, xfProto string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/heron.v1.AdminService/Login", strings.NewReader(`{"password":"`+password+`"}`))
	req.Header.Set("Content-Type", "application/json")
	if xfProto != "" {
		req.Header.Set("X-Forwarded-Proto", xfProto)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status %d", resp.StatusCode)
	}
	return resp
}

// 登录失败按可信代理追加在另起一行里的真实地址计：客户端每次换一个伪造的第一行，锁定照样落在它自己的来源上，
// 换了伪造值带正确密码也进不去。
func TestLoginLockoutKeysOnEveryForwardedForLine(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "127.0.0.1/32")
	h.auth.SetPassword(t.Context(), password)
	login := func(pw string, forged int) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/heron.v1.AdminService/Login", strings.NewReader(`{"password":"`+pw+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Add("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", forged))
		req.Header.Add("X-Forwarded-For", "198.51.100.9")
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	const lockedText = "too many failed logins from this source"
	locked := false
	for i := range 20 { // 远多于 auth 的锁定上限
		if _, body := login("wrong", i+1); strings.Contains(body, lockedText) {
			locked = true
			break
		}
	}
	if !locked {
		t.Fatal("20 failed logins with a new forged first X-Forwarded-For line each time never locked the real source")
	}
	if code, body := login(password, 200); code != http.StatusUnauthorized || !strings.Contains(body, lockedText) {
		t.Fatalf("correct password from the locked source behind a fresh forged line: %d %s", code, body)
	}
}

func TestSessionCookieIsHostOnlyStrictAndSecureOnlyBehindTLSProxy(t *testing.T) {
	t.Parallel()
	plain := newHarness(t, "")
	plain.auth.SetPassword(context.Background(), password)
	cookies := loginRaw(t, plain, "https").Cookies() // 对端不可信：转发头不采信
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	c := cookies[0]
	if c.Name != "heron_session" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Domain != "" || c.Path != "/" || c.Secure || c.MaxAge != 30*24*60*60 {
		t.Errorf("cookie = %+v", c)
	}
	if len(c.Value) != 64 {
		t.Fatalf("token length %d, want 64 hex chars", len(c.Value))
	}

	proxied := newHarness(t, "127.0.0.1/32")
	proxied.auth.SetPassword(context.Background(), password)
	if c := loginRaw(t, proxied, "https").Cookies()[0]; !c.Secure {
		t.Fatal("cookie behind a trusted TLS proxy must be Secure")
	}
	if c := loginRaw(t, proxied, "http").Cookies()[0]; c.Secure {
		t.Fatal("cookie behind a trusted plain proxy must not be Secure")
	}
}

// 两项协议约束各自独立：非 JSON/proto 的 Content-Type 被 connect-go 以 415 拒绝；
// 未标为无副作用的方法不接受 GET（405）；两者都不会进入方法体。带有效 cookie 才有意义。
func TestCrossSiteRequestShapesAreRejectedWithoutSideEffects(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	var calls atomic.Int64
	outbound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer outbound.Close()
	c := saveChannel(t, h, webhook(outbound.URL))
	if _, err := h.admin.TestNotifyChannel(t.Context(), connect.NewRequest(&heronv1.TestNotifyChannelRequest{Id: c.Id})); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("JSON control sent %d requests", calls.Load())
	}
	calls.Store(0)
	before := rowCounts(t, h.dbPath)
	services := heronv1.File_heron_v1_admin_proto.Services()
	for i := 0; i < services.Len(); i++ {
		service := services.Get(i)
		for j := 0; j < service.Methods().Len(); j++ {
			method := service.Methods().Get(j)
			if method.Name() == "Login" {
				continue
			}
			url := h.srv.URL + "/" + string(service.FullName()) + "/" + string(method.Name())
			for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data"} {
				req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(fmt.Sprintf("{\"id\":%d,\"name\":\"csrf\"}", c.Id)))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", ct)
				resp, err := h.http.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusUnsupportedMediaType {
					t.Errorf("%s %s: status %d, want 415", method.Name(), ct, resp.StatusCode)
				}
			}
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("cross-site TestNotifyChannel sent %d requests", calls.Load())
	}
	after := rowCounts(t, h.dbPath)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("cross-site changed counts: before=%v after=%v", before, after)
	}
	resp, err := h.http.Get(h.srv.URL + "/heron.v1.AdminService/CreateNode?connect=v1&encoding=json&message=%7B%22name%22%3A%22csrf%22%7D")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status %d, want 405", resp.StatusCode)
	}
}

func TestPasswordChangeAndExpiryEndSessions(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	if err := h.auth.SetPassword(ctx, "a completely new password"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.ListNodes(ctx, connect.NewRequest(&heronv1.ListNodesRequest{})); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("session survived password change: %v", err)
	}
	if _, err := h.admin.Login(ctx, connect.NewRequest(&heronv1.LoginRequest{Password: "a completely new password"})); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(auth.SessionIdle)
	if _, err := h.admin.ListNodes(ctx, connect.NewRequest(&heronv1.ListNodesRequest{})); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("idle session survived: %v", err)
	}
}

func TestCreatedTokenReportsAndDeleteForgetsEverything(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	id, tok := h.createNode(t, "  web-01 \x00 ")
	if n, _ := h.store.GetNode(ctx, id); n.Name != "web-01" {
		t.Fatalf("name = %q, want trimmed and sanitized", n.Name)
	}
	if err := h.report(t, tok, &heronv1.Metrics{CpuPct: proto.Float64(3)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.live.Get(id); !ok {
		t.Fatal("report did not reach live")
	}
	if _, err := h.admin.DeleteNode(ctx, connect.NewRequest(&heronv1.DeleteNodeRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.live.Get(id); ok {
		t.Fatal("live state survived DeleteNode")
	}
	if err := h.report(t, tok, &heronv1.Metrics{}); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("deleted node's token still reports: %v", err)
	}
	if _, err := h.admin.DeleteNode(ctx, connect.NewRequest(&heronv1.DeleteNodeRequest{Id: id})); codeOf(err) != connect.CodeNotFound {
		t.Fatalf("deleting twice: %v, want NotFound", err)
	}
}

func TestRotateTokenInvalidatesTheOldOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	id, old := h.createNode(t, "n")
	resp, err := h.admin.RotateNodeToken(ctx, connect.NewRequest(&heronv1.RotateNodeTokenRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.report(t, old, &heronv1.Metrics{}); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("old token still accepted: %v", err)
	}
	if err := h.report(t, h.claimNode(t, resp.Msg.GetToken()), &heronv1.Metrics{}); err != nil {
		t.Fatalf("new token rejected: %v", err)
	}
	if _, err := h.admin.RotateNodeToken(ctx, connect.NewRequest(&heronv1.RotateNodeTokenRequest{Id: 999})); codeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown node: %v", err)
	}
}

func TestRegisterAdoptsPrecreatedNodeToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	created, err := h.admin.CreateNode(ctx, connect.NewRequest(&heronv1.CreateNodeRequest{Name: "precreated"}))
	if err != nil {
		t.Fatal(err)
	}
	id, token := created.Msg.Node.Id, created.Msg.Token
	// 不要求注册窗口：管理员签发的安装凭据只认领既有节点。
	resp, err := h.agent.Register(ctx, connect.NewRequest(&heronv1.RegisterRequest{Key: token, Name: "hostname"}))
	if err != nil {
		t.Fatalf("register with a node token: %v", err)
	}
	fresh := resp.Msg.GetToken()
	if resp.Msg.GetNodeId() != id || fresh == "" || fresh == token {
		t.Fatalf("register response = %v, want node %d with a fresh token", resp.Msg, id)
	}
	if err := h.report(t, fresh, &heronv1.Metrics{}); err != nil {
		t.Fatalf("the returned token must report: %v", err)
	}
	if err := h.report(t, token, &heronv1.Metrics{}); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("the install token must be dead after adoption: %v", err)
	}
	// 认领不新建节点，名称仍是面板里设的那个。
	if nodes, err := h.store.ListNodes(ctx); err != nil || len(nodes) != 1 || nodes[0].Name != "precreated" {
		t.Fatalf("nodes = %v %v, want only the precreated one", nodes, err)
	}
}

func TestUpdateAndReorderNodes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	a, _ := h.createNode(t, "a")
	b, _ := h.createNode(t, "b")
	upd, err := h.admin.UpdateNode(ctx, connect.NewRequest(&heronv1.UpdateNodeRequest{Id: a, Name: "a2", Public: true, Note: "note\u202e", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0)}))
	if err != nil || upd.Msg.GetNode().GetName() != "a2" || !upd.Msg.GetNode().GetPublic() || upd.Msg.GetNode().GetNote() != "note" {
		t.Fatalf("UpdateNode = %v %v", upd, err)
	}
	if _, err := h.admin.UpdateNode(ctx, connect.NewRequest(&heronv1.UpdateNodeRequest{Id: a, Name: strings.Repeat("x", 65), OfflineGraceS: proto.Uint32(0)})); codeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("65-char name: %v", err)
	}
	if _, err := h.admin.UpdateNode(ctx, connect.NewRequest(&heronv1.UpdateNodeRequest{Id: 999, Name: "x", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0)})); codeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown node: %v", err)
	}
	if _, err := h.admin.ReorderNodes(ctx, connect.NewRequest(&heronv1.ReorderNodesRequest{Ids: []int64{b}})); codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "exactly once") {
		t.Fatalf("partial reorder: %v", err)
	}
	if _, err := h.admin.ReorderNodes(ctx, connect.NewRequest(&heronv1.ReorderNodesRequest{Ids: []int64{b, a}})); err != nil {
		t.Fatal(err)
	}
	list, _ := h.admin.ListNodes(ctx, connect.NewRequest(&heronv1.ListNodesRequest{}))
	if ids := []int64{list.Msg.Nodes[0].Id, list.Msg.Nodes[1].Id}; ids[0] != b || ids[1] != a {
		t.Fatalf("order = %v, want [b a]", ids)
	}
}

func TestRegisterWindowLifecycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	if _, err := h.admin.OpenRegisterWindow(ctx, connect.NewRequest(&heronv1.OpenRegisterWindowRequest{TtlS: 10, MaxNodes: 1})); codeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("ttl 10s: %v", err)
	}
	opened, err := h.admin.OpenRegisterWindow(ctx, connect.NewRequest(&heronv1.OpenRegisterWindowRequest{TtlS: 3600, MaxNodes: 2}))
	if err != nil {
		t.Fatal(err)
	}
	if opened.Msg.GetExpiresAt() != h.clk.Now().Add(time.Hour).Unix() || len(opened.Msg.GetKey()) != 64 {
		t.Fatalf("opened = %v", opened.Msg)
	}
	got, _ := h.admin.GetRegisterWindow(ctx, connect.NewRequest(&heronv1.GetRegisterWindowRequest{}))
	if !got.Msg.GetOpen() || got.Msg.GetRemaining() != 2 {
		t.Fatalf("window = %v", got.Msg)
	}
	if _, err := h.agent.Register(ctx, connect.NewRequest(&heronv1.RegisterRequest{Key: opened.Msg.GetKey(), Name: "via-window"})); err != nil {
		t.Fatal(err)
	}
	got, _ = h.admin.GetRegisterWindow(ctx, connect.NewRequest(&heronv1.GetRegisterWindowRequest{}))
	if got.Msg.GetRemaining() != 1 {
		t.Fatalf("remaining = %d after one registration", got.Msg.GetRemaining())
	}
	if _, err := h.admin.CloseRegisterWindow(ctx, connect.NewRequest(&heronv1.CloseRegisterWindowRequest{})); err != nil {
		t.Fatal(err)
	}
	if got, _ = h.admin.GetRegisterWindow(ctx, connect.NewRequest(&heronv1.GetRegisterWindowRequest{})); got.Msg.GetOpen() {
		t.Fatal("closed window reported open")
	}
	if _, err := h.admin.OpenRegisterWindow(ctx, connect.NewRequest(&heronv1.OpenRegisterWindowRequest{TtlS: 3600, MaxNodes: 1})); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(time.Hour)
	if got, _ = h.admin.GetRegisterWindow(ctx, connect.NewRequest(&heronv1.GetRegisterWindowRequest{})); got.Msg.GetOpen() {
		t.Fatal("expired window reported open")
	}
}

func TestGetSnapshotReportsHubVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	resp, err := h.admin.GetSnapshot(context.Background(), connect.NewRequest(&heronv1.GetSnapshotRequest{}))
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if resp.Msg.HubVersion != "test-hub-version" {
		t.Fatalf("want hub_version test-hub-version, got %q", resp.Msg.HubVersion)
	}
}

func TestSnapshotReflectsLiveState(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	online, tok := h.createNode(t, "online")
	silent, _ := h.createNode(t, "silent")
	if err := h.report(t, tok, &heronv1.Metrics{CpuPct: proto.Float64(42)}); err != nil {
		t.Fatal(err)
	}
	snap, err := h.admin.GetSnapshot(ctx, connect.NewRequest(&heronv1.GetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Msg.GetNow() != h.clk.Now().Unix() || snap.Msg.GetReportIntervalMs() != 10_000 || len(snap.Msg.Nodes) != 2 {
		t.Fatalf("snapshot = %v", snap.Msg)
	}
	byID := map[int64]*heronv1.NodeStatus{}
	for _, n := range snap.Msg.Nodes {
		byID[n.Id] = n
	}
	if n := byID[online]; !n.GetOnline() || n.GetMetrics().GetCpuPct() != 42 || n.LastSeenAt == nil || *n.LastSeenAt != h.clk.Now().Unix() {
		t.Fatalf("online node = %v", n)
	}
	if n := byID[silent]; n.GetOnline() || n.Metrics != nil || n.LastSeenAt != nil {
		t.Fatalf("silent node = %v", n)
	}
	h.clk.Advance(31 * time.Second)
	snap, _ = h.admin.GetSnapshot(ctx, connect.NewRequest(&heronv1.GetSnapshotRequest{}))
	for _, n := range snap.Msg.Nodes {
		if n.Id == online && (n.GetOnline() || n.GetMetrics().GetCpuPct() != 42) {
			t.Fatalf("after TTL: %v (must be offline but keep the last readings)", n)
		}
	}
}

func TestQueryMetricsShapeAndValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	id, _ := h.createNode(t, "n")
	base := h.clk.Now().Truncate(time.Hour).Unix()
	var rows []metric.Row
	for i := int64(0); i < 10; i++ {
		b := metric.NewBucket()
		b.Add(&heronv1.Metrics{CpuPct: proto.Float64(float64(i)), MemUsed: proto.Uint64(100)})
		rows = append(rows, metric.Row{NodeID: id, TS: base + i*60, CoverageStart: base, Bucket: b})
	}
	if _, err := h.store.WriteMinuteBatch(ctx, metric.Batch{Rows: rows}); err != nil {
		t.Fatal(err)
	}
	resp, err := h.admin.QueryMetrics(ctx, connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: id, From: base + 30, To: base + 600, MaxPoints: 4}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetLevel() != "1m" || resp.Msg.GetStepS() != 180 || len(resp.Msg.Ts) != 4 || resp.Msg.Ts[0] != base {
		t.Fatalf("level %s step %d ts %v", resp.Msg.GetLevel(), resp.Msg.GetStepS(), resp.Msg.Ts)
	}
	if len(resp.Msg.Series) != len(metric.Columns) {
		t.Fatalf("%d series, want one per column", len(resp.Msg.Series))
	}
	cpu := resp.Msg.Series[0]
	if cpu.GetName() != "cpu" || cpu.GetUnit() != "percent" || len(cpu.Samples) != 4 {
		t.Fatalf("cpu series = %v", cpu)
	}
	if s := cpu.Samples[0]; s.GetN() != 3 || s.Mean == nil || *s.Mean != 1 || s.Max == nil || *s.Max != 2 {
		t.Fatalf("cpu sample 0 = %v, want n=3 mean=1 max=2", s)
	}
	swap := resp.Msg.Series[2]
	if swap.GetName() != "swap_used" || swap.Samples[0].GetN() != 0 || swap.Samples[0].Mean != nil || swap.Samples[0].Max != nil {
		t.Fatalf("swap sample 0 = %v, want n=0 with no mean/max", swap.Samples[0])
	}
	if load := resp.Msg.Series[4]; load.GetName() != "load1" || load.GetUnit() != "" {
		t.Fatalf("load series = %v", load)
	}

	for name, req := range map[string]*heronv1.QueryMetricsRequest{
		"from >= to":        {NodeId: id, From: base + 600, To: base + 600},
		"span > 400d":       {NodeId: id, From: base, To: base + 401*86400},
		"max_points > 2000": {NodeId: id, From: base, To: base + 600, MaxPoints: 2001},
	} {
		if _, err := h.admin.QueryMetrics(ctx, connect.NewRequest(req)); codeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("%s: %v, want InvalidArgument", name, err)
		}
	}
	if _, err := h.admin.QueryMetrics(ctx, connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: 999, From: base, To: base + 600})); codeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown node: %v, want NotFound", err)
	}
}

func TestUnicodeValidationAndQueryRangeEdges(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	for _, name := range []string{"", "\x00\u202e ", strings.Repeat("😀", 65)} {
		t.Run("name/"+name, func(t *testing.T) {
			if _, err := h.admin.CreateNode(ctx, connect.NewRequest(&heronv1.CreateNodeRequest{Name: name})); codeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("invalid name accepted: %v", err)
			}
		})
	}
	id, _ := h.createNode(t, strings.Repeat("😀", 64))
	for _, n := range []int{1024, 1025} {
		_, err := h.admin.UpdateNode(ctx, connect.NewRequest(&heronv1.UpdateNodeRequest{Id: id, Name: "n", Note: strings.Repeat("😀", n), TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0)}))
		if (n == 1024 && err != nil) || (n == 1025 && codeOf(err) != connect.CodeInvalidArgument) {
			t.Errorf("note length %d: %v", n, err)
		}
	}
	for _, q := range []*heronv1.QueryMetricsRequest{
		{NodeId: id, From: -1, To: 60},
		{NodeId: id, From: math.MinInt64, To: math.MaxInt64},
		{NodeId: id, From: 1, To: 1 + 18446744074},
	} {
		if _, err := h.admin.QueryMetrics(ctx, connect.NewRequest(q)); codeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("invalid time range accepted: %v err=%v", q, err)
		}
	}

	b := metric.NewBucket()
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(7)})
	ts := int64(math.MaxInt64 - math.MaxInt64%60)
	if _, err := h.store.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{{NodeID: id, TS: ts, CoverageStart: ts, Bucket: b}}}); err != nil {
		t.Fatal(err)
	}
	out, err := h.admin.QueryMetrics(ctx, connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: id, From: math.MaxInt64 - 60, To: math.MaxInt64}))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Msg.Ts) != 1 || out.Msg.Ts[0] != ts || out.Msg.Series[0].Samples[0].GetMean() != 7 {
		t.Fatalf("near-limit query lost its row: %v", out.Msg)
	}
}

func TestDeletedNodeRejectsLateStorageWrites(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	id, _ := h.createNode(t, "deleted")
	if _, err := h.admin.DeleteNode(ctx, connect.NewRequest(&heronv1.DeleteNodeRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertFacts(ctx, id, 1, &heronv1.Facts{Hostname: "late"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("late facts write: %v, want ErrNotFound", err)
	}
	b := metric.NewBucket()
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(1)})
	if n, err := h.store.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{{NodeID: id, TS: h.clk.Now().Unix(), CoverageStart: h.clk.Now().Unix(), Bucket: b}}}); err != nil || n != 1 {
		t.Errorf("late metric write: rejected=%d err=%v", n, err)
	}
	counts := rowCounts(t, h.dbPath)
	if counts["node_facts"] != 0 || counts["metric_1m"] != 0 {
		t.Fatalf("late writes recreated deleted history: %v", counts)
	}
}

func TestSessionBoundaryAndRevocation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	raw := loginRaw(t, h, "").Cookies()[0].Value
	anonymous := heronv1connect.NewAdminServiceClient(h.srv.Client(), h.srv.URL)
	for _, header := range []http.Header{
		{"Authorization": []string{"Bearer " + raw}},
		{"Cookie": []string{"heron_session=not-a-session"}},
		{"Cookie": []string{"heron_session="}},
	} {
		req := connect.NewRequest(&heronv1.ListNodesRequest{})
		for key, values := range header {
			for _, v := range values {
				req.Header().Add(key, v)
			}
		}
		if _, err := anonymous.ListNodes(ctx, req); codeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("invalid credentials admitted: %v err=%v", header, err)
		}
	}
	logout := connect.NewRequest(&heronv1.LogoutRequest{})
	logout.Header().Set("Cookie", SessionCookie+"="+raw)
	out, err := anonymous.Logout(ctx, logout)
	if err != nil {
		t.Fatal(err)
	}
	cookies := (&http.Response{Header: out.Header()}).Cookies()
	// 清除用的 cookie 与签发的一样 host-only：带 Domain 的清除项是另一个 cookie，清不掉 host-only 的会话。
	if len(cookies) != 1 || cookies[0].MaxAge != -1 || cookies[0].Value != "" || cookies[0].Path != "/" || cookies[0].Domain != "" {
		t.Fatalf("logout cookie = %v", cookies)
	}
	replay := connect.NewRequest(&heronv1.ListNodesRequest{})
	replay.Header().Set("Cookie", SessionCookie+"="+raw)
	if _, err := anonymous.ListNodes(ctx, replay); codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked cookie replay admitted: %v", err)
	}
	if _, err := h.admin.CreateNode(ctx, connect.NewRequest(&heronv1.CreateNodeRequest{Name: strings.Repeat("x", maxSettingsBody+1)})); codeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("oversized body: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := anonymous.Login(ctx, connect.NewRequest(&heronv1.LoginRequest{Password: "incorrect"})); codeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("failed login %d: %v", i, err)
		}
	}
	if _, err := anonymous.Login(ctx, connect.NewRequest(&heronv1.LoginRequest{Password: password})); codeOf(err) != connect.CodeUnauthenticated || !strings.Contains(err.Error(), "15 minutes") {
		t.Fatalf("locked login: %v", err)
	}
}

func TestWindowAndQueryAdmissionBounds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	for _, r := range []*heronv1.OpenRegisterWindowRequest{
		{TtlS: 604801, MaxNodes: 1}, {TtlS: 60, MaxNodes: 0}, {TtlS: 60, MaxNodes: 1001},
	} {
		if _, err := h.admin.OpenRegisterWindow(ctx, connect.NewRequest(r)); codeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("invalid window accepted: %v err=%v", r, err)
		}
	}
	for _, r := range []*heronv1.OpenRegisterWindowRequest{{TtlS: 60, MaxNodes: 1}, {TtlS: 604800, MaxNodes: 1000}} {
		if _, err := h.admin.OpenRegisterWindow(ctx, connect.NewRequest(r)); err != nil {
			t.Fatalf("window boundary rejected: %v err=%v", r, err)
		}
	}
	id, _ := h.createNode(t, "n")
	for _, tc := range []struct {
		span  int64
		level string
		step  uint32
	}{
		{6 * 3600, "1m", 60}, {6*3600 + 1, "5m", 300}, {7 * 86400, "5m", 900}, {7*86400 + 1, "1h", 3600},
	} {
		resp, err := h.admin.QueryMetrics(ctx, connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: id, From: 0, To: tc.span}))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Msg.Level != tc.level || resp.Msg.StepS != tc.step {
			t.Errorf("default query span %d: level=%s step=%d", tc.span, resp.Msg.Level, resp.Msg.StepS)
		}
	}
}

// testBootID 是夹具用的启动周期标识：准入只收 UUID（ingest.validateBootID），与真实 agent 读到的写法一致。
const testBootID = "3f2b8c1e-6a4d-4e9b-8c7f-1d2e3f4a5b6c"

func netCounters(boot string, rx, tx uint64) *heronv1.Metrics {
	return &heronv1.Metrics{BootId: boot, NetRxTotal: proto.Uint64(rx), NetTxTotal: proto.Uint64(tx)}
}

func TestTrafficIsReportedAdjustedAndConfigured(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	id, tok := h.createNode(t, "n")
	if err := h.report(t, tok, netCounters(testBootID, 1000, 1000)); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(10 * time.Second)
	if err := h.report(t, tok, netCounters(testBootID, 1000+1<<20, 2000)); err != nil {
		t.Fatal(err)
	}
	jan1, feb1, jan15 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC).Unix(), time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC).Unix()

	snap, err := h.admin.GetSnapshot(ctx, connect.NewRequest(&heronv1.GetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if tr := snap.Msg.Nodes[0].GetTraffic(); tr.GetTotalRx() != 1<<20 || tr.GetPeriodRx() != 1<<20 || tr.GetTotalTx() != 1000 || tr.GetResetDay() != 1 || tr.GetPeriodStart() != jan1 || tr.GetNextResetAt() != feb1 {
		t.Fatalf("snapshot traffic = %v", tr)
	}
	all, err := h.admin.GetTraffic(ctx, connect.NewRequest(&heronv1.GetTrafficRequest{}))
	if err != nil || len(all.Msg.Nodes) != 1 || all.Msg.Nodes[0].GetNodeId() != id || all.Msg.Nodes[0].GetName() != "n" || all.Msg.Nodes[0].GetTraffic().GetTotalTx() != 1000 || all.Msg.GetNow() != h.clk.Now().Unix() {
		t.Fatalf("GetTraffic = %v %v", all, err)
	}

	if all.Msg.GetTimezone() != "UTC" {
		t.Fatalf("timezone = %q, want UTC", all.Msg.GetTimezone())
	}

	adj, err := h.admin.AdjustTraffic(ctx, connect.NewRequest(&heronv1.AdjustTrafficRequest{NodeId: id, PeriodRx: 5 << 30, PeriodTx: 0}))
	if err != nil {
		t.Fatal(err)
	}
	if tr := adj.Msg.GetTraffic(); tr.GetPeriodRx() != 5<<30 || tr.GetTotalRx() != 5<<30 || tr.GetPeriodTx() != 0 || tr.GetTotalTx() != 0 {
		t.Fatalf("after adjust: %v", tr)
	}
	h.clk.Advance(10 * time.Second)
	if err := h.report(t, tok, netCounters(testBootID, 1000+1<<20+100, 2000)); err != nil {
		t.Fatal(err)
	}
	if e, _ := h.book.Get(id); e.PeriodRx != 5<<30+100 || len(e.Baseline) != 1 || e.Baseline[0].Rx != 1000+1<<20+100 {
		t.Fatalf("baseline must survive the adjustment: %+v", e)
	}
	if _, err := h.admin.AdjustTraffic(ctx, connect.NewRequest(&heronv1.AdjustTrafficRequest{NodeId: 999, PeriodRx: 1})); codeOf(err) != connect.CodeNotFound {
		t.Fatalf("adjust unknown node: %v, want NotFound", err)
	}
	if _, ok := h.book.Get(999); ok {
		t.Fatal("rejected adjustment left a traffic entry behind")
	}

	upd, err := h.admin.UpdateNode(ctx, connect.NewRequest(&heronv1.UpdateNodeRequest{Id: id, Name: "n", TrafficResetDay: 15, OfflineGraceS: proto.Uint32(0)}))
	if err != nil || upd.Msg.GetNode().GetTrafficResetDay() != 15 {
		t.Fatalf("UpdateNode reset day: %v %v", upd, err)
	}
	all, _ = h.admin.GetTraffic(ctx, connect.NewRequest(&heronv1.GetTrafficRequest{}))
	if tr := all.Msg.Nodes[0].GetTraffic(); tr.GetResetDay() != 15 || tr.GetNextResetAt() != jan15 {
		t.Fatalf("traffic after changing the reset day: %v", tr)
	}
	for _, day := range []uint32{0, 29} {
		_, err := h.admin.UpdateNode(ctx, connect.NewRequest(&heronv1.UpdateNodeRequest{Id: id, Name: "n", TrafficResetDay: day, OfflineGraceS: proto.Uint32(0)}))
		if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "traffic_reset_day") {
			t.Fatalf("reset day %d: %v, want InvalidArgument naming the field", day, err)
		}
	}
	list, _ := h.admin.ListNodes(ctx, connect.NewRequest(&heronv1.ListNodesRequest{}))
	if list.Msg.Nodes[0].GetTrafficResetDay() != 15 {
		t.Fatalf("rejected updates must not change the reset day: %v", list.Msg.Nodes[0])
	}
	// 库里没有的节点：更新失败，内存里的重置日也不得被改。
	if _, err := h.admin.UpdateNode(ctx, connect.NewRequest(&heronv1.UpdateNodeRequest{Id: 999, Name: "x", TrafficResetDay: 20, OfflineGraceS: proto.Uint32(0)})); codeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown node: %v", err)
	}
	if day := h.book.View(999).ResetDay; day != 1 {
		t.Fatalf("failed update changed the in-memory reset day to %d", day)
	}
}

func TestQueryMetricsEmitsSumForAdditiveColumns(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	id, _ := h.createNode(t, "n")
	base := h.clk.Now().Truncate(time.Hour).Unix()
	filled := metric.NewBucket()
	filled.AddSum(metric.RxBytes, 1500)
	filled.AddSum(metric.RxBytes, 500)
	empty := metric.NewBucket()
	empty.Add(&heronv1.Metrics{CpuPct: proto.Float64(1)})
	if _, err := h.store.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{{NodeID: id, TS: base, CoverageStart: base, Bucket: filled}, {NodeID: id, TS: base + 60, CoverageStart: base, Bucket: empty}}}); err != nil {
		t.Fatal(err)
	}
	resp, err := h.admin.QueryMetrics(ctx, connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: id, From: base, To: base + 120, MaxPoints: 2}))
	if err != nil {
		t.Fatal(err)
	}
	var rx *heronv1.MetricSeries
	for _, s := range resp.Msg.Series {
		if s.GetName() == "rx_bytes" {
			rx = s
		}
	}
	if rx == nil || rx.GetUnit() != "bytes" || len(rx.Samples) != 2 {
		t.Fatalf("rx_bytes series = %v", rx)
	}
	if s := rx.Samples[0]; s.GetN() != 2 || s.Sum == nil || *s.Sum != 2000 || s.Mean != nil || s.Max != nil {
		t.Fatalf("rx sample 0 = %v, want n=2 sum=2000 and no mean/max", s)
	}
	if s := rx.Samples[1]; s.GetN() != 0 || s.Sum != nil {
		t.Fatalf("rx sample 1 = %v, want n=0 with no sum", s)
	}
	if cpu := resp.Msg.Series[0].Samples[1]; cpu.Sum != nil || cpu.Mean == nil {
		t.Fatalf("cpu sample = %v, want mean without sum", cpu)
	}
}

func TestAdjustTrafficRejectsOutOfRangeUsage(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	for _, tc := range []struct {
		field  string
		rx, tx uint64
	}{{"period_rx", math.MaxUint64, 0}, {"period_tx", 0, math.MaxUint64}} {
		t.Run(tc.field, func(t *testing.T) {
			_, err := h.admin.AdjustTraffic(t.Context(), connect.NewRequest(&heronv1.AdjustTrafficRequest{NodeId: id, PeriodRx: tc.rx, PeriodTx: tc.tx}))
			if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("%s: %v, want InvalidArgument naming the field", tc.field, err)
			}
		})
	}
}
