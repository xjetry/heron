package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

// noServeEnv 是空环境：生命周期用例只看显式 flag，不受跑测试的 shell 里 HERON_* 的影响。
func noServeEnv(string) (string, bool) { return "", false }

func lifecycleOptions(t *testing.T, flags ...string) serveOptions {
	t.Helper()
	opts, err := parseServeOptions(append([]string{"--db", filepath.Join(t.TempDir(), "hub.db"), "--timezone", "UTC"}, flags...), noServeEnv)
	if err != nil {
		t.Fatal(err)
	}
	return opts
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// seedAdminSecurity 在 db 建库、设管理员密码，并把 admin_security 的 data 原样写成 data；不经 auth 序列化，所以可以
// 写进 auth 读不懂的内容。admin_security 只在有管理员时才读得到（store.AdminSecurity 与 admin 表连接），故先设密码。
// 返回前关库：调用方随后以 newHub 或 runServeWith 重新打开。
func seedAdminSecurity(t *testing.T, db, data string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(db, clock.Real(), discardLog(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetAdminPassword(ctx, "unused-hash"); err != nil {
		t.Fatal(err)
	}
	before, err := st.AdminSecurity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.CommitAdminSecurity(ctx, before, data, false, nil, now, now, ""); err != nil {
		t.Fatal(err)
	}
}

// settledGoroutines 等协程数回到 baseline 以下。cmd/hub 的用例不并行，baseline 之后新起而未退出的协程只能来自被测的
// hub：库的写协程与后台循环都在其中，漏关库或漏停循环都会让计数停在 baseline 之上。
func settledGoroutines(t *testing.T, baseline int) {
	t.Helper()
	testwait.Until(t, 10*time.Millisecond, func() bool { return runtime.NumGoroutine() <= baseline },
		"goroutines did not return to %d: %v", baseline, testwait.When(func() string { return strconv.Itoa(runtime.NumGoroutine()) }))
}

func requireStoreClosed(t *testing.T, h *hub) {
	t.Helper()
	if _, err := h.st.ListNodes(context.Background()); err == nil || !strings.Contains(err.Error(), "database is closed") {
		t.Fatalf("store still open after run returned: %v", err)
	}
}

// 装配在库打开之后失败时，newHub 自己关库：调用方拿不到 hub，也就没有别人能关它。这里的失败是库里 admin_security
// 的 data 不是 JSON，ConfigureWebAuthn 读安全状态时解析失败，这是开库之后真实存在的失败路径。
//
// 不并行：断言以 runtime.NumGoroutine 为基线，协程数是进程级的，并行用例的协程会算进来。
func TestNewHubClosesTheStoreWhenAssemblyFails(t *testing.T) {
	opts := lifecycleOptions(t, "--listen", "127.0.0.1:0")
	seedAdminSecurity(t, opts.db, "{not json")
	baseline := runtime.NumGoroutine()
	h, err := newHub(opts, clock.NewFake(time.Now()), discardLog())
	var syntax *json.SyntaxError
	if err == nil || h != nil || !errors.As(err, &syntax) {
		t.Fatalf("newHub = %v, %v; want an error from parsing the unreadable admin_security", h, err)
	}
	// 损坏的库要报成绑定这一步的失败，不能被报成 --admin-origin 的配置问题：这里根本没给那个 flag。
	if msg := err.Error(); !strings.HasPrefix(msg, "passkey binding: ") || strings.Contains(msg, "--admin-origin") {
		t.Errorf("err = %q, want it prefixed with the passkey binding step and not naming --admin-origin", msg)
	}
	settledGoroutines(t, baseline)
}

// 监听失败时 run 仍是库的唯一所有者：返回前关库，不过更新门，不起任何循环。
//
// 不并行：断言以 runtime.NumGoroutine 为基线，协程数是进程级的，并行用例的协程会算进来。
func TestRunClosesTheStoreWhenListenFails(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	baseline := runtime.NumGoroutine()
	h, err := newHub(lifecycleOptions(t, "--listen", blocker.Addr().String()), clock.NewFake(time.Now()), discardLog())
	if err != nil {
		t.Fatal(err)
	}
	gated := false
	h.gate = func(context.Context) error { gated = true; return nil }
	if err := h.run(context.Background(), discardLog()); err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("run = %v, want a bind error", err)
	}
	if gated {
		t.Fatal("update gate ran without a listener")
	}
	requireStoreClosed(t, h)
	settledGoroutines(t, baseline)
}

// 更新门拒绝时，已建立的监听要关掉、库要关掉：门报告就绪的前提是监听已绑上，门失败则不留下半启动的 hub。
//
// 不并行：断言以 runtime.NumGoroutine 为基线，协程数是进程级的，并行用例的协程会算进来。
func TestRunClosesTheListenerAndStoreWhenTheGateFails(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	baseline := runtime.NumGoroutine()
	h, err := newHub(lifecycleOptions(t, "--listen", addr), clock.NewFake(time.Now()), discardLog())
	if err != nil {
		t.Fatal(err)
	}
	boundAtGate := false
	h.gate = func(context.Context) error {
		if l, err := net.Listen("tcp", addr); err == nil {
			l.Close()
		} else {
			boundAtGate = true
		}
		return errors.New("refused for the test")
	}
	if err := h.run(context.Background(), discardLog()); err == nil || err.Error() != "update startup gate: refused for the test" {
		t.Fatalf("run = %v, want the gate error", err)
	}
	if !boundAtGate {
		t.Fatal("gate ran before the listener was bound")
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listener left open after the gate failed: %v", err)
	}
	l.Close()
	requireStoreClosed(t, h)
	settledGoroutines(t, baseline)
}

// 取消后 run 先排空在途请求再关库：请求处理期间库必须仍可用，处理器写完的响应必须送达，run 随后才返回。
//
// 不并行：断言以 runtime.NumGoroutine 为基线，协程数是进程级的，并行用例的协程会算进来。
func TestRunDrainsInFlightRequestsBeforeClosingTheStore(t *testing.T) {
	baseline := runtime.NumGoroutine()
	h, err := newHub(lifecycleOptions(t, "--listen", "127.0.0.1:0"), clock.NewFake(time.Now()), discardLog())
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	next := h.handler
	h.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slow" {
			next.ServeHTTP(w, r)
			return
		}
		close(entered)
		<-release
		_, err := h.st.ListNodes(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		io.WriteString(w, "drained")
	})
	events := make(serveEvents, 128)
	run := runInBackground(t, func(ctx context.Context) error { return h.run(ctx, slog.New(slog.NewJSONHandler(events, nil))) })
	// 晚于 runInBackground 注册、先于它运行：用例中途失败时先放走停在处理器里的请求，run 的排空才等得到它。
	var released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var addr string
	for addr == "" {
		select {
		case e := <-events:
			if string(e["msg"]) == `"hub listening"` {
				if err := json.Unmarshal(e["listen"], &addr); err != nil {
					t.Fatal(err)
				}
			}
		case <-run.finished:
			t.Fatalf("run returned before listening: %v", run.result)
		case <-time.After(testwait.Bound):
			t.Fatal("no startup line")
		}
	}
	type response struct {
		status int
		body   string
		err    error
	}
	responses := make(chan response, 1)
	go func() {
		resp, err := ownedClient(t).Get("http://" + addr + "/slow")
		if err != nil {
			responses <- response{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		responses <- response{status: resp.StatusCode, body: string(body), err: err}
	}()
	select {
	case <-entered:
	case <-time.After(testwait.Bound):
		t.Fatal("request did not reach the handler")
	}
	run.cancel()
	select {
	case <-run.finished:
		t.Fatalf("run returned with a request in flight: %v", run.result)
	// 负向窗口：排空未完成时 run 不应返回。窗口短只会漏掉稍晚才提前返回的缺陷，不会把仍在等待的 run 判失败。
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	select {
	case r := <-responses:
		if r.err != nil || r.status != http.StatusOK || r.body != "drained" {
			t.Fatalf("in-flight request = %d %q %v, want 200 drained", r.status, r.body, r.err)
		}
	case <-time.After(testwait.Bound):
		t.Fatal("in-flight request got no response")
	}
	select {
	case <-run.finished:
		if run.result != nil {
			t.Fatalf("run = %v", run.result)
		}
	case <-time.After(testwait.Bound):
		t.Fatal("run did not return after the drain")
	}
	requireStoreClosed(t, h)
	settledGoroutines(t, baseline)
}
