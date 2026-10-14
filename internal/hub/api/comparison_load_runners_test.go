package api

// 受控环境的负载台运行器：父测试进程只跑 hub（store + HTTP 入口），重锤与读者分别
// 是独立的子进程（go test 二进制用 -test.run 拉起帮助测试），参数全部经环境变量传递。
// 只有 HERON_LOAD_CHILD 非空时这些帮助测试才运行；父进程合并子进程写出的逐样本日志。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	heronv1connect "github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/testdeps"
)

// ---- 子进程参数（环境变量）----

const (
	envChild   = "HERON_LOAD_CHILD"   // hammer / reader
	envAddr    = "HERON_LOAD_ADDR"    // hub 的 HTTP 地址
	envStart   = "HERON_LOAD_T0"      // 全组共享起点（UnixNano）
	envSource  = "HERON_LOAD_SRC"     // 子进程的来源 IP
	envShape   = "HERON_LOAD_SHAPE"   // cmp365d / probes365d / metrics365d
	envK       = "HERON_LOAD_K"       // 0 = 开环突发+10/s 计划；>0 = 闭环 k 个在飞
	envSeconds = "HERON_LOAD_SECONDS" // 计划时长
	envSamples = "HERON_LOAD_SAMPLES" // 子进程写出的逐样本文件
	envTasks   = "HERON_LOAD_TASKS"
	envNodes   = "HERON_LOAD_CHUNKNODES"
)

// loadSample 是一条重锤请求区间或读者样本；kind 区分两者。
type loadSample struct {
	Kind     string `json:"kind"` // hammer / reader-metrics / reader-probes
	StartNS  int64  `json:"start_ns"`
	EndNS    int64  `json:"end_ns"`
	OK       bool   `json:"ok"`
	Code     string `json:"code"` // ok / limited / gate / quota / other / ctx
	Series   int    `json:"series,omitempty"`
	Points   int    `json:"points,omitempty"`
	Verified bool   `json:"verified,omitempty"` // 响应体核对（序列与样本数）
}

// ---- 子进程入口 ----

// TestLoadHammerChild 只在父测试用环境变量点名时跑：按计划发重请求，逐条写出
// [发起时刻, 完成时刻, 结果]；在飞期间 = 从发起到完成的闭区间。
func TestLoadHammerChild(t *testing.T) {
	t.Parallel()
	if os.Getenv(envChild) != "hammer" {
		t.Skip("child runner")
	}
	runHammerChild(t)
}

// TestLoadReaderChild 同上：两个读者交替每 200ms 一个 ① 形状请求。
func TestLoadReaderChild(t *testing.T) {
	t.Parallel()
	if os.Getenv(envChild) != "reader" {
		t.Skip("child runner")
	}
	runReaderChild(t)
}

func childEnv(t *testing.T) (addr, src, samplesPath string, start time.Time, seconds int) {
	t.Helper()
	addr = os.Getenv(envAddr)
	src = os.Getenv(envSource)
	samplesPath = os.Getenv(envSamples)
	ns, err := strconv.ParseInt(os.Getenv(envStart), 10, 64)
	if err != nil || addr == "" || src == "" || samplesPath == "" {
		t.Fatalf("子进程参数不全: addr=%q src=%q samples=%q t0=%q err=%v", addr, src, samplesPath, os.Getenv(envStart), err)
	}
	seconds, _ = strconv.Atoi(os.Getenv(envSeconds))
	if seconds <= 0 {
		seconds = 30
	}
	return addr, src, samplesPath, time.Unix(0, ns), seconds
}

// loadBenchNow 是负载台全部查询的"现在"：生成库时的水位与窗口都按它写，父进程与子进程
// （同一个测试二进制）读同一个值，窗口对得上。
var loadBenchNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func loadNow() time.Time { return loadBenchNow }

// hammerFire 发一个重请求，返回样本。shape 三种：cmp365d（N_max 节点分块）、
// probes365d（单节点 64 任务）、metrics365d（单节点指标）。
func hammerFire(t *testing.T, c heronv1connect.PublicServiceClient, shape string, tasks, chunkNodes int) loadSample {
	now := loadNow().Unix()
	begin := time.Now()
	s := loadSample{Kind: "hammer", StartNS: begin.UnixNano()}
	var err error
	var out *loadOutcome
	switch shape {
	case "cmp365d":
		var r *connect.Response[heronv1.QueryProbeComparisonResponse]
		r, err = c.QueryProbeComparison(t.Context(), connect.NewRequest(&heronv1.QueryProbeComparisonRequest{
			TaskId: 1, NodeIds: firstN(chunkNodes), From: now - 365*day, To: now, MaxPoints: 720}))
		out = comparisonOutcome(r, chunkNodes)
	case "probes365d":
		var r *connect.Response[heronv1.QueryProbesResponse]
		r, err = c.QueryProbes(t.Context(), connect.NewRequest(&heronv1.QueryProbesRequest{
			NodeId: 1, From: now - 365*day, To: now, MaxPoints: 720}))
		out = seriesOutcome(r, tasks)
	case "metrics365d":
		var r *connect.Response[heronv1.QueryMetricsResponse]
		r, err = c.QueryMetrics(t.Context(), connect.NewRequest(&heronv1.QueryMetricsRequest{
			NodeId: 1, From: now - 365*day, To: now, MaxPoints: 720}))
		out = metricsOutcome(r)
	}
	s.EndNS = time.Now().UnixNano()
	if err != nil {
		s.Code = "other"
		if connect.CodeOf(err) == connect.CodeResourceExhausted {
			if strings.Contains(err.Error(), "concurrent history queries") {
				s.Code = "gate"
			} else {
				s.Code = "limited"
			}
		} else if connect.CodeOf(err) == connect.CodeFailedPrecondition {
			s.Code = "quota"
		}
		return s
	}
	s.OK = true
	s.Code = "ok"
	s.Series, s.Points = out.series, out.samples
	s.Verified = out.series > 0 && out.samples > 0 && (out.wantSeries == 0 || out.series == out.wantSeries)
	return s
}

// runHammerChild：k=0 走开环（每个开半周期开头突发 60，之后每 100ms 一个）；
// k>0 走闭环（k 个 worker 各自完成一个立刻发下一个，被拒的请求退避 100ms 重试）。
func runHammerChild(t *testing.T) {
	addr, src, samplesPath, start, seconds := childEnv(t)
	tasks, _ := strconv.Atoi(os.Getenv(envTasks))
	chunkNodes, _ := strconv.Atoi(os.Getenv(envNodes))
	shape := os.Getenv(envShape)
	k, _ := strconv.Atoi(os.Getenv(envK))
	c := newSourcedClient(&http.Client{Transport: testdeps.OwnedTransport(t)}, addr, src)
	out, err := os.Create(samplesPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	w := bufio.NewWriter(out)
	defer w.Flush()
	var mu sync.Mutex
	writeSample := func(s loadSample) {
		mu.Lock()
		defer mu.Unlock()
		line, _ := json.Marshal(s)
		w.Write(line)
		w.WriteByte('\n')
	}
	if k > 0 {
		end := start.Add(time.Duration(seconds) * time.Second)
		var wg sync.WaitGroup
		for range k {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(end) {
					s := hammerFire(t, c, shape, tasks, chunkNodes)
					writeSample(s)
					if s.Code == "limited" {
						time.Sleep(100 * time.Millisecond)
					}
				}
			}()
		}
		wg.Wait()
		return
	}
	// 开环：4 个"开一半、关一半"的循环，开头突发 60，之后每 100ms 一个。
	half := time.Duration(seconds) * time.Second / 8
	for cycle := range 4 {
		openStart := start.Add(time.Duration(cycle) * 2 * half)
		for i := range 60 {
			at := openStart.Add(time.Duration(i) * 5 * time.Millisecond)
			go func() {
				time.Sleep(time.Until(at))
				if time.Now().Before(openStart.Add(half)) {
					writeSample(hammerFire(t, c, shape, tasks, chunkNodes))
				}
			}()
		}
		for i := 60; ; i++ {
			at := openStart.Add(time.Duration(i) * 100 * time.Millisecond)
			if !at.Before(openStart.Add(half)) {
				break
			}
			go func() {
				time.Sleep(time.Until(at))
				writeSample(hammerFire(t, c, shape, tasks, chunkNodes))
			}()
		}
	}
	time.Sleep(time.Until(start.Add(time.Duration(seconds) * time.Second)))
}

// runReaderChild：metrics 与 probes 读者各每 200ms 一个，交替错开 100ms。
func runReaderChild(t *testing.T) {
	addr, src, samplesPath, start, seconds := childEnv(t)
	tasks, _ := strconv.Atoi(os.Getenv(envTasks))
	hc := &http.Client{Transport: testdeps.OwnedTransport(t)}
	m := newSourcedClient(hc, addr, src)
	p := newSourcedClient(hc, addr, src+"-p")
	out, err := os.Create(samplesPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	w := bufio.NewWriter(out)
	defer w.Flush()
	var mu sync.Mutex
	writeSample := func(s loadSample) {
		mu.Lock()
		defer mu.Unlock()
		line, _ := json.Marshal(s)
		w.Write(line)
		w.WriteByte('\n')
	}
	now := loadNow().Unix()
	var wg sync.WaitGroup
	for i := 0; ; i++ {
		at := start.Add(time.Duration(i) * 200 * time.Millisecond)
		if !at.Before(start.Add(time.Duration(seconds) * time.Second)) {
			break
		}
		wg.Add(2)
		go func(at time.Time) {
			defer wg.Done()
			time.Sleep(time.Until(at))
			begin := time.Now()
			r, err := m.QueryMetrics(t.Context(), connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: 1, From: now - 6*3600, To: now, MaxPoints: 720}))
			s := loadSample{Kind: "reader-metrics", StartNS: begin.UnixNano(), EndNS: time.Now().UnixNano()}
			if err != nil {
				s.Code = "other"
			} else {
				s.OK = true
				s.Code = "ok"
				o := metricsOutcome(r)
				s.Series, s.Points = o.series, o.samples
				s.Verified = o.series > 0
			}
			writeSample(s)
		}(at)
		go func(at time.Time) {
			defer wg.Done()
			time.Sleep(time.Until(at.Add(100 * time.Millisecond)))
			begin := time.Now()
			r, err := p.QueryProbes(t.Context(), connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: 1, From: now - 6*3600, To: now, MaxPoints: 720}))
			s := loadSample{Kind: "reader-probes", StartNS: begin.UnixNano(), EndNS: time.Now().UnixNano()}
			if err != nil {
				s.Code = "other"
			} else {
				s.OK = true
				s.Code = "ok"
				o := seriesOutcome(r, tasks)
				s.Series, s.Points = o.series, o.samples
				s.Verified = o.series == tasks && o.samples > 0
			}
			writeSample(s)
		}(at)
	}
	wg.Wait()
}

// ---- 父进程：拉起与合并 ----

// childRun 拉起一个子进程（go test 二进制 -test.run 单个帮助测试），等它结束并
// 读回逐样本文件。子进程的 stdout/stderr 只在失败时打印。
func childRun(t *testing.T, role, addr string, start time.Time, seconds int, env map[string]string, samplesPath string) []loadSample {
	t.Helper()
	args := []string{"-test.run", "^TestLoad" + map[string]string{"hammer": "Hammer", "reader": "Reader"}[role] + "Child$", "-test.count=1", "-test.timeout=30m", "-test.v=false"}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(),
		envChild+"="+role,
		envAddr+"="+addr,
		envSource+"="+env[envSource],
		envStart+"="+strconv.FormatInt(start.UnixNano(), 10),
		envSeconds+"="+strconv.Itoa(seconds),
		envSamples+"="+samplesPath,
	)
	for k, v := range env {
		if k != envSource {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	// 测试二进制把测试输出（含 t.Fatalf 的原因）写到 stdout，只收 stderr 会把失败原因丢掉。
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("子进程 %s 失败: %v\n%s", role, err, output)
	}
	f, err := os.Open(samplesPath)
	if err != nil {
		t.Fatalf("子进程 %s 没有写出样本文件 %s: %v", role, samplesPath, err)
	}
	defer f.Close()
	var out []loadSample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var s loadSample
		if err := json.Unmarshal(sc.Bytes(), &s); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// classifyReaders 按样本发出时刻是否落在任何一个重请求区间内分类（压着/没压着）。
func classifyReaders(readers, hammers []loadSample) (idle, loaded map[string][]time.Duration) {
	idle = map[string][]time.Duration{"reader-metrics": nil, "reader-probes": nil}
	loaded = map[string][]time.Duration{"reader-metrics": nil, "reader-probes": nil}
	var intervals [][2]int64
	for _, h := range hammers {
		if h.Code == "limited" || h.Code == "gate" {
			continue // 被拒的重请求不占库、不算压着
		}
		intervals = append(intervals, [2]int64{h.StartNS, h.EndNS})
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i][0] < intervals[j][0] })
	merged := intervals[:0]
	for _, iv := range intervals {
		if n := len(merged); n > 0 && iv[0] <= merged[n-1][1] {
			if iv[1] > merged[n-1][1] {
				merged[n-1][1] = iv[1]
			}
		} else {
			merged = append(merged, iv)
		}
	}
	for _, r := range readers {
		// 二分：找到最后一个 start <= r.StartNS 的区间，看 r.StartNS 是否 < end。
		i := sort.Search(len(merged), func(i int) bool { return merged[i][0] > r.StartNS }) - 1
		in := i >= 0 && r.StartNS < merged[i][1]
		lat := time.Duration(r.EndNS - r.StartNS)
		if r.OK {
			if in {
				loaded[r.Kind] = append(loaded[r.Kind], lat)
			} else {
				idle[r.Kind] = append(idle[r.Kind], lat)
			}
		}
	}
	return idle, loaded
}

// ---- 环境报告与时钟漂移探针 ----

func envReportLine() string {
	cpu := ""
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			cpu = strings.TrimSpace(string(out))
		}
	case "linux":
		if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
					cpu = strings.TrimSpace(v)
					break
				}
			}
		}
	}
	// P/E 核拆分（异构机型才有）。
	perfLevels := ""
	if runtime.GOOS == "darwin" {
		var parts []string
		for i := 0; ; i++ {
			out, err := exec.Command("sysctl", "-n", fmt.Sprintf("hw.perflevel%d.physicalcpu", i)).Output()
			if err != nil {
				break
			}
			name, _ := exec.Command("sysctl", "-n", fmt.Sprintf("hw.perflevel%d.name", i)).Output()
			parts = append(parts, fmt.Sprintf("%s=%s", strings.TrimSpace(string(name)), strings.TrimSpace(string(out))))
		}
		perfLevels = strings.Join(parts, " ")
	}
	h, _ := os.Hostname()
	return fmt.Sprintf("机型 %s %s/%s CPU=%s 核=%d(%s) GOMAXPROCS=%d", h, runtime.GOOS, runtime.GOARCH, cpu, runtime.NumCPU(), perfLevels, runtime.GOMAXPROCS(0))
}

var driftSink uint64

// driftUnit 是一个固定的单线程工作单元（FNV 混合两千万次），用来探时钟漂移/调度
// 压制：组前组后各跑 5 次，取最小值与中位数；后/前中位数比 > 1.15 记"环境不平稳"。
func driftUnit() time.Duration {
	start := time.Now()
	var x uint64 = 1469598103934665603
	for i := uint64(0); i < 20_000_000; i++ {
		x ^= i
		x *= 1099511628211
	}
	driftSink = x
	return time.Since(start)
}

func driftProbe() (min, median time.Duration) {
	var ds []time.Duration
	for range 5 {
		ds = append(ds, driftUnit())
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[0], ds[2]
}

// loadAvgLine 读系统的 1 分钟负载均值。
func loadAvgLine() string {
	if runtime.GOOS == "linux" {
		if b, err := os.ReadFile("/proc/loadavg"); err == nil {
			return strings.Fields(string(b))[0]
		}
	}
	if out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output(); err == nil {
		f := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{ }"))
		if len(f) > 0 {
			return f[0]
		}
	}
	return "?"
}

func ctxOf(t *testing.T) context.Context { return t.Context() }

// curveDur：闭环每档的时长；smoke 缩到 1 秒。
func curveDur() time.Duration {
	if smoke {
		return time.Second
	}
	return 30 * time.Second
}

// reportPairedPhase 合并一组饱和相位：重锤记账 + 读者按区间分类后的闲/压比值。
func reportPairedPhase(t *testing.T, report *loadReport, name string, hammerS, readerS []loadSample, stable, extra string) {
	t.Helper()
	g := &loadGroup{name: name}
	g.planned = len(hammerS) // 开环的计划由子进程的调度结构决定；父进程按发起数对齐。
	for _, s := range hammerS {
		g.issued++
		g.latencies = append(g.latencies, time.Duration(s.EndNS-s.StartNS))
		switch s.Code {
		case "ok":
			g.success++
			g.seriesChecked++
			if !s.Verified {
				g.seriesFailed++
			}
		case "limited":
			g.limited++
		case "gate":
			g.gateRejected++
		case "quota":
			g.quotaRejected++
		default:
			g.other++
		}
	}
	idle, loaded := classifyReaders(readerS, hammerS)
	mr := pairRatio(idle["reader-metrics"], loaded["reader-metrics"])
	pr := pairRatio(idle["reader-probes"], loaded["reader-probes"])
	readerErrs := 0
	for _, r := range readerS {
		if !r.OK {
			readerErrs++
		}
	}
	report.group(g)
	report.note(fmt.Sprintf("%s：周期内读者 指标 闲 %d 个 p99=%v / 压 %d 个 p99=%v（%.2f×）；探测 闲 %d 个 p99=%v / 压 %d 个 p99=%v（%.2f×）；读者错误 %d；%s；判定：%s；%s",
		name,
		len(idle["reader-metrics"]), quantile(idle["reader-metrics"], 0.99), len(loaded["reader-metrics"]), quantile(loaded["reader-metrics"], 0.99), mr,
		len(idle["reader-probes"]), quantile(idle["reader-probes"], 0.99), len(loaded["reader-probes"]), quantile(loaded["reader-probes"], 0.99), pr,
		readerErrs, stable, pairedVerdict(idle, loaded, mr, pr, readerErrs, stable), extra))
}

// minPairedSamples：每个读者的闲、压两类各自至少这么多个样本，p99 才不是由一两个尖刺决定。
const minPairedSamples = 200

// pairedVerdict 按饱和组门槛判定一组：两个读者全部成功，且压类 p99 都不超过闲类 p99 的 2 倍。
// 任一类样本不足 minPairedSamples，或漂移探针标了"环境不平稳"时不判定：这两种情况下比值说明的
// 是采样或机器，不是 hub。读者出错按门槛直接不通过。
func pairedVerdict(idle, loaded map[string][]time.Duration, mr, pr float64, readerErrs int, stable string) string {
	for _, kind := range []string{"reader-metrics", "reader-probes"} {
		if len(idle[kind]) < minPairedSamples || len(loaded[kind]) < minPairedSamples {
			return fmt.Sprintf("不判定（%s 闲 %d、压 %d 个，少于 %d）", kind, len(idle[kind]), len(loaded[kind]), minPairedSamples)
		}
	}
	if stable != "平稳" {
		return "不判定（" + stable + "）"
	}
	if readerErrs > 0 {
		return "不通过（读者出错）"
	}
	if mr <= 2 && pr <= 2 {
		return "通过"
	}
	return "不通过"
}

func pairRatio(idle, load []time.Duration) float64 {
	if len(idle) == 0 || len(load) == 0 {
		return -1
	}
	return float64(quantile(load, 0.99)) / float64(quantile(idle, 0.99))
}

// reportCurveLevel 汇总闭环曲线一档：重请求吞吐与延迟、读者的 p50/p99。
func reportCurveLevel(label string, k int, hammerS, readerS []loadSample, dur time.Duration) string {
	var heavyLat []time.Duration
	var done, limited, gateRejected, other, checked, failed int
	for _, s := range hammerS {
		if s.Code == "ok" {
			done++
			heavyLat = append(heavyLat, time.Duration(s.EndNS-s.StartNS))
			checked++
			if !s.Verified {
				failed++
			}
		} else if s.Code == "limited" {
			limited++
		} else if s.Code == "gate" {
			gateRejected++
		} else {
			other++
		}
	}
	var rm, rp []time.Duration
	for _, r := range readerS {
		if !r.OK {
			continue
		}
		if r.Kind == "reader-metrics" {
			rm = append(rm, time.Duration(r.EndNS-r.StartNS))
		} else {
			rp = append(rp, time.Duration(r.EndNS-r.StartNS))
		}
	}
	return fmt.Sprintf("⑦ 闭环 %s k=%d（%v）：重请求完成 %d（%.1f/s，p50=%v，限流 %d、并发闸 %d、其他 %d、核对 %d 个%s）；读者指标 p50=%v p99=%v、探测 p50=%v p99=%v",
		label, k, dur.Round(time.Second), done, float64(done)/dur.Seconds(), quantile(heavyLat, 0.50), limited, gateRejected, other, checked,
		map[bool]string{true: " 全过", false: " 有失败"}[failed == 0],
		quantile(rm, 0.50), quantile(rm, 0.99), quantile(rp, 0.50), quantile(rp, 0.99))
}
