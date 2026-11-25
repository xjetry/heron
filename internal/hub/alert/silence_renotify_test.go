package alert

import (
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
)

// nightSilence 是覆盖全部规则与节点的每日窗口 19:00–23:00（夹具时区东八区，即 UTC 11:00–15:00）；夹具时钟起点 UTC 12:00 在窗口内。
func nightSilence() store.Silence {
	return store.Silence{AllRules: true, Name: "夜间维护", Enabled: true, Kind: store.SilenceDaily, StartHHMM: "19:00", EndHHMM: "23:00", AllNodes: true}
}

func (f *fixture) stateRow(t *testing.T, ruleID, nodeID int64) store.StateRow {
	t.Helper()
	rows, err := f.st.ListAlertStates(t.Context())
	must(t, err)
	for _, row := range rows {
		if row.RuleID == ruleID && row.NodeID == nodeID {
			return row
		}
	}
	t.Fatalf("no persisted state for rule %d node %d: %+v", ruleID, nodeID, rows)
	return store.StateRow{}
}

// 静默在 firing 中途结束、节点仍离线：巡检追加一条当下时刻、未静默的 firing 事件并投递，静默期内落库的那条原始事件不变；
// 状态行的 fired_at 仍是最初进入 firing 的时刻，之后的恢复照常投递，持续时长从最初触发起算。补发只发一次。
func TestSilenceEndingMidFiringRenotifies(t *testing.T) {
	f := newFixture(t)
	r := f.nodeRule(t)
	sender := &recorder{}
	f.e.SetSender(sender)
	f.silence(t, nightSilence())
	original := f.fireOffline(t, r, f.ids[0])
	if !original.Silenced || len(original.Deliveries) != 0 {
		t.Fatalf("firing inside the window=%+v", original)
	}
	// 窗口内（UTC 13:01，当地 21:01）再巡检：仍在静默，不补发。
	f.clk.Advance(time.Hour)
	f.sweep(t)
	if n := len(f.events(t)); n != 1 || len(sender.events) != 0 {
		t.Fatalf("inside the window: events=%d sent=%d", n, len(sender.events))
	}
	// 推到 UTC 15:30（当地 23:30），窗口已结束，节点仍未上报。
	f.clk.Advance(2*time.Hour + 29*time.Minute)
	f.sweep(t)
	events := f.events(t)
	if len(events) != 2 {
		t.Fatalf("events after the window=%+v; want the original and one renotification", events)
	}
	renotify, kept := events[0], events[1]
	if renotify.Transition != store.TransitionFiring || renotify.Silenced || len(renotify.Deliveries) != 1 {
		t.Fatalf("renotification=%+v", renotify)
	}
	if !renotify.At.Equal(time.Date(2026, 9, 24, 15, 30, 0, 0, time.UTC)) {
		t.Fatalf("renotification at %v, want the sweep time", renotify.At)
	}
	if !strings.HasPrefix(renotify.Summary, original.Summary) || !strings.Contains(renotify.Summary, "静默结束时仍未恢复，已持续 3 小时 29 分钟") {
		t.Fatalf("renotification summary %q; want the original %q annotated with the elapsed 3h29m", renotify.Summary, original.Summary)
	}
	if renotify.Value != original.Value {
		t.Fatalf("renotification value=%v, want the original %v", renotify.Value, original.Value)
	}
	if kept.ID != original.ID || !kept.Silenced || len(kept.Deliveries) != 0 || kept.Summary != original.Summary || !kept.At.Equal(original.At) {
		t.Fatalf("original event changed: %+v, was %+v", kept, original)
	}
	if len(sender.events) != 1 || sender.events[0].ID != renotify.ID {
		t.Fatalf("sender got %+v; want only the renotification", sender.events)
	}
	// 库与引擎内存两面都钉：fired_at 不动、不再标记静默、冷却从补发时刻起算。
	row := f.stateRow(t, r.ID, f.ids[0])
	if row.State != store.StateFiring || !row.FiredAt.Equal(original.At) || row.FiredSilenced || row.FiredSummary != "" || !row.NotifiedAt.Equal(renotify.At) {
		t.Fatalf("persisted state=%+v; want firing since the original %v, no longer silenced, notified at %v", row, original.At, renotify.At)
	}
	for _, s := range f.e.States() {
		if s.RuleID == r.ID && s.NodeID == f.ids[0] && (!s.FiredAt.Equal(original.At) || s.FiredSilenced || s.FiredSummary != "" || !s.NotifiedAt.Equal(renotify.At)) {
			t.Fatalf("in-memory state=%+v", s)
		}
	}
	// 补发只发一次。
	f.clk.Advance(time.Minute)
	f.sweep(t)
	if n := len(f.events(t)); n != 2 || len(sender.events) != 1 {
		t.Fatalf("second sweep after the window: events=%d sent=%d", n, len(sender.events))
	}
	rec := f.recoverOffline(t, r, f.ids[0])
	if rec.Silenced || len(rec.Deliveries) != 1 || !strings.Contains(rec.Summary, "告警持续 3 小时 30 分钟") {
		t.Fatalf("recovery after the renotification=%+v; want delivered with the duration since the original firing", rec)
	}
}

// webhookChannel 建一个 webhook 渠道并返回它的 id。
func (f *fixture) webhookChannel(t *testing.T) int64 {
	t.Helper()
	c, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "通知", Kind: store.ChannelWebhook, Config: `{"url":"https://example.invalid/hook"}`})
	must(t, err)
	return c.ID
}

// 补发要求节点维护开关与全部静默窗口同时不再覆盖：任一仍在覆盖就不补发，二者都解除后的下一轮巡检补发。
func TestRenotificationWaitsForMaintenanceAndSilence(t *testing.T) {
	f := newFixture(t)
	r := f.nodeRule(t)
	sender := &recorder{}
	f.e.SetSender(sender)
	f.silence(t, nightSilence())
	f.setMaintenance(t, f.ids[0], true)
	if ev := f.fireOffline(t, r, f.ids[0]); !ev.Silenced {
		t.Fatalf("firing=%+v", ev)
	}
	quiet := func(when string) {
		t.Helper()
		f.sweep(t)
		if n := len(f.events(t)); n != 1 || len(sender.events) != 0 {
			t.Fatalf("%s: events=%d sent=%d; want no renotification", when, n, len(sender.events))
		}
	}
	// 维护关闭、静默窗口仍在。
	f.setMaintenance(t, f.ids[0], false)
	f.clk.Advance(time.Minute)
	quiet("maintenance off inside the window")
	// 窗口结束（UTC 15:30），维护又打开。
	f.setMaintenance(t, f.ids[0], true)
	f.clk.Advance(3*time.Hour + 28*time.Minute)
	quiet("window over under maintenance")
	f.setMaintenance(t, f.ids[0], false)
	f.sweep(t)
	events := f.events(t)
	if len(events) != 2 || events[0].Silenced || events[0].Transition != store.TransitionFiring || len(sender.events) != 1 {
		t.Fatalf("both lifted: events=%+v sent=%d; want one renotification", events, len(sender.events))
	}
	if rec := f.recoverOffline(t, r, f.ids[0]); rec.Silenced || len(rec.Deliveries) != 1 {
		t.Fatalf("recovery after the renotification=%+v; want delivered", rec)
	}
}

// 探测规则触发后节点彻底断联、不再有新的分钟样本：探测评估没有转换可做，补发仍由巡检按时发出，与评估无关。
func TestRenotificationWithoutNewSamples(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, f.ids[:1])
	r := f.rule(t, store.AlertRule{Name: "探测", Kind: store.KindProbe, Enabled: true, NodeIDs: f.ids[:1], TaskID: task, Metric: store.MetricLossPct, Threshold: 20, ForMinutes: 1, ChannelIDs: []int64{f.webhookChannel(t)}})
	sender := &recorder{}
	f.e.SetSender(sender)
	f.silence(t, nightSilence())
	minute := f.clk.Now().Unix() - 60
	f.minutes(t, task, f.ids[0], minute, metric.ProbeBucket{Sent: 10, Lost: 10})
	must(t, f.e.EvaluateProbes(t.Context(), minute))
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	original := f.events(t)[0]
	if !original.Silenced {
		t.Fatalf("firing=%+v", original)
	}
	// 窗口结束后的探测评估：缺样本既不恢复也不转换，不产生事件。
	f.clk.Advance(3*time.Hour + 30*time.Minute)
	later := f.clk.Now().Unix()/60*60 - 60
	must(t, f.e.EvaluateProbes(t.Context(), later))
	if n := len(f.events(t)); n != 1 {
		t.Fatalf("probe evaluation without samples produced events: %d", n)
	}
	f.sweep(t)
	events := f.events(t)
	if len(events) != 2 || events[0].Silenced || events[0].Transition != store.TransitionFiring || len(events[0].Deliveries) != 1 || len(sender.events) != 1 {
		t.Fatalf("sweep after the window: events=%+v sent=%d; want one renotification", events, len(sender.events))
	}
	if !strings.HasPrefix(events[0].Summary, original.Summary) || events[0].Value != 100 {
		t.Fatalf("renotification=%+v; want the original summary %q and loss 100", events[0], original.Summary)
	}
}

// 静默期内触发又恢复的短暂故障：触发与恢复都不投递，静默结束后也不补发任何一条。
func TestFaultRecoveredInsideSilenceIsNeverNotified(t *testing.T) {
	f := newFixture(t)
	r := f.nodeRule(t)
	sender := &recorder{}
	f.e.SetSender(sender)
	f.silence(t, nightSilence())
	f.fireOffline(t, r, f.ids[0])
	if rec := f.recoverOffline(t, r, f.ids[0]); !rec.Silenced || len(rec.Deliveries) != 0 {
		t.Fatalf("recovery inside the window=%+v", rec)
	}
	f.clk.Advance(3*time.Hour + 30*time.Minute)
	f.l.Observe(f.ids[0], "", &heronv1.Metrics{})
	f.sweep(t)
	if n := len(f.events(t)); n != 2 || len(sender.events) != 0 {
		t.Fatalf("after the window: events=%d sent=%d; want only the silenced pair", n, len(sender.events))
	}
}

// 冷却从实际发出的触发通知起算：静默期内触发并恢复、没有发出通知的那次不开始冷却，静默解除后冷却期内的新触发照常投递；
// 补发开始冷却，补发之后的冷却期内再触发停在 pending。固定基线 50 ms、冷却 600 秒的 rtt 相对判定。
func TestCooldownCountsFromDeliveredNotifications(t *testing.T) {
	f := newFixture(t)
	node := f.ids[0]
	task := f.task(t, f.ids[:1])
	rule := fixedRule(task, 1, 50)
	rule.ChannelIDs = []int64{f.webhookChannel(t)}
	r := f.rule(t, rule)
	f.silence(t, nightSilence())
	step := func(at time.Time, ms float64, want store.AlertState) {
		t.Helper()
		f.clk.SetWall(at)
		minute := at.Unix()/60*60 - 60
		f.minutes(t, task, node, minute, rttMinute(ms))
		must(t, f.e.EvaluateProbes(t.Context(), minute))
		wantState(t, f.e, r.ID, node, want)
	}
	utc := func(h, m int) time.Time { return time.Date(2026, 9, 24, h, m, 0, 0, time.UTC) }
	step(utc(14, 55), 200, store.StateFiring)
	step(utc(14, 56), 50, store.StateOK)
	// 窗口在 UTC 15:00 结束；距静默期内那次触发 5 分钟，在 600 秒之内，但它没有发出通知。
	step(utc(15, 0), 200, store.StateFiring)
	if ev := f.events(t)[0]; ev.Transition != store.TransitionFiring || ev.Silenced || len(ev.Deliveries) != 1 {
		t.Fatalf("first firing after the window=%+v; want delivered", ev)
	}

	// 另一轮：静默期内触发、窗口结束时仍越带，补发记下补发时刻作冷却起点。
	step(utc(15, 1), 50, store.StateOK)
	step(time.Date(2026, 9, 25, 14, 58, 0, 0, time.UTC), 200, store.StateFiring)
	f.clk.SetWall(time.Date(2026, 9, 25, 15, 0, 10, 0, time.UTC))
	f.sweep(t)
	if ev := f.events(t)[0]; ev.Silenced || ev.Transition != store.TransitionFiring || !strings.Contains(ev.Summary, "静默结束时仍未恢复") {
		t.Fatalf("renotification=%+v", ev)
	}
	step(time.Date(2026, 9, 25, 15, 2, 0, 0, time.UTC), 50, store.StateOK)
	step(time.Date(2026, 9, 25, 15, 3, 0, 0, time.UTC), 200, store.StatePending)
	step(time.Date(2026, 9, 25, 15, 11, 0, 0, time.UTC), 200, store.StateFiring)
}

// 被静默的触发连同摘要随状态行落库：hub 在 firing 中途重启，静默解除后补发的仍是原来的摘要与观测值。
func TestRenotificationSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	r := f.nodeRule(t)
	f.silence(t, nightSilence())
	original := f.fireOffline(t, r, f.ids[0])
	f.restart(t)
	sender := &recorder{}
	f.e.SetSender(sender)
	f.clk.Advance(3*time.Hour + 29*time.Minute)
	f.sweep(t)
	events := f.events(t)
	if len(events) != 2 || events[0].Silenced || len(sender.events) != 1 {
		t.Fatalf("after restart and the window: events=%+v sent=%d", events, len(sender.events))
	}
	if !strings.HasPrefix(events[0].Summary, original.Summary) || events[0].Value != original.Value || !strings.Contains(events[0].Summary, "已持续 3 小时 29 分钟") {
		t.Fatalf("renotification=%+v; want the original %q, value %v, elapsed since %v", events[0], original.Summary, original.Value, original.At)
	}
}
