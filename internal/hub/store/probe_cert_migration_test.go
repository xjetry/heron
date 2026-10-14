package store

import (
	"database/sql"
	"slices"
	"testing"
)

// schemaV30 冻结 v30 的完整 DDL：v29 加上 probe_cert。逐字冻结，生产 DDL 后续变化不影响它。
var schemaV30 = append(slices.Clone(schemaV29),
	`CREATE TABLE probe_cert (
  node_id INTEGER NOT NULL,
  task_id INTEGER NOT NULL,
  not_after INTEGER NOT NULL,
  observed_at INTEGER NOT NULL,
  PRIMARY KEY (node_id, task_id)
) WITHOUT ROWID`,
)

// 旧库升级前没有证书观测，升级后的 probe_cert 必须是空表："没有行"即"无读数"，
// 不是任何证书状态，证书到期评估对无行的 (节点, 任务) 不评估。
func TestMigrationFromV29CreatesProbeCert(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 29, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec("INSERT INTO node (name, token_hash, created_at) VALUES ('legacy', x'01', 0)"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO probe_task (kind, target, interval_s, timeout_ms, created_at) VALUES (1, 'https://legacy.example', 60, 1000, 0)"); err != nil {
			t.Fatal(err)
		}
	})
	var n int
	if err := migrated.r.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='probe_cert'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("probe_cert present=%d err=%v, want the added table", n, err)
	}
	var rows int
	if err := migrated.r.QueryRow("SELECT count(*) FROM probe_cert").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("probe_cert rows = %d, want 0: pre-upgrade databases have no cert observations", rows)
	}
}
