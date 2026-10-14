package store

import (
	"database/sql"
	"slices"
	"testing"
	"time"
)

// 冻结 rtt 相对基线判定引入后的结构，后续生产 DDL 变化不改变旧版夹具。
var schemaV39 = append(slices.Clone(schemaV38),
	`ALTER TABLE alert_rule ADD COLUMN rtt_mode TEXT`,
	`ALTER TABLE alert_rule ADD COLUMN baseline_mode TEXT`,
	`ALTER TABLE alert_rule ADD COLUMN baseline_window_s INTEGER`,
	`ALTER TABLE alert_rule ADD COLUMN baseline_min_samples INTEGER`,
	`ALTER TABLE alert_rule ADD COLUMN upper_deviation_pct REAL`,
	`ALTER TABLE alert_rule ADD COLUMN lower_deviation_pct REAL`,
	`ALTER TABLE alert_rule ADD COLUMN cooldown_s INTEGER`,
	`ALTER TABLE alert_rule ADD COLUMN fixed_baseline_ms REAL`,
	`ALTER TABLE alert_state ADD COLUMN fired_at INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE alert_baseline (
  rule_id INTEGER NOT NULL,
  node_id INTEGER NOT NULL,
  baseline_us INTEGER NOT NULL,
  buckets INTEGER NOT NULL,
  computed_at INTEGER NOT NULL,
  task_fingerprint TEXT NOT NULL,
  accumulate_from INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (rule_id, node_id)
)`,
)

// removeV39Config 把当前版本的配置层快照撤回到 v38 的结构：回填更早的版本号之前必须撤掉，否则迁移 39 的
// ADD COLUMN 与 CREATE TABLE 会撞上已有的列与表。
func removeV39Config(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		"DROP TABLE alert_baseline", "ALTER TABLE alert_state DROP COLUMN fired_at",
		"ALTER TABLE alert_rule DROP COLUMN rtt_mode", "ALTER TABLE alert_rule DROP COLUMN baseline_mode",
		"ALTER TABLE alert_rule DROP COLUMN baseline_window_s", "ALTER TABLE alert_rule DROP COLUMN baseline_min_samples",
		"ALTER TABLE alert_rule DROP COLUMN upper_deviation_pct", "ALTER TABLE alert_rule DROP COLUMN lower_deviation_pct",
		"ALTER TABLE alert_rule DROP COLUMN cooldown_s", "ALTER TABLE alert_rule DROP COLUMN fixed_baseline_ms",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// 迁移 39：已有 rtt 规则显式成为固定阈值（库里不以 NULL 代表它），丢包与非探测规则的八列为空；已在 firing 的状态以
// since_at 作 fired_at，其余为 0。
func TestMigrationFromV38AddsRelativeBaseline(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 38, func(t *testing.T, db *sql.DB) {
		for _, stmt := range []string{
			"INSERT INTO node (id, name, token_hash, created_at) VALUES (7, 'kept', x'01', 1)",
			"INSERT INTO probe_task (id, kind, target, interval_s, timeout_ms, created_at, config_id) VALUES (5, 1, '1.1.1.1', 10, 1000, 1, x'00000000000000000000000000000001')",
			"INSERT INTO alert_rule (id, name, kind, enabled, all_nodes, task_id, metric, threshold, for_minutes, created_at) VALUES (1, 'rtt', 'probe', 1, 1, 5, 'rtt_ms', 80, 3, 1)",
			"INSERT INTO alert_rule (id, name, kind, enabled, all_nodes, task_id, metric, threshold, for_minutes, created_at) VALUES (2, 'loss', 'probe', 1, 1, 5, 'loss_pct', 20, 3, 1)",
			"INSERT INTO alert_rule (id, name, kind, enabled, all_nodes, created_at) VALUES (3, 'off', 'offline', 1, 1, 1)",
			"INSERT INTO alert_state (rule_id, node_id, state, since_at) VALUES (1, 7, 'firing', 100)",
			"INSERT INTO alert_state (rule_id, node_id, state, since_at) VALUES (2, 7, 'pending', 200)",
		} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	})
	rules, err := s.ListAlertRules(t.Context())
	if err != nil || len(rules) != 3 {
		t.Fatalf("rules after migration: %+v %v", rules, err)
	}
	if rules[0].RttMode != RttThreshold || rules[1].RttMode != "" || rules[2].RttMode != "" {
		t.Fatalf("rtt_mode after migration = %q/%q/%q, want threshold/empty/empty", rules[0].RttMode, rules[1].RttMode, rules[2].RttMode)
	}
	for _, r := range rules {
		if err := CheckKindFields(r); err != nil {
			t.Errorf("migrated rule %d fails the field matrix: %v", r.ID, err)
		}
	}
	states, err := s.ListAlertStates(t.Context())
	if err != nil || len(states) != 2 {
		t.Fatalf("states after migration: %+v %v", states, err)
	}
	if !states[0].FiredAt.Equal(time.Unix(100, 0)) || !states[1].FiredAt.IsZero() {
		t.Fatalf("fired_at after migration = %v/%v, want 100 for the firing state and zero for the pending one", states[0].FiredAt, states[1].FiredAt)
	}
}
