package traffic

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

// memStore 是 Storage 的内存实现：记录每次写入，可注入失败。
type memStore struct {
	mu       sync.Mutex
	gateOnce sync.Once
	gate     chan struct{}
	entered  chan struct{}
	recs     map[int64]store.TrafficRecord
	days     map[int64]int
	fail     error
	writes   int
}

func newMem() *memStore {
	return &memStore{recs: map[int64]store.TrafficRecord{}, days: map[int64]int{}}
}

func (m *memStore) LoadTraffic(context.Context) ([]store.TrafficRecord, error) {
	var out []store.TrafficRecord
	for _, r := range m.recs {
		out = append(out, r)
	}
	return out, nil
}

func (m *memStore) WriteTraffic(_ context.Context, recs []store.TrafficRecord) ([]int64, error) {
	if m.gate != nil {
		first := false
		m.gateOnce.Do(func() {
			first = true
			close(m.entered)
		})
		if first {
			<-m.gate
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	m.writes++
	var written []int64
	for _, r := range recs {
		m.recs[r.NodeID] = r
		written = append(written, r.NodeID)
	}
	return written, nil
}

func (m *memStore) TrafficResetDays(context.Context) (map[int64]int, error) { return m.days, nil }

var shanghai = mustZone("Asia/Shanghai")

func mustZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// newBook 的时钟停在上海时间 2026-09-15 12:00。
func newBook(t *testing.T, st *memStore) (*Book, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 9, 15, 4, 0, 0, 0, time.UTC))
	b := New(st, clk, shanghai, slog.Default())
	if err := b.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return b, clk
}

// base 是合计形态基线的唯一一项；没有基线或逐网卡形态时返回 -1/-1，让按合计读数的断言必然不成立。
func base(s State) store.TrafficCounter {
	if len(s.Baseline) != 1 || s.Baseline[0].Name != "" {
		return store.TrafficCounter{Rx: -1, Tx: -1}
	}
	return s.Baseline[0]
}

func counters(boot string, rx, tx uint64) *heronv1.Metrics {
	return &heronv1.Metrics{BootId: boot, NetRxTotal: proto.Uint64(rx), NetTxTotal: proto.Uint64(tx)}
}

func TestFirstReportOnlySetsBaseline(t *testing.T) {
	b, _ := newBook(t, newMem())
	d, ok := b.Account(1, counters("b1", 1000, 2000), nil)
	if ok || d != (Delta{}) {
		t.Fatalf("first report accounted %+v", d)
	}
	e, _ := b.Get(1)
	if !e.HasBaseline() || base(e.State).Rx != 1000 || base(e.State).Tx != 2000 || e.TotalRx != 0 || e.PeriodRx != 0 {
		t.Fatalf("baseline not taken: %+v", e)
	}
	if want := time.Date(2026, 9, 1, 0, 0, 0, 0, shanghai); !e.PeriodStart.Equal(want) {
		t.Fatalf("period start %v, want %v", e.PeriodStart, want)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, shanghai); !e.NextReset.Equal(want) || e.ResetDay != 1 {
		t.Fatalf("next reset %v day %d, want %v day 1", e.NextReset, e.ResetDay, want)
	}
}

func TestDeltaWithinSameBootGoesToTotalsAndPeriod(t *testing.T) {
	b, _ := newBook(t, newMem())
	b.Account(1, counters("b1", 1000, 2000), nil)
	d, ok := b.Account(1, counters("b1", 1100, 2300), nil)
	if !ok || d != (Delta{Rx: 100, Tx: 300}) {
		t.Fatalf("delta %+v ok=%v, want {100 300} true", d, ok)
	}
	e, _ := b.Get(1)
	if e.TotalRx != 100 || e.TotalTx != 300 || e.PeriodRx != 100 || e.PeriodTx != 300 || base(e.State).Rx != 1100 {
		t.Fatalf("after delta: %+v", e)
	}
}

func TestBootChangeAccountsOnlyNewCycle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		uptime uint64
		want   Delta
		ok     bool
	}{
		{"restarted_after_observation", 30, Delta{5_000_000, 6_000_000}, true},
		{"started_before_observation", 90, Delta{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, clk := newBook(t, newMem())
			b.Account(1, counters("b1", 10_000_000, 10_000_000), nil)
			b.Account(1, counters("b1", 10_000_100, 10_000_100), nil)
			clk.Advance(time.Minute)
			m := counters("b2", 5_000_000, 6_000_000)
			m.UptimeS = proto.Uint64(tc.uptime)
			if d, ok := b.Account(1, m, nil); d != tc.want || ok != tc.ok {
				t.Errorf("reboot delta=%+v ok=%v, want %+v ok=%v", d, ok, tc.want, tc.ok)
			}
			e, _ := b.Get(1)
			if e.TotalRx != 100+tc.want.Rx || e.TotalTx != 100+tc.want.Tx || e.PeriodRx != 100+tc.want.Rx || e.PeriodTx != 100+tc.want.Tx {
				t.Errorf("reboot usage=%d/%d period=%d/%d, want %d/%d", e.TotalRx, e.TotalTx, e.PeriodRx, e.PeriodTx, 100+tc.want.Rx, 100+tc.want.Tx)
			}
			if base(e.State).Rx != 5_000_000 || base(e.State).Tx != 6_000_000 || e.BootID != "b2" {
				t.Errorf("baseline after reboot: %+v", e)
			}
			if d, ok := b.Account(1, counters("b2", 5_000_010, 6_000_020), nil); !ok || d != (Delta{Rx: 10, Tx: 20}) {
				t.Errorf("delta after reboot %+v", d)
			}
		})
	}
}

func TestCounterBelowBaselineResetsBaseline(t *testing.T) {
	b, _ := newBook(t, newMem())
	b.Account(1, counters("b1", 1000, 2000), nil)
	if d, ok := b.Account(1, counters("b1", 50, 2100), nil); ok || d != (Delta{}) {
		t.Fatalf("wrapped counter accounted %+v", d)
	}
	e, _ := b.Get(1)
	if base(e.State).Rx != 50 || base(e.State).Tx != 2100 || e.TotalTx != 0 {
		t.Fatalf("baseline after wrap: %+v", e)
	}
}

func TestMissingCountersLeaveBaselineUntouched(t *testing.T) {
	b, _ := newBook(t, newMem())
	b.Account(1, counters("b1", 1000, 2000), nil)
	if d, ok := b.Account(1, &heronv1.Metrics{BootId: "b1"}, nil); ok || d != (Delta{}) {
		t.Fatalf("missing counters accounted %+v", d)
	}
	// 基线仍是 1000/2000：下一次的增量是 5/5，不是把 0 当基线算出的整值。
	if d, ok := b.Account(1, counters("b1", 1005, 2005), nil); !ok || d != (Delta{Rx: 5, Tx: 5}) {
		t.Fatalf("delta after a missing sample %+v", d)
	}
}

func TestTotalsSaturateAtMaxInt64(t *testing.T) {
	b, _ := newBook(t, newMem())
	b.Account(1, counters("b1", 0, 0), nil)
	// 总量已在存储上界，随后的增量不能使它回绕成负数。
	if _, err := b.Adjust(context.Background(), 1, math.MaxInt64, 0); err != nil {
		t.Fatal(err)
	}
	b.Account(1, counters("b1", 10, 0), nil)
	e, _ := b.Get(1)
	if e.TotalRx != math.MaxInt64 || e.PeriodRx != math.MaxInt64 {
		t.Fatalf("total %d period %d, want both MaxInt64", e.TotalRx, e.PeriodRx)
	}
}

func TestPeriodBoundaries(t *testing.T) {
	cases := []struct {
		now             time.Time
		day             int
		wantStart, next time.Time
	}{
		{time.Date(2026, 9, 15, 12, 0, 0, 0, shanghai), 1, time.Date(2026, 9, 1, 0, 0, 0, 0, shanghai), time.Date(2026, 10, 1, 0, 0, 0, 0, shanghai)},
		{time.Date(2026, 9, 15, 12, 0, 0, 0, shanghai), 20, time.Date(2026, 8, 20, 0, 0, 0, 0, shanghai), time.Date(2026, 9, 20, 0, 0, 0, 0, shanghai)},
		{time.Date(2026, 9, 20, 0, 0, 0, 0, shanghai), 20, time.Date(2026, 9, 20, 0, 0, 0, 0, shanghai), time.Date(2026, 10, 20, 0, 0, 0, 0, shanghai)},
		{time.Date(2026, 3, 1, 0, 0, 0, 0, shanghai), 28, time.Date(2026, 2, 28, 0, 0, 0, 0, shanghai), time.Date(2026, 3, 28, 0, 0, 0, 0, shanghai)},
		{time.Date(2026, 1, 3, 12, 0, 0, 0, shanghai), 5, time.Date(2025, 12, 5, 0, 0, 0, 0, shanghai), time.Date(2026, 1, 5, 0, 0, 0, 0, shanghai)},
	}
	for _, c := range cases {
		if got := PeriodStart(c.now, c.day, shanghai); !got.Equal(c.wantStart) {
			t.Errorf("PeriodStart(%v, %d) = %v, want %v", c.now, c.day, got, c.wantStart)
		}
		if got := NextResetAfter(c.wantStart, c.day, shanghai); !got.Equal(c.next) {
			t.Errorf("NextResetAfter(%v, %d) = %v, want %v", c.wantStart, c.day, got, c.next)
		}
	}
}

func TestPeriodRollsOverAtResetDayMidnightKeepingTotals(t *testing.T) {
	b, clk := newBook(t, newMem())
	b.Account(1, counters("b1", 0, 0), nil)
	b.Account(1, counters("b1", 500, 700), nil)
	clk.Advance(16 * 24 * time.Hour) // 上海 10 月 1 日 12:00
	d, ok := b.Account(1, counters("b1", 600, 800), nil)
	if !ok || d != (Delta{Rx: 100, Tx: 100}) {
		t.Fatalf("delta across rollover %+v", d)
	}
	e, _ := b.Get(1)
	if e.PeriodRx != 100 || e.PeriodTx != 100 || e.TotalRx != 600 || e.TotalTx != 800 {
		t.Fatalf("period not reset / totals changed: %+v", e)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, shanghai); !e.PeriodStart.Equal(want) {
		t.Fatalf("period start %v, want %v", e.PeriodStart, want)
	}
	if want := time.Date(2026, 11, 1, 0, 0, 0, 0, shanghai); !e.NextReset.Equal(want) {
		t.Fatalf("next reset %v, want %v", e.NextReset, want)
	}
}

func TestRollOverAlsoHappensOnFlushWithoutReports(t *testing.T) {
	st := newMem()
	b, clk := newBook(t, st)
	b.Account(1, counters("b1", 0, 0), nil)
	b.Account(1, counters("b1", 500, 700), nil)
	clk.Advance(16 * 24 * time.Hour)
	if err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := st.recs[1]; r.PeriodRx != 0 || r.TotalRx != 500 || !r.PeriodStart.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, shanghai)) {
		t.Fatalf("flush did not roll the period: %+v", r)
	}
}

func TestAdjustWritesThroughAndShiftsTotalsByTheSameDelta(t *testing.T) {
	st := newMem()
	b, _ := newBook(t, st)
	b.Account(1, counters("b1", 0, 0), nil)
	b.Account(1, counters("b1", 200, 300), nil)
	e, err := b.Adjust(context.Background(), 1, 1000, 50)
	if err != nil {
		t.Fatal(err)
	}
	if e.PeriodRx != 1000 || e.PeriodTx != 50 || e.TotalRx != 1000 || e.TotalTx != 50 || base(e.State).Rx != 200 {
		t.Fatalf("after adjust: %+v", e)
	}
	if st.writes != 1 || st.recs[1].PeriodRx != 1000 {
		t.Fatalf("adjust not written through: writes=%d rec=%+v", st.writes, st.recs[1])
	}
	if d, ok := b.Account(1, counters("b1", 210, 310), nil); !ok || d != (Delta{Rx: 10, Tx: 10}) {
		t.Fatalf("baseline moved by adjust: %+v", d)
	}
	if e, _ := b.Get(1); e.PeriodRx != 1010 || e.TotalRx != 1010 {
		t.Fatalf("accumulation after adjust: %+v", e)
	}
}

func TestAdjustTotalsNeverGoBelowZero(t *testing.T) {
	b, _ := newBook(t, newMem())
	b.Account(1, counters("b1", 0, 0), nil)
	b.Account(1, counters("b1", 200, 300), nil)
	if _, err := b.Adjust(context.Background(), 1, 5000, 5000); err != nil {
		t.Fatal(err)
	}
	e, err := b.Adjust(context.Background(), 1, 0, 0)
	if err != nil || e.TotalRx != 0 || e.TotalTx != 0 || e.PeriodRx != 0 {
		t.Fatalf("totals after adjusting down: %+v %v", e, err)
	}
}

func TestAdjustBeforeAnyReportSeedsUsageWithoutBaseline(t *testing.T) {
	b, _ := newBook(t, newMem())
	e, err := b.Adjust(context.Background(), 9, 700, 800)
	if err != nil || e.PeriodRx != 700 || e.TotalRx != 700 || e.HasBaseline() {
		t.Fatalf("seed: %+v %v", e, err)
	}
	// 种子条目的 boot_id 也是空串，此时只有“尚无基线”这一条判定能阻止把开机以来的计数一次记入。
	if d, ok := b.Account(9, counters("", 9_000_000, 9_000_000), nil); ok || d != (Delta{}) {
		t.Fatalf("seeded node accounted its first counters: %+v", d)
	}
	if e, _ := b.Get(9); e.PeriodRx != 700 || base(e.State).Rx != 9_000_000 || !e.HasBaseline() {
		t.Fatalf("after first report: %+v", e)
	}
}

func TestAdjustWriteFailureLeavesMemoryUnchanged(t *testing.T) {
	st := newMem()
	b, _ := newBook(t, st)
	b.Account(1, counters("b1", 0, 0), nil)
	b.Account(1, counters("b1", 200, 300), nil)
	st.fail = errors.New("disk full")
	if _, err := b.Adjust(context.Background(), 1, 1000, 1000); err == nil {
		t.Fatal("adjust must report the write failure")
	}
	if e, _ := b.Get(1); e.PeriodRx != 200 || e.TotalRx != 200 {
		t.Fatalf("memory changed although the write failed: %+v", e)
	}
}

func TestFlushWritesDirtyEntriesOnceAndRetriesAfterFailure(t *testing.T) {
	st := newMem()
	b, _ := newBook(t, st)
	b.Account(1, counters("b1", 0, 0), nil)
	b.Account(2, counters("b2", 0, 0), nil)
	if err := b.Flush(context.Background()); err != nil || st.writes != 1 || len(st.recs) != 2 {
		t.Fatalf("first flush: err=%v writes=%d recs=%d", err, st.writes, len(st.recs))
	}
	if err := b.Flush(context.Background()); err != nil || st.writes != 1 {
		t.Fatalf("clean flush must not write: writes=%d", st.writes)
	}
	b.Account(1, counters("b1", 10, 10), nil)
	st.fail = errors.New("locked")
	if err := b.Flush(context.Background()); err == nil {
		t.Fatal("flush must surface the write error")
	}
	st.fail = nil
	if err := b.Flush(context.Background()); err != nil || st.writes != 2 || st.recs[1].TotalRx != 10 {
		t.Fatalf("retry after failure: err=%v writes=%d rec=%+v", err, st.writes, st.recs[1])
	}
}

func TestLoadRestoresBaselineSoRestartDeltaCoversTheGap(t *testing.T) {
	st := newMem()
	st.recs[1] = store.TrafficRecord{NodeID: 1, BootID: "b1", Baseline: []store.TrafficCounter{{Rx: 1000, Tx: 1000}}, TotalRx: 5000, TotalTx: 5000, PeriodRx: 100, PeriodTx: 100, PeriodStart: time.Date(2026, 9, 1, 0, 0, 0, 0, shanghai)}
	st.days[1] = 1
	b, _ := newBook(t, st)
	d, ok := b.Account(1, counters("b1", 1500, 1200), nil)
	if !ok || d != (Delta{Rx: 500, Tx: 200}) {
		t.Fatalf("delta against persisted baseline %+v", d)
	}
	if e, _ := b.Get(1); e.TotalRx != 5500 || e.PeriodRx != 600 {
		t.Fatalf("totals after restart: %+v", e)
	}
}

func TestSetResetDayMovesTheNextBoundaryAndViewCoversUnknownNodes(t *testing.T) {
	b, _ := newBook(t, newMem())
	b.Account(1, counters("b1", 0, 0), nil) // 周期起点 9 月 1 日
	b.SetResetDay(1, 20)
	if e, _ := b.Get(1); e.ResetDay != 20 || !e.NextReset.Equal(time.Date(2026, 9, 20, 0, 0, 0, 0, shanghai)) {
		t.Fatalf("after changing the day: %+v", e)
	}
	v := b.View(2)
	if v.ResetDay != DefaultResetDay || v.HasBaseline() || v.TotalRx != 0 || !v.PeriodStart.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, shanghai)) {
		t.Fatalf("view of an unknown node: %+v", v)
	}
	if _, ok := b.Get(2); ok {
		t.Fatal("View must not create an entry")
	}
}

func TestForgetDropsTheEntry(t *testing.T) {
	b, _ := newBook(t, newMem())
	b.Account(1, counters("b1", 0, 0), nil)
	b.SetResetDay(1, 9)
	b.Forget(1)
	if _, ok := b.Get(1); ok {
		t.Fatal("entry survived Forget")
	}
	if b.View(1).ResetDay != DefaultResetDay {
		t.Fatal("reset day survived Forget")
	}
}

func TestAdjustFailureAcrossRolloverLeavesMemoryUnchanged(t *testing.T) {
	st := newMem()
	b, clk := newBook(t, st)
	b.Account(1, counters("b1", 0, 0), nil)
	b.Account(1, counters("b1", 200, 300), nil)
	if err := b.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := *b.entries[1]
	clk.Advance(16 * 24 * time.Hour)
	st.fail = errors.New("disk full")
	if _, err := b.Adjust(t.Context(), 1, 1000, 1000); err == nil {
		t.Fatal("adjust must report the write failure")
	}
	// Get 会自行滚动周期，直接比较条目才能看见失败操作是否留下了状态变化。
	if after := *b.entries[1]; !reflect.DeepEqual(after, before) {
		t.Fatalf("failed adjust changed entry across rollover: got %+v, want %+v", after, before)
	}
}

func TestFlushCannotOverwriteSuccessfulAdjust(t *testing.T) {
	st := newMem()
	b, _ := newBook(t, st)
	b.Account(1, counters("b1", 0, 0), nil)
	b.Account(1, counters("b1", 200, 300), nil)
	st.gate, st.entered = make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(st.gate) })
	flushed := make(chan error, 1)
	go func() { flushed <- b.Flush(t.Context()) }()
	select {
	case <-st.entered:
	case <-time.After(testwait.Bound):
		t.Fatal("flush did not enter storage")
	}
	adjusted := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, err := b.Adjust(t.Context(), 1, 1000, 50)
		adjusted <- err
	}()
	<-started
	// 旧快照停在存储入口；若校正能越过它，先等校正完成再放行，最终库值会暴露覆盖。
	var adjustErr error
	adjustDone := false
	select {
	case adjustErr = <-adjusted:
		adjustDone = true
	// 负向窗口：旧快照停在存储入口时校正不应先完成。窗口短只会漏掉稍晚才越过的缺陷，不会把仍被挡住的校正判失败。
	case <-time.After(100 * time.Millisecond):
	}
	release.Do(func() { close(st.gate) })
	select {
	case err := <-flushed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testwait.Bound):
		t.Fatal("flush did not finish")
	}
	if !adjustDone {
		select {
		case adjustErr = <-adjusted:
		case <-time.After(testwait.Bound):
			t.Fatal("adjust did not finish")
		}
	}
	if adjustErr != nil {
		t.Fatal(adjustErr)
	}
	if st.recs[1].PeriodRx != 1000 || st.writes != 2 {
		t.Fatalf("old flush overwrote adjust: period=%d writes=%d, want 1000/2", st.recs[1].PeriodRx, st.writes)
	}
}

func TestBookPersistsAndResumesWithStore(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 15, 4, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(t.TempDir(), "traffic.db"), clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, _, err := st.CreateNode(t.Context(), store.NewNode{Name: "n"}, []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	b := New(st, clk, shanghai, slog.Default())
	if err := b.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	b.Account(id, counters("b1", 1000, 2000), nil)
	b.Account(id, counters("b1", 1100, 2300), nil)
	if err := b.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	resumed := New(st, clk, shanghai, slog.Default())
	if err := resumed.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d, ok := resumed.Account(id, counters("b1", 1120, 2330), nil); !ok || d != (Delta{Rx: 20, Tx: 30}) {
		t.Fatalf("delta after persisted restart = %+v/%v, want {20 30}/true", d, ok)
	}
	e := resumed.View(id)
	if e.TotalRx != 120 || e.TotalTx != 330 || e.PeriodRx != 120 || e.PeriodTx != 330 {
		t.Fatalf("persisted totals after restart: %+v", e)
	}
	if err := st.DeleteNode(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Adjust(t.Context(), id, 900, 900); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("adjust deleted node = %v, want ErrNotFound", err)
	}
	if got := resumed.View(id); !reflect.DeepEqual(got, e) {
		t.Fatalf("skipped adjust changed memory: got %+v, want %+v", got, e)
	}
}

func TestLoadClampsNegativeUsage(t *testing.T) {
	st := newMem()
	st.recs[1] = store.TrafficRecord{NodeID: 1, TotalRx: -5, TotalTx: -6, PeriodRx: -7, PeriodTx: -8,
		PeriodStart: time.Date(2026, 9, 1, 0, 0, 0, 0, shanghai)}
	b, _ := newBook(t, st)
	e := b.View(1)
	if e.TotalRx != 0 || e.TotalTx != 0 || e.PeriodRx != 0 || e.PeriodTx != 0 {
		t.Fatalf("negative persisted usage survived Load: %+v", e)
	}
}
