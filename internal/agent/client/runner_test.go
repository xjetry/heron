package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agent/collect"
	"github.com/xjetry/heron/internal/agent/prober"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/probelimit"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

type quietEngine struct{}

func (quietEngine) Probe(context.Context, *heronv1.ProbeTask) prober.Outcome {
	return prober.Outcome{RttUs: 10}
}

func TestRunnerReportsResultsAndReconcilesVersion(t *testing.T) {
	hub := &fakeHub{tasks: &heronv1.ProbeTasks{Version: 8}}
	r, _ := newRunner(t, hub)
	q := prober.NewQueue(prober.QueueCap)
	s := prober.NewScheduler(quietEngine{}, q, r.Clock, r.Log)
	defer s.Stop()
	s.Apply(&heronv1.ProbeTasks{Version: 7})
	r.Prober, r.Results = s, q
	r.Collector.IcmpAvailable = true
	q.Push(prober.Result{TaskID: 42, Outcome: prober.Outcome{RttUs: 321}, At: r.Clock.Mono() - time.Second})
	q.Push(prober.Result{TaskID: 99, At: r.Clock.Mono() - probelimit.MaxResultAge - time.Second})
	round := 0
	r.Sleep = func(context.Context, time.Duration) error {
		round++
		if round == 2 {
			return context.Canceled
		}
		hub.mu.Lock()
		hub.tasks = nil
		hub.mu.Unlock()
		return nil
	}
	if err := r.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	reports := hub.received()
	if len(reports) != 2 {
		t.Fatalf("reports = %d, want 2", len(reports))
	}
	want := &heronv1.ProbeResult{TaskId: 42, AgeMs: 1000, Outcome: &heronv1.ProbeResult_RttUs{RttUs: 321}}
	if len(reports[0].ProbeResults) != 1 || !proto.Equal(reports[0].ProbeResults[0], want) {
		t.Fatalf("first results = %v, want %v", reports[0].ProbeResults, want)
	}
	if len(reports[1].ProbeResults) != 0 {
		t.Fatalf("successful results repeated: %v", reports[1].ProbeResults)
	}
	if !reports[0].GetFacts().GetIcmpAvailable() {
		t.Fatal("ICMP availability missing from reported facts")
	}
}

func TestRunnerFailureRetainsOnlyRetryableResults(t *testing.T) {
	for _, code := range []connect.Code{connect.CodeUnavailable, connect.CodeInvalidArgument} {
		t.Run(code.String(), func(t *testing.T) {
			hub := &fakeHub{probeError: code}
			r, _ := newRunner(t, hub)
			var logs bytes.Buffer
			r.Log = slog.New(slog.NewTextHandler(&logs, nil))
			q := prober.NewQueue(prober.QueueCap)
			r.Results = q
			q.Push(prober.Result{TaskID: 42, Outcome: prober.Outcome{Timeout: true}, At: r.Clock.Mono()})
			round := 0
			r.Sleep = func(context.Context, time.Duration) error {
				round++
				if round == 2 {
					return context.Canceled
				}
				remaining := q.Take(r.Clock.Mono(), probelimit.MaxResultAge, probelimit.MaxResultsPerReport)
				want := 1
				if code == connect.CodeInvalidArgument {
					want = 0
				}
				if len(remaining) != want {
					t.Errorf("queue after %s = %v, want %d results", code, remaining, want)
				}
				q.Requeue(remaining)
				r.Clock.(*clock.Fake).Advance(2 * time.Second)
				hub.mu.Lock()
				hub.probeError = 0
				hub.mu.Unlock()
				return nil
			}
			if err := r.Run(context.Background()); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			reports := hub.received()
			if len(reports) != 2 {
				t.Fatalf("reports = %d, want 2", len(reports))
			}
			if len(reports[0].ProbeResults) != 1 {
				t.Fatalf("first results = %v", reports[0].ProbeResults)
			}
			if code == connect.CodeInvalidArgument {
				if len(reports[1].ProbeResults) != 0 {
					t.Fatalf("invalid results retried: %v", reports[1].ProbeResults)
				}
				if !strings.Contains(logs.String(), "discarding rejected probe results") {
					t.Fatalf("discard warning missing: %s", logs.String())
				}
			} else {
				want := &heronv1.ProbeResult{TaskId: 42, AgeMs: 2000, Outcome: &heronv1.ProbeResult_Timeout{Timeout: &heronv1.Timeout{}}}
				if len(reports[1].ProbeResults) != 1 || !proto.Equal(reports[1].ProbeResults[0], want) {
					t.Fatalf("retry results = %v, want %v", reports[1].ProbeResults, want)
				}
			}
		})
	}
}

func TestRunnerEmptySchedulerReportsNoResultsOrVersion(t *testing.T) {
	hub := &fakeHub{tasks: &heronv1.ProbeTasks{Version: 8}}
	r, _ := newRunner(t, hub)
	r.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	if err := r.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	reports := hub.received()
	if len(reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(reports))
	}
	got := reports[0]
	if len(got.ProbeResults) != 0 || got.GetFacts().GetIcmpAvailable() {
		t.Fatalf("unexpected probe state for empty scheduler: %v", got)
	}
}

// 休眠信号触发时：此前入队的探测结果整体作废（age_ms 按单调钟折算，不含休眠时长），
// 全部速率基线重置，本轮采样成为新基线的首样本；下一周期两钟同步流逝则一切照常。
func TestRunnerDiscardsResultsAndRatesAcrossSuspend(t *testing.T) {
	hub := &fakeHub{interval: 10000}
	r, _ := newRunner(t, hub)
	var logs bytes.Buffer
	r.Log = slog.New(slog.NewTextHandler(&logs, nil))
	clk := clock.NewFake(time.Unix(0, 0))
	fsys := fstest.MapFS{
		"proc/stat":                              {Data: []byte("cpu  100 0 50 800 20 0 10 0 0 0\n")},
		"sys/class/net/eth0/statistics/rx_bytes": {Data: []byte("1000\n")},
		"sys/class/net/eth0/statistics/tx_bytes": {Data: []byte("2000\n")},
		"proc/diskstats":                         {Data: []byte("   8       0 sda 1000 20 4000 500 800 10 2000 300 0 200 400\n")},
		"sys/block/sda":                          {Mode: fs.ModeDir},
	}
	step := 0
	bump := func() {
		step++
		fsys["proc/stat"] = &fstest.MapFile{Data: []byte(fmt.Sprintf("cpu  %d 0 50 %d 20 0 10 0 0 0\n", 100+50*step, 800+50*step))}
		fsys["sys/class/net/eth0/statistics/rx_bytes"] = &fstest.MapFile{Data: []byte(fmt.Sprintf("%d\n", 1000+2000*step))}
		fsys["sys/class/net/eth0/statistics/tx_bytes"] = &fstest.MapFile{Data: []byte(fmt.Sprintf("%d\n", 2000+2000*step))}
		fsys["proc/diskstats"] = &fstest.MapFile{Data: []byte(fmt.Sprintf("   8       0 sda 1010 20 %d 500 805 10 %d 300 0 200 400\n", 4000+1000*step, 2000+1000*step))}
		clk.Advance(10 * time.Second)
	}
	r.Collector = &collect.Collector{Host: hostProcFS(fsys, func() (uint64, uint64, error) { return 0, 0, nil }), Clock: clk, Version: "t"}
	r.Results.Push(prober.Result{TaskID: 1, Outcome: prober.Outcome{RttUs: 7}, At: r.Clock.Mono()})
	round := 0
	r.Sleep = func(context.Context, time.Duration) error {
		round++
		switch round {
		case 1:
			bump()
			// 休眠 25s：墙钟多走而单调钟不走，休眠前入队的结果无法与正常结果区分。
			r.Results.Push(prober.Result{TaskID: 2, Outcome: prober.Outcome{RttUs: 8}, At: r.Clock.Mono()})
			r.Clock.(*clock.Fake).SetWall(r.Clock.Now().Add(25 * time.Second))
		case 2:
			bump()
			r.Clock.(*clock.Fake).Advance(10 * time.Second)
			r.Results.Push(prober.Result{TaskID: 3, Outcome: prober.Outcome{RttUs: 9}, At: r.Clock.Mono()})
		case 3:
			return context.Canceled
		}
		return nil
	}
	if err := r.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	reports := hub.received()
	if len(reports) != 3 {
		t.Fatalf("reports = %d, want 3", len(reports))
	}
	if len(reports[0].ProbeResults) != 1 || reports[0].ProbeResults[0].TaskId != 1 {
		t.Fatalf("first results = %v, want task 1", reports[0].ProbeResults)
	}
	if got := reports[1].ProbeResults; len(got) != 0 {
		t.Fatalf("pre-suspend results must be discarded, got %v", got)
	}
	m := reports[1].Metrics
	if m.CpuPct != nil || m.NetRxBps != nil || m.NetTxBps != nil || m.DiskReadBps != nil || m.DiskWriteBps != nil {
		t.Fatalf("trigger round must be a first sample without rates: %+v", m)
	}
	if len(reports[1].NetInterfaces) == 0 {
		t.Fatal("counters unaffected by the reset must still be reported")
	}
	m = reports[2].Metrics
	if m.GetCpuPct() != 50 || m.GetNetRxBps() != 200 || m.GetDiskReadBps() != 51200 {
		t.Fatalf("rates must resume one round after the trigger: %+v", m)
	}
	if len(reports[2].ProbeResults) != 1 || reports[2].ProbeResults[0].TaskId != 3 {
		t.Fatalf("post-wake results = %v, want task 3", reports[2].ProbeResults)
	}
	if r.Results.Dropped() != 1 {
		t.Fatalf("dropped = %d, want the discarded pre-suspend result counted", r.Results.Dropped())
	}
	for _, want := range []string{"clock jump", "wall_delta=25s", "mono_delta=0s"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log missing %q:\n%s", want, logs.String())
		}
	}
}

type advancingClock struct {
	clock.Clock
	mono time.Duration
}

func TestRunnerRequiresProbeComponents(t *testing.T) {
	for _, field := range []string{"Prober", "Results"} {
		t.Run(field, func(t *testing.T) {
			r, _ := newRunner(t, &fakeHub{})
			if field == "Prober" {
				r.Prober = nil
			} else {
				r.Results = nil
			}
			if err := r.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "Runner."+field+": required") {
				t.Fatalf("missing %s: %v", field, err)
			}
		})
	}
}

func (c *advancingClock) Mono() time.Duration {
	c.mono += time.Second
	return c.mono
}

func TestRunnerUsesOneInstantForResultAge(t *testing.T) {
	hub := &fakeHub{}
	r, _ := newRunner(t, hub)
	r.Clock = &advancingClock{Clock: r.Clock, mono: time.Hour}
	r.Results = prober.NewQueue(1)
	r.Results.Push(prober.Result{TaskID: 1, At: time.Hour + time.Second - probelimit.MaxResultAge})
	r.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	if err := r.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	reports := hub.received()
	if len(reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(reports))
	}
	got := reports[0].ProbeResults
	if len(got) != 1 || got[0].AgeMs != uint32(probelimit.MaxResultAge/time.Millisecond) {
		t.Fatalf("boundary results = %v, want age_ms 120000 at filtering instant", got)
	}
}

func TestRunnerDoesNotWarnAboutEmptyDiscard(t *testing.T) {
	r, _ := newRunner(t, &fakeHub{probeError: connect.CodeInvalidArgument})
	var logs bytes.Buffer
	r.Log = slog.New(slog.NewTextHandler(&logs, nil))
	r.Results = prober.NewQueue(1)
	r.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	if err := r.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "discarding rejected probe results") {
		t.Fatalf("empty batch must not log a discard: %s", logs.String())
	}
}

func TestRunnerWarnsOnlyWhenQueueDropsIncrease(t *testing.T) {
	for _, code := range []connect.Code{0, connect.CodeUnavailable} {
		t.Run(code.String(), func(t *testing.T) {
			r, _ := newRunner(t, &fakeHub{probeError: code})
			var logs bytes.Buffer
			r.Log = slog.New(slog.NewJSONHandler(&logs, nil))
			q := prober.NewQueue(1)
			r.Results = q
			q.Push(prober.Result{At: r.Clock.Mono()})
			q.Push(prober.Result{At: r.Clock.Mono()})
			round := 0
			r.Sleep = func(context.Context, time.Duration) error {
				round++
				var deltas []uint64
				decoder := json.NewDecoder(bytes.NewReader(logs.Bytes()))
				for decoder.More() {
					var record struct {
						Message string `json:"msg"`
						Level   string `json:"level"`
						Dropped uint64 `json:"dropped"`
					}
					if err := decoder.Decode(&record); err != nil {
						t.Fatal(err)
					}
					if record.Message == "probe results dropped" {
						if record.Level != "WARN" {
							t.Errorf("drop log level = %s, want WARN", record.Level)
						}
						deltas = append(deltas, record.Dropped)
					}
				}
				want := []uint64{1}
				if round >= 3 {
					want = append(want, 2)
				}
				if !slices.Equal(deltas, want) {
					t.Errorf("round %d drop deltas = %v, want %v", round, deltas, want)
				}
				if round == 2 {
					q.Take(r.Clock.Mono(), probelimit.MaxResultAge, probelimit.MaxResultsPerReport)
					q.Push(prober.Result{At: r.Clock.Mono() - probelimit.MaxResultAge - 2*time.Second})
					q.Push(prober.Result{At: r.Clock.Mono() - probelimit.MaxResultAge - time.Second})
				}
				if round == 4 {
					return context.Canceled
				}
				return nil
			}
			if err := r.Run(context.Background()); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

// 每个上报周期识别一次：识别文件（以 mountinfo 为代表）每周期只读一遍，
// Metrics 与 Facts 共用同一快照。
func TestRunnerIdentifiesOncePerCycle(t *testing.T) {
	hub := &fakeHub{interval: 10000}
	r, _ := newRunner(t, hub)
	clk := clock.NewFake(time.Unix(0, 0))
	fsys := fstest.MapFS{
		"proc/stat":    {Data: []byte("cpu  100 0 50 800 20 0 10 0 0 0\n")},
		"proc/loadavg": {Data: []byte("0.5 0.5 0.5 1/2 3\n")},
	}
	fsys["proc/self/mountinfo"] = &fstest.MapFile{Data: []byte("42 41 0:22 / /proc rw - proc proc rw\n")}
	counted := &countMountinfo{FS: fsys}
	r.Collector = &collect.Collector{Host: hostProcFS(counted, func() (uint64, uint64, error) { return 0, 0, nil }), Clock: clk, Version: "t"}
	round := 0
	r.Sleep = func(context.Context, time.Duration) error {
		round++
		if round >= 2 {
			return context.Canceled
		}
		return nil
	}
	if err := r.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// hostProcFS 已把 mountinfo 写进 fsys；两次上报周期共两次识别读取。
	if counted.opens != 2 {
		t.Fatalf("identify ran %d times in 2 cycles, want 2 (once per cycle, shared by Metrics and Facts)", counted.opens)
	}
	if hub.received()[0].GetFacts().GetExecution() == nil {
		t.Fatal("Facts must carry the execution scope")
	}
}

// countMountinfo 统计 mountinfo 被打开的次数：识别每做一次就读它一遍。
type countMountinfo struct {
	fs.FS
	opens int
}

func (c *countMountinfo) Open(name string) (fs.File, error) {
	if name == "proc/self/mountinfo" {
		c.opens++
	}
	return c.FS.Open(name)
}

// tasks_digest 在尚未收到任何清单时缺席，收到后等于对持有清单（含被拒任务）的摘要。
func TestRunnerReportsTasksDigest(t *testing.T) {
	valid := &heronv1.ProbeTask{Id: 1, Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "127.0.0.1", IntervalS: 5, TimeoutMs: 1000}
	rejected := &heronv1.ProbeTask{Id: 2, Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "127.0.0.1", IntervalS: 1, TimeoutMs: 1000}
	hub := &fakeHub{interval: 10000, tasks: &heronv1.ProbeTasks{Version: 3, Tasks: []*heronv1.ProbeTask{valid, rejected}}}
	r, _ := newRunner(t, hub)
	runFor(t, r, hub, 2)
	reports := hub.received()
	if reports[0].TasksDigest != nil {
		t.Fatalf("tasks_digest before any task list = %x, want absent", reports[0].TasksDigest)
	}
	want, err := agentwire.TasksDigest(hub.tasks.GetTasks())
	if err != nil {
		t.Fatalf("TasksDigest: %v", err)
	}
	if !bytes.Equal(reports[1].GetTasksDigest(), want) {
		t.Fatalf("tasks_digest = %x, want %x (covers the rejected task)", reports[1].GetTasksDigest(), want)
	}
}

// 请求带上与 metrics 同一次采样的逐网卡计数；网络采集失败时列表缺席。
func TestRunnerReportsNetInterfaces(t *testing.T) {
	hub := &fakeHub{interval: 10000}
	r, _ := newRunner(t, hub)
	r.Collector = &collect.Collector{Host: hostProcFS(fstest.MapFS{
		"sys/class/net/eth1/statistics/rx_bytes": {Data: []byte("30\n")},
		"sys/class/net/eth1/statistics/tx_bytes": {Data: []byte("40\n")},
		"sys/class/net/eth0/statistics/rx_bytes": {Data: []byte("10\n")},
		"sys/class/net/eth0/statistics/tx_bytes": {Data: []byte("20\n")},
	}, func() (uint64, uint64, error) { return 1, 1, nil }), Clock: clock.NewFake(time.Unix(0, 0)), Version: "t"}
	runFor(t, r, hub, 2)
	want := []*heronv1.NetInterfaceCounter{{Name: "eth0", RxBytes: 10, TxBytes: 20}, {Name: "eth1", RxBytes: 30, TxBytes: 40}}
	for i, rep := range hub.received() {
		got := rep.GetNetInterfaces()
		if len(got) != len(want) || !proto.Equal(got[0], want[0]) || !proto.Equal(got[1], want[1]) {
			t.Fatalf("report %d net_interfaces = %v, want %v", i, got, want)
		}
		m := rep.GetMetrics()
		if m.NetRxTotal != nil || m.NetTxTotal != nil || m.GetNetCounterEpoch() != "" {
			t.Fatalf("report %d repeats derived totals: %v", i, m)
		}
	}

	none := &fakeHub{interval: 10000}
	r, _ = newRunner(t, none)
	runFor(t, r, none, 1)
	if rep := none.received()[0]; rep.NetInterfaces != nil || rep.GetMetrics().NetRxTotal != nil {
		t.Fatalf("failed network collection still sent counters: list %v totals %v", rep.NetInterfaces, rep.GetMetrics().NetRxTotal)
	}
}

func TestRunnerOmitsTaskVersion(t *testing.T) {
	r, _ := newRunner(t, &fakeHub{})
	r.Prober.Apply(&heronv1.ProbeTasks{Version: 7})
	rep := driveScript(t, r, []*heronv1.ReportResponse{{}}, nil)[0]
	raw, err := proto.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for len(raw) > 0 {
		num, _, n := protowire.ConsumeField(raw)
		if n < 0 {
			t.Fatal(protowire.ParseError(n))
		}
		if num == 3 || num == 7 {
			t.Fatalf("request carries reserved field %d", num)
		}
		raw = raw[n:]
	}
}
