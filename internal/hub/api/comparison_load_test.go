package api

// 成本验收：对比分块每个窗口的 p99 不超过同窗口的单节点查询；饱和组把样本按是否落在重请求区间分成压着与空闲两类，压着类的 p99 不超过空闲类的 2 倍（样本不足或环境不平稳时不判定）。数据与负载都重（约 56M 行、分钟级耗时、多 GiB 库），默认跳过。跑法：
//
//	HERON_LOAD=1 [HERON_LOAD_DB=<目录>] go test ./internal/hub/api/ -run TestProbeComparisonCostAcceptance -v -timeout 2h
//
// HERON_LOAD_DB 指定的目录里已有 load.db 时跳过生成直接复用（升级计时也用它）。
// 测试输出各组 p50/p99 与服务进程的 HeapAlloc 峰值。
//
// 入口是真实的公开处理器（限流 → 缓存头 → connect → Public），外面包一层按过程计数的
// 中间件当读路径计数器：除 GetSnapshot（另有快照字节缓存）外，进到 connect 层就是进了
// 读取路径；负载全部用 POST，客户端与公开页的缓存都不参与。水位回拨用第二个直连连接
// 写 rollup_state，等价于维护进程在推进水位。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	_ "modernc.org/sqlite"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/live"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
)

// loadClock：墙钟固定在数据的"现在"，单调钟走真实时间——限流桶按它补充令牌。
type loadClock struct {
	wall  time.Time
	start time.Time
}

func (c *loadClock) Now() time.Time      { return c.wall }
func (c *loadClock) Mono() time.Duration { return time.Since(c.start) }

const (
	loadNodesFull = 152 // 总节点数（其余 120 个只有节点行）
	loadBusyFull  = 32  // 各有 64 个任务的节点数
	loadTasksFull = 64  // 每个忙节点的任务数（被比任务 + 另外 63 个）
	loadCallsFull = 30  // 每个尺寸点每窗口的调用次数
	hammerSecFull = 30

	hammerSource        = "198.51.100.1"
	hammerProbesSource  = "198.51.100.5"
	hammerMetricsSource = "198.51.100.6"
	readerProbesSource  = "198.51.100.7"
	readerSource        = "198.51.100.2"
	burstSource         = "198.51.100.3"
	probeSource         = "198.51.100.4" // ④ 受保护读者专用：与前面几组的桶无关。
)

// smoke：HERON_LOAD_SMOKE=1 时全部缩到分钟级，验证跑法本身；结果不作数。
var smoke = os.Getenv("HERON_LOAD_SMOKE") != ""

func loadSizes() (nodes, busy, tasks, calls, hammerSec int) {
	if smoke {
		return 10, 4, 3, 3, 2
	}
	return loadNodesFull, loadBusyFull, loadTasksFull, loadCallsFull, hammerSecFull
}

func TestProbeComparisonCostAcceptance(t *testing.T) {
	t.Parallel()
	if os.Getenv("HERON_LOAD") == "" {
		t.Skip("set HERON_LOAD=1 to run the cost acceptance (heavy: minutes, multi-GB database)")
	}
	now := loadBenchNow
	day := int64(86400)
	dir := os.Getenv("HERON_LOAD_DB")
	if dir == "" {
		dir = t.TempDir()
	}
	path := filepath.Join(dir, "load.db")
	clk := &loadClock{wall: now, start: time.Now()}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		genLoadDatabase(t, path, now.Unix())
	} else {
		t.Logf("reusing existing database %s", path)
	}
	if os.Getenv("HERON_LOAD_GEN_ONLY") != "" {
		t.Logf("database generated; skipping load groups (HERON_LOAD_GEN_ONLY)")
		return
	}
	tuner, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tuner.Close() })

	// 内存峰值采样：50ms 一次，读的是服务进程（即本测试进程）的运行时。
	var peakHeap atomic.Int64
	stopPeak := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopPeak:
				return
			default:
			}
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if int64(m.HeapAlloc) > peakHeap.Load() {
				peakHeap.Store(int64(m.HeapAlloc))
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	defer close(stopPeak)

	st, err := store.Open(path, clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	l := live.New(clk, 30*time.Second)
	book := traffic.New(st, clk, time.UTC, slog.Default())
	reg := probe.New(st, slog.Default())
	if err := errors.Join(book.Load(t.Context()), reg.Load(t.Context())); err != nil {
		t.Fatal(err)
	}
	pub := NewPublic(PublicConfig{ReportInterval: 10 * time.Second, Location: time.UTC,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}},
		PublicDeps{Store: st, Live: l, Traffic: book, Probes: reg, Clock: clk, Log: slog.Default()})
	pubPath, pubHandler := pub.Handler()
	counter := &countingServer{inner: pubHandler}
	_ = pubPath
	srv := httptest.NewServer(counter)
	t.Cleanup(srv.Close)

	nodes, busy, tasks, calls, _ := loadSizes()
	plain := newSourcedClient(srv.Client(), srv.URL, readerSource)

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	baseHeap := mem.HeapAlloc
	report := newLoadReport(path)

	// ① 空载基线：单节点指标与满配任务的探测各 30 次。
	g1m := runLoadGroup("① QueryMetrics 6h", calls, func() (*loadOutcome, error) {
		r, err := plain.QueryMetrics(ctxOf(t), connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: 1, From: now.Unix() - 6*3600, To: now.Unix(), MaxPoints: 720}))
		return metricsOutcome(r), err
	})
	g1p := runLoadGroup("① QueryProbes 6h（64 任务）", calls, func() (*loadOutcome, error) {
		r, err := plain.QueryProbes(ctxOf(t), connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: 1, From: now.Unix() - 6*3600, To: now.Unix(), MaxPoints: 720}))
		return seriesOutcome(r, tasks), err
	})
	report.group(g1m)
	report.group(g1p)

	// ② 对比分块与 ③ 单节点对照。尺寸阶梯到 N_max 为止（16 是 32 时代的尺寸，已随降档移除）。
	sizes := []int{1, 2, 4, 8, store.MaxComparisonNodes}
	sizes = slices.DeleteFunc(sizes, func(n int) bool { return n > store.MaxComparisonNodes || n > nodes })
	var g2, g3 []*loadGroup
	for _, w := range []struct {
		name string
		span int64
	}{{"6h", 6 * 3600}, {"7d", 7 * day}, {"30d", 30 * day}, {"365d", 365 * day}} {
		for _, n := range sizes {
			ids := firstN(n)
			g2 = append(g2, runLoadGroup(fmt.Sprintf("② 对比 %2d 节点 %s", n, w.name), calls, func() (*loadOutcome, error) {
				r, err := plain.QueryProbeComparison(ctxOf(t), connect.NewRequest(&heronv1.QueryProbeComparisonRequest{
					TaskId: 1, NodeIds: ids, From: now.Unix() - w.span, To: now.Unix(), MaxPoints: 720}))
				return comparisonOutcome(r, n), err
			}))
		}
		g3 = append(g3, runLoadGroup(fmt.Sprintf("③ 单节点 %s", w.name), calls, func() (*loadOutcome, error) {
			r, err := plain.QueryProbes(ctxOf(t), connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: 1, From: now.Unix() - w.span, To: now.Unix(), MaxPoints: 720}))
			return seriesOutcome(r, tasks), err
		}))
	}
	for _, g := range g2 {
		report.group(g)
	}
	for _, g := range g3 {
		report.group(g)
	}
	pass23 := true
	for i := range 4 {
		var worst time.Duration
		for j := range len(sizes) {
			if d := g2[len(sizes)*i+j].p99(); d > worst {
				worst = d
			}
			if g2[len(sizes)*i+j].success != calls {
				pass23 = false
			}
		}
		if worst > g3[i].p99() || g3[i].success != calls {
			pass23 = false
		}
	}
	report.note(fmt.Sprintf("② 每窗口 p99 ≤ ③ 同窗口 p99 且全部成功：%v", pass23))

	chunkNodes := min(busy, store.MaxComparisonNodes)
	// ④ 饱和：父进程只当 hub（store + HTTP 入口 + 计数器）；重锤与读者各自是子进程
	// （go test 二进制按 -test.run 拉起），逐样本写文件，父进程按"样本发出时刻是否落在
	// 某个重请求区间内"分压着/没压着两类。每组的判定见 pairedVerdict：任一类样本不足
	// minPairedSamples 或环境不平稳时不判定。
	outDir := os.Getenv("HERON_LOAD_OUT")
	if outDir == "" {
		outDir = t.TempDir()
	}
	// 子进程按路径直接创建样本文件，目录要先在。
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	report.note("样本与报告目录：" + outDir)
	report.note("环境：" + envReportLine())
	if v := os.Getenv("HERON_LOAD_GATE_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pub.testHistoryGate().limit = n
			report.note(fmt.Sprintf("并发闸上限（环境变量）：%d", n))
		}
	}

	hammers := []struct {
		name, shape, srcIP string
	}{
		{fmt.Sprintf("④ 饱和来源（开环 365d %d 节点分块）", chunkNodes), "cmp365d", hammerSource},
		{"④-p 饱和来源（开环单节点 QueryProbes 365d）", "probes365d", hammerProbesSource},
		{"④-m 饱和来源（开环单节点 QueryMetrics 365d）", "metrics365d", hammerMetricsSource},
	}
	// 4 个开/闲周期共 200 秒：每个闲时段要扣掉开时段结束后排队请求的收尾（等空位至多 5 秒再加
	// 一次重查询），120 秒时闲类每个读者只有 180 个上下，不够 minPairedSamples。
	secs := 200
	if smoke {
		secs = 8
	}
	for _, hcfg := range hammers {
		preMin, preMed := driftProbe()
		preLoad := loadAvgLine()
		fds := startFDSampler(path)
		t0 := time.Now().Add(2 * time.Second)
		var hammerS, readerS []loadSample
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			hammerS = childRun(t, "hammer", srv.URL, t0, secs, map[string]string{
				envSource: hcfg.srcIP, envShape: hcfg.shape, envK: "0",
				envTasks: strconv.Itoa(tasks), envNodes: strconv.Itoa(chunkNodes),
			}, filepath.Join(outDir, "hammer-"+hcfg.shape+".jsonl"))
		}()
		go func() {
			defer wg.Done()
			readerS = childRun(t, "reader", srv.URL, t0, secs, map[string]string{
				envSource: readerSource, envTasks: strconv.Itoa(tasks),
			}, filepath.Join(outDir, "readers-"+hcfg.shape+".jsonl"))
		}()
		wg.Wait()
		connLine := fds.stopAndReport()
		postMin, postMed := driftProbe()
		postLoad := loadAvgLine()
		stable := "平稳"
		if preMed > 0 && float64(postMed)/float64(preMed) > 1.15 {
			stable = "环境不平稳"
		}
		reportPairedPhase(t, report, hcfg.name, hammerS, readerS, stable,
			fmt.Sprintf("负载均值 前 %s 后 %s；漂移探针 min %v→%v median %v→%v；连接采样：%s",
				preLoad, postLoad, preMin.Round(time.Millisecond), postMin.Round(time.Millisecond),
				preMed.Round(time.Millisecond), postMed.Round(time.Millisecond), connLine))
	}

	// ⑦ 闭环干扰曲线：同样走子进程；k=0 档是曲线内的读者空载对照。
	report.note("环境（曲线组前）：" + envReportLine() + " 负载 " + loadAvgLine())
	for _, kind := range []struct {
		label, shape string
		ks           []int
	}{{"365d 分块", "cmp365d", []int{0, 1, 2, 4, 8}}, {"单节点 QueryProbes 365d", "probes365d", []int{1, 2}}} {
		for _, k := range kind.ks {
			fds := startFDSampler(path)
			t0 := time.Now().Add(2 * time.Second)
			var hammerS, readerS []loadSample
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				readerS = childRun(t, "reader", srv.URL, t0, int(curveDur().Seconds()), map[string]string{
					envSource: probeSource, envTasks: strconv.Itoa(tasks),
				}, filepath.Join(outDir, fmt.Sprintf("readers-curve-%s-k%d.jsonl", kind.shape, k)))
			}()
			if k > 0 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					hammerS = childRun(t, "hammer", srv.URL, t0, int(curveDur().Seconds()), map[string]string{
						envSource: fmt.Sprintf("203.0.113.%d", k+10), envShape: kind.shape, envK: strconv.Itoa(k),
						envTasks: strconv.Itoa(tasks), envNodes: strconv.Itoa(chunkNodes),
					}, filepath.Join(outDir, fmt.Sprintf("hammer-curve-%s-k%d.jsonl", kind.shape, k)))
				}()
			} else {
				// k=0：曲线内的读者空载对照，锤文件留空（对齐用）。
				hammerFile := filepath.Join(outDir, fmt.Sprintf("hammer-curve-%s-k%d.jsonl", kind.shape, k))
				if f, err := os.Create(hammerFile); err == nil {
					f.Close()
				}
			}
			wg.Wait()
			report.note(reportCurveLevel(kind.label, k, hammerS, readerS, curveDur()) + "；连接采样：" + fds.stopAndReport())
		}
	}

	// ⑤ 维护积压：回拨到额度内重复②③的 365d 形状，再回拨到超出额度，请求被拒。
	setWatermarks := func(w5m, w1h int64) {
		for level, upto := range map[string]int64{"probe_5m": w5m, "probe_1h": w1h} {
			if _, err := tuner.Exec("UPDATE rollup_state SET upto_ts = ? WHERE level = ?", upto, level); err != nil {
				t.Fatal(err)
			}
		}
	}
	// ⑤ 的两档按 N_max=8 校准（R = 12000×8 = 96k；数据范围：1m 表 7 天、5m 表 30 天、1h 表 365 天）：
	// 额度内 = 1h 0 + 5m 6d（13824）+ 1m 7d（80640）= 94464 ≤ R（单节点 755712 ≤ 768000）；
	// 超额 = 1h 335d（64320）+ 5m 23d（52992）+ 1m 7d（80640）= 197952 > R。
	setWatermarks(now.Unix()-24*day, now.Unix()-365*day)
	chunk := min(busy, store.MaxComparisonNodes)
	within := runLoadGroup(fmt.Sprintf("⑤ 额度内 365d（%d 节点）", chunk), calls, func() (*loadOutcome, error) {
		r, err := plain.QueryProbeComparison(ctxOf(t), connect.NewRequest(&heronv1.QueryProbeComparisonRequest{
			TaskId: 1, NodeIds: firstN(chunk), From: now.Unix() - 365*day, To: now.Unix(), MaxPoints: 720}))
		return comparisonOutcome(r, chunk), err
	})
	withinSingle := runLoadGroup("⑤ 额度内 单节点 365d", calls, func() (*loadOutcome, error) {
		r, err := plain.QueryProbes(ctxOf(t), connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: 1, From: now.Unix() - 365*day, To: now.Unix(), MaxPoints: 720}))
		return seriesOutcome(r, tasks), err
	})
	report.group(within)
	report.group(withinSingle)
	report.note(fmt.Sprintf("⑤ 额度内全部成功：%v", within.success == calls && withinSingle.success == calls))

	setWatermarks(now.Unix()-7*day, now.Unix()-30*day)
	over := runLoadGroup(fmt.Sprintf("⑤ 超额 365d（%d 节点）", chunk), calls, func() (*loadOutcome, error) {
		_, err := plain.QueryProbeComparison(ctxOf(t), connect.NewRequest(&heronv1.QueryProbeComparisonRequest{
			TaskId: 1, NodeIds: firstN(chunk), From: now.Unix() - 365*day, To: now.Unix(), MaxPoints: 720}))
		return nil, err
	})
	report.group(over)
	report.note(fmt.Sprintf("⑤ 超额全部被拒（FailedPrecondition）：%v（样例：%v）",
		over.quotaRejected == calls && over.other == 0, oneLine(over.lastQuota)))
	setWatermarks(now.Unix(), now.Unix())

	report.note(fmt.Sprintf("读路径计数（进 connect 层的请求数）：对比 %d、单节点 %d、指标 %d、List %d",
		counter.count(heronv1connect.PublicServiceQueryProbeComparisonProcedure),
		counter.count(heronv1connect.PublicServiceQueryProbesProcedure),
		counter.count(heronv1connect.PublicServiceQueryMetricsProcedure),
		counter.count(heronv1connect.PublicServiceListProbeComparisonNodesProcedure)))
	runtime.ReadMemStats(&mem)
	report.note(fmt.Sprintf("服务进程内存（含生成后残留）：HeapAlloc=%d MiB Sys=%d MiB；生成结束时的 HeapAlloc=%d MiB；读路径期间峰值 HeapAlloc=%d MiB",
		mem.HeapAlloc>>20, mem.Sys>>20, baseHeap>>20, peakHeap.Load()>>20))
	report.write(t)
}

func oneLine(s string) string { return strings.SplitN(s, ";", 2)[0] }

func firstN(n int) []int64 {
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	return ids
}

// ---- 数据生成 ----

func genLoadDatabase(t *testing.T, path string, now int64) {
	t.Helper()
	nodes, busy, tasks, _, _ := loadSizes()
	t.Logf("generating load database at %s (%d nodes, %d×%d series, retention %v)", path, nodes, busy, tasks, genRetention())
	st, err := store.Open(path, clock.NewFake(time.Unix(now, 0)), slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	nodeHash := func(i int) []byte { h := make([]byte, 32); h[0] = byte(i + 1); return h }
	for i := range nodes {
		if _, _, err := st.CreateNode(ctx, store.NewNode{Name: fmt.Sprintf("node%03d", i+1)}, nodeHash(i)); err != nil {
			t.Fatal(err)
		}
	}
	busyIDs := firstN(busy)
	for k := 1; k <= tasks; k++ {
		if _, _, err := st.SaveProbeTask(ctx, &heronv1.ProbeTask{
			Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: fmt.Sprintf("192.0.2.%d", k), IntervalS: 60, TimeoutMs: 1000,
		}, store.NodeSelector{NodeIDs: busyIDs}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)&_pragma=cache_size(-262144)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	d := genRetention()
	begin := time.Now()
	var total int64
	for _, lv := range []struct {
		table string
		step  int64
		span  time.Duration
	}{
		{"probe_1m", 60, d.M1},
		{"probe_5m", 300, d.M5},
		{"probe_1h", 3600, d.H1},
	} {
		span := int64(lv.span.Seconds())
		from := now - span + lv.step
		stmt, err := db.Prepare(fmt.Sprintf(
			"WITH RECURSIVE m(ts) AS (SELECT ? UNION ALL SELECT ts+? FROM m WHERE ts+? < ?) "+
				"INSERT INTO %s SELECT ?, ts, ?, 1, 0, 0, 500, 500, 500 FROM m", lv.table))
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range busyIDs {
			for task := 1; task <= tasks; task++ {
				if _, err := stmt.Exec(from, lv.step, lv.step, now, node, task); err != nil {
					t.Fatal(err)
				}
			}
		}
		stmt.Close()
		n := countRows(t, db, lv.table)
		total += n
		t.Logf("%s: %d rows（累计 %.1fM，%.0fs）", lv.table, n, float64(total)/1e6, time.Since(begin).Seconds())
	}
	// 指标行只填节点 1：① 的读者要有真实读数。reported 只在 1m 表上有意义。
	for _, lv := range []struct {
		table string
		step  int64
		span  time.Duration
	}{
		{"metric_1m", 60, d.M1},
		{"metric_5m", 300, d.M5},
		{"metric_1h", 3600, d.H1},
	} {
		span := int64(lv.span.Seconds())
		from := now - span + lv.step
		tail := "minutes, observed, both) SELECT 1, ts, 500, 1, 500, 1, NULL, NULL FROM m"
		if lv.table == "metric_1m" {
			tail = "reported, observed) SELECT 1, ts, 500, 1, 500, 1, NULL FROM m"
		}
		q := fmt.Sprintf("WITH RECURSIVE m(ts) AS (SELECT %d UNION ALL SELECT ts+%d FROM m WHERE ts+%d < %d) "+
			"INSERT INTO %s (node_id, ts, cpu_sum, cpu_n, cpu_max, "+tail,
			from, lv.step, lv.step, now, lv.table)
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, level := range []string{"probe_5m", "probe_1h", "5m", "1h"} {
		if _, err := db.Exec("UPDATE rollup_state SET upto_ts = ? WHERE level = ?", now, level); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("生成完成 %.0fs，库 %.1f GiB", time.Since(begin).Seconds(), float64(fi.Size())/(1<<30))
}

func genRetention() store.Retention {
	if smoke {
		return store.Retention{M1: 2 * time.Hour, M5: 6 * time.Hour, H1: 24 * time.Hour, AlertEvents: store.DefaultRetention.AlertEvents}
	}
	return store.DefaultRetention
}

func countRows(t *testing.T, db *sql.DB, table string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---- 入口与读路径计数 ----

type countingServer struct {
	inner http.Handler
	mu    sync.Mutex
	hits  map[string]int
}

func (c *countingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	if c.hits == nil {
		c.hits = map[string]int{}
	}
	c.hits[r.URL.Path]++
	c.mu.Unlock()
	c.inner.ServeHTTP(w, r)
}

func (c *countingServer) count(procedure string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits[procedure]
}

// sourcedClient 把来源地址写进 X-Forwarded-For：限流按来源分桶，不同来源互不挤占。
type sourcedClient struct {
	inner  heronv1connect.PublicServiceClient
	source string
}

// hc 由调用方给，Transport 归调用方所有：同一进程里的几个来源共用调用方的一个连接池。
func newSourcedClient(hc *http.Client, url, source string) *sourcedClient {
	return &sourcedClient{inner: heronv1connect.NewPublicServiceClient(hc, url), source: source}
}

func (s *sourcedClient) do[T any](ctx context.Context, req *connect.Request[T]) *connect.Request[T] {
	req.Header().Set("X-Forwarded-For", s.source)
	return req
}

func (s *sourcedClient) GetSnapshot(ctx context.Context, req *connect.Request[heronv1.PublicServiceGetSnapshotRequest]) (*connect.Response[heronv1.PublicSnapshot], error) {
	return s.inner.GetSnapshot(ctx, s.do(ctx, req))
}

func (s *sourcedClient) GetSite(ctx context.Context, req *connect.Request[heronv1.GetSiteRequest]) (*connect.Response[heronv1.PublicSite], error) {
	return s.inner.GetSite(ctx, s.do(ctx, req))
}

func (s *sourcedClient) QueryMetrics(ctx context.Context, req *connect.Request[heronv1.QueryMetricsRequest]) (*connect.Response[heronv1.QueryMetricsResponse], error) {
	return s.inner.QueryMetrics(ctx, s.do(ctx, req))
}

func (s *sourcedClient) QueryProbes(ctx context.Context, req *connect.Request[heronv1.QueryProbesRequest]) (*connect.Response[heronv1.QueryProbesResponse], error) {
	return s.inner.QueryProbes(ctx, s.do(ctx, req))
}

func (s *sourcedClient) QueryProbeComparison(ctx context.Context, req *connect.Request[heronv1.QueryProbeComparisonRequest]) (*connect.Response[heronv1.QueryProbeComparisonResponse], error) {
	return s.inner.QueryProbeComparison(ctx, s.do(ctx, req))
}

func (s *sourcedClient) ListProbeComparisonNodes(ctx context.Context, req *connect.Request[heronv1.ListProbeComparisonNodesRequest]) (*connect.Response[heronv1.ListProbeComparisonNodesResponse], error) {
	return s.inner.ListProbeComparisonNodes(ctx, s.do(ctx, req))
}

// ---- 负载组与记账 ----

type loadOutcome struct {
	series     int
	samples    int
	wantSeries int
}

func metricsOutcome(r *connect.Response[heronv1.QueryMetricsResponse]) *loadOutcome {
	if r == nil {
		return &loadOutcome{}
	}
	return &loadOutcome{series: len(r.Msg.GetSeries()), samples: len(r.Msg.GetTs())}
}

func seriesOutcome(r *connect.Response[heronv1.QueryProbesResponse], wantTasks int) *loadOutcome {
	if r == nil {
		return &loadOutcome{}
	}
	o := &loadOutcome{wantSeries: wantTasks, series: len(r.Msg.GetSeries())}
	for _, s := range r.Msg.GetSeries() {
		o.samples += len(s.GetSamples())
	}
	return o
}

func comparisonOutcome(r *connect.Response[heronv1.QueryProbeComparisonResponse], wantNodes int) *loadOutcome {
	if r == nil {
		return &loadOutcome{}
	}
	o := &loadOutcome{wantSeries: wantNodes, series: len(r.Msg.GetSeries())}
	for _, s := range r.Msg.GetSeries() {
		o.samples += len(s.GetSamples())
	}
	return o
}

type loadGroup struct {
	name                     string
	planned, issued, success int
	limited, gateRejected    int
	other, quotaRejected     int
	latencies                []time.Duration
	// 核对只统计拿到成功响应、且响应体可核对的请求；被限流/被拒/未完成的请求不算失败。
	seriesChecked, seriesFailed int
	incomplete                  int // 计划时刻已到、goroutine 没能在窗口内发出的数量
	err, lastQuota              string
}

// verifiedLine 区分"没核对"与"核对失败"：前者只是没有可核对的响应，后者是正确性问题。
func (g *loadGroup) verifiedLine() string {
	if g.seriesChecked == 0 {
		return "无成功响应可核对"
	}
	if g.seriesFailed == 0 {
		return fmt.Sprintf("核对 %d 个全过", g.seriesChecked)
	}
	return fmt.Sprintf("核对 %d 个、失败 %d 个", g.seriesChecked, g.seriesFailed)
}

// runLoadGroup 按限流补充速度间隔发起（每 100ms 一个）：计量组只测服务端耗时，
// 不让来源桶被自己打空——桶容量 60、每秒补 10，10/s 的节奏只动用补充量。
func runLoadGroup(name string, calls int, one func() (*loadOutcome, error)) *loadGroup {
	g := &loadGroup{name: name, planned: calls}
	for i := range calls {
		if i > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		begin := time.Now()
		out, err := one()
		g.latencies = append(g.latencies, time.Since(begin))
		g.issued++
		switch {
		case err == nil:
			g.success++
			if bad := out != nil && (out.series == 0 || out.samples == 0 || (out.wantSeries != 0 && out.series != out.wantSeries)); out != nil {
				g.seriesChecked++
				if bad {
					g.seriesFailed++
				}
			}
		case connect.CodeOf(err) == connect.CodeResourceExhausted:
			g.limited++
		case connect.CodeOf(err) == connect.CodeFailedPrecondition:
			g.quotaRejected++
			g.lastQuota = err.Error()
		default:
			g.other++
			g.err = err.Error()
		}
	}
	return g
}

func (g *loadGroup) p50() time.Duration { return quantile(g.latencies, 0.50) }
func (g *loadGroup) p99() time.Duration { return quantile(g.latencies, 0.99) }

func quantile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[min(len(s)-1, int(q*float64(len(s))))]
}

const day = int64(86400)

// ---- 饱和组 ----

// loadReport 收集各组的测量行并汇总打印。
type loadReport struct {
	notes []string
}

func newLoadReport(dbPath string) *loadReport {
	return &loadReport{notes: []string{"库: " + dbPath}}
}

func (r *loadReport) group(g *loadGroup) {
	r.notes = append(r.notes, fmt.Sprintf("%s：计划 %d、发起 %d、成功 %d、限流 %d、并发闸 %d、其他 %d、超额拒 %d、未完成 %d；p50=%v p99=%v；%s",
		g.name, g.planned, g.issued, g.success, g.limited, g.gateRejected, g.other, g.quotaRejected, g.incomplete, g.p50(), g.p99(), g.verifiedLine()))
}

func (r *loadReport) note(text string) { r.notes = append(r.notes, text) }

// write 把全部测量行按序打出来，一页读完。
func (r *loadReport) write(t *testing.T) {
	t.Helper()
	for _, line := range r.notes {
		t.Log(line)
	}
}

// startFDSampler 周期性用 lsof 数进程打开的数据库文件句柄（db / -wal / -shm 后缀归类）。
// -wal 与 -shm 的每个连接各持一个，是读池 OpenConnections 的外部观测；InUse 与
// WaitCount 需要暴露 db.Stats 才可得，未改产品代码，以句柄峰值代之。启动先记空载基线。
type fdSampler struct {
	stop    chan struct{}
	done    chan struct{}
	path    string
	baseDB  int
	peakDB  int
	peakWal int
	peakShm int
	peakCon int
	failed  bool
	mu      sync.Mutex
}

func startFDSampler(dbPath string) *fdSampler {
	s := &fdSampler{stop: make(chan struct{}), done: make(chan struct{}), path: dbPath}
	if resolved, err := filepath.EvalSymlinks(dbPath); err == nil {
		s.path = resolved
	}
	c := s.countOnce()
	s.mu.Lock()
	s.baseDB = c.db
	s.peakDB, s.peakWal, s.peakShm, s.peakCon = c.db, c.wal, c.shm, max(c.wal, c.shm)
	s.mu.Unlock()
	go func() {
		defer close(s.done)
		for {
			select {
			case <-s.stop:
				return
			case <-time.After(500 * time.Millisecond):
				s.sample()
			}
		}
	}()
	return s
}

func (s *fdSampler) countOnce() (c struct{ db, wal, shm int }) {
	out, err := exec.Command("lsof", "-p", strconv.Itoa(os.Getpid()), "-F", "n").Output()
	if err != nil {
		s.failed = true
		return
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		name, ok := strings.CutPrefix(line, "n")
		if !ok || !strings.HasPrefix(name, s.path) {
			continue
		}
		switch strings.TrimPrefix(name, s.path) {
		case "":
			c.db++
		case "-wal":
			c.wal++
		case "-shm":
			c.shm++
		}
	}
	return
}

func (s *fdSampler) sample() {
	c := s.countOnce()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peakDB = max(s.peakDB, c.db)
	s.peakWal = max(s.peakWal, c.wal)
	s.peakShm = max(s.peakShm, c.shm)
	s.peakCon = max(s.peakCon, max(c.wal, c.shm))
}

func (s *fdSampler) stopAndReport() string {
	close(s.stop)
	<-s.done
	s.sample()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		return "句柄采样不可用（lsof 失败）"
	}
	return fmt.Sprintf("句柄峰值 db=%d（空载 %d）、-wal=%d、-shm=%d → 打开连接峰值估计 %d；InUse/WaitCount 需暴露 db.Stats，本轮未改产品代码",
		s.peakDB, s.baseDB, s.peakWal, s.peakShm, s.peakCon)
}
