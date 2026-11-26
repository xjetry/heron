package alert

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
)

func TestNextTraffic(t *testing.T) {
	for _, tc := range []struct {
		name string
		cur  store.AlertState
		o    TrafficObservation
		next store.AlertState
		tr   store.Transition
	}{
		{"equal", store.StateOK, TrafficObservation{Current: true, QuotaBytes: 100, Percent: 80, Threshold: 80}, store.StateFiring, store.TransitionFiring},
		{"below", store.StateFiring, TrafficObservation{Current: true, QuotaBytes: 100, Percent: 79, Threshold: 80}, store.StateOK, store.TransitionRecovered},
		{"disabled", store.StateFiring, TrafficObservation{Current: true, Percent: 90, Threshold: 80}, store.StateOK, store.TransitionRecovered},
		{"missing entry", store.StateFiring, TrafficObservation{Current: true, QuotaBytes: 100, Threshold: 80}, store.StateOK, store.TransitionRecovered},
		{"expired", store.StateFiring, TrafficObservation{QuotaBytes: 100, Threshold: 80}, store.StateFiring, ""},
		{"future", store.StateOK, TrafficObservation{QuotaBytes: 100, Percent: 90, Threshold: 80}, store.StateOK, ""},
		{"restart firing", store.StateFiring, TrafficObservation{Current: true, QuotaBytes: 100, Percent: 90, Threshold: 80}, store.StateFiring, ""},
		{"100 percent", store.StateOK, TrafficObservation{Current: true, QuotaBytes: 100, Percent: 100, Threshold: 100}, store.StateFiring, store.TransitionFiring},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, tr := NextTraffic(tc.cur, tc.o)
			got := store.Transition("")
			if tr != nil {
				got = *tr
			}
			if next != tc.next || got != tc.tr {
				t.Fatalf("got %s/%s want %s/%s", next, got, tc.next, tc.tr)
			}
		})
	}
}

func TestTrafficRuleFields(t *testing.T) {
	r := store.AlertRule{Name: "流量", Kind: store.KindTraffic, AllNodes: true, Threshold: 80}
	for _, v := range []float64{0, -1, 101, math.Inf(1), math.NaN()} {
		bad := r
		bad.Threshold = v
		if err := CheckRule(bad); err == nil {
			t.Fatalf("threshold %g accepted", v)
		}
	}
	for _, change := range []func(*store.AlertRule){
		func(r *store.AlertRule) { r.TaskID = 1 }, func(r *store.AlertRule) { r.Metric = store.MetricLossPct },
		func(r *store.AlertRule) { r.ForMinutes = 1 }, func(r *store.AlertRule) { r.DaysBefore = 1 },
		func(r *store.AlertRule) { r.ResourceMetric = store.MetricCpuPct }, func(r *store.AlertRule) { r.RecoveryThreshold = 1 },
	} {
		bad := r
		change(&bad)
		if err := CheckRule(bad); err == nil {
			t.Fatalf("invalid fields accepted: %+v", bad)
		}
	}
	f := newFixture(t)
	saved := f.rule(t, r)
	rows, err := f.st.ListAlertRules(t.Context())
	must(t, err)
	if len(rows) != 1 || rows[0].Threshold != 80 || saved.Threshold != 80 {
		t.Fatalf("threshold not persisted: %+v", rows)
	}
}

func TestTrafficCommittedEvaluationAndPartialScope(t *testing.T) {
	f := newFixture(t)
	b := traffic.New(f.st, f.clk, f.loc, f.log)
	must(t, b.Load(t.Context()))
	f.e.SetTraffic(b)
	queries := 0
	f.e.monitoringNodes = func(ctx context.Context) ([]store.Node, error) { queries++; return f.st.ListMonitoringNodes(ctx) }
	for _, id := range f.ids {
		_, err := f.st.UpdateNode(t.Context(), id, store.NodeEdit{Name: "quota", TrafficResetDay: 1, TrafficQuotaBytes: 100, TrafficQuotaMode: "sum"})
		must(t, err)
		_, err = b.Adjust(t.Context(), id, 60, 30)
		must(t, err)
	}
	r := f.rule(t, store.AlertRule{Name: "流量", Kind: store.KindTraffic, Enabled: true, AllNodes: true, Threshold: 80})
	if evs := f.events(t); len(evs) != 2 || evs[0].ValueUnit != metric.UnitPercent || evs[1].ValueUnit != metric.UnitPercent {
		t.Fatalf("save rule events=%+v", evs)
	}
	queries = 0
	f.sweep(t)
	if queries != 1 {
		t.Fatalf("offline and traffic sweep made %d node queries, want 1", queries)
	}
	before := f.e.entry(stateKey{r.ID, f.ids[1]})
	_, err := b.Adjust(t.Context(), f.ids[0], 10, 10)
	must(t, err)
	must(t, f.e.EvaluateTrafficNode(t.Context(), f.ids[0]))
	if got := f.e.entry(stateKey{r.ID, f.ids[1]}); !reflect.DeepEqual(got, before) || len(f.events(t)) != 3 {
		t.Fatalf("partial evaluation altered peer: %+v, events=%d", got, len(f.events(t)))
	}
	if events := f.events(t); !strings.Contains(events[0].Summary, "20.00% 已低于阈值") || events[0].ValueUnit != metric.UnitPercent {
		t.Fatalf("recovery=%+v", events[0])
	}
	f.restart(t)
	f.e.SetTraffic(b)
	must(t, f.e.SweepTraffic(t.Context()))
	if len(f.events(t)) != 3 {
		t.Fatal("restart repeated transitions")
	}
	f.clk.Advance(40 * 24 * time.Hour)
	f.sweep(t)
	if len(f.events(t)) != 3 {
		t.Fatal("uncommitted rollover recovered")
	}
	must(t, b.Flush(t.Context()))
	f.sweep(t)
	if evs := f.events(t); len(evs) != 4 || evs[0].Value != 0 || evs[0].ValueUnit != metric.UnitPercent {
		t.Fatalf("committed rollover recovery=%+v", evs)
	}
	// 清除流量配额触发的恢复没有配额占比可读，落 0 与 n/a（与周期重置的 0% 区分）。
	_, err = b.Adjust(t.Context(), f.ids[0], 90, 0)
	must(t, err)
	must(t, f.e.EvaluateTrafficNode(t.Context(), f.ids[0]))
	_, err = f.st.UpdateNode(t.Context(), f.ids[0], store.NodeEdit{Name: "quota", TrafficResetDay: 1})
	must(t, err)
	must(t, f.e.EvaluateTrafficNode(t.Context(), f.ids[0]))
	if evs := f.events(t); len(evs) != 6 || evs[0].Transition != store.TransitionRecovered || evs[0].Value != 0 || evs[0].ValueUnit != store.ValueUnitNotApplicable {
		t.Fatalf("cleared quota recovery=%+v", evs)
	}
	r.AllNodes = false
	r.NodeIDs = []int64{f.ids[0]}
	f.rule(t, r)
	if stateOf(f.e, r.ID, f.ids[1]) != "" {
		t.Fatal("removed scope state retained")
	}
}

func TestTrafficSummaryFacts(t *testing.T) {
	n := store.Node{Name: "node", TrafficQuotaMode: "rx"}
	r := store.AlertRule{Name: "rule", Threshold: 80}
	firing, recovery := store.TransitionFiring, store.TransitionRecovered
	o := TrafficObservation{QuotaBytes: 1 << 30, UsedBytes: 900 << 20, Percent: 87.89}
	for _, part := range []string{"87.89%", "900 MiB", "1 GiB", "只收", "rule"} {
		if got, rd := trafficSummary(n, r, o, &firing); !strings.Contains(got, part) || rd != observedValue(87.89) {
			t.Fatalf("missing %q in %s (rd=%+v)", part, got, rd)
		}
	}
	o.Percent = 3
	if got, rd := trafficSummary(n, r, o, &recovery); !strings.Contains(got, "3.00% 已低于阈值 80.00%") || strings.Contains(got, "重置") || rd != observedValue(3) {
		t.Fatalf("got=%q rd=%+v", got, rd)
	}
	o.QuotaBytes = 0
	if got, rd := trafficSummary(n, r, o, &recovery); !strings.Contains(got, "已清除流量配额") || rd != noReading {
		t.Fatalf("got=%q rd=%+v", got, rd)
	}
}

func TestTrafficSilenceMaintenanceAndRetry(t *testing.T) {
	for _, mode := range []string{"maintenance", "silence", "retry"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			b := traffic.New(f.st, f.clk, f.loc, f.log)
			must(t, b.Load(t.Context()))
			f.e.SetTraffic(b)
			edit := store.NodeEdit{Name: "quota", TrafficResetDay: 1, TrafficQuotaBytes: 100, TrafficQuotaMode: "sum", Maintenance: mode == "maintenance"}
			_, err := f.st.UpdateNode(t.Context(), f.ids[0], edit)
			must(t, err)
			if mode == "silence" {
				f.silence(t, store.Silence{AllRules: true, Name: "quiet", Enabled: true, AllNodes: true, Kind: store.SilenceOnce, FromAt: f.clk.Now().Add(-time.Minute).Unix(), UntilAt: f.clk.Now().Add(time.Hour).Unix()})
			}
			r := f.rule(t, store.AlertRule{Name: "quota", Kind: store.KindTraffic, Enabled: true, NodeIDs: f.ids[:1], Threshold: 80})
			_, err = b.Adjust(t.Context(), f.ids[0], 90, 0)
			must(t, err)
			if mode == "retry" {
				deliverySQL(t, deliveryDB(t, f), "CREATE TRIGGER reject_quota BEFORE INSERT ON alert_event BEGIN SELECT RAISE(ABORT,'event unavailable'); END")
			}
			err = f.e.SweepOffline(t.Context())
			if mode == "retry" {
				if err == nil || len(f.events(t)) != 0 || stateOf(f.e, r.ID, f.ids[0]) == store.StateFiring {
					t.Fatalf("failed write published: err=%v events=%v", err, f.events(t))
				}
				deliverySQL(t, deliveryDB(t, f), "DROP TRIGGER reject_quota")
				f.sweep(t)
			} else {
				must(t, err)
			}
			_, err = b.Adjust(t.Context(), f.ids[0], 10, 0)
			must(t, err)
			edit.Maintenance = false
			_, err = f.st.UpdateNode(t.Context(), f.ids[0], edit)
			must(t, err)
			f.sweep(t)
			events := f.events(t)
			if len(events) != 2 || events[0].Transition != store.TransitionRecovered || events[1].Transition != store.TransitionFiring {
				t.Fatalf("pair=%+v", events)
			}
			for _, ev := range events {
				if ev.Silenced != (mode != "retry") {
					t.Fatalf("%s: silenced=%t", ev.Transition, ev.Silenced)
				}
			}
		})
	}
}
