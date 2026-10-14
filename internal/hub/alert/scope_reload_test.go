package alert

import (
	"errors"
	"testing"
	"time"
)

func TestDeletedScopeSurvivesReload(t *testing.T) {
	f := newFixture(t)
	r := offline()
	r.AllNodes, r.NodeIDs = false, f.ids[:1]
	r = f.rule(t, r)
	f.clk.Advance(time.Minute)
	f.sweep(t)
	must(t, f.st.DeleteNode(t.Context(), f.ids[0]))
	f.e.Forget(f.ids[0])
	f.restart(t)
	rules := f.e.Rules()
	if len(rules) != 1 || rules[0].ID != r.ID || rules[0].AllNodes || len(rules[0].NodeIDs) != 0 {
		t.Fatalf("explicit empty rule lost or widened on reload: %+v", rules)
	}
	if len(f.e.States()) != 0 {
		t.Fatalf("deleted node states reloaded: %+v", f.e.States())
	}
	f.clk.Advance(time.Minute)
	f.sweep(t)
	if len(f.e.States()) != 0 {
		t.Fatalf("empty scope created states: %+v", f.e.States())
	}
	_, err := f.e.SaveRule(t.Context(), rules[0])
	var field FieldError
	if !errors.As(err, &field) || field.Path != "node_ids" || field.Constraint != "must not be empty unless all_nodes or selector_tags is set" {
		t.Fatalf("empty save scope error=%v", err)
	}
}
