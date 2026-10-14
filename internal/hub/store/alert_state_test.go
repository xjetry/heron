package store

import (
	"testing"
	"time"
)

func TestDeleteAlertStateDeletesOnlyThePair(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	a := saveRule(t, s, AlertRule{Name: "a", Kind: KindOffline, AllNodes: true, Enabled: true})
	b := saveRule(t, s, AlertRule{Name: "b", Kind: KindOffline, AllNodes: true, Enabled: true})
	for _, key := range [][2]int64{{a.ID, ids[0]}, {a.ID, ids[1]}, {b.ID, ids[0]}} {
		if err := s.SetAlertState(t.Context(), key[0], key[1], StateFiring, s.clk.Now(), time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := s.DeleteAlertState(t.Context(), a.ID, ids[0]); err != nil {
			t.Fatalf("idempotent delete: %v", err)
		}
	}
	rows, err := s.ListAlertStates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].RuleID != a.ID || rows[0].NodeID != ids[1] || rows[1].RuleID != b.ID || rows[1].NodeID != ids[0] {
		t.Fatalf("single-pair deletion changed other states: %v", rows)
	}
}

func TestSaveAlertRuleClearsStatesOnIdentityChange(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"kind", "task", "metric", "threshold", "name"} {
		t.Run(change, func(t *testing.T) {
			s, ids, _, task := alertFixture(t)
			r := saveRule(t, s, AlertRule{Name: "r", Kind: KindProbe, AllNodes: true, Enabled: true, TaskID: task, Metric: MetricLossPct, Threshold: 20, ForMinutes: 1})
			if err := s.SetAlertState(t.Context(), r.ID, ids[0], StateFiring, s.clk.Now(), time.Time{}); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "kind":
				// 换成离线要同时清掉探测字段：带着它们的离线规则是非法组合，存储层拒绝。
				r = AlertRule{ID: r.ID, Name: r.Name, Kind: KindOffline, AllNodes: true, Enabled: true}
			case "task":
				p, _, err := s.SaveProbeTask(t.Context(), taskForTest(), NodeSelector{AllNodes: false, NodeIDs: nil})
				if err != nil {
					t.Fatal(err)
				}
				r.TaskID = p.Task.Id
			case "metric":
				r.Metric, r.RttMode = MetricRttMs, RttThreshold
			case "threshold":
				r.Threshold = 30
			case "name":
				r.Name = "renamed"
			}
			saveRule(t, s, r)
			rows, err := s.ListAlertStates(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if change == "name" || change == "threshold" {
				want = 1
			}
			if len(rows) != want {
				t.Fatalf("%s change left %v states, want %d", change, rows, want)
			}
		})
	}
}
