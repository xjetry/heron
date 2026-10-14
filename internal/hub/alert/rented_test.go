package alert

import (
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

func setRented(t *testing.T, f *fixture, id int64, rented bool, billing store.Billing) {
	t.Helper()
	_, err := f.st.UpdateNode(t.Context(), id, store.NodeEdit{
		Name: "出租节点", Rented: rented, OfflineGraceS: 60, TrafficResetDay: 1, Billing: billing,
	})
	must(t, err)
}

func wantRentedState(t *testing.T, f *fixture, rule, node int64, want store.AlertState) {
	t.Helper()
	if got := stateOf(f.e, rule, node); got != want {
		t.Errorf("memory: node %d rule %d state=%q want %q", node, rule, got, want)
	}
	rows, err := f.st.ListAlertStates(t.Context())
	must(t, err)
	var got store.AlertState
	for _, row := range rows {
		if row.RuleID == rule && row.NodeID == node {
			got = row.State
		}
	}
	if got != want {
		t.Errorf("database: node %d rule %d state=%q want %q", node, rule, got, want)
	}
}

func TestSweepOfflineSkipsRentedAndPrunesExistingState(t *testing.T) {
	for _, initial := range []store.AlertState{store.StatePending, store.StateFiring, store.StateOK} {
		t.Run(string(initial), func(t *testing.T) {
			f := newFixture(t)
			id := f.ids[0]
			setRented(t, f, id, false, store.Billing{})
			r := f.rule(t, offline())
			if initial != store.StatePending {
				f.clk.Advance(2 * time.Minute)
			} else {
				f.clk.Advance(31 * time.Second)
			}
			f.sweep(t)
			want := initial
			if initial == store.StateOK {
				want = store.StateFiring
			}
			wantRentedState(t, f, r.ID, id, want)
			setRented(t, f, id, true, store.Billing{})
			if initial == store.StateOK {
				f.l.Observe(id, "", &heronv1.Metrics{})
			}
			f.sweep(t)
			wantRentedState(t, f, r.ID, id, "")
			f.clk.Advance(10 * time.Minute)
			if initial == store.StateOK {
				f.l.Observe(id, "", &heronv1.Metrics{})
			}
			f.sweep(t)
			wantRentedState(t, f, r.ID, id, "")
		})
	}
}

func TestOfflineGraceRestartsWhenRentedCleared(t *testing.T) {
	f := newFixture(t)
	id := f.ids[0]
	setRented(t, f, id, true, store.Billing{})
	rules := []store.AlertRule{f.rule(t, offline()), f.rule(t, offline())}
	f.clk.Advance(10 * time.Minute)
	f.sweep(t)
	for _, r := range rules {
		wantRentedState(t, f, r.ID, id, "")
	}
	setRented(t, f, id, false, store.Billing{})
	f.sweep(t)
	for _, r := range rules {
		wantRentedState(t, f, r.ID, id, "")
	}
	f.clk.Advance(31 * time.Second)
	f.sweep(t)
	for _, r := range rules {
		wantRentedState(t, f, r.ID, id, store.StatePending)
	}
	f.clk.Advance(30 * time.Second)
	f.sweep(t)
	for _, r := range rules {
		wantRentedState(t, f, r.ID, id, store.StateFiring)
	}
}

func TestSweepExpirySkipsRented(t *testing.T) {
	f := newFixture(t)
	id := f.ids[0]
	billing := store.Billing{ExpiresOn: "2026-09-27"}
	setRented(t, f, id, false, billing)
	r := f.rule(t, expiryRule())
	must(t, f.e.SweepExpiry(t.Context()))
	wantRentedState(t, f, r.ID, id, store.StateFiring)
	setRented(t, f, id, true, billing)
	must(t, f.e.SweepExpiry(t.Context()))
	wantRentedState(t, f, r.ID, id, "")
	f.clk.Advance(24 * time.Hour)
	must(t, f.e.SweepExpiry(t.Context()))
	wantRentedState(t, f, r.ID, id, "")
	setRented(t, f, id, false, billing)
	must(t, f.e.SweepExpiry(t.Context()))
	wantRentedState(t, f, r.ID, id, store.StateFiring)
}

func TestOfflineRentedLowerBoundTracksReports(t *testing.T) {
	for _, report := range []string{"older", "equal", "newer"} {
		t.Run(report, func(t *testing.T) {
			f := newFixture(t)
			id := f.ids[0]
			setRented(t, f, id, true, store.Billing{})
			r := f.rule(t, offline())
			f.l.Observe(id, "", &heronv1.Metrics{})
			f.clk.Advance(10 * time.Minute)
			f.sweep(t)
			f.clk.Advance(10 * time.Minute)
			f.sweep(t)
			until := f.clk.Mono()
			if got, ok := f.e.rentedUntil[id]; !ok || got != until {
				t.Errorf("rented lower bound=(%v, %v), want %v", got, ok, until)
			}
			setRented(t, f, id, false, store.Billing{})
			if report == "newer" {
				f.clk.Advance(10 * time.Second)
			}
			if report != "older" {
				f.l.Observe(id, "", &heronv1.Metrics{})
			}
			f.sweep(t)
			wantRentedState(t, f, r.ID, id, "")
			_, retained := f.e.rentedUntil[id]
			if retained != (report == "older") {
				t.Errorf("report=%s: lower bound retained=%v", report, retained)
			}
			f.clk.Advance(31 * time.Second)
			f.sweep(t)
			wantRentedState(t, f, r.ID, id, store.StatePending)
			f.clk.Advance(30 * time.Second)
			f.sweep(t)
			wantRentedState(t, f, r.ID, id, store.StateFiring)
		})
	}
}
