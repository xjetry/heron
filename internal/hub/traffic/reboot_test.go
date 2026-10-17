package traffic

import (
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

func TestRebootAccountingBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		uptime  *uint64
		unknown bool
		rx, tx  uint64
		want    Delta
		ok      bool
	}{
		{"both_drop", proto.Uint64(30), false, 10, 20, Delta{10, 20}, true},
		{"rx_equal_tx_drop", proto.Uint64(30), false, 100, 99, Delta{100, 99}, true},
		{"rx_drop_tx_equal", proto.Uint64(30), false, 99, 100, Delta{99, 100}, true},
		{"both_equal", proto.Uint64(30), false, 100, 100, Delta{}, false},
		{"rx_grows", proto.Uint64(30), false, 101, 100, Delta{}, false},
		{"tx_grows", proto.Uint64(30), false, 100, 101, Delta{}, false},
		{"both_grow", proto.Uint64(30), false, 200, 200, Delta{}, false},
		{"other_machine", proto.Uint64(61), false, 10, 20, Delta{}, false},
		{"equal_start", proto.Uint64(60), false, 10, 20, Delta{}, false},
		{"missing_uptime", nil, false, 10, 20, Delta{}, false},
		{"unknown_observation", proto.Uint64(30), true, 10, 20, Delta{}, false},
		{"uptime_after_epoch", proto.Uint64(2_000_000_000), false, 10, 20, Delta{}, false},
		{"max_uptime", proto.Uint64(math.MaxUint64), false, 10, 20, Delta{}, false},
		{"zero_uptime", proto.Uint64(0), false, 10, 20, Delta{10, 20}, true},
		{"one_direction_continues", proto.Uint64(30), false, 110, 1, Delta{110, 1}, true},
	} {
		for _, oldList := range []bool{false, true} {
			for _, newList := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/list_%v_to_%v", tc.name, oldList, newList), func(t *testing.T) {
					b, clk := newBook(t, newMem())
					before := nics(nic{"eth0", 100, 100})
					oldMetrics := totalsOf("old", before)
					if !oldList {
						before = nil
					}
					b.Account(1, oldMetrics, before)
					if tc.unknown {
						b.entries[1].ObservedAt = time.Time{}
					}
					clk.Advance(time.Minute)
					after := nics(nic{"eth0", tc.rx, tc.tx})
					m := totalsOf("new", after)
					if newList {
						m = listOnly("new")
					} else {
						after = nil
					}
					m.UptimeS = tc.uptime
					if d, ok := b.Account(1, m, after); d != tc.want || ok != tc.ok {
						t.Errorf("reboot delta=%+v ok=%v, want %+v ok=%v", d, ok, tc.want, tc.ok)
					}
					e := b.View(1)
					if e.TotalRx != tc.want.Rx || e.TotalTx != tc.want.Tx || e.PeriodRx != tc.want.Rx || e.PeriodTx != tc.want.Tx {
						t.Errorf("usage=%d/%d period=%d/%d, want %+v", e.TotalRx, e.TotalTx, e.PeriodRx, e.PeriodTx, tc.want)
					}
					if e.BootID != "new" || e.perInterface() != newList || len(e.Baseline) != 1 || e.Baseline[0].Rx != int64(tc.rx) || e.Baseline[0].Tx != int64(tc.tx) {
						t.Errorf("new baseline=%+v", e.State)
					}
				})
			}
		}
	}
}

func TestRebootInterfaceSetsUseTotals(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after []*heronv1.NetInterfaceCounter
		want          Delta
	}{
		{"idle_interface", nics(nic{"eth0", 1_000_000, 1_000_000}, nic{"eth1", 0, 0}), nics(nic{"eth0", 1000, 1000}, nic{"eth1", 0, 0}), Delta{1000, 1000}},
		{"shrinking_set", nics(nic{"eth0", 1000, 1000}, nic{"eth1", 1000, 1000}), nics(nic{"eth0", 1100, 1100}), Delta{1100, 1100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, clk := newBook(t, newMem())
			b.Account(1, listOnly("old"), tc.before)
			clk.Advance(time.Minute)
			m := listOnly("new")
			m.UptimeS = proto.Uint64(30)
			account(t, b, "reboot", m, tc.after, tc.want, true)
		})
	}
}

func TestObservedAtTracksOnlyCounterReports(t *testing.T) {
	b, clk := newBook(t, newMem())
	clk.Advance(123 * time.Millisecond)
	var want time.Time
	for _, tc := range []struct {
		name     string
		m        *heronv1.Metrics
		ifaces   []*heronv1.NetInterfaceCounter
		observed bool
	}{
		{"first_baseline", counters("old", 100, 100), nil, true},
		{"increment", counters("old", 200, 200), nil, true},
		{"replace_baseline", counters("new", 10, 10), nil, true},
		{"no_counters", listOnly("new"), nil, false},
		{"partial_counters", &heronv1.Metrics{BootId: "new", NetRxTotal: proto.Uint64(20)}, nil, false},
		{"interface_counters", listOnly("new"), nics(nic{"eth0", 30, 30}), true},
	} {
		clk.Advance(time.Second)
		b.Account(1, tc.m, tc.ifaces)
		if tc.observed {
			want = time.Unix(clk.Now().Unix(), 0).UTC()
		}
		if got := b.View(1).ObservedAt; got != want {
			t.Errorf("%s: observed=%v, want %v", tc.name, got, want)
		}
	}
}

func TestRebootAccountingSurvivesHubRestart(t *testing.T) {
	for _, list := range []bool{false, true} {
		t.Run(fmt.Sprintf("list_%v", list), func(t *testing.T) {
			b, clk := newBook(t, newMem())
			st, err := store.Open(filepath.Join(t.TempDir(), "traffic.db"), clk, slog.Default(), store.MigrateSchema)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			id, _, err := st.CreateNode(t.Context(), store.NewNode{Name: "node"}, []byte{1})
			if err != nil {
				t.Fatal(err)
			}
			b.st = st
			clk.Advance(987 * time.Millisecond)
			before := nics(nic{"eth0", 1000, 2000})
			m := totalsOf("old", before)
			if !list {
				before = nil
			}
			b.Account(id, m, before)
			wantObserved := time.Unix(clk.Now().Unix(), 0).UTC()
			if err := b.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			restarted := New(st, clk, shanghai, slog.Default())
			if err := restarted.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := restarted.View(id).ObservedAt; got != wantObserved {
				t.Errorf("loaded observed=%v, want %v", got, wantObserved)
			}
			clk.Advance(time.Minute)
			after := nics(nic{"eth0", 10, 20})
			m = totalsOf("new", after)
			m.UptimeS = proto.Uint64(30)
			if !list {
				after = nil
			}
			for _, book := range []*Book{b, restarted} {
				if d, ok := book.Account(id, m, after); !ok || d != (Delta{10, 20}) {
					t.Errorf("reboot delta=%+v ok=%v, want {10 20} true", d, ok)
				}
			}
			got, want := restarted.View(id), b.View(id)
			got.PeriodStart, want.PeriodStart = got.PeriodStart.UTC(), want.PeriodStart.UTC()
			if !reflect.DeepEqual(got, want) {
				t.Errorf("restart state=%+v, uninterrupted=%+v", got, want)
			}
			if err := restarted.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := restarted.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			if d, ok := restarted.Account(id, m, after); !ok || d != (Delta{}) {
				t.Errorf("already persisted reboot accounted again: %+v ok=%v", d, ok)
			}
		})
	}
}
