package store

import (
	"database/sql"
	"reflect"
	"slices"
	"testing"
	"time"
)

// v11 的完整 DDL：v10 加上维护簿记表。
var schemaV11 = append(slices.Clone(schemaV10), "CREATE TABLE maintenance_state (\n  name TEXT PRIMARY KEY,\n  finished_at INTEGER NOT NULL\n)")

// 旧库的状态行升级后 recovered_at 为 NULL，读出是零值（从未恢复过），不会被读成 1970 年恢复过。
func TestMigrationFromV11AddsRecoveredAtAsNull(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 11, func(t *testing.T, db *sql.DB) {
		seedMinuteRow(t, db)
		for _, stmt := range []string{
			"INSERT INTO alert_rule (id, name, kind, enabled, all_nodes, created_at) VALUES (3, 'r', 'offline', 1, 1, 1)",
			"INSERT INTO alert_state (rule_id, node_id, state, since_at) VALUES (3, 7, 'firing', 1)",
		} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
	})
	if v := userVersion(t, migrated.r); v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	var null bool
	if err := migrated.r.QueryRow("SELECT recovered_at IS NULL FROM alert_state").Scan(&null); err != nil || !null {
		t.Fatalf("recovered_at after migration is null = %v (%v)", null, err)
	}
	want := []StateRow{{RuleID: 3, NodeID: 7, State: StateFiring, SinceAt: time.Unix(1, 0).UTC(), FiredAt: time.Unix(1, 0).UTC()}}
	if states, err := migrated.ListAlertStates(t.Context()); err != nil || !reflect.DeepEqual(states, want) {
		t.Fatalf("states after migration: %+v %v", states, err)
	}
}

// 两个写入口都整行写 recovered_at：给出的时刻原样读回，零值写 NULL、读回零值。
func TestAlertStateWritesRecoveredAt(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	r := saveRule(t, s, AlertRule{Name: "offline", Kind: KindOffline, AllNodes: true, Enabled: true})
	at := time.Unix(1_800_000_000, 0).UTC()
	read := func() StateRow {
		t.Helper()
		rows, err := s.ListAlertStates(ctx)
		if err != nil || len(rows) != 1 {
			t.Fatalf("states = %+v %v", rows, err)
		}
		return rows[0]
	}
	if _, err := s.RecordTransition(ctx, r.ID, id, StateOK, "", at, AlertEvent{Transition: TransitionRecovered, At: at}, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(); !got.RecoveredAt.Equal(at) {
		t.Fatalf("after recovery recovered_at = %v, want %v", got.RecoveredAt, at)
	}
	if err := s.SetAlertState(ctx, r.ID, id, StatePending, at.Add(time.Minute), at); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.State != StatePending || !got.RecoveredAt.Equal(at) {
		t.Fatalf("pending carrying recovered_at = %+v", got)
	}
	if err := s.SetAlertState(ctx, r.ID, id, StatePending, at.Add(time.Minute), time.Time{}); err != nil {
		t.Fatal(err)
	}
	var null bool
	if err := s.r.QueryRow("SELECT recovered_at IS NULL FROM alert_state").Scan(&null); err != nil || !null {
		t.Fatalf("zero recovered_at stored as null = %v (%v)", null, err)
	}
	if got := read(); !got.RecoveredAt.IsZero() {
		t.Fatalf("null recovered_at read back as %v", got.RecoveredAt)
	}
}
