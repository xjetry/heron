package alert

import (
	"slices"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
)

func ruleSilence(f *fixture, ids ...int64) store.Silence {
	return store.Silence{Name: "维护", Enabled: true, AllNodes: true, RuleIDs: ids, Kind: store.SilenceOnce, FromAt: f.clk.Now().Unix(), UntilAt: f.clk.Now().Add(time.Hour).Unix()}
}

func TestSilenceRuleIntersection(t *testing.T) {
	f := newFixture(t)
	a, b := f.nodeRule(t), f.nodeRule(t)
	f.silence(t, ruleSilence(f, a.ID))
	f.clk.Advance(time.Minute)
	f.sweep(t)
	for _, r := range []store.AlertRule{a, b} {
		found := false
		for _, ev := range f.events(t) {
			if ev.RuleID != r.ID {
				continue
			}
			found = true
			want := r.ID == a.ID
			if ev.Silenced != want || (len(ev.Deliveries) == 0) != want {
				t.Fatalf("rule %d silenced=%v deliveries=%d, want silenced=%v", r.ID, ev.Silenced, len(ev.Deliveries), want)
			}
		}
		if !found {
			t.Fatalf("no event for rule %d", r.ID)
		}
	}
}

func TestSilenceAllRulesCoversFutureRules(t *testing.T) {
	f := newFixture(t)
	si := ruleSilence(f)
	si.AllRules = true
	f.silence(t, si)
	r := f.nodeRule(t)
	if ev := f.fireOffline(t, r, f.ids[0]); !ev.Silenced || len(ev.Deliveries) != 0 {
		t.Fatalf("future rule not silenced: %+v", ev)
	}
}

func TestSilenceDeletedRulesNeverWiden(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "memory"
		if restart {
			name = "load"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			a := f.nodeRule(t)
			si := f.silence(t, ruleSilence(f, a.ID))
			must(t, f.e.DeleteRule(t.Context(), a.ID))
			if restart {
				f.restart(t)
			}
			f.e.mu.RLock()
			cached := cloneSilence(f.e.silences[si.ID])
			f.e.mu.RUnlock()
			if cached.AllRules || len(cached.RuleIDs) != 0 {
				t.Fatalf("deleted rule remains in snapshot: %+v", cached)
			}
			b := f.nodeRule(t)
			if ev := f.fireOffline(t, b, f.ids[0]); ev.Silenced || len(ev.Deliveries) != 1 {
				t.Fatalf("empty scope suppressed new rule: %+v", ev)
			}
		})
	}
}

func TestSilenceRuleSnapshotOwnership(t *testing.T) {
	f := newFixture(t)
	a, b := f.nodeRule(t), f.nodeRule(t)
	saved := f.silence(t, ruleSilence(f, a.ID))
	saved.RuleIDs[0] = b.ID
	f.e.mu.RLock()
	got := slices.Clone(f.e.silences[saved.ID].RuleIDs)
	f.e.mu.RUnlock()
	if !slices.Equal(got, []int64{a.ID}) {
		t.Errorf("saved response aliases snapshot: %v", got)
	}
	loaded := ruleSilence(f, a.ID)
	loaded.ID = saved.ID
	f.e.publishSilences([]store.Silence{loaded})
	loaded.RuleIDs[0] = b.ID
	f.e.mu.RLock()
	got = slices.Clone(f.e.silences[saved.ID].RuleIDs)
	f.e.mu.RUnlock()
	if !slices.Equal(got, []int64{a.ID}) {
		t.Errorf("published input aliases snapshot: %v", got)
	}
}

func TestMaintenanceCoversEveryRule(t *testing.T) {
	f := newFixture(t)
	a, b := f.nodeRule(t), f.nodeRule(t)
	f.silence(t, ruleSilence(f, a.ID))
	f.setMaintenance(t, f.ids[0], true)
	f.clk.Advance(time.Minute)
	f.sweep(t)
	events := f.events(t)
	if len(events) != 2 {
		t.Fatalf("events=%+v", events)
	}
	for _, ev := range events {
		if !ev.Silenced || len(ev.Deliveries) != 0 {
			t.Fatalf("maintenance did not cover rule %d (rules %d/%d): %+v", ev.RuleID, a.ID, b.ID, ev)
		}
	}
}
