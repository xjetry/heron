package alert

import (
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

func (f *fixture) silence(t *testing.T, s store.Silence) store.Silence {
	t.Helper()
	saved, err := f.e.SaveSilence(t.Context(), s)
	must(t, err)
	return saved
}

// fireOffline 把节点推进 firing 并返回它的触发事件。时钟推进一分钟：默认宽限是 TTL 的 30 秒下限。
// 规则覆盖多个节点时一次巡检会产生多条事件，按节点与转换在新增事件里找。
func (f *fixture) fireOffline(t *testing.T, r store.AlertRule, id int64) store.AlertEvent {
	t.Helper()
	before := len(f.events(t))
	f.clk.Advance(time.Minute)
	f.sweep(t)
	wantState(t, f.e, r.ID, id, store.StateFiring)
	return f.newTransition(t, before, id, store.TransitionFiring)
}

func (f *fixture) recoverOffline(t *testing.T, r store.AlertRule, id int64) store.AlertEvent {
	t.Helper()
	before := len(f.events(t))
	f.l.Observe(id, "", &heronv1.Metrics{})
	f.sweep(t)
	wantState(t, f.e, r.ID, id, store.StateOK)
	return f.newTransition(t, before, id, store.TransitionRecovered)
}

// newTransition 在比 before 新的事件（列表按时间倒序，新事件在前）里找 id 的 tr 转换。
func (f *fixture) newTransition(t *testing.T, before int, id int64, tr store.Transition) store.AlertEvent {
	t.Helper()
	events := f.events(t)
	for _, ev := range events[:len(events)-before] {
		if ev.NodeID == id && ev.Transition == tr {
			return ev
		}
	}
	t.Fatalf("no new %s event for node %d: %+v", tr, id, events)
	return store.AlertEvent{}
}

func (f *fixture) channelRule(t *testing.T) store.AlertRule {
	t.Helper()
	c, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "通知", Kind: store.ChannelWebhook, Config: `{"url":"https://example.invalid/hook"}`})
	must(t, err)
	r := offline()
	r.ChannelIDs = []int64{c.ID}
	return f.rule(t, r)
}

// nodeRule 是只覆盖第一个节点的 channelRule：单节点断言（sender 计数、事件条数）不受另一个节点干扰。
func (f *fixture) nodeRule(t *testing.T) store.AlertRule {
	t.Helper()
	r := f.channelRule(t)
	r.AllNodes, r.NodeIDs = false, f.ids[:1]
	return f.rule(t, r)
}

func (f *fixture) setMaintenance(t *testing.T, id int64, on bool) {
	t.Helper()
	name := "node1"
	if id == f.ids[1] {
		name = "node2"
	}
	_, err := f.st.UpdateNode(t.Context(), id, store.NodeEdit{Name: name, TrafficResetDay: 1, Maintenance: on})
	must(t, err)
}

// 窗口判定的唯一口径：DAILY 按 hub 时区的墙钟、起含止不含、允许跨午夜；ONCE 按 Unix 秒闭开区间。
func TestSilenceActive(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	at := func(day, hour, minute int) time.Time {
		return time.Date(2026, 9, day, hour, minute, 0, 0, loc)
	}
	daily := store.Silence{Kind: store.SilenceDaily, StartHHMM: "22:00", EndHHMM: "06:00"}
	for _, c := range []struct {
		name string
		s    store.Silence
		now  time.Time
		want bool
	}{
		{"跨午夜 23:30 生效", daily, at(24, 23, 30), true},
		{"跨午夜次日 05:30 生效", daily, at(25, 5, 30), true},
		{"跨午夜 07:00 不生效", daily, at(25, 7, 0), false},
		{"跨午夜 21:59 不生效", daily, at(24, 21, 59), false},
		{"起点含", daily, at(24, 22, 0), true},
		{"终点不含", daily, at(25, 6, 0), false},
		{"日内窗口 12:00 生效", store.Silence{Kind: store.SilenceDaily, StartHHMM: "09:00", EndHHMM: "17:00"}, at(24, 12, 0), true},
		{"日内窗口 08:59 不生效", store.Silence{Kind: store.SilenceDaily, StartHHMM: "09:00", EndHHMM: "17:00"}, at(24, 8, 59), false},
		{"日内窗口 17:00 不生效", store.Silence{Kind: store.SilenceDaily, StartHHMM: "09:00", EndHHMM: "17:00"}, at(24, 17, 0), false},
		{"一次窗口起点含", store.Silence{Kind: store.SilenceOnce, FromAt: 1000, UntilAt: 2000}, time.Unix(1000, 0), true},
		{"一次窗口内", store.Silence{Kind: store.SilenceOnce, FromAt: 1000, UntilAt: 2000}, time.Unix(1999, 0), true},
		{"一次窗口终点不含", store.Silence{Kind: store.SilenceOnce, FromAt: 1000, UntilAt: 2000}, time.Unix(2000, 0), false},
		{"一次窗口之前", store.Silence{Kind: store.SilenceOnce, FromAt: 1000, UntilAt: 2000}, time.Unix(999, 0), false},
	} {
		if got := SilenceActive(c.s, c.now, loc); got != c.want {
			t.Errorf("%s: SilenceActive = %v, want %v", c.name, got, c.want)
		}
	}
	// UTC 墙钟同一时刻按不同时区得出不同答案：判定只认 hub 的时区。
	night := time.Date(2026, 9, 24, 15, 30, 0, 0, time.UTC) // 上海 23:30，UTC 15:30
	if !SilenceActive(daily, night, loc) {
		t.Error("上海 23:30 应在 22:00–06:00 窗口内")
	}
	if SilenceActive(daily, night, time.UTC) {
		t.Error("UTC 15:30 不应在 22:00–06:00 窗口内")
	}
}

// 夹具时区是东八区：跨午夜窗口在 23:30 与次日 05:30 抑制，在 07:00 与 20:00 不抑制。
func TestDailySilenceAcrossMidnightSuppressesByHubWallClock(t *testing.T) {
	for _, c := range []struct {
		name     string
		wallUTC  time.Time // 触发评估所在分钟的起点；触发发生在一分钟后
		silenced bool
	}{
		{"23:30 生效", time.Date(2026, 9, 24, 15, 29, 0, 0, time.UTC), true},
		{"次日 05:30 生效", time.Date(2026, 9, 24, 21, 29, 0, 0, time.UTC), true},
		{"07:00 不生效", time.Date(2026, 9, 24, 22, 59, 0, 0, time.UTC), false},
		{"20:00 不生效", time.Date(2026, 9, 24, 11, 59, 0, 0, time.UTC), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			r := f.nodeRule(t)
			sender := &recorder{}
			f.e.SetSender(sender)
			f.silence(t, store.Silence{Name: "夜间维护", Enabled: true, AllNodes: true, Kind: store.SilenceDaily, StartHHMM: "22:00", EndHHMM: "06:00"})
			f.clk.SetWall(c.wallUTC)
			ev := f.fireOffline(t, r, f.ids[0])
			if ev.Silenced != c.silenced {
				t.Fatalf("event silenced=%v, want %v", ev.Silenced, c.silenced)
			}
			if c.silenced {
				if len(ev.Deliveries) != 0 || len(sender.events) != 0 {
					t.Fatalf("silenced firing delivered: %+v sent=%d", ev.Deliveries, len(sender.events))
				}
			} else if len(ev.Deliveries) != 1 || len(sender.events) != 1 {
				t.Fatalf("unsilenced firing did not deliver: %+v sent=%d", ev.Deliveries, len(sender.events))
			}
		})
	}
}

// 被静默的触发：事件照常落库（silenced=1）但没有投递行，状态行记 fired_silenced；覆盖外的节点不受影响。
func TestSilencedFiringRecordsEventWithoutDelivery(t *testing.T) {
	f := newFixture(t)
	r := f.channelRule(t)
	sender := &recorder{}
	f.e.SetSender(sender)
	f.silence(t, store.Silence{Name: "维护", Enabled: true, Kind: store.SilenceDaily, StartHHMM: "19:00", EndHHMM: "23:00", NodeIDs: f.ids[:1]})
	covered := f.fireOffline(t, r, f.ids[0])
	// 同一次巡检里覆盖外的节点也触发，它照常投递。
	uncovered := f.newTransition(t, 0, f.ids[1], store.TransitionFiring)
	if !covered.Silenced || len(covered.Deliveries) != 0 {
		t.Fatalf("covered firing=%+v", covered)
	}
	if uncovered.Silenced || len(uncovered.Deliveries) != 1 {
		t.Fatalf("uncovered firing=%+v", uncovered)
	}
	if len(sender.events) != 1 {
		t.Fatalf("sender got %d events, want only the uncovered one", len(sender.events))
	}
	for _, s := range f.e.States() {
		if s.RuleID == r.ID && s.NodeID == f.ids[0] && !s.FiredSilenced {
			t.Fatalf("state=%+v, want fired_silenced", s)
		}
	}
	rows, err := f.st.ListAlertStates(t.Context())
	must(t, err)
	for _, row := range rows {
		if row.RuleID == r.ID && row.NodeID == f.ids[0] && !row.FiredSilenced {
			t.Fatalf("persisted state=%+v, want fired_silenced", row)
		}
	}
	// 关闭的静默不抑制。
	f.silence(t, store.Silence{Name: "关闭的", Enabled: false, Kind: store.SilenceDaily, StartHHMM: "19:00", EndHHMM: "23:00", NodeIDs: f.ids[1:]})
	if ev := f.recoverOffline(t, r, f.ids[1]); ev.Silenced {
		t.Fatalf("uncovered recovery=%+v", ev)
	}
	f.clk.Advance(flapGrace + time.Second)
	if ev := f.fireOffline(t, r, f.ids[1]); ev.Silenced || len(ev.Deliveries) != 1 {
		t.Fatalf("disabled silence suppressed: %+v", ev)
	}
}

// 节点自己的维护开关与静默同义：进入 firing 不投递，恢复抄配对触发的值。
func TestNodeMaintenanceSilencesFiring(t *testing.T) {
	f := newFixture(t)
	r := f.nodeRule(t)
	sender := &recorder{}
	f.e.SetSender(sender)
	f.setMaintenance(t, f.ids[0], true)
	if ev := f.fireOffline(t, r, f.ids[0]); !ev.Silenced || len(ev.Deliveries) != 0 || len(sender.events) != 0 {
		t.Fatalf("maintenance firing=%+v sent=%d", ev, len(sender.events))
	}
	// 维护在 firing 中途解除：不补发触发，恢复仍不投递。
	f.setMaintenance(t, f.ids[0], false)
	f.clk.Advance(time.Minute)
	f.sweep(t)
	if len(f.events(t)) != 1 || len(sender.events) != 0 {
		t.Fatalf("maintenance lifted mid-firing backfilled: events=%d sent=%d", len(f.events(t)), len(sender.events))
	}
	if ev := f.recoverOffline(t, r, f.ids[0]); !ev.Silenced || len(ev.Deliveries) != 0 {
		t.Fatalf("recovery of a silenced firing=%+v", ev)
	}
}

// 恢复是否投递只看配对的触发是否投递过，与恢复时刻的静默无关：两个方向都钉死。
func TestRecoveryFollowsThePairedFiring(t *testing.T) {
	// 触发被静默，恢复时静默已结束：恢复仍不投递，recovered_at 照常写。
	f := newFixture(t)
	r := f.nodeRule(t)
	sender := &recorder{}
	f.e.SetSender(sender)
	si := f.silence(t, store.Silence{Name: "维护", Enabled: true, Kind: store.SilenceDaily, StartHHMM: "19:00", EndHHMM: "23:00", NodeIDs: f.ids[:1]})
	if ev := f.fireOffline(t, r, f.ids[0]); !ev.Silenced {
		t.Fatalf("firing=%+v", ev)
	}
	si.Enabled = false
	f.silence(t, si)
	ev := f.recoverOffline(t, r, f.ids[0])
	if !ev.Silenced || len(ev.Deliveries) != 0 || len(sender.events) != 0 {
		t.Fatalf("recovery after the silence ended=%+v sent=%d", ev, len(sender.events))
	}
	rows, err := f.st.ListAlertStates(t.Context())
	must(t, err)
	if len(rows) != 1 || rows[0].RecoveredAt.IsZero() || !rows[0].RecoveredAt.Equal(ev.At) || rows[0].FiredSilenced {
		t.Fatalf("state after recovery=%+v, want recovered_at=%s and fired_silenced cleared", rows, ev.At)
	}

	// 触发已投递，恢复时静默正在生效：恢复照常投递。
	f2 := newFixture(t)
	r2 := f2.nodeRule(t)
	sender2 := &recorder{}
	f2.e.SetSender(sender2)
	if ev := f2.fireOffline(t, r2, f2.ids[0]); ev.Silenced {
		t.Fatalf("firing=%+v", ev)
	}
	f2.silence(t, store.Silence{Name: "维护", Enabled: true, Kind: store.SilenceDaily, StartHHMM: "19:00", EndHHMM: "23:00", AllNodes: true})
	if ev := f2.recoverOffline(t, r2, f2.ids[0]); ev.Silenced || len(ev.Deliveries) != 1 {
		t.Fatalf("recovery during a silence=%+v, want delivered", ev)
	}
}

// 静默在 firing 中途结束不补发通知：状态没有转换就没有事件，队列也没有补投。
func TestSilenceEndingMidFiringDoesNotBackfill(t *testing.T) {
	f := newFixture(t)
	r := f.nodeRule(t)
	sender := &recorder{}
	f.e.SetSender(sender)
	f.silence(t, store.Silence{Name: "夜间维护", Enabled: true, Kind: store.SilenceDaily, StartHHMM: "19:00", EndHHMM: "23:00", AllNodes: true})
	if ev := f.fireOffline(t, r, f.ids[0]); !ev.Silenced {
		t.Fatalf("firing=%+v", ev)
	}
	// 墙钟跨过窗口结束（23:00 本地 = 15:00 UTC），节点仍离线：再巡检也只是 firing 保持，不产生任何事件与投递。
	f.clk.SetWall(time.Date(2026, 9, 24, 15, 30, 0, 0, time.UTC))
	f.sweep(t)
	f.clk.Advance(time.Hour)
	f.sweep(t)
	if len(f.events(t)) != 1 || len(sender.events) != 0 {
		t.Fatalf("silence end backfilled: events=%d sent=%d", len(f.events(t)), len(sender.events))
	}
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
}

// 到期提醒永远不被静默：维护中的节点、且在生效静默覆盖内，到期事件照常投递。
func TestExpiryRulesAreNeverSilenced(t *testing.T) {
	f := newFixture(t)
	c, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "通知", Kind: store.ChannelWebhook, Config: `{"url":"https://example.invalid/hook"}`})
	must(t, err)
	r := expiryRule()
	r.ChannelIDs = []int64{c.ID}
	r = f.rule(t, r)
	sender := &recorder{}
	f.e.SetSender(sender)
	f.silence(t, store.Silence{Name: "维护", Enabled: true, AllNodes: true, Kind: store.SilenceDaily, StartHHMM: "19:00", EndHHMM: "23:00"})
	f.setMaintenance(t, f.ids[0], true)
	f.billing(t, f.ids[0], store.Billing{ExpiresOn: "2026-09-28"})
	f.sweepExpiry(t)
	events := f.events(t)
	if len(events) != 1 || events[0].Silenced || len(events[0].Deliveries) != 1 || len(sender.events) != 1 {
		t.Fatalf("expiry firing under maintenance and silence=%+v sent=%d", events, len(sender.events))
	}
}

// 一次性窗口按 Unix 秒闭开区间抑制：起点含、终点不含，过期后不再抑制。
func TestOnceSilenceBoundsSuppression(t *testing.T) {
	start := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	from, until := start.Add(time.Hour).Unix(), start.Add(2*time.Hour).Unix()
	fire := func(t *testing.T, wall time.Time) store.AlertEvent {
		f := newFixture(t)
		r := f.channelRule(t)
		f.silence(t, store.Silence{Name: "窗口", Enabled: true, AllNodes: true, Kind: store.SilenceOnce, FromAt: from, UntilAt: until})
		f.clk.SetWall(wall)
		return f.fireOffline(t, r, f.ids[0])
	}
	if ev := fire(t, time.Unix(from, 0).Add(-2*time.Minute)); ev.Silenced {
		// 触发发生在窗口开始之前一分钟。
		t.Fatalf("firing before from_at silenced: %+v", ev)
	}
	if ev := fire(t, time.Unix(from, 0).Add(-time.Second)); !ev.Silenced {
		// 触发发生在一分钟后，即 from_at 恰好吃进窗口（起点含）。
		t.Fatalf("firing at from_at not silenced: %+v", ev)
	}
	if ev := fire(t, time.Unix(from, 0).Add(30*time.Minute)); !ev.Silenced {
		t.Fatalf("firing inside the window not silenced: %+v", ev)
	}
	if ev := fire(t, time.Unix(until, 0).Add(-time.Minute)); ev.Silenced {
		// 触发恰发生在 until，终点不含。
		t.Fatalf("firing at until_at silenced: %+v", ev)
	}
}
