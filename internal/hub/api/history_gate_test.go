package api

import (
	"context"
	"log/slog"
	"runtime"
	"slices"
	"strconv"
	"sync/atomic"

	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

// gateForTest 用调用方给的等待上限（测拒绝路径的用例缩到毫秒级），并把每来源上限固定为 4：通用语义在多空位下测，
// 结果不随测试机的 CPU 数变化；上限按 CPU 数的取法由 TestHistoryInFlightPerSourceFollowsCPUs 单独核对。
//
// 用例要先观察到等待者挂上唤醒路径、再腾出空位时，wait 取 testwait.Bound：等待者到点就放弃并把自己从计数里减掉，
// wait 短于观察所需的时间（负载下一次调度停顿可达几十毫秒），用例就在等待者已经放弃之后才去看，误报"没有挂上"。
// 这类用例的断言都是计数与状态，不含等待时长。
func gateForTest(wait time.Duration) *historyGate {
	g := newHistoryGate()
	g.wait = wait
	g.limit = 4
	return g
}

// waitForWaiters 等到 source 上恰有 want 个等待者挂在唤醒路径上（sourceInFlight.wait）。
func waitForWaiters(t *testing.T, g *historyGate, source string, want int) {
	t.Helper()
	waiting := func() int {
		g.mu.Lock()
		defer g.mu.Unlock()
		if e := g.sources[source]; e != nil {
			return e.wait
		}
		return 0
	}
	testwait.Until(t, time.Millisecond, func() bool { return waiting() == want },
		"waiters on %q = %v, want %d", source, testwait.When(func() string { return strconv.Itoa(waiting()) }), want)
}

// 每来源上限是 GOMAXPROCS 的四分之一、至少 1（依据见 historyInFlightPerSource 的注释）。
//
// 不并行：GOMAXPROCS 是进程级设置，并行用例在它被改小的期间新建的 historyGate 会拿到错的上限，调度也被压到少数处理器上。
func TestHistoryInFlightPerSourceFollowsCPUs(t *testing.T) {
	prev := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
	for _, c := range []struct{ procs, want int }{{1, 1}, {2, 1}, {3, 1}, {4, 1}, {7, 1}, {8, 2}, {16, 4}, {64, 16}} {
		runtime.GOMAXPROCS(c.procs)
		if got := newHistoryGate().limit; got != c.want {
			t.Errorf("GOMAXPROCS=%d: limit = %d, want %d", c.procs, got, c.want)
		}
	}
}

// 上限：同一来源第 5 个在飞等不到空位，按 ResourceExhausted 拒绝，文案与限流区分。
func TestHistoryGateLimitReached(t *testing.T) {
	t.Parallel()
	g := gateForTest(20 * time.Millisecond)
	for range g.limit {
		if _, err := g.acquire(context.Background(), "a"); err != nil {
			t.Fatalf("前 %d 个应当立即拿到: %v", g.limit, err)
		}
	}
	begin := time.Now()
	if _, err := g.acquire(context.Background(), "a"); err == nil {
		t.Fatal("超过上限的请求必须被拒")
	} else if connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "concurrent history queries") {
		t.Fatalf("错误应当是并发在飞过多的 ResourceExhausted: %v", err)
	}
	if d := time.Since(begin); d < 15*time.Millisecond {
		t.Fatalf("被拒前应当等满等待上限: %v", d)
	}
}

// 等待后成功：腾出空位时等待者醒来拿到。
func TestHistoryGateWaitThenSuccess(t *testing.T) {
	t.Parallel()
	g := gateForTest(testwait.Bound)
	held := make([]func(), g.limit)
	for i := range held {
		held[i], _ = g.acquire(context.Background(), "a")
	}
	got := make(chan error, 1)
	go func() {
		_, err := g.acquire(context.Background(), "a")
		got <- err
	}()
	// 等它真挂上唤醒路径再腾空位，测的才是"等待后成功"而不是"来时已有空位"。
	waitForWaiters(t, g, "a", 1)
	held[0]() // 腾出一个空位
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("有空位后等待者应当成功: %v", err)
		}
	case <-time.After(testwait.Bound):
		t.Fatal("有空位后等待者没有被唤醒")
	}
}

// 来源之间互不阻塞：来源 A 占满不挡来源 B。
//
// 不并行：断言含 20ms 的耗时上界，与负载下的调度停顿同一量级。
func TestHistoryGateSourcesIndependent(t *testing.T) {
	g := gateForTest(50 * time.Millisecond)
	for range g.limit {
		if _, err := g.acquire(context.Background(), "A"); err != nil {
			t.Fatal(err)
		}
	}
	begin := time.Now()
	if _, err := g.acquire(context.Background(), "B"); err != nil {
		t.Fatalf("来源 B 不应被来源 A 挡住: %v", err)
	}
	if time.Since(begin) > 20*time.Millisecond {
		t.Fatal("来源 B 应当立即拿到空位")
	}
}

// 错误路径也要释放：查询失败后同来源的下一个请求拿得到空位。泄漏由闸门自己的等待上界（gateForTest 的 2 秒）暴露：
// 额度被错误路径漏掉一个，最后那个正常请求就等不到空位而失败。不再另设墙钟上界——"立即"在负载下量不准，
// 而漏没漏只有"拿到 / 拿不到"两个答案。
func TestHistoryErrorPathReleasesSlot(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	g := gateForTest(2 * time.Second)
	hh := history{store: h.store, probes: h.reg, log: slog.Default(), gate: g}
	// 取消的上下文让存储读取立即失败：错误返回后空位必须已释放。把整份额度都
	// 走一遍错误路径，任何一处漏释放都会让最后一个正常请求等不到空位。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for range g.limit {
		if _, err := hh.metrics(cancelled, &heronv1.QueryMetricsRequest{NodeId: 1, From: 0, To: 3600}, 10, func(context.Context) error { return nil }); err == nil {
			t.Fatal("取消的上下文应当失败")
		}
	}
	if _, err := hh.metrics(context.Background(), &heronv1.QueryMetricsRequest{NodeId: 1, From: 0, To: 3600}, 10, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("错误路径之后同来源应当拿到空位（等待超时即空位被错误路径泄漏）: %v", err)
	}
}

// panic 路径释放：recover 之后空位可用。
func TestHistoryGateReleaseOnPanic(t *testing.T) {
	t.Parallel()
	g := gateForTest(50 * time.Millisecond)
	func() {
		defer func() { _ = recover() }()
		release, err := g.acquire(context.Background(), "a")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		panic("boom")
	}()
	for range g.limit {
		if _, err := g.acquire(context.Background(), "a"); err != nil {
			t.Fatalf("panic 之后空位没有归还: %v", err)
		}
	}
}

// 状态有界：既无在飞也无等待的来源要从表里删掉。
func TestHistoryGateIdleReclaimed(t *testing.T) {
	t.Parallel()
	g := gateForTest(testwait.Bound)
	for range 3 {
		release, err := g.acquire(context.Background(), "a")
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if len(g.sources) != 0 {
		t.Fatalf("空闲来源没有被回收: %v", g.sources)
	}
	// 等待者存在时不能删：占满份额后，等待者真挂在唤醒路径上时来源行必须还在。
	var held []func()
	for range g.limit {
		r, err := g.acquire(context.Background(), "a")
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, r)
	}
	waited := make(chan func(), 1)
	go func() {
		r, err := g.acquire(context.Background(), "a")
		if err != nil {
			r = func() {}
		}
		waited <- r
	}()
	// 等这个等待者真挂上唤醒路径（wait=1）再断言来源行还在。
	waitForWaiters(t, g, "a", 1)
	g.mu.Lock()
	if _, alive := g.sources["a"]; !alive {
		t.Fatalf("有等待者时来源不应被删除: %v", g.sources)
	}
	g.mu.Unlock()
	held[0]() // 腾出一个空位
	select {
	case waiterRelease := <-waited:
		waiterRelease()
	case <-time.After(testwait.Bound):
		t.Fatal("等待者没有被唤醒")
	}
	for _, r := range held[1:] {
		r()
	}
	release, err := g.acquire(context.Background(), "a")
	if err != nil {
		t.Fatalf("等待者拿过的空位没有归还: %v", err)
	}
	release()
	g.mu.Lock()
	if len(g.sources) != 0 {
		t.Fatalf("等待者走后来源应当回收: %v", g.sources)
	}
	g.mu.Unlock()
}

// 被唤醒的等待者拿到空位后，全部归还时来源同样回收；两个等待者争一个空位时，没抢到的重新排队，
// 不把自己多算一次。等待计数漏减时，来源永远回收不掉，状态随出现过争用的来源数增长。
func TestHistoryGateWokenWaitersReclaimed(t *testing.T) {
	t.Parallel()
	g := gateForTest(testwait.Bound)
	held := make([]func(), g.limit)
	for i := range held {
		var err error
		if held[i], err = g.acquire(context.Background(), "a"); err != nil {
			t.Fatal(err)
		}
	}
	const waiters = 2
	got := make(chan func(), waiters)
	for range waiters {
		go func() {
			release, err := g.acquire(context.Background(), "a")
			if err != nil {
				t.Error(err)
				release = func() {}
			}
			got <- release
		}()
	}
	waitFor := func(want int) {
		t.Helper()
		waitForWaiters(t, g, "a", want)
	}
	waitFor(waiters)
	held[0]() // 一个空位、两个等待者：一个拿到，另一个重新排队
	first := <-got
	waitFor(waiters - 1)
	held[1]()
	second := <-got
	waitFor(0)
	first()
	second()
	for _, release := range held[2:] {
		release()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.sources["a"]; ok {
		t.Fatalf("全部归还后来源没有被回收: inUse=%d wait=%d", e.inUse, e.wait)
	}
}

func (s *Service) testHistoryGate() *historyGate { return s.history.gate }
func (p *Public) testHistoryGate() *historyGate  { return p.history.gate }

// 两个服务的全部历史查询入口都经过按来源并发闸：占满该来源的空位后，入口返回
// 并发在飞过多的 ResourceExhausted；空位归还后入口恢复。任一入口绕过闸都会变绿而红。
func TestHistoryEntriesGated(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := t.Context()
	id, _ := h.createNode(t, "n")
	h.setPublic(t, id, "n", true)

	// 公开端：来源键是限流器的客户端来源（回环地址）。
	pg := h.pub.testHistoryGate()
	pg.wait = 10 * time.Millisecond
	pub := h.publicClient()
	publicEntries := map[string]func() error{
		"QueryMetrics": func() error {
			_, err := pub.QueryMetrics(ctx, connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: 1, From: 0, To: 3600}))
			return err
		},
		"QueryProbes": func() error {
			_, err := pub.QueryProbes(ctx, connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: 1, From: 0, To: 3600}))
			return err
		},
		"QueryProbeComparison": func() error {
			_, err := pub.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{TaskId: 1, NodeIds: []int64{1}, From: 0, To: 3600}))
			return err
		},
	}
	for name, call := range publicEntries {
		t.Run("公开/"+name, func(t *testing.T) {
			assertGated(t, pg, "127.0.0.1", call)
		})
	}

	// 管理端：来源键是调用方凭据（API token 编号）。
	admin, tokenID, _ := grantedClient(t, h, &heronv1.TokenGrant{AllNodes: true})
	ag := h.svc.testHistoryGate()
	ag.wait = 10 * time.Millisecond
	key := "api-token/" + strconv.FormatInt(tokenID, 10)
	adminEntries := map[string]func() error{
		"QueryMetrics": func() error {
			_, err := admin.QueryMetrics(ctx, connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: 1, From: 0, To: 3600}))
			return err
		},
		"QueryProbes": func() error {
			_, err := admin.QueryProbes(ctx, connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: 1, From: 0, To: 3600}))
			return err
		},
		"QueryProbeComparison": func() error {
			_, err := admin.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{TaskId: 1, NodeIds: []int64{1}, From: 0, To: 3600}))
			return err
		},
	}
	for name, call := range adminEntries {
		t.Run("管理/"+name, func(t *testing.T) {
			assertGated(t, ag, key, call)
		})
	}
}

func assertGated(t *testing.T, g *historyGate, key string, call func() error) {
	t.Helper()
	var held []func()
	for range g.limit {
		r, err := g.acquire(t.Context(), key)
		if err != nil {
			t.Fatalf("占位失败: %v", err)
		}
		held = append(held, r)
	}
	err := call()
	if err == nil || connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "concurrent history queries") {
		t.Fatalf("占满空位后应当返回并发在飞过多: %v", err)
	}
	for _, r := range held {
		r()
	}
	if err := call(); err != nil && strings.Contains(err.Error(), "concurrent history queries") {
		t.Fatalf("归还空位后入口仍被并发闸拒绝: %v", err)
	}
}

// 来源键必须区分调用方：键若退化成全局，一个来源的重查询会挡住所有来源。
// 这里从真实入口验证——凭据 A 占满自己的份额后，凭据 B 的同型查询立即执行。
func TestHistorySourcesDoNotBlockEachOther(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := t.Context()
	h.createNode(t, "n")
	ca, ida, _ := grantedClient(t, h, &heronv1.TokenGrant{AllNodes: true})
	cb, idb, _ := grantedClient(t, h, &heronv1.TokenGrant{AllNodes: true})
	_ = ca
	if ida == idb {
		t.Fatal("两个 token 应当不同")
	}
	ag := h.svc.testHistoryGate()
	ag.wait = 10 * time.Millisecond
	for range ag.limit {
		if _, err := ag.acquire(context.Background(), historySource(store.WithPrincipal(context.Background(), store.APIToken{ID: ida}))); err != nil {
			t.Fatal(err)
		}
	}
	begin := time.Now()
	if _, err := cb.QueryMetrics(ctx, connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: 1, From: 0, To: 3600, MaxPoints: 10})); err != nil {
		t.Fatalf("凭据 B 不应被凭据 A 挡住: %v", err)
	}
	if time.Since(begin) > time.Second {
		t.Fatal("凭据 B 应当立即执行")
	}
}

// 不变式：历史请求的节点准入要读库，必须发生在它持有本来源空位期间；只有不碰库的
// 前置校验（窗口、编号范围、分块清单的形状）允许在拿空位之前。可观测事实：占满来源
// 的全部空位后，再来的请求连节点准入都答不出 404——它等不到空位，超时后只能拿到
// ResourceExhausted；准入若在拿空位之前碰了库，请求会立刻以 404 返回（这个断言在
// 注入"准入挪回拿空位之前"时变红）。对比入口对缺失节点不报 404，这条断言照不到它们的
// 准入顺序，只核对闸与释放；准入顺序由 TestComparisonNodeChecksHappenInsideTheSlot 在
// history 层直接计数。
func TestHistoryNodeChecksHappenInsideTheSlot(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := t.Context()
	id, _ := h.createNode(t, "n")
	h.setPublic(t, id, "n", true)
	saved, err := h.admin.SaveProbeTask(ctx, connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "127.0.0.1", IntervalS: 5, TimeoutMs: 1000}, NodeIds: []int64{id}}))
	if err != nil {
		t.Fatal(err)
	}
	task := saved.Msg.Task.Task.Id
	const missing = int64(99999) // 不存在的节点编号：准入检查会读库并答 404

	// 管理端：token 凭据；公开端：客户端来源（回环地址）。
	admin, tokenID, _ := grantedClient(t, h, &heronv1.TokenGrant{AllNodes: true})
	ag, pg := h.svc.testHistoryGate(), h.pub.testHistoryGate()
	ag.wait, pg.wait = 10*time.Millisecond, 10*time.Millisecond

	adminKey := "api-token/" + strconv.FormatInt(tokenID, 10)
	publicKey := "127.0.0.1"
	pub := h.publicClient()

	adminCalls := map[string]func() error{
		"QueryMetrics": func() error {
			_, err := admin.QueryMetrics(ctx, connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: missing, From: 0, To: 3600, MaxPoints: 10}))
			return err
		},
		"QueryProbes": func() error {
			_, err := admin.QueryProbes(ctx, connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: missing, From: 0, To: 3600, MaxPoints: 10}))
			return err
		},
		"QueryProbeComparison": func() error {
			resp, err := admin.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{TaskId: task, NodeIds: []int64{missing}, From: 0, To: 3600, MaxPoints: 10}))
			if err == nil && !slices.Contains(resp.Msg.GetUnavailableNodeIds(), missing) {
				t.Fatalf("缺失节点应当进 unavailable_node_ids: %v", resp.Msg.GetUnavailableNodeIds())
			}
			return err
		},
	}
	publicCalls := map[string]func() error{
		"QueryMetrics": func() error {
			_, err := pub.QueryMetrics(ctx, connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: missing, From: 0, To: 3600, MaxPoints: 10}))
			return err
		},
		"QueryProbes": func() error {
			_, err := pub.QueryProbes(ctx, connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: missing, From: 0, To: 3600, MaxPoints: 10}))
			return err
		},
		"QueryProbeComparison": func() error {
			resp, err := pub.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{TaskId: task, NodeIds: []int64{missing}, From: 0, To: 3600, MaxPoints: 10}))
			if err == nil && !slices.Contains(resp.Msg.GetUnavailableNodeIds(), missing) {
				t.Fatalf("缺失节点应当进 unavailable_node_ids: %v", resp.Msg.GetUnavailableNodeIds())
			}
			return err
		},
	}

	for name, call := range adminCalls {
		t.Run("管理/"+name, func(t *testing.T) {
			assertBehindGate(t, ag, adminKey, call, wantErrFor(name))
		})
	}
	for name, call := range publicCalls {
		t.Run("公开/"+name, func(t *testing.T) {
			assertBehindGate(t, pg, publicKey, call, wantErrFor(name))
		})
	}
}

// 对比入口对缺失节点不报错（进 unavailable_node_ids），占满空位时它照样拿到闸的拒绝，入口上分不出
// "准入在持位后执行"与"准入先碰了库再排队"。这里在 history 层直接数逐节点准入的调用：占满本来源
// 的空位时一次都不能调用，放开后每个节点调用一次。两端的入口只把各自的谓词交给 history.comparison，
// 准入的执行顺序只在这一处决定。
func TestComparisonNodeChecksHappenInsideTheSlot(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	for _, side := range []struct {
		name string
		hist history
	}{{"管理", h.svc.history}, {"公开", h.pub.history}} {
		t.Run(side.name, func(t *testing.T) {
			g := side.hist.gate
			g.wait = 10 * time.Millisecond
			var calls atomic.Int64
			visible := func(context.Context, int64) (bool, error) {
				calls.Add(1)
				return false, nil
			}
			key := historySource(t.Context())
			var held []func()
			for range g.limit {
				r, err := g.acquire(t.Context(), key)
				if err != nil {
					t.Fatalf("占位失败: %v", err)
				}
				held = append(held, r)
			}
			_, err := side.hist.comparison(t.Context(), 1, []int64{7, 8}, 0, 3600, 10, visible)
			for _, r := range held {
				r()
			}
			if connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "concurrent history queries") {
				t.Fatalf("占满空位时应当被闸拒绝: %v", err)
			}
			if n := calls.Load(); n != 0 {
				t.Fatalf("占满空位时逐节点准入被调用了 %d 次，应为 0", n)
			}
			resp, err := side.hist.comparison(t.Context(), 1, []int64{7, 8}, 0, 3600, 10, visible)
			if err != nil {
				t.Fatal(err)
			}
			if n := calls.Load(); n != 2 {
				t.Fatalf("放开后逐节点准入调用 %d 次，应为 2", n)
			}
			if got := resp.GetUnavailableNodeIds(); !slices.Equal(got, []int64{7, 8}) {
				t.Fatalf("unavailable_node_ids = %v, want [7 8]", got)
			}
		})
	}
}

// assertBehindGate：占满来源的空位后，调用必须等不到空位而 ResourceExhausted（说明
// 准入没有先碰库）；放开后同一个调用要走到库并得到预期的准入错误码。
func wantErrFor(name string) connect.Code {
	if name == "QueryProbeComparison" {
		return 0 // 对比入口对缺失节点不加区分地进不可用清单，不报 404
	}
	return connect.CodeNotFound
}

func assertBehindGate(t *testing.T, g *historyGate, key string, call func() error, wantErr connect.Code) {
	t.Helper()
	var held []func()
	for range g.limit {
		r, err := g.acquire(t.Context(), key)
		if err != nil {
			t.Fatalf("占位失败: %v", err)
		}
		held = append(held, r)
	}
	err := call()
	for _, r := range held {
		r()
	}
	if err == nil || connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "concurrent history queries") {
		t.Fatalf("占满空位时必须等不到空位而拒绝（说明准入碰了库）: %v", err)
	}
	// 放开空位后：准入照常执行；错误路径（404）或不可用清单照常释放空位。
	err = call()
	if wantErr != 0 && err == nil {
		t.Fatal("放开后准入应当执行并报准入错误")
	}
	if err != nil && connect.CodeOf(err) == connect.CodeResourceExhausted && strings.Contains(err.Error(), "concurrent history queries") {
		t.Fatalf("放开后仍被闸拒绝（空位没释放）: %v", err)
	}
	// 闸上不留来源行。
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.sources) != 0 {
		t.Fatalf("准入错误路径之后来源没有回收: %v", g.sources)
	}
}
