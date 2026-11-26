package traffic

import (
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
)

// hub 在上海、节点的计费时区是 UTC、重置日 1、重置时刻 00:00：周期边界是 UTC 的零点（上海 08:00），不是上海的零点。
// 计费时区为空的节点仍按 hub 时区，与引入计费时区之前逐一相同。
func TestNodeBillingZoneMovesTheBoundaryOffTheHubZone(t *testing.T) {
	st := newMem()
	st.schedules = map[int64]store.TrafficSchedule{1: {Day: 1, Timezone: "UTC"}, 2: {Day: 1}}
	b, _ := newBook(t, st) // 上海 9 月 15 日 12:00
	for _, c := range []struct {
		node        int64
		start, next time.Time
		zone        string
	}{
		{1, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "UTC"},
		{2, time.Date(2026, 9, 1, 0, 0, 0, 0, shanghai), time.Date(2026, 10, 1, 0, 0, 0, 0, shanghai), "Asia/Shanghai"},
	} {
		v := b.View(c.node)
		if !v.PeriodStart.Equal(c.start) || !v.NextReset.Equal(c.next) || v.Schedule.Zone.String() != c.zone {
			t.Errorf("node %d: period [%s, %s) in %s, want [%s, %s) in %s", c.node, v.PeriodStart.UTC(), v.NextReset.UTC(), v.Schedule.Zone, c.start.UTC(), c.next.UTC(), c.zone)
		}
	}
}

// 服务商按 UTC 重置、hub 设上海：上海的零点（UTC 16:00）到 UTC 零点之间的那 8 小时，入账仍记在旧周期里；
// UTC 零点之后才进新周期。按 hub 时区算边界时，这段用量会被记进新周期，旧周期的配额告警因此不发。
func TestAccountKeepsTheProviderPeriodUntilItsOwnReset(t *testing.T) {
	st := newMem()
	st.schedules = map[int64]store.TrafficSchedule{1: {Day: 1, Timezone: "UTC"}}
	b, clk := newBook(t, st)
	b.Account(1, counters("b1", 0, 0), nil)
	b.Account(1, counters("b1", 900, 0), nil)
	clk.SetWall(time.Date(2026, 9, 30, 18, 0, 0, 0, time.UTC)) // 上海已是 10 月 1 日 02:00
	b.Account(1, counters("b1", 1000, 0), nil)
	if e, _ := b.Get(1); e.PeriodRx != 1000 || !e.PeriodStart.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("before the UTC reset: period %d from %s, want 1000 from 2026-09-01 UTC", e.PeriodRx, e.PeriodStart.UTC())
	}
	clk.SetWall(time.Date(2026, 10, 1, 0, 0, 5, 0, time.UTC))
	b.Account(1, counters("b1", 1100, 0), nil)
	if e, _ := b.Get(1); e.PeriodRx != 100 || !e.PeriodStart.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("after the UTC reset: period %d from %s, want 100 from 2026-10-01 UTC", e.PeriodRx, e.PeriodStart.UTC())
	}
}

// 重置时刻不是零点、时区偏移不是整小时：Asia/Kolkata（+05:30）重置日 1、08:30，边界是 UTC 03:00；
// Asia/Kathmandu（+05:45）00:00 的边界是 UTC 前一日 18:15。
func TestResetMinuteAndHalfHourZones(t *testing.T) {
	for _, c := range []struct {
		zone   string
		minute int
		now    time.Time
		start  time.Time
		next   time.Time
	}{
		{"Asia/Kolkata", 8*60 + 30, time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)},
		{"Asia/Kolkata", 8*60 + 30, time.Date(2026, 9, 1, 2, 59, 0, 0, time.UTC), time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC), time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)},
		{"Asia/Kathmandu", 0, time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 31, 18, 15, 0, 0, time.UTC), time.Date(2026, 9, 30, 18, 15, 0, 0, time.UTC)},
	} {
		s := Schedule{Day: 1, Minute: c.minute, Zone: mustZone(c.zone)}
		start := PeriodStart(c.now, s)
		if !start.Equal(c.start) {
			t.Errorf("%s %02d:%02d at %s: period starts %s, want %s", c.zone, c.minute/60, c.minute%60, c.now, start.UTC(), c.start)
		}
		if next := NextResetAfter(start, s); !next.Equal(c.next) {
			t.Errorf("%s %02d:%02d: next reset %s, want %s", c.zone, c.minute/60, c.minute%60, next.UTC(), c.next)
		}
	}
}

// 夏令时两个方向（America/New_York）：重置时刻落在拨快的空档里（3 月 8 日 02:30 不存在）时取跳变那一刻 03:00 EDT；
// 落在拨慢的重复时段里（11 月 1 日 01:30 出现两次）时取第一次，即 01:30 EDT。两者都是自身周期的起点，前一周期的
// 下次重置恰是它。
func TestResetMinuteAcrossDaylightSavingTransitions(t *testing.T) {
	ny := mustZone("America/New_York")
	for _, c := range []struct {
		name string
		s    Schedule
		now  time.Time
		want time.Time
	}{
		{"spring forward gap", Schedule{Day: 8, Minute: 2*60 + 30, Zone: ny}, time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC)},
		{"fall back repeat", Schedule{Day: 1, Minute: 1*60 + 30, Zone: ny}, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)},
	} {
		got := PeriodStart(c.now, c.s)
		if !got.Equal(c.want) {
			t.Errorf("%s: period starts %s (%s), want %s", c.name, got.UTC(), got.In(ny), c.want)
			continue
		}
		if !PeriodStart(got, c.s).Equal(got) {
			t.Errorf("%s: boundary %s is not its own period start", c.name, got.UTC())
		}
		prev := PeriodStart(got.Add(-time.Second), c.s)
		if next := NextResetAfter(prev, c.s); !next.Equal(got) {
			t.Errorf("%s: the previous period %s resets at %s, want %s", c.name, prev.UTC(), next.UTC(), got.UTC())
		}
	}
}

// 改计费时区或重置时刻与改重置日走同一条重算：起点往后挪时新周期按 [新起点, now) 的桶重算，往前挪时只补两个起点之间
// 的桶。上海的 9 月 1 日零点换成 UTC 的零点（晚 8 小时），再换成 UTC 的 00:30（再晚 30 分钟），再换回上海（早 8.5 小时）。
func TestScheduleChangeRecomputesFromTheNewBoundary(t *testing.T) {
	st := newMem()
	st.bytesBetween = func(int64, time.Time, time.Time) (int64, int64, error) { return 10, 1, nil }
	b, clk := newBook(t, st)
	b.Account(1, counters("b1", 0, 0), nil)
	b.Account(1, counters("b1", 500, 50), nil)
	shanghaiStart := time.Date(2026, 9, 1, 0, 0, 0, 0, shanghai)
	utcStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	halfPast := utcStart.Add(30 * time.Minute)

	b.SetSchedule(1, store.TrafficSchedule{Day: 1, Timezone: "UTC"})
	if e, _ := b.Get(1); !e.PeriodStart.Equal(utcStart) || e.PeriodRx != 10 || e.TotalRx != 500 {
		t.Fatalf("after switching to UTC: %+v", e)
	}
	b.SetSchedule(1, store.TrafficSchedule{Day: 1, Minute: 30, Timezone: "UTC"})
	if e, _ := b.Get(1); !e.PeriodStart.Equal(halfPast) || e.PeriodRx != 10 {
		t.Fatalf("after moving the reset time to 00:30: %+v", e)
	}
	b.SetSchedule(1, store.TrafficSchedule{Day: 1})
	if e, _ := b.Get(1); !e.PeriodStart.Equal(shanghaiStart) || e.PeriodRx != 20 {
		t.Fatalf("after switching back to the hub zone: %+v", e)
	}
	queried(t, st, byteRange{1, utcStart, clk.Now()}, byteRange{1, halfPast, clk.Now()}, byteRange{1, shanghaiStart, halfPast})
}

// 上一周期起点是严格早于本周期起点的最近一个边界，按同一规则独立算出，不由本周期起点前推一个月：重置日 31、
// Asia/Kolkata 08:30，3 月 15 日时本周期从钳过的 2 月 28 日起，上一周期从 1 月 31 日起（前推一个月会得到 1 月 28 日）；
// 10 月 15 日时本周期从 9 月 30 日起，上一周期从 8 月 31 日起。
func TestEntryCarriesThePreviousPeriodStart(t *testing.T) {
	kolkata := mustZone("Asia/Kolkata")
	st := newMem()
	st.schedules = map[int64]store.TrafficSchedule{1: {Day: 31, Minute: 8*60 + 30, Timezone: "Asia/Kolkata"}}
	b, clk := newBook(t, st)
	for _, c := range []struct {
		now                   time.Time
		previous, start, next time.Time
	}{
		{time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 31, 8, 30, 0, 0, kolkata), time.Date(2026, 2, 28, 8, 30, 0, 0, kolkata), time.Date(2026, 3, 31, 8, 30, 0, 0, kolkata)},
		{time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 31, 8, 30, 0, 0, kolkata), time.Date(2026, 9, 30, 8, 30, 0, 0, kolkata), time.Date(2026, 10, 31, 8, 30, 0, 0, kolkata)},
	} {
		clk.SetWall(c.now)
		v := b.View(1)
		if !v.PreviousStart.Equal(c.previous) || !v.PeriodStart.Equal(c.start) || !v.NextReset.Equal(c.next) {
			t.Errorf("at %s: periods %s | %s | %s, want %s | %s | %s", c.now, v.PreviousStart.In(kolkata), v.PeriodStart.In(kolkata), v.NextReset.In(kolkata), c.previous, c.start, c.next)
		}
	}
}
