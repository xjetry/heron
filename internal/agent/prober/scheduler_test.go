package prober

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/probelimit"
	"github.com/xjetry/heron/internal/testwait"
)

type sleepCall struct {
	ctx     context.Context
	delay   time.Duration
	release chan struct{}
}

func controlledSleep(calls chan<- sleepCall) func(context.Context, time.Duration) error {
	return func(ctx context.Context, delay time.Duration) error {
		call := sleepCall{ctx: ctx, delay: delay, release: make(chan struct{})}
		select {
		case calls <- call:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-call.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(testwait.Bound):
		t.Fatal("timed out waiting for controlled event")
		var zero T
		return zero
	}
}

func task(id uint64) *heronv1.ProbeTask {
	return &heronv1.ProbeTask{Id: id, Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "127.0.0.1", IntervalS: 5, TimeoutMs: 1000}
}

func logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type probeCall struct {
	ctx    context.Context
	id     uint64
	target string
}

func TestApplyStartsNewStopsGoneKeepsUnchanged(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	calls := make(chan sleepCall, 32)
	probes := make(chan probeCall, 32)
	s := NewScheduler(engineFunc(func(ctx context.Context, task *heronv1.ProbeTask) Outcome {
		probes <- probeCall{ctx, task.Id, task.Target}
		return Outcome{RttUs: 1}
	}), NewQueue(QueueCap), clk, logger())
	s.Sleep, s.Rand = controlledSleep(calls), func() float64 { return .25 }
	defer s.Stop()
	s.Apply(&heronv1.ProbeTasks{Version: 1, Tasks: []*heronv1.ProbeTask{task(1), task(2), task(4)}})
	var initial []sleepCall
	for range 3 {
		c := receive(t, calls)
		if c.delay != 1250*time.Millisecond {
			t.Fatalf("initial offset=%v", c.delay)
		}
		initial = append(initial, c)
	}
	for _, c := range initial {
		close(c.release)
	}
	byID := map[uint64]probeCall{}
	for range 3 {
		c := receive(t, probes)
		byID[c.id] = c
	}
	asleep := map[context.Context]sleepCall{}
	for range 3 {
		c := receive(t, calls)
		asleep[c.ctx] = c
	}
	changed := task(2)
	changed.Target = "localhost"
	s.Apply(&heronv1.ProbeTasks{Version: 2, Tasks: []*heronv1.ProbeTask{task(1), changed, task(3)}})
	for _, id := range []uint64{2, 4} {
		if byID[id].ctx.Err() == nil {
			t.Fatalf("task %d not canceled", id)
		}
	}
	if byID[1].ctx.Err() != nil {
		t.Fatal("unchanged task canceled")
	}
	first := []sleepCall{receive(t, calls), receive(t, calls)}
	for _, c := range first {
		if c.delay != 1250*time.Millisecond {
			t.Fatalf("new offset=%v", c.delay)
		}
	}
	close(asleep[byID[1].ctx].release)
	again := receive(t, probes)
	if again.id != 1 || again.ctx != byID[1].ctx {
		t.Fatalf("unchanged task restarted: %+v", again)
	}
	c := receive(t, calls)
	if c.delay != 5*time.Second {
		t.Fatalf("unchanged interval=%v", c.delay)
	}
	for _, c := range first {
		close(c.release)
	}
	want := map[uint64]string{2: "localhost", 3: "127.0.0.1"}
	for range 2 {
		c := receive(t, probes)
		if want[c.id] != c.target {
			t.Fatalf("new task=%+v", c)
		}
		delete(want, c.id)
	}
	for range 2 {
		receive(t, calls)
	}
	s.Apply(&heronv1.ProbeTasks{Version: 3})
	s.mu.Lock()
	n := len(s.running)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("empty list: running=%d", n)
	}
}

func TestApplyRejectsInvalidAndOverLimitWithErrorResults(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	q := NewQueue(QueueCap)
	calls := make(chan sleepCall, 128)
	probes := make(chan uint64, 128)
	s := NewScheduler(engineFunc(func(_ context.Context, task *heronv1.ProbeTask) Outcome {
		probes <- task.Id
		return Outcome{RttUs: 1}
	}), q, clk, logger())
	s.Sleep, s.Rand = controlledSleep(calls), func() float64 { return 0 }
	defer s.Stop()
	var tasks []*heronv1.ProbeTask
	for id := uint64(65); id > 0; id-- {
		tasks = append(tasks, task(id))
	}
	tasks[64].IntervalS = 1
	s.Apply(&heronv1.ProbeTasks{Version: 7, Tasks: tasks})
	rs := q.Take(clk.Mono(), probelimit.MaxResultAge, probelimit.MaxResultsPerReport)
	if len(rs) != 2 || rs[0].TaskID != 1 || !strings.Contains(rs[0].Outcome.Err, "interval_s must be between 5 and 3600") ||
		rs[1].TaskID != 65 || !strings.Contains(rs[1].Outcome.Err, "more than 64 tasks assigned") ||
		rs[0].At != clk.Mono() || rs[1].At != clk.Mono() {
		t.Fatalf("rejections=%v", rs)
	}
	s.mu.Lock()
	n := len(s.running)
	s.mu.Unlock()
	if n != 63 {
		t.Fatalf("running=%d want 63", n)
	}
	var initial []sleepCall
	for range 63 {
		initial = append(initial, receive(t, calls))
	}
	for _, c := range initial {
		close(c.release)
	}
	seen := map[uint64]bool{}
	for range 63 {
		id := receive(t, probes)
		if id < 2 || id > 64 || seen[id] {
			t.Fatalf("forbidden or repeated engine call: %d", id)
		}
		seen[id] = true
	}
}

func TestRunKeepsPeriodDespiteProbeDuration(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	q := NewQueue(QueueCap)
	calls := make(chan sleepCall, 4)
	s := NewScheduler(engineFunc(func(context.Context, *heronv1.ProbeTask) Outcome {
		clk.Advance(2 * time.Second)
		return Outcome{RttUs: 2000000}
	}), q, clk, logger())
	s.Sleep, s.Rand = controlledSleep(calls), func() float64 { return 0 }
	defer s.Stop()
	s.Apply(&heronv1.ProbeTasks{Tasks: []*heronv1.ProbeTask{task(1)}})
	c := receive(t, calls)
	close(c.release)
	for i := range 3 {
		c = receive(t, calls)
		if c.delay != 3*time.Second {
			t.Fatalf("remaining interval=%v want 3s", c.delay)
		}
		rs := q.Take(clk.Mono(), probelimit.MaxResultAge, probelimit.MaxResultsPerReport)
		if len(rs) != 1 || rs[0].At != clk.Mono() || rs[0].Outcome.RttUs != 2000000 {
			t.Fatalf("result=%v", rs)
		}
		if i < 2 {
			clk.Advance(c.delay)
			close(c.release)
		}
	}
}

func TestStopDoesNotEnqueueInFlightResult(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	q := NewQueue(QueueCap)
	entered := make(chan struct{})
	calls := make(chan sleepCall, 2)
	s := NewScheduler(engineFunc(func(ctx context.Context, _ *heronv1.ProbeTask) Outcome {
		close(entered)
		<-ctx.Done()
		return Outcome{RttUs: 1}
	}), q, clk, logger())
	s.Sleep, s.Rand = controlledSleep(calls), func() float64 { return 0 }
	s.Apply(&heronv1.ProbeTasks{Tasks: []*heronv1.ProbeTask{task(1)}})
	c := receive(t, calls)
	close(c.release)
	receive(t, entered)
	s.Stop()
	if rs := q.Take(clk.Mono(), probelimit.MaxResultAge, probelimit.MaxResultsPerReport); len(rs) != 0 {
		t.Fatalf("stopped result queued: %v", rs)
	}
}

func TestApplyOwnsTaskSnapshot(t *testing.T) {
	calls := make(chan sleepCall, 2)
	probes := make(chan string, 1)
	s := NewScheduler(engineFunc(func(_ context.Context, t *heronv1.ProbeTask) Outcome {
		probes <- t.Target
		return Outcome{}
	}), NewQueue(3), clock.Real(), logger())
	s.Sleep, s.Rand = controlledSleep(calls), func() float64 { return 0 }
	defer s.Stop()
	input := task(1)
	s.Apply(&heronv1.ProbeTasks{Tasks: []*heronv1.ProbeTask{input}})
	first := receive(t, calls)
	input.Target = "changed.invalid"
	close(first.release)
	if got := receive(t, probes); got != "127.0.0.1" {
		t.Fatalf("input mutation reached engine: %s", got)
	}
}

func TestSleepHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- sleep(ctx, time.Hour) }()
	cancel()
	if err := receive(t, done); err != context.Canceled {
		t.Fatalf("canceled sleep=%v", err)
	}
	if err := sleep(t.Context(), 0); err != nil {
		t.Fatalf("zero sleep=%v", err)
	}
}

// 一个塞满空任务的清单只留一行日志：受限的响应不能被逐条告警放大成无界的日志。
func TestApplyLogsOneSummaryForAnyNumberOfRejections(t *testing.T) {
	var logs bytes.Buffer
	q := NewQueue(QueueCap)
	s := NewScheduler(engineFunc(func(context.Context, *heronv1.ProbeTask) Outcome { return Outcome{} }), q, clock.NewFake(time.Unix(0, 0)), slog.New(slog.NewTextHandler(&logs, nil)))
	defer s.Stop()
	tasks := make([]*heronv1.ProbeTask, 32750)
	for i := range tasks {
		tasks[i] = &heronv1.ProbeTask{}
	}
	s.Apply(&heronv1.ProbeTasks{Version: 1, Tasks: tasks})
	if n := strings.Count(logs.String(), "\n"); n != 1 || !strings.Contains(logs.String(), "count=32750") {
		t.Fatalf("%d log lines (%d bytes), want one summary with count=32750:\n%.500s", n, logs.Len(), logs.String())
	}
}

// 清单摘要代表收到并持有的整份清单（含被拒任务），与版本计数无关；尚未收到任何清单时缺席。
func TestSchedulerTasksDigest(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	s := NewScheduler(quietEngine{}, NewQueue(QueueCap), clk, logger())
	defer s.Stop()
	if d := s.TasksDigest(); d != nil {
		t.Fatalf("digest before any task list = %x, want absent", d)
	}
	received := []*heronv1.ProbeTask{task(2), task(1)}
	bad := task(3)
	bad.IntervalS = 1 // 被 CheckTask 拒绝，但仍在持有的清单里。
	received = append(received, bad)
	s.Apply(&heronv1.ProbeTasks{Version: 7, Tasks: received})
	want, err := agentwire.TasksDigest(received)
	if err != nil {
		t.Fatalf("TasksDigest: %v", err)
	}
	if d := s.TasksDigest(); !bytes.Equal(d, want) {
		t.Fatalf("digest = %x, want %x (covers the rejected task)", d, want)
	}
	// 全局版本变了而清单不变：摘要相等。
	s.Apply(&heronv1.ProbeTasks{Version: 8, Tasks: received})
	if d := s.TasksDigest(); !bytes.Equal(d, want) {
		t.Fatalf("digest after version-only bump = %x, want unchanged %x", d, want)
	}
	// 内容变化改变摘要。
	changed := []*heronv1.ProbeTask{task(2), task(1), task(4)}
	s.Apply(&heronv1.ProbeTasks{Version: 9, Tasks: changed})
	wantChanged, err := agentwire.TasksDigest(changed)
	if err != nil {
		t.Fatalf("TasksDigest: %v", err)
	}
	if d := s.TasksDigest(); bytes.Equal(d, want) || !bytes.Equal(d, wantChanged) {
		t.Fatalf("digest after content change = %x, want %x", d, wantChanged)
	}
	// 空清单的摘要是空串的 SHA-256，与缺席不同。
	s.Apply(&heronv1.ProbeTasks{Version: 10})
	if d := s.TasksDigest(); len(d) != 32 {
		t.Fatalf("digest of empty list = %x, want 32 bytes", d)
	}
}

type quietEngine struct{}

func (quietEngine) Probe(context.Context, *heronv1.ProbeTask) Outcome { return Outcome{RttUs: 1} }

// 每条结果回显产生它时的任务身份：被拒任务的 error 结果与正常探测结果都带。
func TestSchedulerEchoesConfigIDOnResults(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	q := NewQueue(QueueCap)
	calls := make(chan sleepCall, 4)
	s := NewScheduler(quietEngine{}, q, clk, logger())
	s.Sleep, s.Rand = controlledSleep(calls), func() float64 { return 0 }
	defer s.Stop()
	cfg := bytes.Repeat([]byte{9}, 16)
	ok := task(1)
	ok.ConfigId = cfg
	bad := task(2)
	bad.IntervalS = 1
	bad.ConfigId = cfg
	s.Apply(&heronv1.ProbeTasks{Version: 1, Tasks: []*heronv1.ProbeTask{ok, bad}})
	rs := q.Take(clk.Mono(), probelimit.MaxResultAge, probelimit.MaxResultsPerReport)
	if len(rs) != 1 || rs[0].TaskID != 2 || !bytes.Equal(rs[0].ConfigID, cfg) {
		t.Fatalf("rejection result = %+v, want task 2 error carrying its config_id", rs)
	}
	c := receive(t, calls)
	close(c.release)
	receive(t, calls) // 第一次探测完成、进入周期休眠后结果已入队。
	rs = q.Take(clk.Mono(), probelimit.MaxResultAge, probelimit.MaxResultsPerReport)
	if len(rs) != 1 || rs[0].TaskID != 1 || !bytes.Equal(rs[0].ConfigID, cfg) {
		t.Fatalf("probe result = %+v, want task 1 result carrying its config_id", rs)
	}
}

// 违反 pin / config_id 共用约束的任务在 agent 侧同样被拒（留 error 结果），不静默忽略 pin。
func TestApplyRejectsPinConstraintViolations(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	q := NewQueue(QueueCap)
	s := NewScheduler(quietEngine{}, q, clk, logger())
	defer s.Stop()
	pin := make([]byte, 32)
	pinHTTP := task(1) // http:// 目标上钉指纹
	pinHTTP.Kind, pinHTTP.Target = heronv1.ProbeKind_PROBE_KIND_HTTP, "http://example.com/"
	pinHTTP.CertSpkiSha256 = pin
	pinDNS := task(2) // DNS 分支提前 return，也不能放过 pin
	pinDNS.Kind, pinDNS.Target, pinDNS.DnsServer = heronv1.ProbeKind_PROBE_KIND_DNS, "example.com", "1.1.1.1:53"
	pinDNS.CertSpkiSha256 = pin
	badID := task(3)
	badID.ConfigId = make([]byte, 8)
	s.Apply(&heronv1.ProbeTasks{Version: 1, Tasks: []*heronv1.ProbeTask{pinHTTP, pinDNS, badID}})
	rs := q.Take(clk.Mono(), probelimit.MaxResultAge, probelimit.MaxResultsPerReport)
	if len(rs) != 3 {
		t.Fatalf("rejections=%v", rs)
	}
	for _, r := range rs {
		if !strings.Contains(r.Outcome.Err, "cert_spki_sha256") && !strings.Contains(r.Outcome.Err, "config_id") {
			t.Fatalf("rejection for task %d = %q, want pin/config_id constraint", r.TaskID, r.Outcome.Err)
		}
	}
	s.mu.Lock()
	n := len(s.running)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("running=%d want 0", n)
	}
}

// orderEngine 记录 pruneTasks 与 Probe 的调用顺序；Probe 在登记完成前自旋等待，
// 使"先起跑后登记"的顺序错误确定性地留下 probe 先于 prune 的记录，不靠时序运气。
type orderEngine struct {
	mu     sync.Mutex
	events []string
	pruned atomic.Bool
	probed chan struct{}
	once   sync.Once
}

func (e *orderEngine) Probe(_ context.Context, _ *heronv1.ProbeTask) Outcome {
	e.mu.Lock()
	e.events = append(e.events, "probe")
	e.mu.Unlock()
	e.once.Do(func() { close(e.probed) })
	for !e.pruned.Load() {
		runtime.Gosched()
	}
	return Outcome{RttUs: 1}
}

func (e *orderEngine) pruneTasks(map[uint64]string) {
	e.mu.Lock()
	e.events = append(e.events, "prune")
	e.mu.Unlock()
	e.pruned.Store(true)
}

// 新任务的 pruneTasks（登记身份）必须早于它的第一次 Probe：登记簿先对齐清单，探测才起跑，
// 晚于登记簿的探测因此一定来自已被换下的旧任务。
func TestApplyRegistersIdentityBeforeFirstProbe(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	eng := &orderEngine{probed: make(chan struct{})}
	s := NewScheduler(eng, NewQueue(QueueCap), clk, logger())
	s.Rand = func() float64 { return 0 }
	defer s.Stop()
	s.Apply(&heronv1.ProbeTasks{Version: 1, Tasks: []*heronv1.ProbeTask{task(1)}})
	select {
	case <-eng.probed:
	case <-time.After(5 * time.Second):
		t.Fatal("no probe ran within 5s")
	}
	eng.mu.Lock()
	events := slices.Clone(eng.events)
	eng.mu.Unlock()
	if len(events) == 0 || events[0] != "prune" {
		t.Fatalf("call order = %v, want prune before first probe", events)
	}
}

// 摘要编码失败（清单含无法确定性编码的任务）时：上报不带摘要（hub 退回按计数对账），
// 恢复后重新带上；日志只在状态翻转时记，不随每次 Apply 重复。
func TestSchedulerTasksDigestFailureOmitsDigest(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	s := NewScheduler(quietEngine{}, NewQueue(QueueCap), clk, logger())
	defer s.Stop()
	s.Apply(&heronv1.ProbeTasks{Version: 1, Tasks: []*heronv1.ProbeTask{task(1)}})
	if s.TasksDigest() == nil {
		t.Fatal("digest absent on a valid list")
	}
	// 无法编码的清单：proto3 string 含非法 UTF-8。正常路径到不了这里（解码那关先拒），
	// 但失败时必须按"摘要缺席"处理而不是吞掉错误。
	bad := task(2)
	bad.Target = string([]byte{0xff, 0xfe})
	s.Apply(&heronv1.ProbeTasks{Version: 2, Tasks: []*heronv1.ProbeTask{task(1), bad}})
	if d := s.TasksDigest(); d != nil {
		t.Fatalf("digest after encode failure = %x, want absent (hub falls back to counter reconciliation)", d)
	}
	// 失败状态下再失败不崩；恢复后摘要回来。
	s.Apply(&heronv1.ProbeTasks{Version: 3, Tasks: []*heronv1.ProbeTask{bad}})
	if d := s.TasksDigest(); d != nil {
		t.Fatalf("digest still present while failing: %x", d)
	}
	s.Apply(&heronv1.ProbeTasks{Version: 4, Tasks: []*heronv1.ProbeTask{task(1)}})
	if s.TasksDigest() == nil {
		t.Fatal("digest did not recover on a valid list")
	}
}
