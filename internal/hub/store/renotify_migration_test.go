package store

import (
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

// 冻结 v50 结构：alert_state 加补发内容两列与 notified_at。
var schemaV50 = append(slices.Clone(schemaV49),
	`ALTER TABLE alert_state ADD COLUMN fired_summary TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE alert_state ADD COLUMN fired_value REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE alert_state ADD COLUMN notified_at INTEGER NOT NULL DEFAULT 0`,
)

// seedRenotifyMigration 写四种 v49 状态行：两条被静默的 firing（一条有配对事件、一条事件已清理）、一条已投递的 firing、
// 一条已恢复的 ok。被静默的那对还留着一条更早的、同样被静默的 firing 事件（之前那轮触发与恢复），回填必须取最新的那条。
func seedRenotifyMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		"INSERT INTO node (id, name, token_hash, created_at) VALUES (1, 'tokyo', x'01', 1), (2, 'osaka', x'02', 1)",
		"INSERT INTO alert_rule (id, name, kind, enabled, all_nodes, created_at) VALUES (1, 'offline', 'offline', 1, 1, 1), (2, 'other', 'offline', 1, 1, 1)",
		"INSERT INTO alert_state (rule_id, node_id, state, since_at, fired_silenced, fired_at) VALUES (1, 1, 'firing', 300, 1, 300)",
		"INSERT INTO alert_state (rule_id, node_id, state, since_at, fired_silenced, fired_at) VALUES (1, 2, 'firing', 310, 1, 310)",
		"INSERT INTO alert_state (rule_id, node_id, state, since_at, fired_silenced, fired_at) VALUES (2, 1, 'firing', 320, 0, 320)",
		"INSERT INTO alert_state (rule_id, node_id, state, since_at, fired_silenced, fired_at, recovered_at) VALUES (2, 2, 'ok', 400, 0, 330, 400)",
		"INSERT INTO alert_event (rule_id, node_id, transition, at, summary, value, silenced) VALUES (1, 1, 'firing', 100, '更早的一次', 1, 1)",
		"INSERT INTO alert_event (rule_id, node_id, transition, at, summary, value, silenced) VALUES (1, 1, 'firing', 300, '节点 tokyo 离线', 42, 1)",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func assertRenotifyMigrated(t *testing.T, s *Store) {
	t.Helper()
	states, err := s.ListAlertStates(t.Context())
	if err != nil || len(states) != 4 {
		t.Fatalf("states after migration: %+v %v", states, err)
	}
	type want struct {
		summary  string
		value    float64
		notified int64
	}
	for i, w := range []want{
		{"节点 tokyo 离线", 42, 0},
		{"节点 osaka 规则 offline 在静默期内触发", 0, 0},
		{"", 0, 320},
		{"", 0, 330},
	} {
		got := states[i]
		var notified int64
		if !got.NotifiedAt.IsZero() {
			notified = got.NotifiedAt.Unix()
		}
		if got.FiredSummary != w.summary || got.FiredValue != w.value || notified != w.notified {
			t.Errorf("state %d/%d after migration: summary=%q value=%v notified_at=%d; want %q %v %d",
				got.RuleID, got.NodeID, got.FiredSummary, got.FiredValue, notified, w.summary, w.value, w.notified)
		}
	}
}

// 迁移 50：被静默的 firing 回填补发内容（配对事件优先，事件已清理时按节点名与规则名拼出）且不开始冷却；
// 其余行的 notified_at 沿用 fired_at，升级前已在进行的冷却不变。
func TestMigrationFromV49BackfillsRenotifyColumns(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 49, seedRenotifyMigration)
	assertRenotifyMigrated(t, s)
}

// 配置快照的恢复走同一组迁移语句；49 版快照里的状态行恢复后与原地迁移的结果相同。
func TestRestoreConfigSnapshotFrom49BackfillsRenotifyColumns(t *testing.T) {
	t.Parallel()
	path, db := frozenSchemaFixture(t, 49)
	seedRenotifyMigration(t, db)
	source := &Store{path: path, clk: clock.NewFake(time.Unix(2_000_000_000, 0))}
	config := filepath.Join(t.TempDir(), "config.db")
	if err := source.snapshot(t.Context(), config, "config", configSnapshotTables, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.db")
	if _, err := Restore(t.Context(), target, config, "", "", time.Now(), slog.Default()); err != nil {
		t.Fatal(err)
	}
	s, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertRenotifyMigrated(t, s)
}

// 补发内容与 notified_at 随快照往返：当前版本的快照恢复后三列原样保留。
func TestSnapshotRenotifyColumnsRoundTrip(t *testing.T) {
	t.Parallel()
	source, _ := open(t)
	for _, query := range []string{
		`INSERT INTO node (id,name,token_hash,created_at) VALUES (1,'node',x'01',1)`,
		`INSERT INTO alert_rule (id,name,kind,all_nodes,created_at) VALUES (1,'offline','offline',1,1)`,
		`INSERT INTO alert_state (rule_id,node_id,state,since_at,fired_silenced,fired_at,fired_summary,fired_value,notified_at) VALUES (1,1,'firing',300,1,300,'原文',7.5,120)`,
	} {
		mustExec(t, source, query)
	}
	config := filepath.Join(t.TempDir(), "config.db")
	if err := source.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.db")
	if _, err := Restore(t.Context(), target, config, "", "", time.Now(), slog.Default()); err != nil {
		t.Fatal(err)
	}
	s, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	states, err := s.ListAlertStates(t.Context())
	if err != nil || len(states) != 1 {
		t.Fatalf("restored states: %+v %v", states, err)
	}
	if got := states[0]; !got.FiredSilenced || got.FiredSummary != "原文" || got.FiredValue != 7.5 || got.NotifiedAt.Unix() != 120 {
		t.Fatalf("restored state=%+v", got)
	}
}

// 写入口：被静默的触发把摘要与观测值留在状态行上、不开始冷却；补发清掉它们、记补发时刻，fired_at 与 since_at 不动，
// 原始事件不动；同一次被静默的触发只能补发一次，已恢复的不能补发。
func TestRecordUnsilencedFiring(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.SaveAlertRule(ctx, AlertRule{Name: "离线", Kind: KindOffline, Enabled: true, AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	fired := clk.Now()
	original, err := s.RecordTransition(ctx, r.ID, id, StateFiring, "", time.Time{}, AlertEvent{Transition: TransitionFiring, At: fired, Summary: "原文", Value: 42, Silenced: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	states, err := s.ListAlertStates(ctx)
	if err != nil || len(states) != 1 {
		t.Fatalf("states: %+v %v", states, err)
	}
	if got := states[0]; !got.FiredSilenced || got.FiredSummary != "原文" || got.FiredValue != 42 || !got.NotifiedAt.IsZero() || !got.FiredAt.Equal(fired.Truncate(time.Second)) {
		t.Fatalf("silenced firing state=%+v", got)
	}

	clk.Advance(time.Hour)
	ev, err := s.RecordUnsilencedFiring(ctx, r.ID, id, AlertEvent{Transition: TransitionRecovered, Silenced: true, At: clk.Now(), Summary: "补发", Value: 42}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Transition != TransitionFiring || ev.Silenced || ev.ID == original.ID {
		t.Fatalf("renotification=%+v; want a new unsilenced firing event", ev)
	}
	states, err = s.ListAlertStates(ctx)
	if err != nil || len(states) != 1 {
		t.Fatalf("states: %+v %v", states, err)
	}
	got := states[0]
	if got.State != StateFiring || got.FiredSilenced || got.FiredSummary != "" || got.FiredValue != 0 {
		t.Fatalf("state after the renotification=%+v", got)
	}
	if !got.NotifiedAt.Equal(ev.At) || !got.FiredAt.Equal(fired.Truncate(time.Second)) || !got.SinceAt.Equal(fired.Truncate(time.Second)) {
		t.Fatalf("state after the renotification=%+v; want notified_at=%v and fired_at/since_at kept at %v", got, ev.At, fired)
	}
	events, err := s.ListAlertEvents(ctx, 0, 0, 10)
	if err != nil || len(events) != 2 || events[1].ID != original.ID || !events[1].Silenced || events[1].Summary != "原文" {
		t.Fatalf("events=%+v %v; want the original kept as is", events, err)
	}
	if _, err := s.RecordUnsilencedFiring(ctx, r.ID, id, AlertEvent{At: clk.Now(), Summary: "再补"}, nil); !errors.Is(err, ErrNoSilencedFiring) {
		t.Fatalf("second renotification err=%v, want ErrNoSilencedFiring", err)
	}
	if _, err := s.RecordTransition(ctx, r.ID, id, StateOK, "", clk.Now(), AlertEvent{Transition: TransitionRecovered, At: clk.Now()}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordUnsilencedFiring(ctx, r.ID, id, AlertEvent{At: clk.Now(), Summary: "恢复后"}, nil); !errors.Is(err, ErrNoSilencedFiring) {
		t.Fatalf("renotification after recovery err=%v, want ErrNoSilencedFiring", err)
	}
	if events, err := s.ListAlertEvents(ctx, 0, 0, 10); err != nil || len(events) != 3 {
		t.Fatalf("events after rejected renotifications=%+v %v; want firing, renotification, recovery", events, err)
	}
}
