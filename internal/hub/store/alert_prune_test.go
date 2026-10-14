package store

import (
	"bytes"
	"database/sql"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestPruneAlertEventsKeepsBoundaryAndDeletesDeliveries(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	r := saveRule(t, s, AlertRule{Name: "offline", Kind: KindOffline, AllNodes: true})
	before := s.clk.Now().Add(-90 * 24 * time.Hour)
	var events []AlertEvent
	for _, at := range []time.Time{before.Add(-24 * time.Hour), before, before.Add(24 * time.Hour)} {
		ev, err := s.RecordTransition(t.Context(), r.ID, ids[0], StateFiring, "", time.Time{}, AlertEvent{At: at, Transition: TransitionFiring}, newBatches(cs[0].ID, cs[1].ID))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.BeginBatchAttempt(t.Context(), ev.Deliveries[0].BatchID, []int64{ev.Deliveries[0].ID}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateBatch(t.Context(), ev.Deliveries[0].BatchID, DeliveryResult{OK: true, Done: true, DeliveredAt: at}); err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	n, err := s.PruneAlertEvents(t.Context(), before)
	if err != nil || n != 1 {
		t.Fatalf("pruned=%d err=%v", n, err)
	}
	assertAlertRows(t, s, "alert_event", "id = 1", 0)
	assertAlertRows(t, s, "alert_delivery", "event_id = 1", 0)
	for _, ev := range events[1:] {
		got, err := s.GetAlertEvent(t.Context(), ev.ID)
		if err != nil || len(got.Deliveries) != 2 {
			t.Fatalf("retained=%+v err=%v", got, err)
		}
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "unfinished_deliveries=1") {
		t.Fatalf("missing unfinished warning: %s", &logs)
	}
}

func TestPruneAlertEventsRollsBackDeliveriesOnFailure(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Name: "offline", Kind: KindOffline})
	ev := recordEvent(t, s, r.ID, ids[0], []int64{cs[0].ID})
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TRIGGER stop_event_delete BEFORE DELETE ON alert_event BEGIN SELECT RAISE(ABORT, 'deny deletion'); END")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneAlertEvents(t.Context(), s.clk.Now().Add(time.Second)); err == nil {
		t.Fatal("deletion unexpectedly succeeded")
	}
	got, err := s.GetAlertEvent(t.Context(), ev.ID)
	if err != nil || len(got.Deliveries) != 1 {
		t.Fatalf("partial deletion: %+v %v", got, err)
	}
}

// 维护每分钟在写事务里清理事件，全表扫描会随事件量线性占用单写协程。
// 直接检查生产 SQL 的计划，避免新建与迁移同时漏掉索引时结构对照仍通过。
func TestPruneAlertEventsUsesTimeIndex(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"pending_count", countExpiredPendingDeliveries},
		{"deliveries", deleteExpiredDeliveries},
		{"events", deleteExpiredAlertEvents},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := s.r.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+tc.query, s.clk.Now().Unix())
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				details = append(details, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			plan := strings.Join(details, "\n")
			t.Logf("query plan:\n%s", plan)
			if !strings.Contains(plan, "alert_event_by_at") || strings.Contains(plan, "SCAN alert_event") {
				t.Fatalf("cleanup must use alert_event_by_at without scanning alert_event:\n%s", plan)
			}
		})
	}
}

func TestAlertRetentionMinimum(t *testing.T) {
	t.Parallel()
	if DefaultRetention.AlertEvents != 90*24*time.Hour {
		t.Fatalf("default=%v", DefaultRetention.AlertEvents)
	}
	for _, d := range []time.Duration{0, -time.Second, 24*time.Hour - time.Second} {
		r := DefaultRetention
		r.AlertEvents = d
		if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "minimum is 24h0m0s") {
			t.Fatalf("retention %v validation=%v", d, err)
		}
	}
	r := DefaultRetention
	r.AlertEvents = 24 * time.Hour
	if err := r.Validate(); err != nil {
		t.Fatalf("minimum retention rejected: %v", err)
	}
}
