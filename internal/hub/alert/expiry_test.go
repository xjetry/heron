package alert

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata" // 夏令时用例要真实时区库；嵌入的库让结论不随测试机的系统时区库变化。

	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

func expiryRule() store.AlertRule {
	return store.AlertRule{Name: "到期", Kind: store.KindExpiry, Enabled: true, AllNodes: true, DaysBefore: 7}
}

func date(s string) time.Time {
	d, err := ParseDate(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestFiveYearRenewal(t *testing.T) {
	for _, c := range []struct{ from, today, want string }{
		{"2024-02-29", "2026-01-01", "2029-02-28"},
		{"2000-01-31", "2026-01-01", "2030-01-31"},
	} {
		got, ok := renewedExpiry(store.Billing{Cycle: "quinquennial", ExpiresOn: c.from, AutoRenew: true}, date(c.today))
		if !ok || got != c.want {
			t.Fatalf("five-year renewal %s -> %q,%v; want %s", c.from, got, ok, c.want)
		}
	}
}

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// "合法日期"的口径：四位年、两位月日、日子真实存在，前后不带任何字符。
func TestParseDateAcceptsOnlyRealYYYYMMDD(t *testing.T) {
	for _, ok := range []string{"2026-02-28", "2028-02-29", "0000-01-01", "9999-12-31"} {
		if _, err := ParseDate(ok); err != nil {
			t.Errorf("ParseDate(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "2026-02-29", "2026-02-30", "2026-13-01", "2026-1-05", "2026-01-5", "20260105", "2026/01/05",
		" 2026-01-05", "2026-01-05 ", "2026-01-05T00:00:00Z", "10000-01-01", "２０２６-01-05"} {
		if _, err := ParseDate(bad); err == nil {
			t.Errorf("ParseDate(%q) accepted", bad)
		}
	}
}

// 同一时刻在不同时区是不同的日历日：UTC 16:30 在东八区已是次日。
func TestTodayTakesTheCalendarDayInTheZone(t *testing.T) {
	now := time.Date(2026, 9, 24, 16, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		loc  *time.Location
		want string
	}{
		{time.UTC, "2026-09-24"},
		{time.FixedZone("UTC+8", 8*3600), "2026-09-25"},
		{zone(t, "America/Los_Angeles"), "2026-09-24"},
	} {
		if got := Today(now, c.loc); !got.Equal(date(c.want)) {
			t.Errorf("Today in %s = %s, want %s", c.loc, got.Format(time.DateOnly), c.want)
		}
	}
}

// 剩余天数是日历日之差：跨夏令时不差一天，跨几千年也不因 time.Duration 饱和而算错。
func TestDaysLeftCountsCalendarDays(t *testing.T) {
	today := date("2026-09-27")
	for _, c := range []struct {
		expiresOn string
		want      int
	}{
		{"2026-10-01", 4},
		{"2026-09-27", 0},
		{"2026-09-20", -7},
		{"2027-03-28", 182}, // 跨过北半球夏令时的开始与结束
		{"9999-12-31", 2912173},
		{"0000-01-01", -740251},
	} {
		got, ok := DaysLeft(c.expiresOn, today)
		if !ok || got != c.want {
			t.Errorf("DaysLeft(%s) = %d %v, want %d", c.expiresOn, got, ok, c.want)
		}
	}
	for _, none := range []string{"", "2026-02-30"} {
		if got, ok := DaysLeft(none, today); ok {
			t.Errorf("DaysLeft(%q) = %d, want none", none, got)
		}
	}
}

func TestCycleMonthsCoversEveryStoredCycle(t *testing.T) {
	want := map[store.BillingCycle]int{store.CycleMonthly: 1, store.CycleQuarterly: 3, store.CycleSemiannual: 6,
		store.CycleYearly: 12, store.CycleBiennial: 24, store.CycleTriennial: 36, store.CycleQuinquennial: 60}
	cycles := store.BillingCycles()
	if len(cycles) != len(want) {
		t.Fatalf("store.BillingCycles() = %v, want the %d cycles listed beside cycleMonths", cycles, len(want))
	}
	for _, c := range cycles {
		if got, ok := cycleMonths(c); !ok || got != want[c] {
			t.Errorf("cycleMonths(%s) = %d %v, want %d", c, got, ok, want[c])
		}
	}
	for _, c := range []store.BillingCycle{store.CycleNone, "weekly"} {
		if got, ok := cycleMonths(c); ok {
			t.Errorf("cycleMonths(%q) = %d, want no renewal", c, got)
		}
	}
}

// 推后到不早于今天为止；每一步从上一步的结果推后，钳到月末之后不回弹。
func TestRenewedExpiry(t *testing.T) {
	monthly := store.Billing{Cycle: store.CycleMonthly, AutoRenew: true}
	with := func(b store.Billing, expiresOn string) store.Billing { b.ExpiresOn = expiresOn; return b }
	for _, c := range []struct {
		name  string
		b     store.Billing
		today string
		want  string
	}{
		{"one month", with(monthly, "2026-09-10"), "2026-09-24", "2026-10-10"},
		{"several months, clamped then drifting", with(monthly, "2026-01-31"), "2026-04-01", "2026-04-28"},
		{"leap February", with(monthly, "2028-01-31"), "2028-02-15", "2028-02-29"},
		{"yearly from February 29", store.Billing{Cycle: store.CycleYearly, AutoRenew: true, ExpiresOn: "2024-02-29"}, "2025-03-01", "2026-02-28"},
		{"quarterly lands on today", store.Billing{Cycle: store.CycleQuarterly, AutoRenew: true, ExpiresOn: "2026-06-24"}, "2026-09-24", "2026-09-24"},
		{"triennial", store.Billing{Cycle: store.CycleTriennial, AutoRenew: true, ExpiresOn: "2020-01-15"}, "2026-09-24", "2029-01-15"},
		{"from year 0", with(monthly, "0000-01-31"), "0000-03-01", "0000-03-29"},
	} {
		got, ok := renewedExpiry(c.b, date(c.today))
		if !ok || got != c.want {
			t.Errorf("%s: renewedExpiry = %q %v, want %q", c.name, got, ok, c.want)
		}
	}
	for _, c := range []struct {
		name string
		b    store.Billing
	}{
		{"auto renew off", store.Billing{Cycle: store.CycleMonthly, ExpiresOn: "2026-09-10"}},
		{"no cycle", store.Billing{AutoRenew: true, ExpiresOn: "2026-09-10"}},
		{"unknown cycle", store.Billing{Cycle: "weekly", AutoRenew: true, ExpiresOn: "2026-09-10"}},
		{"no date", with(monthly, "")},
		{"unreadable date", with(monthly, "2026-09-31")},
		{"due today", with(monthly, "2026-09-24")},
		{"in the future", with(monthly, "2026-10-01")},
	} {
		if got, ok := renewedExpiry(c.b, date("2026-09-24")); ok {
			t.Errorf("%s: renewed to %s", c.name, got)
		}
	}
}

// "已续费"至少推后一个周期，再按自动续期的规则步进到不早于今天；不看自动续期开关。
func TestRenewedExpiryForManualRenewal(t *testing.T) {
	monthly := store.Billing{Cycle: store.CycleMonthly}
	with := func(b store.Billing, expiresOn string) store.Billing { b.ExpiresOn = expiresOn; return b }
	for _, c := range []struct {
		name  string
		b     store.Billing
		today string
		want  string
	}{
		{"in the future: exactly one cycle", with(monthly, "2026-10-10"), "2026-09-24", "2026-11-10"},
		{"due today: one cycle", with(monthly, "2026-09-24"), "2026-09-24", "2026-10-24"},
		{"in the future, clamped to month end", with(monthly, "2026-01-31"), "2026-01-20", "2026-02-28"},
		{"expired: keeps the billing day", with(monthly, "2026-09-10"), "2026-09-24", "2026-10-10"},
		{"expired several cycles: clamped then drifting", with(monthly, "2026-01-31"), "2026-04-01", "2026-04-28"},
		{"expired, a step lands on today", store.Billing{Cycle: store.CycleQuarterly, ExpiresOn: "2026-06-24"}, "2026-09-24", "2026-09-24"},
		{"auto renew on does not matter", store.Billing{Cycle: store.CycleYearly, AutoRenew: true, ExpiresOn: "2026-12-01"}, "2026-09-24", "2027-12-01"},
	} {
		got, err := RenewedExpiry(c.b, date(c.today))
		if err != nil || got != c.want {
			t.Errorf("%s: RenewedExpiry = %q %v, want %q", c.name, got, err, c.want)
		}
	}
	for _, c := range []struct {
		name string
		b    store.Billing
		want error
	}{
		{"no cycle", store.Billing{ExpiresOn: "2026-09-10"}, ErrRenewNoCycle},
		{"unknown cycle", store.Billing{Cycle: "weekly", ExpiresOn: "2026-09-10"}, ErrRenewNoCycle},
		{"no date", with(monthly, ""), ErrRenewNoExpiry},
	} {
		if got, err := RenewedExpiry(c.b, date("2026-09-24")); !errors.Is(err, c.want) {
			t.Errorf("%s: RenewedExpiry = %q %v, want %v", c.name, got, err, c.want)
		}
	}
	if got, err := RenewedExpiry(with(monthly, "2026-09-31"), date("2026-09-24")); err == nil || errors.Is(err, ErrRenewNoExpiry) {
		t.Errorf("unreadable date: RenewedExpiry = %q %v, want a parse error", got, err)
	}
}

// 已过期的节点手动续期与自动续期落在同一天：两条路径共用一套步进，不是两份各自正确的规则。逐个周期、跨月末与闰年
// 枚举到期日，凡早于今天的都比对。
func TestManualRenewalMatchesAutoRenewalWhenExpired(t *testing.T) {
	today := date("2028-03-01")
	for _, cycle := range store.BillingCycles() {
		for d := date("2023-01-28"); d.Before(today); d = d.AddDate(0, 0, 1) {
			b := store.Billing{Cycle: cycle, ExpiresOn: d.Format(time.DateOnly), AutoRenew: true}
			auto, ok := renewedExpiry(b, today)
			manual, err := RenewedExpiry(b, today)
			if !ok || err != nil || auto != manual {
				t.Fatalf("%s from %s: auto %q %v, manual %q %v", cycle, b.ExpiresOn, auto, ok, manual, err)
			}
		}
	}
}

// 下一个日界是本地日期变化的第一个时刻：严格晚于 now，本地日期是明天，再早 1 纳秒还是今天。
// 夏令时在零点开始的时区里零点不存在，新的一天从 01:00 开始。go1.27.1 的 time.Date 对不存在的零点给出的时刻两个方向
// 都有：Santiago、Havana 往回给前一天的 23:00，本地日期没变，要取 ZoneBounds 的 end；Cairo、Beirut 往前给新一天的
// 01:00，本地日期已变，它本身就是答案。两类各有用例，只认其中一类的写法会在另一类上定错日界。
func TestNextDayStartIsTheFirstInstantOfTheNextLocalDay(t *testing.T) {
	for _, c := range []struct {
		loc  *time.Location
		now  string // loc 里的本地时刻
		want string // loc 里的本地时刻与偏移
	}{
		{time.UTC, "2026-09-27T00:00:00", "2026-09-28T00:00:00Z"},
		{time.FixedZone("UTC+8", 8*3600), "2026-09-24T20:00:00", "2026-09-25T00:00:00+08:00"},
		{zone(t, "America/New_York"), "2026-03-07T22:00:00", "2026-03-08T00:00:00-05:00"},
		{zone(t, "America/Santiago"), "2026-09-05T22:00:00", "2026-09-06T01:00:00-03:00"},
		{zone(t, "America/Santiago"), "2026-04-04T22:00:00", "2026-04-05T00:00:00-04:00"},
		{zone(t, "America/Havana"), "2026-03-07T22:00:00", "2026-03-08T01:00:00-04:00"},
		{zone(t, "America/Havana"), "2026-10-31T22:00:00", "2026-11-01T00:00:00-04:00"},
		{zone(t, "Africa/Cairo"), "2026-04-23T22:00:00", "2026-04-24T01:00:00+03:00"},
		{zone(t, "Asia/Beirut"), "2026-03-28T22:00:00", "2026-03-29T01:00:00+03:00"},
	} {
		now, err := time.ParseInLocation("2006-01-02T15:04:05", c.now, c.loc)
		if err != nil {
			t.Fatal(err)
		}
		got := nextDayStart(now, c.loc)
		if got.Format(time.RFC3339) != c.want {
			t.Errorf("%s from %s: next day starts %s, want %s", c.loc, c.now, got.Format(time.RFC3339), c.want)
		}
		today, tomorrow := Today(now, c.loc), Today(now, c.loc).AddDate(0, 0, 1)
		if !got.After(now) || !Today(got, c.loc).Equal(tomorrow) || !Today(got.Add(-time.Nanosecond), c.loc).Equal(today) {
			t.Errorf("%s from %s: %s is not the first instant of the next local day", c.loc, c.now, got.Format(time.RFC3339Nano))
		}
	}
}

func TestNextExpiry(t *testing.T) {
	firing, recoveredTr := store.TransitionFiring, store.TransitionRecovered
	for _, c := range []struct {
		name   string
		cur    store.AlertState
		o      ExpiryObservation
		want   store.AlertState
		wantTr *store.Transition
	}{
		{"no date, never fired", store.StateOK, ExpiryObservation{DaysBefore: 7}, store.StateOK, nil},
		{"date cleared while firing", store.StateFiring, ExpiryObservation{DaysBefore: 7}, store.StateOK, &recoveredTr},
		{"outside the window", store.StateOK, ExpiryObservation{HasExpiry: true, DaysLeft: 8, DaysBefore: 7}, store.StateOK, nil},
		{"enters the window on its edge", store.StateOK, ExpiryObservation{HasExpiry: true, DaysLeft: 7, DaysBefore: 7}, store.StateFiring, &firing},
		{"already expired", store.StateOK, ExpiryObservation{HasExpiry: true, DaysLeft: -3, DaysBefore: 7}, store.StateFiring, &firing},
		{"stays firing", store.StateFiring, ExpiryObservation{HasExpiry: true, DaysLeft: -1, DaysBefore: 7}, store.StateFiring, nil},
		{"renewed out of the window", store.StateFiring, ExpiryObservation{HasExpiry: true, DaysLeft: 8, DaysBefore: 7}, store.StateOK, &recoveredTr},
		{"no pending stage", store.StatePending, ExpiryObservation{HasExpiry: true, DaysLeft: 1, DaysBefore: 7}, store.StateFiring, &firing},
	} {
		got, tr := NextExpiry(c.cur, c.o)
		if got != c.want || (tr == nil) != (c.wantTr == nil) || (tr != nil && *tr != *c.wantTr) {
			t.Errorf("%s: NextExpiry = %s %v, want %s %v", c.name, got, tr, c.want, c.wantTr)
		}
	}
}

func (f *fixture) billing(t *testing.T, id int64, b store.Billing) {
	t.Helper()
	_, err := f.st.UpdateNode(t.Context(), id, store.NodeEdit{Name: fmt.Sprintf("node%d", id), TrafficResetDay: 1, Billing: b})
	must(t, err)
}

func (f *fixture) expiresOn(t *testing.T, id int64) string {
	t.Helper()
	n, err := f.st.GetNode(t.Context(), id)
	must(t, err)
	return n.Billing.ExpiresOn
}

func (f *fixture) sweepExpiry(t *testing.T) { t.Helper(); must(t, f.e.SweepExpiry(t.Context())) }

// 夹具的今天是东八区的 2026-09-24。触发与恢复的文案按 expirySummary 的句子，没有到期日的节点不产生状态。
func TestSweepExpiryFiresAndRecoversWithSpecSummaries(t *testing.T) {
	f := newFixture(t)
	node1, node2 := f.ids[0], f.ids[1]
	r := f.rule(t, expiryRule())
	for _, step := range []struct {
		expiresOn string
		state     store.AlertState
		summary   string
		value     float64
	}{
		{"2026-09-28", store.StateFiring, "节点 node1 将于 2026-09-28 到期（剩 4 天，规则 到期）", 4},
		{"2026-11-01", store.StateOK, "节点 node1 到期日已更新为 2026-11-01（规则 到期），告警持续 不到 1 分钟", 38},
		{"2026-09-17", store.StateFiring, "节点 node1 已于 2026-09-17 到期（已过期 7 天，规则 到期）", -7},
		{"", store.StateOK, "节点 node1 已清除到期日（规则 到期），告警持续 不到 1 分钟", 0},
	} {
		f.billing(t, node1, store.Billing{ExpiresOn: step.expiresOn})
		before := len(f.events(t))
		f.sweepExpiry(t)
		f.sweepExpiry(t)
		events := f.events(t)
		if len(events) != before+1 {
			t.Fatalf("expires_on %q: %d new events, want exactly 1 across two sweeps", step.expiresOn, len(events)-before)
		}
		if ev := events[0]; ev.NodeID != node1 || ev.Summary != step.summary || ev.Value != step.value {
			t.Fatalf("expires_on %q: event %+v, want summary %q value %v", step.expiresOn, ev, step.summary, step.value)
		}
		wantState(t, f.e, r.ID, node1, step.state)
		wantState(t, f.e, r.ID, node2, "")
	}
}

// 今天取 hub 时区的日历日：UTC 15:59 与 16:00 之间东八区跨过零点，剩余天数从 8 变成 7，规则在这一刻触发。
func TestSweepExpiryCountsDaysInTheHubZone(t *testing.T) {
	f := newFixture(t)
	r := f.rule(t, expiryRule())
	f.billing(t, f.ids[0], store.Billing{ExpiresOn: "2026-10-02"})
	f.clk.SetWall(time.Date(2026, 9, 24, 15, 59, 0, 0, time.UTC))
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], "")
	f.clk.SetWall(time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC))
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
}

// 同一次扫描里先推后、再评估：已触发的告警随续期直接恢复，推后的日期落库并记一行日志。
func TestSweepExpiryRenewsBeforeEvaluating(t *testing.T) {
	f := newFixture(t)
	var logs bytes.Buffer
	f.log = slog.New(slog.NewTextHandler(&logs, nil))
	f.restart(t)
	r := f.rule(t, expiryRule())
	f.billing(t, f.ids[0], store.Billing{Cycle: store.CycleMonthly, ExpiresOn: "2026-09-20"})
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	f.billing(t, f.ids[0], store.Billing{Cycle: store.CycleMonthly, ExpiresOn: "2026-09-20", AutoRenew: true})
	f.sweepExpiry(t)
	if got := f.expiresOn(t, f.ids[0]); got != "2026-10-20" {
		t.Fatalf("expires_on after renewal = %s, want 2026-10-20", got)
	}
	events := f.events(t)
	if len(events) != 2 || events[0].Transition != store.TransitionRecovered || events[0].Summary != "节点 node1 到期日已更新为 2026-10-20（规则 到期），告警持续 不到 1 分钟" {
		t.Fatalf("events after renewal: %+v", events)
	}
	wantState(t, f.e, r.ID, f.ids[0], store.StateOK)
	if !strings.Contains(logs.String(), `msg="node expiry renewed" node_id=1 node=node1 cycle=monthly from=2026-09-20 to=2026-10-20`) {
		t.Fatalf("renewal log line missing:\n%s", logs.String())
	}
}

// 周期为空或读不懂时扫描自己不推后，不依赖保存入口拦下这种组合。
func TestSweepExpiryDoesNotRenewWithoutAUsableCycle(t *testing.T) {
	f := newFixture(t)
	for _, cycle := range []store.BillingCycle{store.CycleNone, "weekly"} {
		f.billing(t, f.ids[0], store.Billing{Cycle: cycle, ExpiresOn: "2026-08-31", AutoRenew: true})
		f.sweepExpiry(t)
		if got := f.expiresOn(t, f.ids[0]); got != "2026-08-31" {
			t.Fatalf("cycle %q renewed to %s", cycle, got)
		}
	}
}

// 库里读不懂的到期日（只有手改会产生）既不触发也不恢复：已触发的状态原样保留，开着自动续期也不推后，每次扫描对
// 每个这样的节点记一行 Warn，不去重：扫描两次就是两行。
func TestSweepExpirySkipsUnreadableDates(t *testing.T) {
	f := newFixture(t)
	var logs bytes.Buffer
	f.log = slog.New(slog.NewTextHandler(&logs, nil))
	f.restart(t)
	r := f.rule(t, expiryRule())
	f.billing(t, f.ids[0], store.Billing{ExpiresOn: "2026-09-25"})
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	f.billing(t, f.ids[0], store.Billing{Cycle: store.CycleMonthly, AutoRenew: true, ExpiresOn: "2026-09-31"})
	f.billing(t, f.ids[1], store.Billing{ExpiresOn: "2026/09/25"})
	f.sweepExpiry(t)
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	wantState(t, f.e, r.ID, f.ids[1], "")
	if events := f.events(t); len(events) != 1 {
		t.Fatalf("events = %+v, want only the first firing", events)
	}
	if got := f.expiresOn(t, f.ids[0]); got != "2026-09-31" {
		t.Fatalf("unreadable date renewed to %s", got)
	}
	for _, want := range []string{"node_id=1 expires_on=2026-09-31", "node_id=2 expires_on=2026/09/25"} {
		line := `level=WARN msg="node expires_on is not a YYYY-MM-DD date; expiry rules skip this node" ` + want
		if n := strings.Count(logs.String(), line); n != 2 {
			t.Fatalf("%d lines of %q, want 2:\n%s", n, line, logs.String())
		}
	}
}

// 作用域外的节点不评估；停用的规则不评估。
func TestSweepExpiryHonoursScopeAndEnabled(t *testing.T) {
	f := newFixture(t)
	scoped := expiryRule()
	scoped.AllNodes, scoped.NodeIDs = false, []int64{f.ids[1]}
	scoped = f.rule(t, scoped)
	disabled := expiryRule()
	disabled.Enabled = false
	disabled = f.rule(t, disabled)
	f.billing(t, f.ids[0], store.Billing{ExpiresOn: "2026-09-25"})
	f.sweepExpiry(t)
	wantState(t, f.e, scoped.ID, f.ids[0], "")
	wantState(t, f.e, disabled.ID, f.ids[0], "")
	if events := f.events(t); len(events) != 0 {
		t.Fatalf("events = %+v", events)
	}
}

// 另一个进程删掉的节点（heron-hub node delete 删了库里的状态行，没经过 Forget）不再是候选，内存里的状态随扫描清掉。
func TestSweepExpiryDropsStatesOfNodesDeletedElsewhere(t *testing.T) {
	f := newFixture(t)
	r := f.rule(t, expiryRule())
	f.billing(t, f.ids[0], store.Billing{ExpiresOn: "2026-09-25"})
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	must(t, f.st.DeleteNode(t.Context(), f.ids[0]))
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], "")
}

// 保存启用的到期规则即评估一次：不等下一个日界。停用的规则保存后没有状态。
func TestSaveRuleEvaluatesEnabledExpiryRules(t *testing.T) {
	f := newFixture(t)
	f.billing(t, f.ids[0], store.Billing{ExpiresOn: "2026-09-27"})
	disabled := expiryRule()
	disabled.Enabled = false
	disabled = f.rule(t, disabled)
	wantState(t, f.e, disabled.ID, f.ids[0], "")
	enabled := f.rule(t, expiryRule())
	wantState(t, f.e, enabled.ID, f.ids[0], store.StateFiring)
	enabled.DaysBefore = 2
	f.rule(t, enabled)
	wantState(t, f.e, enabled.ID, f.ids[0], store.StateOK)
}

// 恢复文案按离开窗口的原因。日期没变、只是提前天数调小：已不在提醒窗口内，第一次恢复前重启过一次（日期从库里读回），
// 第二次没有（日期来自内存）。firing 期间改过日期再调小提前天数：日期与触发时不同，写到期日已更新。每一步保存规则
// 之前先照常扫描一次：留在 firing 的扫描不改记下的日期。事件从这次扫描之前数起，所以重启之后的第一次扫描也在断言
// 之内：状态由 Load 从库里读回，留在 firing 不再发第二条触发。
func TestSweepExpiryRecoverySummaryFollowsTheReason(t *testing.T) {
	f := newFixture(t)
	node1 := f.ids[0]
	f.billing(t, node1, store.Billing{ExpiresOn: "2026-09-27"})
	r := f.rule(t, expiryRule())
	f.restart(t)
	for _, step := range []struct {
		expiresOn  string
		daysBefore int
		state      store.AlertState
		summary    string
		value      float64
	}{
		{"2026-09-27", 2, store.StateOK, "节点 node1 已不在提醒窗口内（规则 到期），告警持续 不到 1 分钟", 3},
		{"2026-09-27", 7, store.StateFiring, "节点 node1 将于 2026-09-27 到期（剩 3 天，规则 到期）", 3},
		{"2026-09-27", 2, store.StateOK, "节点 node1 已不在提醒窗口内（规则 到期），告警持续 不到 1 分钟", 3},
		{"2026-09-27", 7, store.StateFiring, "节点 node1 将于 2026-09-27 到期（剩 3 天，规则 到期）", 3},
		{"2026-09-29", 7, store.StateFiring, "", 0},
		{"2026-09-29", 4, store.StateOK, "节点 node1 到期日已更新为 2026-09-29（规则 到期），告警持续 不到 1 分钟", 5},
	} {
		before := len(f.events(t))
		f.billing(t, node1, store.Billing{ExpiresOn: step.expiresOn})
		f.sweepExpiry(t)
		r.DaysBefore = step.daysBefore
		r = f.rule(t, r)
		events := f.events(t)
		switch {
		case step.summary == "" && len(events) != before:
			t.Fatalf("%s with days_before %d: %d new events, want none", step.expiresOn, step.daysBefore, len(events)-before)
		case step.summary != "" && (len(events) != before+1 || events[0].Summary != step.summary || events[0].Value != step.value):
			t.Fatalf("%s with days_before %d: events %+v, want one new event %q value %v", step.expiresOn, step.daysBefore, events, step.summary, step.value)
		}
		wantState(t, f.e, r.ID, node1, step.state)
	}
}

// 一条规则对一个节点只提醒一次：从"将于"走到到期当天、再走到"已于"都不发第二条。到期当天写"剩 0 天"。
func TestSweepExpiryNotifiesOncePerEntry(t *testing.T) {
	f := newFixture(t)
	f.rule(t, expiryRule())
	f.billing(t, f.ids[0], store.Billing{ExpiresOn: "2026-09-25"})
	f.billing(t, f.ids[1], store.Billing{ExpiresOn: "2026-09-24"})
	for day := range 4 {
		f.clk.SetWall(time.Date(2026, 9, 24+day, 12, 0, 0, 0, time.UTC))
		f.sweepExpiry(t)
	}
	var got []string
	for _, ev := range f.events(t) {
		got = append(got, ev.Summary)
	}
	slices.Sort(got)
	want := []string{"节点 node1 将于 2026-09-25 到期（剩 1 天，规则 到期）", "节点 node2 将于 2026-09-24 到期（剩 0 天，规则 到期）"}
	if !slices.Equal(got, want) {
		t.Fatalf("summaries over four days = %q, want %q", got, want)
	}
}

// 循环一启动就扫描一次：停机跨过的日界由它补上；取消后返回。
func TestRunExpirySweepSweepsOnStartAndStops(t *testing.T) {
	f := newFixture(t)
	f.billing(t, f.ids[0], store.Billing{Cycle: store.CycleMonthly, ExpiresOn: "2026-09-01", AutoRenew: true})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); f.e.RunExpirySweep(ctx) }()
	testwait.Until(t, 10*time.Millisecond, func() bool { return f.expiresOn(t, f.ids[0]) == "2026-10-01" },
		"expires_on = %s, want 2026-10-01 after the startup sweep", testwait.When(func() string { return f.expiresOn(t, f.ids[0]) }))
	cancel()
	select {
	case <-done:
	case <-time.After(testwait.Bound):
		t.Fatal("RunExpirySweep did not return after cancel")
	}
}

// 续期的条件更新没有写入，说明快照之后这一行变了（计费被改过，或节点被删除）：本轮不按过期的快照评估它，不触发
// 也不推后；库里的值与快照一致之后照常续期。触发器让续期的 UPDATE 落空，对 RenewExpiry 的效果与"扫描读快照之后、写回之前有人改了计费"
// 相同。
func TestSweepExpirySkipsANodeWhoseSnapshotIsStale(t *testing.T) {
	f := newFixture(t)
	r := f.rule(t, expiryRule())
	f.billing(t, f.ids[0], store.Billing{Cycle: store.CycleMonthly, ExpiresOn: "2026-09-20", AutoRenew: true})
	db, err := sql.Open("sqlite", f.path)
	must(t, err)
	defer db.Close()
	_, err = db.ExecContext(t.Context(), "CREATE TRIGGER stale BEFORE UPDATE OF expires_on ON node BEGIN SELECT RAISE(IGNORE); END")
	must(t, err)
	f.sweepExpiry(t)
	if events := f.events(t); len(events) != 0 {
		t.Fatalf("a node with a stale snapshot was evaluated: %+v", events)
	}
	wantState(t, f.e, r.ID, f.ids[0], "")
	_, err = db.ExecContext(t.Context(), "DROP TRIGGER stale")
	must(t, err)
	f.sweepExpiry(t)
	if got := f.expiresOn(t, f.ids[0]); got != "2026-10-20" {
		t.Fatalf("expires_on = %s, want 2026-10-20 once the snapshot matches", got)
	}
	if events := f.events(t); len(events) != 0 {
		t.Fatalf("renewed node produced events: %+v", events)
	}
}

// 续期写回出错时，这个节点在库里的有效到期日未定：本轮不评估它，错误随扫描返回，下一轮重试续期。若按未推后的快照
// 评估，开着自动续期的节点会先收到"已过期"，续期成功的下一轮再收到"到期日已更新"，这一对通知都是假的。
func TestSweepExpirySkipsANodeWhoseRenewalFailed(t *testing.T) {
	f := newFixture(t)
	r := f.rule(t, expiryRule())
	f.billing(t, f.ids[0], store.Billing{Cycle: store.CycleMonthly, ExpiresOn: "2026-09-20", AutoRenew: true})
	db, err := sql.Open("sqlite", f.path)
	must(t, err)
	defer db.Close()
	_, err = db.ExecContext(t.Context(), "CREATE TRIGGER renew_fails BEFORE UPDATE OF expires_on ON node BEGIN SELECT RAISE(ABORT, 'renewal write failed'); END")
	must(t, err)
	if err := f.e.SweepExpiry(t.Context()); err == nil || !strings.Contains(err.Error(), "renewal write failed") {
		t.Fatalf("SweepExpiry error = %v, want the renewal write error", err)
	}
	if events := f.events(t); len(events) != 0 {
		t.Fatalf("a node whose renewal failed was evaluated: %+v", events)
	}
	wantState(t, f.e, r.ID, f.ids[0], "")
	_, err = db.ExecContext(t.Context(), "DROP TRIGGER renew_fails")
	must(t, err)
	f.sweepExpiry(t)
	if got := f.expiresOn(t, f.ids[0]); got != "2026-10-20" {
		t.Fatalf("expires_on = %s, want 2026-10-20 once the renewal write succeeds", got)
	}
	if events := f.events(t); len(events) != 0 {
		t.Fatalf("renewed node produced events: %+v", events)
	}
}

// tickingClock 的墙钟从 base 起随真实时间前进。RunExpirySweep 的定时器按真实时间走，墙钟随它一起跨过日界，用例走的
// 是"定时器等到日界再扫"这条路。
type tickingClock struct{ base, start time.Time }

func (c tickingClock) Now() time.Time      { return c.base.Add(time.Since(c.start)) }
func (c tickingClock) Mono() time.Duration { return time.Since(c.start) }

// 启动扫描之后，循环在 hub 时区的日界到点时再扫描一次：墙钟从东八区 2026-09-24 23:59:59.5 起走，约半秒后进入次日，
// 今天到期、开着自动续期的节点由零点之后的扫描推后。启动扫描读到的若还是 24 日，不推后它（到期日不早于今天）；循环
// 在启动扫描之前读钟，定时器定在 25 日零点，启动扫描再慢也不会把下一次推到 26 日。
func TestRunExpirySweepSweepsAgainAtTheDayBoundary(t *testing.T) {
	f := newFixture(t)
	f.billing(t, f.ids[0], store.Billing{Cycle: store.CycleMonthly, ExpiresOn: "2026-09-24", AutoRenew: true})
	clk := tickingClock{base: time.Date(2026, 9, 24, 23, 59, 59, 500_000_000, f.loc), start: time.Now()}
	e := New(Config{TTL: 30 * time.Second, Location: f.loc}, f.st, f.st.Evaluation(), f.l, clk, f.log)
	must(t, e.Load(t.Context()))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); e.RunExpirySweep(ctx) }()
	testwait.Until(t, 10*time.Millisecond, func() bool { return f.expiresOn(t, f.ids[0]) == "2026-10-24" },
		"expires_on = %s, want 2026-10-24 after the sweep at the day boundary", testwait.When(func() string { return f.expiresOn(t, f.ids[0]) }))
	cancel()
	select {
	case <-done:
	case <-time.After(testwait.Bound):
		t.Fatal("RunExpirySweep did not return after cancel")
	}
}

// scriptedClock 在 arm 之前一律返回 before；arm 之后前 beforeReads 次读仍返回 before，此后一律返回 after。它按读钟的
// 次序而不是真实时间切换，用例借此逐次指定循环与扫描各自读到零点前还是零点后。
type scriptedClock struct {
	mu            sync.Mutex
	before, after time.Time
	armed         bool
	beforeReads   int
}

func (c *scriptedClock) arm() { c.mu.Lock(); c.armed = true; c.mu.Unlock() }

func (c *scriptedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed {
		return c.before
	}
	if c.beforeReads > 0 {
		c.beforeReads--
		return c.before
	}
	return c.after
}

func (c *scriptedClock) Mono() time.Duration { return 0 }

// 一轮扫描跨过零点时，新一天的扫描不被跳过。arm 之后每一轮先是循环读钟定下一次触发，再是扫描读钟取今天，扫描结束后
// 循环再读一次钟算定时器时长（夹具里没有规则，扫描不再读钟）。零点前是东八区 9 月 24 日，零点后是 25 日；节点 24 日
// 到期、开着按月自动续期，只有按 25 日扫描才推后到 10 月 24 日。两例各钉住一种跳过：
//   - 前 1 次读在零点前：循环读到 24 日、定到 25 日零点，扫描已读到 25 日并推后。循环若改成扫描之后才读钟，扫描读到
//     24 日不推后，循环读到 25 日而定到 26 日零点。
//   - 前 2 次读在零点前：循环与扫描都读到 24 日，扫描不推后；结束时已过零点，定时器时长为负、立即触发，重扫按 25 日
//     推后。负时长若被改成等到再下一个日界，25 日的扫描就被推到 26 日零点。
func TestRunExpirySweepDoesNotSkipADayBoundaryCrossedDuringASweep(t *testing.T) {
	for _, beforeReads := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d reads before midnight", beforeReads), func(t *testing.T) {
			f := newFixture(t)
			f.billing(t, f.ids[0], store.Billing{Cycle: store.CycleMonthly, ExpiresOn: "2026-09-24", AutoRenew: true})
			clk := &scriptedClock{before: time.Date(2026, 9, 24, 23, 59, 59, 900_000_000, f.loc), after: time.Date(2026, 9, 25, 0, 0, 0, 100_000_000, f.loc), beforeReads: beforeReads}
			e := New(Config{TTL: 30 * time.Second, Location: f.loc}, f.st, f.st.Evaluation(), f.l, clk, f.log)
			must(t, e.Load(t.Context()))
			clk.arm()
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { defer close(done); e.RunExpirySweep(ctx) }()
			testwait.Until(t, 10*time.Millisecond, func() bool { return f.expiresOn(t, f.ids[0]) == "2026-10-24" },
				"expires_on = %s, want 2026-10-24: the day boundary crossed during the sweep was skipped", testwait.When(func() string { return f.expiresOn(t, f.ids[0]) }))
			cancel()
			select {
			case <-done:
			case <-time.After(testwait.Bound):
				t.Fatal("RunExpirySweep did not return after cancel")
			}
		})
	}
}

// 载入与保存经同一个 CheckRule：绕过 Engine 直接写进库的越界到期规则载入时跳过并记一行 Warn，合法的照常载入并带着
// 提前天数。
func TestLoadChecksExpiryRules(t *testing.T) {
	f := newFixture(t)
	var logs bytes.Buffer
	f.log = slog.New(slog.NewTextHandler(&logs, nil))
	bad := expiryRule()
	bad.DaysBefore = 400
	_, err := f.st.SaveAlertRule(t.Context(), bad)
	must(t, err)
	good, err := f.st.SaveAlertRule(t.Context(), expiryRule())
	must(t, err)
	f.restart(t)
	if rules := f.e.Rules(); len(rules) != 1 || rules[0].ID != good.ID || rules[0].DaysBefore != 7 {
		t.Fatalf("loaded rules = %+v, want only rule %d with days_before 7", rules, good.ID)
	}
	if want := `level=WARN msg="invalid alert rule skipped" rule_id=1 err="invalid: days_before must be between 1 and 365"`; strings.Count(logs.String(), want) != 1 {
		t.Fatalf("want one line %q in:\n%s", want, logs.String())
	}
}

func TestNewRequiresLocation(t *testing.T) {
	defer func() {
		if r := recover(); r != "alert.Config.Location must be set" {
			t.Fatalf("panic = %v", r)
		}
	}()
	New(Config{TTL: time.Second}, nil, nil, nil, nil, nil)
}

// 下一次扫描的时刻：没有失败时是下一个日界；连续失败 n 次时是扫描开始后 1 分钟 × 2^(n-1)，上限 1 小时，日界先到就取
// 日界。start 是东八区的本地时刻。
func TestNextSweepAt(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.ParseInLocation("2006-01-02T15:04:05", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	midnight := at("2026-09-25T00:00:00")
	for _, c := range []struct {
		start    string
		failures int
		want     time.Time
	}{
		{"2026-09-24T12:00:00", 0, midnight},
		{"2026-09-24T12:00:00", 1, at("2026-09-24T12:01:00")},
		{"2026-09-24T12:00:00", 2, at("2026-09-24T12:02:00")},
		{"2026-09-24T12:00:00", 3, at("2026-09-24T12:04:00")},
		{"2026-09-24T12:00:00", 4, at("2026-09-24T12:08:00")},
		{"2026-09-24T12:00:00", 5, at("2026-09-24T12:16:00")},
		{"2026-09-24T12:00:00", 6, at("2026-09-24T12:32:00")},
		{"2026-09-24T12:00:00", 7, at("2026-09-24T13:00:00")},  // 64 分钟封顶为 1 小时
		{"2026-09-24T12:00:00", 60, at("2026-09-24T13:00:00")}, // 连续失败很多次也不溢出
		{"2026-09-24T23:30:00", 7, midnight},                   // 日界早于退避：按日界扫
		{"2026-09-24T23:50:00", 5, midnight},
		{"2026-09-24T23:00:00", 7, midnight}, // 退避恰好落在日界上
	} {
		if got := nextSweepAt(at(c.start), loc, c.failures); !got.Equal(c.want) {
			t.Errorf("nextSweepAt(%s, %d failures) = %s, want %s", c.start, c.failures, got.Format(time.RFC3339), c.want.Format(time.RFC3339))
		}
	}
}

// renewalFails 让节点的续期写回失败：UPDATE OF expires_on 一律 RAISE(ABORT)，SweepExpiry 因此返回错误。
// 返回的函数撤掉触发器。
func renewalFails(t *testing.T, f *fixture) (restore func()) {
	t.Helper()
	db, err := sql.Open("sqlite", f.path)
	must(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.ExecContext(t.Context(), "CREATE TRIGGER renew_fails BEFORE UPDATE OF expires_on ON node BEGIN SELECT RAISE(ABORT, 'renewal write failed'); END")
	must(t, err)
	return func() {
		t.Helper()
		_, err := db.ExecContext(t.Context(), "DROP TRIGGER renew_fails")
		must(t, err)
	}
}

// 一轮扫描出错时连续失败次数加一、按退避定下一次；成功即清零、回到日界。夹具的钟不走：UTC 12:00 是东八区 20:00，
// 距下一个日界 4 小时。
func TestSweepRoundBacksOffOnFailureAndResetsOnSuccess(t *testing.T) {
	f := newFixture(t)
	f.billing(t, f.ids[0], store.Billing{Cycle: store.CycleMonthly, ExpiresOn: "2026-09-20", AutoRenew: true})
	restore := renewalFails(t, f)
	for _, c := range []struct {
		failures, wantFailures int
		wantWait               time.Duration
	}{
		{0, 1, time.Minute},
		{1, 2, 2 * time.Minute},
		{6, 7, time.Hour},
	} {
		wait, failures := f.e.sweepRound(t.Context(), c.failures)
		if wait != c.wantWait || failures != c.wantFailures {
			t.Fatalf("failing round after %d failures: wait %s, %d failures; want %s, %d", c.failures, wait, failures, c.wantWait, c.wantFailures)
		}
	}
	restore()
	if wait, failures := f.e.sweepRound(t.Context(), 7); wait != 4*time.Hour || failures != 0 {
		t.Fatalf("successful round after 7 failures: wait %s, %d failures; want 4h0m0s until the day boundary, 0", wait, failures)
	}
	if got := f.expiresOn(t, f.ids[0]); got != "2026-10-20" {
		t.Fatalf("expires_on = %s, want 2026-10-20 after the successful round", got)
	}
}

// syncBuffer 让循环协程写日志、用例同时读日志时不产生数据竞争。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// 扫描出错时循环按退避重扫，不等日界。钟在 arm 之后第一次读（每一轮的第一次读钟就是 sweepRound 记下的 start，这正是
// RunExpirySweep 注释要求的不变式）返回东八区 24 日 12:00，此后一律返回 1 分钟退避到期前 1ns（扫描自己再读钟也还在
// 同一天）：第一次失败后定时器只等 1ns 就重扫；第二次仍失败，退避翻倍为 2 分钟。日界在 12 小时之后，循环若按日界定时，
// 第二次扫描不会在时限内发生。
func TestRunExpirySweepRetriesAFailedSweepWithBackoff(t *testing.T) {
	f := newFixture(t)
	logs := &syncBuffer{}
	f.log = slog.New(slog.NewTextHandler(logs, nil))
	f.billing(t, f.ids[0], store.Billing{Cycle: store.CycleMonthly, ExpiresOn: "2026-09-20", AutoRenew: true})
	renewalFails(t, f)
	start := time.Date(2026, 9, 24, 12, 0, 0, 0, f.loc)
	clk := &scriptedClock{before: start, after: start.Add(time.Minute - time.Nanosecond), beforeReads: 1}
	e := New(Config{TTL: 30 * time.Second, Location: f.loc}, f.st, f.st.Evaluation(), f.l, clk, f.log)
	must(t, e.Load(t.Context()))
	clk.arm()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); e.RunExpirySweep(ctx) }()
	// 只认 msg 与行尾的 retry_in，不带驱动格式化的错误原文：驱动换了错误文本的写法，这里不该跟着等满时限。
	retryLines := func(retryIn string) int {
		n := 0
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, `msg="expiry sweep failed"`) && strings.HasSuffix(line, "retry_in="+retryIn) {
				n++
			}
		}
		return n
	}
	testwait.Until(t, 10*time.Millisecond, func() bool { return retryLines("2m0s") > 0 },
		"no retry after the first failed sweep; logs:\n%s", testwait.When(logs.String))
	cancel()
	select {
	case <-done:
	case <-time.After(testwait.Bound):
		t.Fatal("RunExpirySweep did not return after cancel")
	}
	if n := retryLines("1ns"); n != 1 {
		t.Fatalf("%d lines with retry_in=1ns, want 1 (the wait after the first failure counts from the clock after the sweep):\n%s", n, logs.String())
	}
}
