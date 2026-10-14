package alert

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
)

// 抖动抑制的状态机：离线开始时距上次恢复不足一小时（不含一小时），宽限取 max(节点宽限, 30 分钟)。每行都从 pending、
// 未上报出发。30 分钟与一小时写字面值，不引用常量：常量改了而 spec 没改，这里要红。
func TestNextOfflineFlapGrace(t *testing.T) {
	ttl := 30 * time.Second
	firing := ptr(store.TransitionFiring)
	cases := []struct {
		name          string
		grace         time.Duration
		recovered     bool
		sinceRecovery time.Duration
		unseen        time.Duration
		want          store.AlertState
		tr            *store.Transition
		deferred      bool
	}{
		{"恢复后 10 分钟再离线，到节点宽限仍 pending", 2 * time.Minute, true, 10 * time.Minute, 2 * time.Minute, store.StatePending, nil, true},
		{"恢复后 10 分钟再离线，差 1 秒满 30 分钟仍 pending", 2 * time.Minute, true, 10 * time.Minute, 30*time.Minute - time.Second, store.StatePending, nil, true},
		{"恢复后 10 分钟再离线，满 30 分钟触发", 2 * time.Minute, true, 10 * time.Minute, 30 * time.Minute, store.StateFiring, firing, false},
		{"未满节点宽限的 pending 不算抖动推迟", 2 * time.Minute, true, 10 * time.Minute, time.Minute, store.StatePending, nil, false},
		{"恢复后 61 分钟再离线，按节点宽限触发", 2 * time.Minute, true, 61 * time.Minute, 2 * time.Minute, store.StateFiring, firing, false},
		{"恰好一小时不在窗口内", 2 * time.Minute, true, time.Hour, 2 * time.Minute, store.StateFiring, firing, false},
		{"差 1 秒一小时仍在窗口内", 2 * time.Minute, true, time.Hour - time.Second, 2 * time.Minute, store.StatePending, nil, true},
		{"节点宽限大于 30 分钟时 30 分钟不触发", 45 * time.Minute, true, 10 * time.Minute, 30 * time.Minute, store.StatePending, nil, false},
		{"节点宽限大于 30 分钟时按节点宽限触发", 45 * time.Minute, true, 10 * time.Minute, 45 * time.Minute, store.StateFiring, firing, false},
		{"恢复记在离线开始之后的巡检里（负时长）仍在窗口内", 2 * time.Minute, true, -5 * time.Second, 2 * time.Minute, store.StatePending, nil, true},
		{"从未恢复过按节点宽限", 2 * time.Minute, false, 0, 2 * time.Minute, store.StateFiring, firing, false},
		{"从未恢复过、未满节点宽限的 pending 不算抖动推迟", 2 * time.Minute, false, 0, time.Minute, store.StatePending, nil, false},
		// 未上报且未满 TTL 时 NextOffline 保持现状：这样的 pending 不做抖动抑制也不会触发（重启后头一个 TTL 即是此情形），
		// 宽限为 0（读侧取 TTL）时已离线时长虽不小于节点宽限，也不标。
		{"未满 TTL 保持现状的 pending 在窗口内不标", 0, true, 10 * time.Minute, 10 * time.Second, store.StatePending, nil, false},
		{"未满 TTL 保持现状的 pending 从未恢复过不标", 0, false, 0, 10 * time.Second, store.StatePending, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := Observation{Unseen: c.unseen, Grace: c.grace, TTL: ttl, Recovered: c.recovered, SinceRecovery: c.sinceRecovery}
			got, tr := NextOffline(store.StatePending, o)
			if got != c.want || !reflect.DeepEqual(tr, c.tr) {
				t.Fatalf("got %s %v, want %s %v", got, tr, c.want, c.tr)
			}
			if d := FlapDeferred(store.StatePending, o); d != c.deferred {
				t.Fatalf("FlapDeferred = %v, want %v", d, c.deferred)
			}
		})
	}
}

func flappingOf(e *Engine, r, n int64) bool {
	for _, s := range e.States() {
		if s.RuleID == r && s.NodeID == n {
			return s.Flapping
		}
	}
	return false
}

// flapFixture：节点宽限 60 秒的离线规则，节点先离线满宽限进入 firing，再上报恢复。返回规则与恢复时刻之后的夹具。
func flapFixture(t *testing.T) (*fixture, store.AlertRule) {
	t.Helper()
	f := newFixture(t)
	f.grace(t, f.ids[0], 60)
	r := offline()
	r.AllNodes, r.NodeIDs = false, f.ids[:1]
	r = f.rule(t, r)
	f.l.Observe(f.ids[0], "", &heronv1.Metrics{})
	f.clk.Advance(61 * time.Second)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	f.l.Observe(f.ids[0], "", &heronv1.Metrics{})
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateOK)
	return f, r
}

func transitions(t *testing.T, f *fixture) []store.Transition {
	t.Helper()
	var out []store.Transition
	for _, ev := range f.events(t) {
		out = append([]store.Transition{ev.Transition}, out...)
	}
	return out
}

// 恢复后 10 分钟再离线：到节点宽限仍 pending 并标出抖动，满 30 分钟才触发；恢复通知照发。恢复时刻落库，
// 之后写成 pending 也沿用它。
func TestReofflineWithinWindowWaitsForFlapGrace(t *testing.T) {
	f, r := flapFixture(t)
	recoveredAt := time.Unix(f.clk.Now().Unix(), 0).UTC()
	f.clk.Advance(10 * time.Minute)
	f.l.Observe(f.ids[0], "", &heronv1.Metrics{})
	f.sweep(t)
	f.clk.Advance(61 * time.Second)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StatePending)
	if !flappingOf(f.e, r.ID, f.ids[0]) {
		t.Fatal("pending held back only by the flap grace is not marked flapping")
	}
	rows, err := f.st.ListAlertStates(t.Context())
	must(t, err)
	if len(rows) != 1 || rows[0].State != store.StatePending || !rows[0].RecoveredAt.Equal(recoveredAt) {
		t.Fatalf("stored state = %+v, want pending carrying recovered_at %v", rows, recoveredAt)
	}
	f.clk.Advance(30*time.Minute - 61*time.Second - time.Second)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StatePending)
	f.clk.Advance(time.Second)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	if flappingOf(f.e, r.ID, f.ids[0]) {
		t.Fatal("firing state still marked flapping")
	}
	want := []store.Transition{store.TransitionFiring, store.TransitionRecovered, store.TransitionFiring}
	if got := transitions(t, f); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// 恢复后 61 分钟再离线：窗口已过，按节点宽限触发。
func TestReofflineAfterWindowUsesNodeGrace(t *testing.T) {
	f, r := flapFixture(t)
	f.clk.Advance(61 * time.Minute)
	f.l.Observe(f.ids[0], "", &heronv1.Metrics{})
	f.sweep(t)
	f.clk.Advance(61 * time.Second)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
}

// hub 重启不丢窗口：恢复时刻从库读回。夹具没有刷出分钟桶，库里没有最后上报，离线开始退回启动时刻（恢复后 5 分钟），
// 仍在窗口里；已离线时长照重启不变式从启动时刻起算。
func TestFlapWindowSurvivesRestart(t *testing.T) {
	f, r := flapFixture(t)
	f.clk.Advance(5 * time.Minute)
	f.restart(t)
	f.sweep(t)
	f.clk.Advance(61 * time.Second)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StatePending)
	if !flappingOf(f.e, r.ID, f.ids[0]) {
		t.Fatal("pending after restart is not marked flapping")
	}
	f.clk.Advance(30*time.Minute - 61*time.Second)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
}

// flushLive 走 hub 停机时的全量刷出路径（ingest.Service.RunFlusher 在 ctx 结束时 live.Drain 交给 store.WriteMinuteBatch），
// 把 live 当时的最后上报时刻写进 node.last_seen_at，并核对它确实落库。
func (f *fixture) flushLive(t *testing.T, node int64, want time.Time) {
	t.Helper()
	rejected, err := f.st.WriteMinuteBatch(t.Context(), f.l.Drain())
	must(t, err)
	if rejected != 0 {
		t.Fatalf("flush rejected %d rows", rejected)
	}
	if got := storedLastSeen(t, f, node); !got.Equal(want) {
		t.Fatalf("stored last_seen_at = %v, want %v", got, want)
	}
}

func storedLastSeen(t *testing.T, f *fixture, node int64) time.Time {
	t.Helper()
	nodes, err := f.st.ListNodes(t.Context())
	must(t, err)
	for _, n := range nodes {
		if n.ID == node {
			return n.LastSeenAt
		}
	}
	t.Fatalf("node %d not found", node)
	return time.Time{}
}

// 重启时仍在 pending 的离线，离线开始跨重启取落库的最后上报时刻，按它定下的抖动宽限重启前后不换。已离线时长照重启
// 不变式从启动时刻重新计时，所以重启的一组在启动后满 30 分钟才触发，不重启的对照组在离线开始后满 30 分钟触发；
// 重启后满节点宽限（60 秒）的同一时刻，两组都停在 pending 并标抖动。重启已过窗口结束的那一行：按启动时刻判窗口的
// 写法会把这次离线判到窗口外、换成节点宽限，在重启后 60 秒触发。
func TestFlapGraceSurvivesRestartDuringPending(t *testing.T) {
	cases := []struct {
		name               string
		lastReport, reboot time.Duration // 相对恢复时刻
	}{
		{"重启已过窗口结束", 50 * time.Minute, 65 * time.Minute},
		{"重启仍在窗口内", 10 * time.Minute, 20 * time.Minute},
	}
	type check struct {
		at       time.Duration // 相对恢复时刻
		state    store.AlertState
		flapping bool
	}
	for _, c := range cases {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restart=%v", c.name, restart), func(t *testing.T) {
				f, r := flapFixture(t)
				recoveredAt := f.clk.Now()
				f.clk.Advance(c.lastReport)
				f.l.Observe(f.ids[0], "", &heronv1.Metrics{})
				f.sweep(t)
				checks := []check{
					{c.lastReport + 61*time.Second, store.StatePending, true},
					{c.reboot, store.StatePending, true},
				}
				fireAt := c.lastReport + 30*time.Minute
				if restart {
					fireAt = c.reboot + 30*time.Minute
				}
				// 重启后未满 TTL 时 NextOffline 保持现状，不做抖动抑制也停在 pending，不标。
				after := []check{
					{c.reboot + 10*time.Second, store.StatePending, !restart},
					{c.reboot + 60*time.Second, store.StatePending, true},
					{fireAt - time.Second, store.StatePending, true},
					{fireAt, store.StateFiring, false},
				}
				run := func(checks []check) {
					t.Helper()
					for _, ck := range checks {
						f.clk.Advance(recoveredAt.Add(ck.at).Sub(f.clk.Now()))
						f.sweep(t)
						if got, flapping := stateOf(f.e, r.ID, f.ids[0]), flappingOf(f.e, r.ID, f.ids[0]); got != ck.state || flapping != ck.flapping {
							t.Fatalf("at R+%v: state=%s flapping=%v, want %s flapping=%v", ck.at, got, flapping, ck.state, ck.flapping)
						}
					}
				}
				run(checks)
				if restart {
					f.flushLive(t, f.ids[0], recoveredAt.Add(c.lastReport))
					f.restart(t)
				}
				run(after)
			})
		}
	}
}

// 库里没有最后上报（last_seen_at 为 NULL）时离线开始退回启动时刻。恢复记在启动前两小时，窗口已过，按节点宽限在
// 启动后 60 秒触发；把 NULL 读出的零值当离线开始会算出远早于恢复的离线开始，误判进窗口而停在 pending。
func TestOfflineStartWithoutStoredLastSeenFallsBackToBoot(t *testing.T) {
	f, r := flapFixture(t)
	f.clk.Advance(2 * time.Hour)
	f.restart(t)
	if seen := storedLastSeen(t, f, f.ids[0]); !seen.IsZero() {
		t.Fatalf("stored last_seen_at = %v, want NULL", seen)
	}
	f.clk.Advance(59 * time.Second)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StatePending)
	if flappingOf(f.e, r.ID, f.ids[0]) {
		t.Fatal("pending outside the window is marked flapping")
	}
	f.clk.Advance(time.Second)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
}

// 从未恢复过（recovered_at 为 NULL）的 pending 在重启后不标抖动：未满 TTL 时 NextOffline 保持现状，它停在 pending 是因为
// 重启重新计时，不做抖动抑制也不会触发。节点宽限为 0（读侧取 TTL），已离线时长从一开始就不小于节点宽限。
func TestNeverRecoveredPendingAfterRestartIsNotFlapping(t *testing.T) {
	f := newFixture(t)
	f.grace(t, f.ids[0], 60)
	r := offline()
	r.AllNodes, r.NodeIDs = false, f.ids[:1]
	r = f.rule(t, r)
	f.clk.Advance(30 * time.Second)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StatePending)
	f.grace(t, f.ids[0], 0)
	f.restart(t)
	rows, err := f.st.ListAlertStates(t.Context())
	must(t, err)
	if len(rows) != 1 || rows[0].State != store.StatePending || !rows[0].RecoveredAt.IsZero() {
		t.Fatalf("stored state = %+v, want pending without recovered_at", rows)
	}
	for _, want := range []store.AlertState{store.StatePending, store.StatePending, store.StateFiring} {
		f.clk.Advance(10 * time.Second)
		f.sweep(t)
		wantState(t, f.e, r.ID, f.ids[0], want)
		if flappingOf(f.e, r.ID, f.ids[0]) {
			t.Fatalf("never-recovered %s is marked flapping", want)
		}
	}
}

// 窗口按离线开始的时刻判定：恢复后 50 分钟开始的离线，到恢复后 61 分钟（已走出窗口、已满节点宽限）仍按抖动宽限
// 停在 pending，满 30 分钟才触发。按评估时刻判定的写法会在走出窗口的那一轮改回节点宽限而提前触发。
func TestFlapGraceFollowsTheOfflineStartNotTheEvaluationTime(t *testing.T) {
	f, r := flapFixture(t)
	f.clk.Advance(50 * time.Minute)
	f.l.Observe(f.ids[0], "", &heronv1.Metrics{})
	f.sweep(t)
	f.clk.Advance(11 * time.Minute)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StatePending)
	f.clk.Advance(19 * time.Minute)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
}

// 抖动抑制只属于离线规则：探测规则恢复时不记恢复时刻，状态行的 recovered_at 保持 NULL。
func TestProbeRecoveryDoesNotRecordRecoveredAt(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, f.ids[:1])
	ts := f.clk.Now().Unix() - 120
	r := f.rule(t, store.AlertRule{Name: "探测", Kind: store.KindProbe, Enabled: true, AllNodes: true, TaskID: task, Metric: store.MetricLossPct, Threshold: 20, ForMinutes: 1})
	f.minutes(t, task, f.ids[0], ts, metric.ProbeBucket{Sent: 1, Lost: 1}, metric.ProbeBucket{Sent: 1})
	must(t, f.e.EvaluateProbes(t.Context(), ts))
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	must(t, f.e.EvaluateProbes(t.Context(), ts+60))
	wantState(t, f.e, r.ID, f.ids[0], store.StateOK)
	rows, err := f.st.ListAlertStates(t.Context())
	must(t, err)
	if len(rows) != 1 || rows[0].State != store.StateOK || !rows[0].RecoveredAt.IsZero() {
		t.Fatalf("probe recovery stored %+v, want ok without recovered_at", rows)
	}
}
