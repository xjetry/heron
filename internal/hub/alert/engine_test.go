package alert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/live"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

type fixture struct {
	st   *store.Store
	clk  *clock.Fake
	loc  *time.Location
	l    *live.Live
	e    *Engine
	log  *slog.Logger
	ids  []int64
	path string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	// 时区东八区：夹具时钟的 UTC 12:00 是当地 20:00，UTC 16:00 起当地已是次日，到期测试据此区分"按哪个时区取今天"。
	f := &fixture{clk: clock.NewFake(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)), loc: time.FixedZone("UTC+8", 8*3600), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var err error
	f.path = filepath.Join(t.TempDir(), "hub.db")
	f.st, err = store.Open(f.path, f.clk, f.log, store.MigrateSchema)
	must(t, err)
	t.Cleanup(func() { must(t, f.st.Close()) })
	for i := 0; i < 2; i++ {
		id, _, err := f.st.CreateNode(t.Context(), store.NewNode{Name: fmt.Sprintf("node%d", i+1)}, []byte(fmt.Sprintf("hash%d", i)))
		must(t, err)
		f.ids = append(f.ids, id)
	}
	f.restart(t)
	return f
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) restart(t *testing.T) {
	t.Helper()
	f.l = live.New(f.clk, 30*time.Second)
	f.e = New(Config{TTL: 30 * time.Second, Location: f.loc}, f.st, f.st.Evaluation(), f.l, f.clk, f.log)
	must(t, f.e.Load(t.Context()))
}
func (f *fixture) rule(t *testing.T, r store.AlertRule) store.AlertRule {
	t.Helper()
	saved, err := f.e.SaveRule(t.Context(), r)
	must(t, err)
	return saved
}
func offline() store.AlertRule {
	return store.AlertRule{Name: "离线", Kind: store.KindOffline, Enabled: true, AllNodes: true}
}
func (f *fixture) grace(t *testing.T, id int64, seconds int) {
	t.Helper()
	_, err := f.st.UpdateNode(t.Context(), id, store.NodeEdit{Name: fmt.Sprintf("node%d", id), TrafficResetDay: 1, OfflineGraceS: seconds})
	must(t, err)
}
func (f *fixture) sweep(t *testing.T) { t.Helper(); must(t, f.e.SweepOffline(t.Context())) }
func (f *fixture) events(t *testing.T) []store.AlertEvent {
	t.Helper()
	ev, err := f.st.ListAlertEvents(t.Context(), 0, 0, 100)
	must(t, err)
	return ev
}
func stateOf(e *Engine, r, n int64) store.AlertState {
	for _, s := range e.States() {
		if s.RuleID == r && s.NodeID == n {
			return s.State
		}
	}
	return ""
}
func wantState(t *testing.T, e *Engine, r, n int64, want store.AlertState) {
	t.Helper()
	if got := stateOf(e, r, n); got != want {
		t.Fatalf("node %d state=%q want %q", n, got, want)
	}
}

type recorder struct{ events []store.AlertEvent }

func (s *recorder) Enqueue(ev store.AlertEvent) { s.events = append(s.events, ev) }

func TestSweepOfflineFollowsRestartInvariant(t *testing.T) {
	f := newFixture(t)
	for _, id := range f.ids {
		f.grace(t, id, 60)
	}
	c, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "通知", Kind: store.ChannelTelegram, Config: `{"bot_token":"secret","chat_id":"chat"}`})
	must(t, err)
	r := offline()
	r.ChannelIDs = []int64{c.ID}
	r = f.rule(t, r)
	sender := &recorder{}
	f.e.SetSender(sender)
	f.l.Observe(f.ids[0], "", &heronv1.Metrics{})
	f.sweep(t)
	if len(f.events(t)) != 0 {
		t.Fatal("startup must not fire from zero monotonic baseline")
	}
	f.clk.Advance(31 * time.Second)
	f.sweep(t)
	for _, id := range f.ids {
		wantState(t, f.e, r.ID, id, store.StatePending)
	}
	f.clk.SetWall(f.clk.Now().Add(-24 * time.Hour))
	f.clk.Advance(30 * time.Second)
	f.sweep(t)
	for _, id := range f.ids {
		wantState(t, f.e, r.ID, id, store.StateFiring)
	}
	events := f.events(t)
	if len(events) != 2 || len(sender.events) != 2 {
		t.Fatalf("events=%d sent=%d want 2", len(events), len(sender.events))
	}
	for _, ev := range events {
		if len(ev.Deliveries) != 1 || ev.Deliveries[0].ChannelID != c.ID || !ev.At.Equal(f.clk.Now()) || !strings.Contains(ev.Summary, "离线") || !strings.Contains(ev.Summary, "node") {
			t.Fatalf("event=%+v", ev)
		}
	}
	f.l.Observe(f.ids[0], "", &heronv1.Metrics{})
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateOK)
	wantState(t, f.e, r.ID, f.ids[1], store.StateFiring)
	if ev := f.events(t); len(ev) != 3 || ev[0].Transition != store.TransitionRecovered {
		t.Fatalf("recovery events=%+v", ev)
	}
	// 重启前让节点再次离线，才能验证持久化 firing 在没有新上报时不恢复。刚恢复就再次离线落在抖动窗口里，要满
	// flapGrace 才重新进入 firing，节点自己的 60 秒宽限不够。
	f.clk.Advance(flapGrace + time.Second)
	f.sweep(t)
	f.restart(t)
	f.sweep(t)
	f.clk.Advance(61 * time.Second)
	f.sweep(t)
	if len(f.events(t)) != 4 {
		t.Fatal("restart generated a transition without a report")
	}
	for _, id := range f.ids {
		wantState(t, f.e, r.ID, id, store.StateFiring)
	}
	f.l.Observe(f.ids[0], "", &heronv1.Metrics{})
	f.sweep(t)
	if ev := f.events(t); len(ev) != 5 || ev[0].Transition != store.TransitionRecovered {
		t.Fatalf("restart recovery events=%+v", ev)
	}
}

func TestSweepUsesNodeGraceWithTTLFloor(t *testing.T) {
	for _, grace := range []int{0, 10, 120} {
		t.Run(fmt.Sprint(grace), func(t *testing.T) {
			f := newFixture(t)
			f.grace(t, f.ids[0], grace)
			r := offline()
			r.AllNodes = false
			r.NodeIDs = f.ids[:1]
			r = f.rule(t, r)
			f.clk.Advance(29 * time.Second)
			f.sweep(t)
			if len(f.events(t)) != 0 {
				t.Fatal("fired below TTL")
			}
			f.clk.Advance(time.Second)
			f.sweep(t)
			want := store.StateFiring
			if grace == 120 {
				want = store.StatePending
			}
			wantState(t, f.e, r.ID, f.ids[0], want)
			f.clk.Advance(90 * time.Second)
			f.sweep(t)
			wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
		})
	}
}

func TestSweepRespectsScopeAndEnabled(t *testing.T) {
	f := newFixture(t)
	r := offline()
	r.AllNodes = false
	r.NodeIDs = f.ids[:1]
	r = f.rule(t, r)
	f.clk.Advance(time.Minute)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	wantState(t, f.e, r.ID, f.ids[1], "")
	r.Enabled = false
	f.rule(t, r)
	f.sweep(t)
	rows, err := f.st.ListAlertStates(t.Context())
	must(t, err)
	if len(f.e.States()) != 0 || len(rows) != 0 || len(f.events(t)) != 1 {
		t.Fatal("disabled rule retained or evaluated states")
	}
	// 节点删除把显式作用域变为空集，重载不能把它解释成全部节点。
	r.Enabled = true
	f.rule(t, r)
	must(t, f.st.DeleteNode(t.Context(), f.ids[0]))
	f.restart(t)
	f.clk.Advance(time.Minute)
	f.sweep(t)
	if len(f.e.States()) != 0 {
		t.Fatal("empty explicit scope matched nodes")
	}
	if len(f.e.Rules()) != 1 {
		t.Fatal("empty explicit scope rule disappeared on reload")
	}
}

func TestSaveRuleShrinkingScopeDropsStates(t *testing.T) {
	f := newFixture(t)
	r := f.rule(t, offline())
	f.clk.Advance(time.Minute)
	f.sweep(t)
	r.AllNodes = false
	r.NodeIDs = f.ids[:1]
	f.rule(t, r)
	rows, err := f.st.ListAlertStates(t.Context())
	must(t, err)
	if len(rows) != 1 || len(f.e.States()) != 1 || rows[0].NodeID != f.ids[0] {
		t.Fatalf("states=%v cached=%v", rows, f.e.States())
	}
	must(t, f.e.DeleteRule(t.Context(), r.ID))
	if len(f.e.Rules()) != 0 || len(f.e.States()) != 0 {
		t.Fatal("deleted rule remains cached")
	}
}

func (f *fixture) task(t *testing.T, ids []int64) uint64 {
	t.Helper()
	p, _, err := f.st.SaveProbeTask(t.Context(), &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "127.0.0.1", IntervalS: 5, TimeoutMs: 100}, store.NodeSelector{AllNodes: false, NodeIDs: ids})
	must(t, err)
	return p.Task.Id
}
func (f *fixture) minutes(t *testing.T, task uint64, node int64, start int64, buckets ...metric.ProbeBucket) {
	t.Helper()
	var rows []metric.ProbeRow
	for i, b := range buckets {
		rows = append(rows, metric.ProbeRow{NodeID: node, TaskID: task, TS: start + int64(i)*60, Bucket: &b})
	}
	_, err := f.st.WriteMinuteBatch(t.Context(), metric.Batch{Probes: rows})
	must(t, err)
}

func TestEvaluateProbesReadsClosedMinutes(t *testing.T) {
	for _, metricName := range []store.ProbeMetric{store.MetricLossPct, store.MetricRttMs} {
		t.Run(string(metricName), func(t *testing.T) {
			f := newFixture(t)
			task := f.task(t, f.ids)
			other := f.task(t, f.ids)
			ts := f.clk.Now().Unix() - 240
			r := store.AlertRule{Name: "探测", Kind: store.KindProbe, Enabled: true, AllNodes: true, TaskID: task, Metric: metricName, Threshold: 20, ForMinutes: 3}
			if metricName == store.MetricRttMs {
				r.RttMode = store.RttThreshold
			}
			r = f.rule(t, r)
			high := metric.ProbeBucket{Sent: 4, Lost: 2, RttN: 2, RttSumUs: 101_000, RttMinUs: 50_000, RttMaxUs: 51_000}
			low := metric.ProbeBucket{Sent: 2, Errors: 1, RttN: 1, RttSumUs: 1_000, RttMinUs: 1_000, RttMaxUs: 1_000}
			f.minutes(t, task, f.ids[0], ts, high, high, high, low)
			f.minutes(t, other, f.ids[0], ts, low, low, low)
			must(t, f.e.EvaluateProbes(t.Context(), ts+120))
			wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
			ev := f.events(t)
			want := 50.0
			if metricName == store.MetricRttMs {
				want = 50.5
			}
			if len(ev) != 1 || ev[0].Value != want || !strings.Contains(ev[0].Summary, fmt.Sprintf("%.1f", want)) || !strings.Contains(ev[0].Summary, "探测") {
				t.Fatalf("events=%+v want value %v", ev, want)
			}
			must(t, f.e.EvaluateProbes(t.Context(), ts+180))
			wantState(t, f.e, r.ID, f.ids[0], store.StateOK)
			if ev = f.events(t); len(ev) != 2 || ev[0].Transition != store.TransitionRecovered {
				t.Fatalf("events=%+v", ev)
			}
			f.minutes(t, task, f.ids[0], ts+240, high, high, high)
			must(t, f.e.EvaluateProbes(t.Context(), ts+360))
			missing := metric.ProbeBucket{}
			if metricName == store.MetricRttMs {
				missing = metric.ProbeBucket{Sent: 1, Lost: 1}
			}
			f.minutes(t, task, f.ids[0], ts+420, missing)
			must(t, f.e.EvaluateProbes(t.Context(), ts+420))
			wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
			must(t, f.e.EvaluateProbes(t.Context(), ts+480))
			wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
		})
	}
}

func TestEvaluateProbesOnlyForAssignedNodes(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, f.ids[:1])
	ts := f.clk.Now().Unix() - 60
	r := f.rule(t, store.AlertRule{Name: "探测", Kind: store.KindProbe, Enabled: true, AllNodes: true, TaskID: task, Metric: store.MetricLossPct, Threshold: 20, ForMinutes: 1})
	for _, id := range f.ids {
		f.minutes(t, task, id, ts, metric.ProbeBucket{Sent: 1, Lost: 1})
	}
	must(t, f.e.EvaluateProbes(t.Context(), ts))
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	wantState(t, f.e, r.ID, f.ids[1], "")
}

// 探测规则的候选节点取任务的覆盖（store.ProbeTaskNodeIDs）：all_nodes 任务覆盖全部节点，含任务保存之后才建的节点；
// 显式空分配的任务不覆盖任何节点。
func TestEvaluateProbesFollowsTaskCoverage(t *testing.T) {
	f := newFixture(t)
	p, _, err := f.st.SaveProbeTask(t.Context(), &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "127.0.0.1", IntervalS: 5, TimeoutMs: 100}, store.NodeSelector{AllNodes: true, NodeIDs: nil})
	must(t, err)
	all := p.Task.Id
	none := f.task(t, nil)
	late, _, err := f.st.CreateNode(t.Context(), store.NewNode{Name: "late"}, []byte("late"))
	must(t, err)
	nodes := append(slices.Clone(f.ids), late)
	ts := f.clk.Now().Unix() - 60
	var rules []store.AlertRule
	for _, task := range []uint64{all, none} {
		rules = append(rules, f.rule(t, store.AlertRule{Name: "探测", Kind: store.KindProbe, Enabled: true, AllNodes: true, TaskID: task, Metric: store.MetricLossPct, Threshold: 20, ForMinutes: 1}))
		for _, id := range nodes {
			f.minutes(t, task, id, ts, metric.ProbeBucket{Sent: 1, Lost: 1})
		}
	}
	must(t, f.e.EvaluateProbes(t.Context(), ts))
	for _, id := range nodes {
		wantState(t, f.e, rules[0].ID, id, store.StateFiring)
		wantState(t, f.e, rules[1].ID, id, "")
	}
}

func TestFailedStateWriteDoesNotPublish(t *testing.T) {
	for _, elapsed := range []time.Duration{31 * time.Second, 61 * time.Second} {
		t.Run(elapsed.String(), func(t *testing.T) {
			f := newFixture(t)
			for _, id := range f.ids {
				f.grace(t, id, 60)
			}
			r := f.rule(t, offline())
			sender := &recorder{}
			f.e.SetSender(sender)
			must(t, f.st.DeleteAlertRule(t.Context(), r.ID))
			f.clk.Advance(elapsed)
			if err := f.e.SweepOffline(t.Context()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("error=%v", err)
			}
			rows, err := f.st.ListAlertStates(t.Context())
			must(t, err)
			if len(f.e.States()) != 0 || len(rows) != 0 || len(sender.events) != 0 {
				t.Fatalf("failed write published states=%v rows=%v sent=%v", f.e.States(), rows, sender.events)
			}
		})
	}
}

func TestChannelTokenMergeAndSnapshotOwnership(t *testing.T) {
	f := newFixture(t)
	c, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "通知", Kind: store.ChannelTelegram, Config: `{"bot_token":"old","chat_id":"one"}`})
	must(t, err)
	c.Config = `{"bot_token":"authoritative","chat_id":"one"}`
	_, err = f.st.SaveNotifyChannel(t.Context(), c)
	must(t, err)
	c.Config = `{"bot_token":"","chat_id":"two"}`
	c, err = f.e.SaveChannel(t.Context(), c)
	must(t, err)
	var config map[string]string
	must(t, json.Unmarshal([]byte(c.Config), &config))
	if config["bot_token"] != "authoritative" || config["chat_id"] != "two" {
		t.Fatalf("merged=%v", config)
	}
	r := offline()
	r.AllNodes = false
	r.NodeIDs = append([]int64(nil), f.ids...)
	r.ChannelIDs = []int64{c.ID}
	r = f.rule(t, r)
	r.NodeIDs[0] = 999
	r.ChannelIDs[0] = 999
	rules := f.e.Rules()
	if rules[0].NodeIDs[0] != f.ids[0] || rules[0].ChannelIDs[0] != c.ID {
		t.Fatal("save result aliases cache")
	}
	rules[0].NodeIDs[0] = 888
	rules[0].ChannelIDs[0] = 888
	if got := f.e.Rules()[0]; got.NodeIDs[0] != f.ids[0] || got.ChannelIDs[0] != c.ID {
		t.Fatal("Rules aliases cache")
	}
	channels := f.e.Channels()
	channels[0].Config = "changed"
	if f.e.Channels()[0].Config != c.Config {
		t.Fatal("Channels snapshot lost config or allowed external mutation")
	}
	f.clk.Advance(time.Minute)
	f.sweep(t)
	states := f.e.States()
	states[0].State = store.StateOK
	if reflect.DeepEqual(states, f.e.States()) {
		t.Fatal("States snapshot lost firing or allowed external mutation")
	}
	must(t, f.st.DeleteNode(t.Context(), f.ids[0]))
	f.e.Forget(f.ids[0])
	wantState(t, f.e, r.ID, f.ids[0], "")
	must(t, f.e.DeleteRule(t.Context(), r.ID))
	must(t, f.e.DeleteChannel(t.Context(), c.ID))
	if len(f.e.Channels()) != 0 {
		t.Fatal("deleted channel remains cached")
	}
	if _, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "new", Kind: store.ChannelTelegram, Config: `{"chat_id":"two"}`}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("new blank token error=%v", err)
	}
}

func TestRunLoopsStopOnCancellation(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// 取消已经发生。正确实现在 select 里看到 ctx 结束就返回。
	// 先等第一次自然唤醒再查取消的路径不会早于这次唤醒：离线巡检是 OfflineSweepEvery，
	// 探测评估是 nextProbeAt 距现在。Bound 只兜永远不返回。
	cases := []struct {
		name string
		run  func(context.Context)
		wake time.Duration
	}{
		{"offline sweep", f.e.RunOfflineSweep, OfflineSweepEvery},
		{"probe evaluation", f.e.RunProbeEvaluation, nextProbeAt(f.clk.Now()).Sub(f.clk.Now())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			start := time.Now()
			go func() { tc.run(ctx); close(done) }()
			select {
			case <-done:
				if elapsed := time.Since(start); elapsed >= tc.wake {
					t.Fatalf("returned %v after cancellation, want before first wake %v", elapsed, tc.wake)
				}
			case <-time.After(testwait.Bound):
				t.Fatal("run loop ignored cancellation")
			}
		})
	}
}

func TestAllNodesIncludesNewNodes(t *testing.T) {
	f := newFixture(t)
	r := f.rule(t, offline())
	id, _, err := f.st.CreateNode(t.Context(), store.NewNode{Name: "new"}, []byte("new-hash"))
	must(t, err)
	f.clk.Advance(time.Minute)
	f.sweep(t)
	wantState(t, f.e, r.ID, id, store.StateFiring)
}

func TestEvaluateProbesRespectsScopeAndEnabled(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, f.ids)
	ts := f.clk.Now().Unix() - 60
	r := f.rule(t, store.AlertRule{Name: "探测", Kind: store.KindProbe, Enabled: true, NodeIDs: f.ids[:1], TaskID: task, Metric: store.MetricLossPct, Threshold: 20, ForMinutes: 1})
	for _, id := range f.ids {
		f.minutes(t, task, id, ts, metric.ProbeBucket{Sent: 1, Lost: 1})
	}
	must(t, f.e.EvaluateProbes(t.Context(), ts))
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	wantState(t, f.e, r.ID, f.ids[1], "")
	r.Enabled = false
	f.rule(t, r)
	must(t, f.e.EvaluateProbes(t.Context(), ts))
	if len(f.e.States()) != 0 || len(f.events(t)) != 1 {
		t.Fatal("disabled probe rule evaluated")
	}
}
