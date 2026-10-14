package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/agent/collect"
	"github.com/xjetry/heron/internal/agent/collect/collecttest"
	"github.com/xjetry/heron/internal/agent/prober"
	"github.com/xjetry/heron/internal/agentconfig"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hubclient"
	"github.com/xjetry/heron/internal/testwait"
)

func TestConfigRoundTripAndPermissions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "config.json")
	if err := agentconfig.Save(p, agentconfig.Config{Hub: "https://h", Token: "t", Name: "n"}); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 600", st.Mode().Perm())
	}
	c, err := LoadConfig(p)
	if err != nil || c.Hub != "https://h" || c.Token != "t" || c.Name != "n" {
		t.Fatalf("%+v %v", c, err)
	}
}

// 落盘后的配置对其他用户不可读，即使残留的临时文件或旧配置本身权限更宽。
func TestSaveConfigEnforcesModeOverStaleFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p+".tmp", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := agentconfig.Save(p, agentconfig.Config{Hub: "https://h", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 600: a stale world-readable temp file must not carry its mode onto the config", st.Mode().Perm())
	}
	c, err := LoadConfig(p)
	if err != nil || c.Token != "t" {
		t.Fatalf("config not replaced: %+v %v", c, err)
	}
}

func TestFactsHashIsStableAndSensitive(t *testing.T) {
	a := &heronv1.Facts{Hostname: "h", CpuCores: 2}
	b := &heronv1.Facts{Hostname: "h", CpuCores: 2}
	if FactsHash(a) != FactsHash(b) {
		t.Fatal("equal facts must hash equal")
	}
	b.CpuCores = 3
	if FactsHash(a) == FactsHash(b) {
		t.Fatal("different facts must hash differently")
	}
}

func TestBackoffGrowsAndCapsAtThreeIntervals(t *testing.T) {
	interval := 10 * time.Second
	one := func() float64 { return 1 }
	zero := func() float64 { return 0 }
	if d := Backoff(1, interval, one); d != interval {
		t.Fatalf("attempt 1 max = %v, want %v", d, interval)
	}
	if d := Backoff(2, interval, one); d != 2*interval {
		t.Fatalf("attempt 2 max = %v, want %v", d, 2*interval)
	}
	for attempt := 3; attempt < 40; attempt++ {
		if d := Backoff(attempt, interval, one); d != 3*interval {
			t.Fatalf("attempt %d max = %v, want cap %v", attempt, d, 3*interval)
		}
		if d := Backoff(attempt, interval, zero); d < 3*interval/2 {
			t.Fatalf("attempt %d min = %v, jitter must keep at least half the base", attempt, d)
		}
	}
}

// fakeHub 记录收到的上报并按脚本应答。
type fakeHub struct {
	reconcile  bool
	factsHash  uint64
	mu         sync.Mutex
	reports    []*heronv1.ReportRequest
	wantNext   bool
	fail       bool
	interval   uint32
	tasks      *heronv1.ProbeTasks
	probeError connect.Code
	detection  *heronv1.NetworkDetection
}

func (f *fakeHub) Register(context.Context, *connect.Request[heronv1.RegisterRequest]) (*connect.Response[heronv1.RegisterResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (f *fakeHub) Report(_ context.Context, req *connect.Request[heronv1.ReportRequest]) (*connect.Response[heronv1.ReportResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Header().Get("Authorization") != "Bearer tok" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("no"))
	}
	if f.fail {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}
	f.reports = append(f.reports, req.Msg)
	if f.probeError != 0 {
		return nil, connect.NewError(f.probeError, errors.New("rejected report"))
	}
	want := f.wantNext
	if f.reconcile {
		if req.Msg.Facts != nil {
			f.factsHash = req.Msg.FactsHash
		}
		want = req.Msg.FactsHash != f.factsHash
	}
	f.wantNext = false
	return connect.NewResponse(&heronv1.ReportResponse{ReportIntervalMs: f.interval, WantFacts: want, Tasks: f.tasks, Detection: f.detection}), nil
}

// Runner 的上报路径不调 GetRelease；补上方法只是满足 AgentServiceHandler 接口，与 Register 同样拒绝。
func (f *fakeHub) GetRelease(context.Context, *connect.Request[heronv1.GetReleaseRequest]) (*connect.Response[heronv1.GetReleaseResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (f *fakeHub) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.reports) }

// Report 只追加记录，不修改已收到的请求；取消 runner 不保证服务端已处理完
// 在途请求，因此读者必须在 mu 下复制切片，不能直接读取仍可能被追加的 reports。
func (f *fakeHub) received() []*heronv1.ReportRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*heronv1.ReportRequest(nil), f.reports...)
}

// hostProcFS 构造带识别注入的 ProcFS：挂载根是真根 cgroup2、被消费的 /proc 文件都在
// procfs 设备上（mountinfo 缺省补一条），与 collect 包内同名夹具同一套约定。读者根与宿主根是同一棵树。
func hostProcFS(fsys fs.FS, diskUsage func() (uint64, uint64, error)) *collect.ProcFS {
	if m, ok := fsys.(fstest.MapFS); ok {
		collecttest.EthernetDefaults(m)
		if _, exists := m["proc/self/mountinfo"]; !exists {
			m["proc/self/mountinfo"] = &fstest.MapFile{Data: []byte("42 41 0:22 / /proc rw - proc proc rw\n")}
		}
	}
	r := collecttest.New(fsys)
	r.UsageFunc = diskUsage
	r.StatIDFunc = func(string) (string, uint64, error) { return "0:22", 1, nil }
	r.FSTypeFunc = func(string) (uint64, error) { return 0x63677270, nil }
	return &collect.ProcFS{Root: r, Reader: r}
}

func newRunner(t *testing.T, hub *fakeHub) (*Runner, chan time.Duration) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(heronv1connect.NewAgentServiceHandler(hub))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	sleeps := make(chan time.Duration, 100)
	r := &Runner{
		Collector: &collect.Collector{Host: hostProcFS(fstest.MapFS{"proc/loadavg": {Data: []byte("0 0 0 1/2 3\n")}}, func() (uint64, uint64, error) { return 1, 1, nil }), Clock: clock.NewFake(time.Unix(0, 0)), Version: "t"},
		Client:    hubclient.New(srv.URL, 5*time.Second, agentwire.MaxResponseBytes),
		Token:     "tok",
		Clock:     clock.NewFake(time.Unix(0, 0)),
		Sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case sleeps <- d:
			default:
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				return nil
			}
		},
		Rand:     func() float64 { return 1 },
		Log:      slog.Default(),
		Interval: 10 * time.Second,
	}
	r.Results = prober.NewQueue(prober.QueueCap)
	r.Prober = prober.NewScheduler(quietEngine{}, r.Results, r.Clock, r.Log)
	t.Cleanup(r.Prober.Stop)
	return r, sleeps
}

// runFor 以 hub 侧计数为就绪信号，只适合断言 hub 已收到的内容。
// 断言 runner 侧事件（sleep）的测试必须直接在该事件上等待：hub 计数并不
// 保证 runner 已处理响应，取消可能让 Runner.Run 直接返回而不再产生该事件。
func runFor(t *testing.T, r *Runner, hub *fakeHub, reports int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	testwait.Until(t, time.Millisecond, func() bool { return hub.count() >= reports }, "got %s reports, want at least %d", testwait.When(func() string { return fmt.Sprint(hub.count()) }), reports)
	cancel()
	<-done
	if hub.count() < reports {
		t.Fatalf("got %d reports, want at least %d", hub.count(), reports)
	}
}

func TestFirstReportCarriesFactsThenOnlyOnRequest(t *testing.T) {
	hub := &fakeHub{interval: 10000}
	r, _ := newRunner(t, hub)
	hub.wantNext = false
	runFor(t, r, hub, 2)
	reports := hub.received()
	if reports[0].Facts == nil || reports[0].FactsHash == 0 {
		t.Fatal("first report must carry facts and their hash")
	}
	if reports[1].Facts != nil || reports[1].FactsHash != reports[0].FactsHash {
		t.Fatal("second report must carry only the hash")
	}
}

func TestFactsChangeIsReportedWithoutRestart(t *testing.T) {
	hub := &fakeHub{interval: 5000, reconcile: true}
	r, _ := newRunner(t, hub)
	fs := r.Collector.Host.(*collect.ProcFS).Root.(*collecttest.Root).FS.(fstest.MapFS)
	fs["proc/sys/kernel/hostname"] = &fstest.MapFile{Data: []byte("old\n")}
	round := 0
	r.Sleep = func(context.Context, time.Duration) error {
		round++
		if round == 1 {
			fs["proc/sys/kernel/hostname"] = &fstest.MapFile{Data: []byte("new\n")}
		}
		if round == 3 {
			return context.Canceled
		}
		return nil
	}
	if err := r.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	reports := hub.received()
	if reports[0].Facts == nil {
		t.Fatal("first report missing facts")
	}
	if reports[1].FactsHash == reports[0].FactsHash {
		t.Fatal("facts hash did not change after hostname changed")
	}
	if f := reports[2].Facts; f == nil || f.Hostname != "new" {
		t.Fatalf("requested facts = %+v, want new hostname", f)
	}
}

func TestWantFactsTriggersResend(t *testing.T) {
	hub := &fakeHub{interval: 5000}
	r, _ := newRunner(t, hub)
	hub.mu.Lock()
	hub.wantNext = true // 首次响应就要求 facts → 第二次上报再次携带
	hub.mu.Unlock()
	runFor(t, r, hub, 2)
	reports := hub.received()
	if reports[1].Facts == nil {
		t.Fatal("want_facts must make the next report carry facts")
	}
}

func TestAdoptsIntervalFromResponse(t *testing.T) {
	hub := &fakeHub{interval: 7000}
	r, sleeps := newRunner(t, hub)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case d := <-sleeps:
		if d != 7*time.Second {
			t.Fatalf("slept %v after first response, want the assigned 7s", d)
		}
	case <-time.After(testwait.Bound):
		t.Fatal("runner never slept after the first response")
	}
}

func TestFailureBacksOffWithinThreeIntervals(t *testing.T) {
	hub := &fakeHub{interval: 5000, fail: true}
	r, sleeps := newRunner(t, hub)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	var seen []time.Duration
	for len(seen) < 6 {
		select {
		case d := <-sleeps:
			seen = append(seen, d)
		case <-time.After(testwait.Bound):
			t.Fatal("timed out waiting for backoff sleep")
		}
	}
	cancel()
	<-done
	if seen[0] != 10*time.Second || seen[1] != 20*time.Second {
		t.Fatalf("first two backoffs = %v, want 10s then 20s", seen[:2])
	}
	for _, d := range seen[2:] {
		if d != 30*time.Second {
			t.Fatalf("backoff %v exceeds cap 3×interval", d)
		}
	}
}

// hub 下发越界的间隔时 agent 取边界而不是照办，同一个越界值只告警一次；回到合法值后照常采用。
func TestClampsAssignedIntervalOutOfBounds(t *testing.T) {
	lo := time.Duration(agentwire.ReportIntervalMs(agentwire.MinTTL)) * time.Millisecond
	hi := time.Duration(agentwire.ReportIntervalMs(agentwire.MaxTTL)) * time.Millisecond
	for _, tc := range []struct {
		name     string
		assigned []uint32
		want     []time.Duration
		warns    int
	}{
		{"zero", []uint32{0, 0, 0}, []time.Duration{lo, lo, lo}, 1},
		{"tiny", []uint32{1, 1, 1}, []time.Duration{lo, lo, lo}, 1},
		{"huge", []uint32{^uint32(0), ^uint32(0)}, []time.Duration{hi, hi}, 1},
		{"change", []uint32{1, 2, 2}, []time.Duration{lo, lo, lo}, 2},
		{"recover", []uint32{1, 7000, 1}, []time.Duration{lo, 7 * time.Second, lo}, 2},
		{"legal", []uint32{agentwire.ReportIntervalMs(agentwire.MinTTL), agentwire.ReportIntervalMs(agentwire.MaxTTL)}, []time.Duration{lo, hi}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := &fakeHub{}
			r, _ := newRunner(t, hub)
			var logs bytes.Buffer
			r.Log = slog.New(slog.NewTextHandler(&logs, nil))
			var slept []time.Duration
			hub.interval = tc.assigned[0]
			r.Sleep = func(_ context.Context, d time.Duration) error {
				slept = append(slept, d)
				if len(slept) == len(tc.assigned) {
					return context.Canceled
				}
				hub.mu.Lock()
				hub.interval = tc.assigned[len(slept)]
				hub.mu.Unlock()
				return nil
			}
			if err := r.Run(context.Background()); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if fmt.Sprint(slept) != fmt.Sprint(tc.want) {
				t.Fatalf("slept %v, want %v", slept, tc.want)
			}
			if n := strings.Count(logs.String(), "outside the protocol bounds"); n != tc.warns {
				t.Fatalf("%d clamp warnings, want %d:\n%s", n, tc.warns, logs.String())
			}
		})
	}
}
