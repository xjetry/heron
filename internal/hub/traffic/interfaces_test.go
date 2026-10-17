package traffic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

type nic struct {
	name   string
	rx, tx uint64
}

func nics(list ...nic) []*heronv1.NetInterfaceCounter {
	out := make([]*heronv1.NetInterfaceCounter, len(list))
	for i, n := range list {
		out[i] = &heronv1.NetInterfaceCounter{Name: n.name, RxBytes: n.rx, TxBytes: n.tx}
	}
	return out
}

// listOnly 是只带逐网卡列表的上报：Metrics 里没有合计与摘要，账本只能从列表推合计视图。
func listOnly(boot string) *heronv1.Metrics { return &heronv1.Metrics{BootId: boot} }

// totalsOf 是一台把同一组网卡只报成合计的 agent（网卡多于上限，或这次没有逐网卡列表）会发的 Metrics。
func totalsOf(boot string, list []*heronv1.NetInterfaceCounter) *heronv1.Metrics {
	sum := agentwire.NetInterfaceSummary(list)
	return &heronv1.Metrics{BootId: boot, NetRxTotal: proto.Uint64(sum.Rx), NetTxTotal: proto.Uint64(sum.Tx), NetCounterEpoch: sum.Epoch}
}

func wantBaseline(t *testing.T, b *Book, id int64, want ...store.TrafficCounter) {
	t.Helper()
	e, ok := b.Get(id)
	if !ok || !slices.Equal(e.Baseline, want) {
		t.Fatalf("baseline = %+v, want %+v", e.Baseline, want)
	}
	if e.perInterface() && e.NetCounterEpoch != "" {
		t.Fatalf("per-interface baseline carries epoch %q", e.NetCounterEpoch)
	}
}

func account(t *testing.T, b *Book, step string, m *heronv1.Metrics, list []*heronv1.NetInterfaceCounter, want Delta, wantOK bool) {
	t.Helper()
	if d, ok := b.Account(1, m, list); ok != wantOK || d != want {
		t.Fatalf("%s: delta %+v ok=%v, want %+v ok=%v", step, d, ok, want, wantOK)
	}
}

// 逐网卡差分：两次都在且不倒退的网卡计入；新出现的只建基线，消失的丢掉，倒退的只重建它自己；同时增删与倒退互不牵连。
func TestPerInterfaceDeltaAddRemoveRegress(t *testing.T) {
	b, _ := newBook(t, newMem())
	account(t, b, "first", listOnly("b1"), nics(nic{"eth0", 100, 100}, nic{"eth1", 1000, 1000}), Delta{}, false)
	account(t, b, "eth1 gone, eth2 new", listOnly("b1"), nics(nic{"eth0", 150, 120}, nic{"eth2", 5000, 5000}), Delta{Rx: 50, Tx: 20}, true)
	wantBaseline(t, b, 1, store.TrafficCounter{Name: "eth0", Rx: 150, Tx: 120}, store.TrafficCounter{Name: "eth2", Rx: 5000, Tx: 5000})
	account(t, b, "eth0 regressed, eth2 grew, eth3 new", listOnly("b1"), nics(nic{"eth0", 10, 10}, nic{"eth2", 5100, 5050}, nic{"eth3", 7, 7}), Delta{Rx: 100, Tx: 50}, true)
	wantBaseline(t, b, 1, store.TrafficCounter{Name: "eth0", Rx: 10, Tx: 10}, store.TrafficCounter{Name: "eth2", Rx: 5100, Tx: 5050}, store.TrafficCounter{Name: "eth3", Rx: 7, Tx: 7})
	account(t, b, "all continue", listOnly("b1"), nics(nic{"eth0", 20, 30}, nic{"eth2", 5100, 5050}, nic{"eth3", 8, 9}), Delta{Rx: 11, Tx: 22}, true)
	account(t, b, "whole set replaced", listOnly("b1"), nics(nic{"wlan0", 1, 1}), Delta{}, false)
	// 新网卡的名字排在已有网卡之前：按名称配对，不能按位置把它当成 wlan0 的延续。
	account(t, b, "new name sorts first", listOnly("b1"), nics(nic{"eth9", 50, 50}, nic{"wlan0", 2, 2}), Delta{Rx: 1, Tx: 1}, true)
	if e, _ := b.Get(1); e.TotalRx != 162 || e.TotalTx != 93 || e.PeriodRx != 162 {
		t.Fatalf("totals = %+v, want 162/93", e)
	}
}

// 收方向正常而发方向倒退的网卡整张重建，不只计它的收方向：两个方向来自同一组计数，一个倒退说明这张卡重置过。
func TestPerInterfaceRxForwardTxBackRebuildsThatInterface(t *testing.T) {
	b, _ := newBook(t, newMem())
	account(t, b, "first", listOnly("b1"), nics(nic{"eth0", 100, 100}, nic{"eth1", 100, 100}), Delta{}, false)
	account(t, b, "eth0 tx back", listOnly("b1"), nics(nic{"eth0", 200, 50}, nic{"eth1", 130, 140}), Delta{Rx: 30, Tx: 40}, true)
	wantBaseline(t, b, 1, store.TrafficCounter{Name: "eth0", Rx: 200, Tx: 50}, store.TrafficCounter{Name: "eth1", Rx: 130, Tx: 140})
	account(t, b, "only eth0, tx back", listOnly("b1"), nics(nic{"eth0", 300, 10}), Delta{}, false)
	account(t, b, "eth0 continues", listOnly("b1"), nics(nic{"eth0", 310, 20}), Delta{Rx: 10, Tx: 10}, true)
}

// 换 boot_id 的上报未带 uptime、合计也未下降：逐网卡与跨形态都只重建基线。
func TestBootChangeRebuildsInEveryForm(t *testing.T) {
	list := nics(nic{"eth0", 100, 100})
	grown := nics(nic{"eth0", 200, 200})
	for _, tc := range []struct {
		name        string
		first, next func(boot string) (*heronv1.Metrics, []*heronv1.NetInterfaceCounter)
	}{
		{"list_to_list", func(b string) (*heronv1.Metrics, []*heronv1.NetInterfaceCounter) { return listOnly(b), list },
			func(b string) (*heronv1.Metrics, []*heronv1.NetInterfaceCounter) { return listOnly(b), grown }},
		{"totals_to_list", func(b string) (*heronv1.Metrics, []*heronv1.NetInterfaceCounter) { return totalsOf(b, list), nil },
			func(b string) (*heronv1.Metrics, []*heronv1.NetInterfaceCounter) { return listOnly(b), grown }},
		{"list_to_totals", func(b string) (*heronv1.Metrics, []*heronv1.NetInterfaceCounter) { return listOnly(b), list },
			func(b string) (*heronv1.Metrics, []*heronv1.NetInterfaceCounter) { return totalsOf(b, grown), nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newBook(t, newMem())
			m, l := tc.first("b1")
			b.Account(1, m, l)
			m, l = tc.next("b2")
			account(t, b, "new boot", m, l, Delta{}, false)
			if e, _ := b.Get(1); e.BootID != "b2" || e.TotalRx != 0 {
				t.Fatalf("after boot change: %+v", e)
			}
		})
	}
}

// 合计基线 → 逐网卡上报：比较列表推出的合计视图，同集合且不倒退时按合计差入账，之后基线换成逐网卡。上报是否同时带
// 合计（双报）不影响结果。集合不同或合计倒退时只重建；空摘要基线与任何列表都对不上。
func TestTotalsBaselineToPerInterface(t *testing.T) {
	before := nics(nic{"eth0", 400, 1000}, nic{"eth1", 600, 1000})
	after := nics(nic{"eth0", 450, 1010}, nic{"eth1", 650, 1090})
	for _, tc := range []struct {
		name   string
		base   *heronv1.Metrics
		report *heronv1.Metrics
		want   Delta
		ok     bool
	}{
		{"list_only", totalsOf("b1", before), listOnly("b1"), Delta{Rx: 100, Tx: 100}, true},
		{"dual", totalsOf("b1", before), totalsOf("b1", after), Delta{Rx: 100, Tx: 100}, true},
		{"other_set", totalsOf("b1", nics(nic{"eth0", 400, 1000})), listOnly("b1"), Delta{}, false},
		{"legacy_epoch", &heronv1.Metrics{BootId: "b1", NetRxTotal: proto.Uint64(1000), NetTxTotal: proto.Uint64(2000)}, listOnly("b1"), Delta{}, false},
		{"regressed", totalsOf("b1", nics(nic{"eth0", 400, 5000}, nic{"eth1", 600, 1000})), listOnly("b1"), Delta{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newBook(t, newMem())
			b.Account(1, tc.base, nil)
			account(t, b, tc.name, tc.report, after, tc.want, tc.ok)
			wantBaseline(t, b, 1, store.TrafficCounter{Name: "eth0", Rx: 450, Tx: 1010}, store.TrafficCounter{Name: "eth1", Rx: 650, Tx: 1090})
		})
	}
}

// 逐网卡基线 → 只带合计的上报（agent 回退版本、网卡多于上限）：同集合且合计不倒退时按合计差入账，不丢区间；集合不同、
// 空摘要或合计倒退时只重建。之后基线换成合计形态，摘要随上报。
func TestPerInterfaceBaselineToTotals(t *testing.T) {
	before := nics(nic{"eth0", 400, 1000}, nic{"eth1", 600, 1000})
	same := totalsOf("b1", nics(nic{"eth0", 450, 1010}, nic{"eth1", 650, 1090}))
	for _, tc := range []struct {
		name   string
		report *heronv1.Metrics
		want   Delta
		ok     bool
	}{
		{"same_set", same, Delta{Rx: 100, Tx: 100}, true},
		{"other_set", totalsOf("b1", nics(nic{"eth0", 2000, 3000})), Delta{}, false},
		{"legacy_epoch", &heronv1.Metrics{BootId: "b1", NetRxTotal: same.NetRxTotal, NetTxTotal: same.NetTxTotal}, Delta{}, false},
		{"regressed", totalsOf("b1", nics(nic{"eth0", 100, 1010}, nic{"eth1", 650, 1090})), Delta{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newBook(t, newMem())
			b.Account(1, listOnly("b1"), before)
			account(t, b, tc.name, tc.report, nil, tc.want, tc.ok)
			e, _ := b.Get(1)
			if e.perInterface() || e.NetCounterEpoch != tc.report.NetCounterEpoch || base(e.State).Rx != int64(tc.report.GetNetRxTotal()) {
				t.Fatalf("baseline after totals report: %+v", e.State)
			}
		})
	}
}

// 计入网卡在 agentwire.MaxNetInterfaces 与多一张之间来回：多出一张时 agent 只报合计。Book 用集合摘要识别合计基线，
// 两侧摘要不同就只重建、不入账，跨越的那一段整段丢失；留在同一侧的上报照常入账。多轮之后累计值恰是"同侧区间之和"。
func TestSixtyFourSixtyFiveRoundsLoseOnlyTheCrossings(t *testing.T) {
	b, _ := newBook(t, newMem())
	var counted []*heronv1.NetInterfaceCounter
	var extra *heronv1.NetInterfaceCounter
	for i := range agentwire.MaxNetInterfaces {
		counted = append(counted, &heronv1.NetInterfaceCounter{Name: fmt.Sprintf("veth%03d", i)})
	}
	extra = &heronv1.NetInterfaceCounter{Name: "veth999"}
	grow := func() {
		for _, c := range append(slices.Clone(counted), extra) {
			c.RxBytes, c.TxBytes = c.RxBytes+1, c.TxBytes+2
		}
	}
	snapshot := func(list []*heronv1.NetInterfaceCounter) []*heronv1.NetInterfaceCounter {
		out := make([]*heronv1.NetInterfaceCounter, len(list))
		for i, c := range list {
			out[i] = proto.Clone(c).(*heronv1.NetInterfaceCounter)
		}
		return out
	}
	under := func() (*heronv1.Metrics, []*heronv1.NetInterfaceCounter) {
		l := snapshot(counted)
		return totalsOf("b1", l), l
	}
	over := func() (*heronv1.Metrics, []*heronv1.NetInterfaceCounter) {
		return totalsOf("b1", snapshot(append(slices.Clone(counted), extra))), nil
	}
	m, l := under()
	account(t, b, "first", m, l, Delta{}, false)
	var want Delta
	for round := range 3 {
		grow()
		m, l = under()
		account(t, b, fmt.Sprintf("round %d: 64 → 64", round), m, l, Delta{Rx: 64, Tx: 128}, true)
		want.Rx, want.Tx = want.Rx+64, want.Tx+128
		grow()
		m, l = over()
		account(t, b, fmt.Sprintf("round %d: 64 → 65 crossing", round), m, l, Delta{}, false)
		grow()
		m, l = over()
		account(t, b, fmt.Sprintf("round %d: 65 → 65", round), m, l, Delta{Rx: 65, Tx: 130}, true)
		want.Rx, want.Tx = want.Rx+65, want.Tx+130
		grow()
		m, l = under()
		account(t, b, fmt.Sprintf("round %d: 65 → 64 crossing", round), m, l, Delta{}, false)
	}
	if e, _ := b.Get(1); e.TotalRx != want.Rx || e.TotalTx != want.Tx {
		t.Fatalf("totals after rounds = %d/%d, want %d/%d", e.TotalRx, e.TotalTx, want.Rx, want.Tx)
	}
}

// 数值口径：逐网卡计数各自截到 MaxInt64；跨形态比较截断后的合计视图。
func TestPerInterfaceNumericBoundaries(t *testing.T) {
	const big = uint64(math.MaxInt64)/2 + 1 // 两项之和越过 MaxInt64，各自不越过
	t.Run("uint64_max_items_clamp", func(t *testing.T) {
		b, _ := newBook(t, newMem())
		account(t, b, "first", listOnly("b1"), nics(nic{"eth0", math.MaxUint64, math.MaxUint64}, nic{"eth1", 5, 5}), Delta{}, false)
		wantBaseline(t, b, 1, store.TrafficCounter{Name: "eth0", Rx: math.MaxInt64, Tx: math.MaxInt64}, store.TrafficCounter{Name: "eth1", Rx: 5, Tx: 5})
		account(t, b, "clamped item stays flat", listOnly("b1"), nics(nic{"eth0", math.MaxUint64, math.MaxUint64}, nic{"eth1", 8, 9}), Delta{Rx: 3, Tx: 4}, true)
	})
	t.Run("increments_saturate", func(t *testing.T) {
		b, _ := newBook(t, newMem())
		account(t, b, "first", listOnly("b1"), nics(nic{"a", 0, 0}, nic{"b", 0, 0}), Delta{}, false)
		account(t, b, "two full increments", listOnly("b1"), nics(nic{"a", math.MaxUint64, 1}, nic{"b", math.MaxUint64, 1}), Delta{Rx: math.MaxInt64, Tx: 2}, true)
		if e, _ := b.Get(1); e.TotalRx != math.MaxInt64 {
			t.Fatalf("total after saturated increment: %+v", e)
		}
	})
	t.Run("items_below_sum_above", func(t *testing.T) {
		b, _ := newBook(t, newMem())
		account(t, b, "first", listOnly("b1"), nics(nic{"a", big, big}, nic{"b", big, big}), Delta{}, false)
		account(t, b, "per-interface keeps exact increments", listOnly("b1"), nics(nic{"a", big + 1, big + 2}, nic{"b", big + 3, big + 4}), Delta{Rx: 4, Tx: 6}, true)
	})
	t.Run("truncated_totals_baseline_then_list", func(t *testing.T) {
		b, _ := newBook(t, newMem())
		before := nics(nic{"a", big, big}, nic{"b", big, big})
		b.Account(1, totalsOf("b1", before), nil)
		if e, _ := b.Get(1); base(e.State).Rx != math.MaxInt64 {
			t.Fatalf("fixture: totals baseline %+v, want truncated", e.State)
		}
		account(t, b, "list against truncated totals", listOnly("b1"), nics(nic{"a", big + 1, big + 1}, nic{"b", big + 1, big + 1}), Delta{}, true)
		account(t, b, "then per-interface", listOnly("b1"), nics(nic{"a", big + 2, big + 3}, nic{"b", big + 4, big + 5}), Delta{Rx: 4, Tx: 6}, true)
	})
	t.Run("item_over_flush_load_back_to_totals", func(t *testing.T) {
		st := newMem()
		b, _ := newBook(t, st)
		over := uint64(math.MaxInt64) + 5
		b.Account(1, listOnly("b1"), nics(nic{"a", over, 1}, nic{"b", 10, 1}))
		if err := b.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := st.recs[1].Baseline; !slices.Equal(got, []store.TrafficCounter{{Name: "a", Rx: math.MaxInt64, Tx: 1}, {Name: "b", Rx: 10, Tx: 1}}) {
			t.Fatalf("flushed baseline %+v", got)
		}
		reloaded, _ := newBook(t, st)
		if d, ok := reloaded.Account(1, totalsOf("b1", nics(nic{"a", over + 7, 2}, nic{"b", 12, 3})), nil); !ok || d != (Delta{Rx: 0, Tx: 3}) {
			t.Fatalf("totals report against a loaded per-interface baseline: %+v ok=%v, want {0 3} true (rx beyond 2^63 is truncated)", d, ok)
		}
	})
	t.Run("adjust_then_cross_form", func(t *testing.T) {
		b, _ := newBook(t, newMem())
		list := nics(nic{"eth0", 100, 100})
		b.Account(1, totalsOf("b1", list), nil)
		if _, err := b.Adjust(t.Context(), 1, 5000, 6000); err != nil {
			t.Fatal(err)
		}
		account(t, b, "list after adjust", listOnly("b1"), nics(nic{"eth0", 130, 140}), Delta{Rx: 30, Tx: 40}, true)
		if e, _ := b.Get(1); e.PeriodRx != 5030 || e.PeriodTx != 6040 {
			t.Fatalf("period after adjust and cross-form delta: %+v", e)
		}
	})
}

// 落盘的是合计基线、内存已换成逐网卡（入账之后、刷出之前崩溃）：重启后只带列表的上报相对落盘的合计基线接上，
// 补回崩溃丢掉的内存增量。落盘的是逐网卡基线时同理按逐网卡补回。
func TestCrashRecoveryAcrossForms(t *testing.T) {
	list := func(rx0, rx1 uint64) []*heronv1.NetInterfaceCounter {
		return nics(nic{"eth0", rx0, rx0}, nic{"eth1", rx1, rx1})
	}
	t.Run("persisted_totals_memory_per_interface", func(t *testing.T) {
		st := newMem()
		b, _ := newBook(t, st)
		b.Account(1, totalsOf("b1", list(100, 100)), nil)
		if err := b.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		b.Account(1, listOnly("b1"), list(150, 120)) // 换成逐网卡，未刷出即崩溃
		restarted, _ := newBook(t, st)
		if d, ok := restarted.Account(1, listOnly("b1"), list(160, 130)); !ok || d != (Delta{Rx: 90, Tx: 90}) {
			t.Fatalf("restart delta %+v ok=%v, want {90 90} true", d, ok)
		}
	})
	t.Run("persisted_per_interface", func(t *testing.T) {
		st := newMem()
		b, _ := newBook(t, st)
		b.Account(1, listOnly("b1"), list(100, 100))
		b.Account(1, listOnly("b1"), list(110, 110))
		if err := b.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		b.Account(1, listOnly("b1"), list(150, 120))
		restarted, _ := newBook(t, st)
		if d, ok := restarted.Account(1, listOnly("b1"), list(160, 130)); !ok || d != (Delta{Rx: 70, Tx: 70}) {
			t.Fatalf("restart delta %+v ok=%v, want {70 70} true", d, ok)
		}
		if e, _ := restarted.Get(1); e.TotalRx != 90 {
			t.Fatalf("restart totals %+v, want 20 persisted + 70 recovered", e)
		}
	})
}

// 网卡计数缺席的上报不入账、不动基线；恢复上报未带 uptime，boot_id 改变时只重建；同周期集合改变也只重建。
func TestMissingCountersThenChangedBootOrSetOnlyRebuilds(t *testing.T) {
	list := nics(nic{"eth0", 100, 100}, nic{"eth1", 100, 100})
	for _, tc := range []struct {
		name string
		m    *heronv1.Metrics
		list []*heronv1.NetInterfaceCounter
	}{
		{"boot_changed_list", listOnly("b2"), nics(nic{"eth0", 500, 500}, nic{"eth1", 500, 500})},
		{"boot_changed_totals", totalsOf("b2", nics(nic{"eth0", 500, 500}, nic{"eth1", 500, 500})), nil},
		{"set_changed_list", listOnly("b1"), nics(nic{"eth2", 500, 500}, nic{"eth3", 500, 500})},
		{"set_changed_totals", totalsOf("b1", nics(nic{"eth0", 500, 500})), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newBook(t, newMem())
			b.Account(1, listOnly("b1"), list)
			before, _ := b.Get(1)
			account(t, b, "missing", &heronv1.Metrics{BootId: "b1"}, nil, Delta{}, false)
			if after, _ := b.Get(1); !reflect.DeepEqual(after, before) {
				t.Fatalf("missing counters moved the baseline: %+v → %+v", before, after)
			}
			account(t, b, tc.name, tc.m, tc.list, Delta{}, false)
			if e, _ := b.Get(1); e.TotalRx != 0 || e.TotalTx != 0 {
				t.Fatalf("invented an increment: %+v", e)
			}
		})
	}
}

// 基线是不可变的值：Get 交出的副本、Committed 的副本与刷出抓到的快照都不会被之后的 Account 改写。gated storage 让
// 刷出停在写库之前，期间入账一次：写下的是相匹配的旧基线与旧累计，第二次刷出或重启恰好补上差额。
func TestFlushSnapshotSurvivesConcurrentAccount(t *testing.T) {
	first := nics(nic{"eth0", 100, 100}, nic{"eth1", 100, 100})
	second := nics(nic{"eth0", 110, 120}, nic{"eth1", 100, 100})
	third := nics(nic{"eth0", 140, 150}, nic{"eth1", 160, 170})
	gatedFlush := func(t *testing.T, st *memStore, b *Book, during func()) {
		t.Helper()
		st.gateOnce = sync.Once{}
		st.gate, st.entered = make(chan struct{}), make(chan struct{})
		flushed := make(chan error, 1)
		go func() { flushed <- b.Flush(context.Background()) }()
		select {
		case <-st.entered:
		case <-time.After(testwait.Bound):
			t.Fatal("flush did not enter storage")
		}
		during()
		close(st.gate)
		if err := <-flushed; err != nil {
			t.Fatal(err)
		}
		st.gate = nil
	}
	setup := func(t *testing.T) (*memStore, *Book, State) {
		st := newMem()
		b, _ := newBook(t, st)
		b.Account(1, listOnly("b1"), first)
		b.Account(1, listOnly("b1"), second)
		e, _ := b.Get(1)
		// 深拷贝：之后的断言要能看见对共享底层数组的原地改写，不能拿同一块内存与自己比。
		old := e.State
		old.Baseline = slices.Clone(old.Baseline)
		return st, b, old
	}
	t.Run("second_flush_and_restart", func(t *testing.T) {
		st, b, old := setup(t)
		held, _ := b.Get(1)
		heldCopy := slices.Clone(held.Baseline)
		gatedFlush(t, st, b, func() {
			if d, ok := b.Account(1, listOnly("b1"), third); !ok || d != (Delta{Rx: 90, Tx: 100}) {
				t.Errorf("account during flush: %+v %v", d, ok)
			}
		})
		if got := st.recs[1]; !slices.Equal(got.Baseline, old.Baseline) || got.TotalRx != old.TotalRx || got.TotalTx != old.TotalTx {
			t.Fatalf("flush wrote %+v, want the state it captured %+v", got, old)
		}
		if got := b.Committed()[1]; !reflect.DeepEqual(got, old) {
			t.Fatalf("committed %+v, want captured %+v", got, old)
		}
		if !slices.Equal(held.Baseline, heldCopy) {
			t.Fatalf("a later Account rewrote the baseline handed out by Get: %+v, was %+v", held.Baseline, heldCopy)
		}
		restarted, _ := newBook(t, st)
		if d, ok := restarted.Account(1, listOnly("b1"), third); !ok || d != (Delta{Rx: 90, Tx: 100}) {
			t.Fatalf("restart after the gated flush: %+v %v, want exactly the unflushed {90 100}", d, ok)
		}
		if err := b.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		now, _ := b.Get(1)
		if got := st.recs[1]; !slices.Equal(got.Baseline, now.Baseline) || got.TotalRx != now.TotalRx || got.TotalRx != old.TotalRx+90 {
			t.Fatalf("second flush wrote %+v, want %+v", got, now.State)
		}
	})
	t.Run("adjust_after_interleave", func(t *testing.T) {
		st, b, _ := setup(t)
		gatedFlush(t, st, b, func() { b.Account(1, listOnly("b1"), third) })
		entryBefore := *b.entries[1]
		committedBefore := b.Committed()[1]
		st.fail = errors.New("disk full")
		if _, err := b.Adjust(t.Context(), 1, 1, 1); err == nil {
			t.Fatal("adjust must report the write failure")
		}
		if !reflect.DeepEqual(*b.entries[1], entryBefore) || !reflect.DeepEqual(b.Committed()[1], committedBefore) {
			t.Fatal("failed adjust changed memory or the committed copy")
		}
		st.fail = nil
		e, err := b.Adjust(t.Context(), 1, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.recs[1]; !slices.Equal(got.Baseline, e.Baseline) || !slices.Equal(e.Baseline, entryBefore.Baseline) || got.PeriodRx != 1 {
			t.Fatalf("adjust wrote %+v; memory baseline %+v, want the baseline unchanged", got, e.Baseline)
		}
		if got := b.Committed()[1]; !reflect.DeepEqual(got, e.State) {
			t.Fatalf("committed after adjust %+v, want %+v", got, e.State)
		}
	})
	t.Run("commit_after_interleave", func(t *testing.T) {
		st, b, _ := setup(t)
		gatedFlush(t, st, b, func() { b.Account(1, listOnly("b1"), third) })
		e, err := b.Commit(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.recs[1]; !slices.Equal(got.Baseline, e.Baseline) || got.TotalRx != e.TotalRx {
			t.Fatalf("commit wrote %+v, want %+v", got, e.State)
		}
	})
	t.Run("forget_during_flush", func(t *testing.T) {
		st, b, _ := setup(t)
		gatedFlush(t, st, b, func() { b.Forget(1) })
		if _, ok := b.Get(1); ok {
			t.Fatal("forgotten entry came back after the flush")
		}
		if _, ok := b.Committed()[1]; ok {
			t.Fatal("flush published a committed copy for a forgotten node")
		}
		account(t, b, "after forget", listOnly("b1"), third, Delta{}, false)
	})
}
