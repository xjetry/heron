package alert

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
)

// setZone 设节点的计费时区，其余可编辑字段取 f.billing 同样的写法。
func (f *fixture) setZone(t *testing.T, id int64, zone string, b store.Billing, quota uint64) {
	t.Helper()
	_, err := f.st.UpdateNode(t.Context(), id, store.NodeEdit{Name: fmt.Sprintf("node%d", id), TrafficResetDay: 1, BillingTimezone: zone, Billing: b, TrafficQuotaBytes: quota, TrafficQuotaMode: "sum"})
	must(t, err)
}

func counterReport(rx uint64) *heronv1.Metrics {
	return &heronv1.Metrics{BootId: "b1", NetRxTotal: proto.Uint64(rx), NetTxTotal: proto.Uint64(0)}
}

// hub 在东八区，node1 的计费时区是 UTC、node2 没设（沿用 hub）。东八区 10 月 1 日零点（UTC 9 月 30 日 16:00）之后、
// UTC 零点之前的那 8 小时，node1 的用量仍记在 9 月的周期里，满额即触发；node2 在 hub 零点已滚进新周期，同一笔用量
// 只占新周期的一成，不触发——这正是按 hub 时区切周期时服务商重置前用超、告警不发的情形。
func TestTrafficAlertFollowsTheNodeBillingPeriod(t *testing.T) {
	f := newFixture(t)
	node1, node2 := f.ids[0], f.ids[1]
	f.setZone(t, node1, "UTC", store.Billing{}, 1000)
	f.setZone(t, node2, "", store.Billing{}, 1000)
	b := traffic.New(f.st, f.clk, f.loc, f.log)
	must(t, b.Load(t.Context()))
	f.e.SetTraffic(b)
	r := f.rule(t, store.AlertRule{Name: "流量", Kind: store.KindTraffic, Enabled: true, AllNodes: true, Threshold: 100})
	f.clk.SetWall(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	for _, id := range []int64{node1, node2} {
		b.Account(id, counterReport(0), nil)
		b.Account(id, counterReport(900), nil)
	}
	must(t, b.Flush(t.Context()))
	must(t, f.e.SweepTraffic(t.Context()))
	for _, id := range []int64{node1, node2} {
		if stateOf(f.e, r.ID, id) == store.StateFiring {
			t.Fatalf("node %d fired at 90%%", id)
		}
	}

	f.clk.SetWall(time.Date(2026, 9, 30, 18, 0, 0, 0, time.UTC))
	for _, id := range []int64{node1, node2} {
		b.Account(id, counterReport(1000), nil)
	}
	must(t, b.Flush(t.Context()))
	must(t, f.e.SweepTraffic(t.Context()))
	wantState(t, f.e, r.ID, node1, store.StateFiring)
	if stateOf(f.e, r.ID, node2) == store.StateFiring {
		t.Fatal("node billed in the hub zone fired on usage that fell into its new period")
	}
}

// 报告里每个节点的周期与距重置的天数按节点的计费时区：东京 10 月 1 日 05:00（UTC 9 月 30 日 20:00）时，UTC 计费的
// 节点还在 9 月的周期里、明天重置；没设计费时区的节点已在东京 10 月 1 日零点进入新周期、用量为零、下月重置。
func TestReportRowsUseTheNodeBillingPeriod(t *testing.T) {
	t.Parallel()
	tokyo := zone(t, "Asia/Tokyo")
	now := time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC)
	nodes := []store.Node{
		{ID: 1, Name: "utc", TrafficResetDay: 1, BillingTimezone: "UTC", TrafficQuotaBytes: 1000, TrafficQuotaMode: "sum"},
		{ID: 2, Name: "hub", TrafficResetDay: 1, TrafficQuotaBytes: 1000, TrafficQuotaMode: "sum"},
	}
	committed := map[int64]traffic.State{
		1: {PeriodRx: 950, PeriodStart: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		2: {PeriodRx: 950, PeriodStart: time.Date(2026, 9, 1, 0, 0, 0, 0, tokyo)},
	}
	rows := ReportRows(nodes, committed, now, tokyo)
	want := []ReportRow{
		{Name: "utc", UsedBytes: 950, QuotaBytes: 1000, Mode: "sum", Percent: 95, DaysLeft: 1},
		{Name: "hub", UsedBytes: 0, QuotaBytes: 1000, Mode: "sum", Percent: 0, DaysLeft: 31},
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

// 到期剩余天数按节点计费时区的今天：UTC 9 月 24 日 16:00 东八区已是 25 日，没设计费时区的 node2 剩 7 天、进入提醒窗口；
// 计费时区是 UTC 的 node1 在 UTC 还是 24 日、剩 8 天，到 UTC 25 日零点才进入。
func TestSweepExpiryCountsDaysInTheNodeBillingZone(t *testing.T) {
	f := newFixture(t)
	node1, node2 := f.ids[0], f.ids[1]
	r := f.rule(t, expiryRule())
	f.setZone(t, node1, "UTC", store.Billing{ExpiresOn: "2026-10-02"}, 0)
	f.setZone(t, node2, "", store.Billing{ExpiresOn: "2026-10-02"}, 0)
	f.clk.SetWall(time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC))
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, node1, "")
	wantState(t, f.e, r.ID, node2, store.StateFiring)
	f.clk.SetWall(time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC))
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, node1, store.StateFiring)
}

// 自动续期的"今天"同样按节点计费时区：UTC 9 月 24 日 16:00，到期日 24 日在东八区已过、在 UTC 还没过。
func TestAutoRenewUsesTheNodeBillingZone(t *testing.T) {
	f := newFixture(t)
	node1, node2 := f.ids[0], f.ids[1]
	b := store.Billing{Cycle: store.CycleMonthly, ExpiresOn: "2026-09-24", AutoRenew: true}
	f.setZone(t, node1, "UTC", b, 0)
	f.setZone(t, node2, "", b, 0)
	f.clk.SetWall(time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC))
	f.sweepExpiry(t)
	if got := f.expiresOn(t, node1); got != "2026-09-24" {
		t.Errorf("node billed in UTC renewed to %s before its own today passed the date", got)
	}
	if got := f.expiresOn(t, node2); got != "2026-10-24" {
		t.Errorf("node billed in the hub zone = %s, want renewed to 2026-10-24", got)
	}
}

// 到期扫描在每个在用时区的日界都醒一次：hub 东八区与有到期日的节点的计费时区（America/New_York），取 start 之后最早
// 的那个；没有到期日的节点的时区不算在内。
func TestExpirySweepWakesAtTheEarliestZoneBoundary(t *testing.T) {
	t.Parallel()
	hub := time.FixedZone("UTC+8", 8*3600)
	ny := zone(t, "America/New_York")
	nodes := []store.Node{
		{ID: 1, BillingTimezone: "America/New_York", Billing: store.Billing{ExpiresOn: "2026-10-02"}},
		{ID: 2, BillingTimezone: "Asia/Kolkata"},
		{ID: 3, Billing: store.Billing{ExpiresOn: "2026-10-02"}},
	}
	zones := expiryZones(nodes, hub)
	if len(zones) != 2 || zones[0].String() != "America/New_York" || zones[1] != hub {
		t.Fatalf("zones = %v, want [America/New_York UTC+8]", zones)
	}
	for _, c := range []struct {
		start, want time.Time
	}{
		// 东八区零点（UTC 16:00）先到。
		{time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC), time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC)},
		// 东八区零点刚过，纽约零点（EDT，UTC 04:00）先到。
		{time.Date(2026, 9, 24, 17, 0, 0, 0, time.UTC), time.Date(2026, 9, 25, 4, 0, 0, 0, time.UTC)},
	} {
		if got := nextSweepAt(c.start, zones, 0); !got.Equal(c.want) {
			t.Errorf("from %s: next sweep %s, want %s", c.start, got.UTC(), c.want)
		}
	}
	if got := nextSweepAt(time.Date(2026, 9, 24, 17, 0, 0, 0, time.UTC), []*time.Location{hub, ny}, 0); !got.Equal(time.Date(2026, 9, 25, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("zone order must not matter: %s", got.UTC())
	}
}

// 时区集合只在变化时发出唤醒信号；扫描循环自己那一轮发布的变化由它自己消化，不留下信号让下一轮白扫。
func TestExpiryZonesSignalOnlyWhenTheSetChanges(t *testing.T) {
	f := newFixture(t)
	pending := func() bool { return len(f.e.expiryRearm) > 0 }
	f.sweepExpiry(t)
	if !pending() {
		t.Fatal("the first published zone set did not signal")
	}
	<-f.e.expiryRearm
	f.sweepExpiry(t)
	if pending() {
		t.Fatal("an unchanged zone set signalled")
	}
	f.setZone(t, f.ids[0], "America/New_York", store.Billing{ExpiresOn: "2026-10-02"}, 0)
	f.sweepExpiry(t)
	if !pending() {
		t.Fatal("a node moving to a new billing zone did not signal")
	}
	// UTC 9 月 24 日 17:00：东八区下一个零点在 23 小时后，纽约零点（EDT）在 11 小时后，取后者。
	f.clk.SetWall(time.Date(2026, 9, 24, 17, 0, 0, 0, time.UTC))
	wait, _ := f.e.sweepRound(t.Context(), 0)
	if pending() {
		t.Fatal("sweepRound left its own signal behind")
	}
	if wait != 11*time.Hour {
		t.Fatalf("wait = %s, want 11h to the New York boundary", wait)
	}
}

// 扫描循环按旧集合定好的定时器不会错过新时区的日界：别处触发的扫描发布了新集合、发出唤醒信号，循环立刻再扫一轮。
// 墙钟停在 UTC 9 月 24 日 17:00（东八区 25 日 01:00），循环开头那一轮之后按东八区下一个零点定时（23 小时后）；随后绕过
// 扫描把开着自动续期的节点的到期日改成已过的 24 日，只有被唤醒的那一轮能把它推后。循环自己那一轮会清掉残留的信号，
// 所以在推后之前反复投递，直到循环停在等待上收到它。
func TestRunExpirySweepWakesOnTheRearmSignal(t *testing.T) {
	f := newFixture(t)
	renewing := store.Billing{Cycle: store.CycleMonthly, ExpiresOn: "2026-10-30", AutoRenew: true}
	f.setZone(t, f.ids[0], "", renewing, 0)
	f.clk.SetWall(time.Date(2026, 9, 24, 17, 0, 0, 0, time.UTC))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); f.e.RunExpirySweep(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(10 * time.Second)
	for f.e.sweepZones.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the loop never ran its first sweep")
		}
		time.Sleep(10 * time.Millisecond)
	}
	renewing.ExpiresOn = "2026-09-24"
	f.setZone(t, f.ids[0], "", renewing, 0)
	for f.expiresOn(t, f.ids[0]) != "2026-10-24" {
		if time.Now().After(deadline) {
			t.Fatalf("the loop did not sweep after the wake-up; expires_on = %s", f.expiresOn(t, f.ids[0]))
		}
		select {
		case f.e.expiryRearm <- struct{}{}:
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
}
