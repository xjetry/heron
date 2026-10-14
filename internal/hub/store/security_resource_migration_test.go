package store

import (
	"database/sql"
	"slices"
	"testing"
)

// 测试冻结独立文本，不引用当前 DDL 或生产迁移，以便比较两条建库路径。
var schemaV20 = func() []string {
	out := append(slices.Clone(schemaV19),
		`CREATE TABLE admin_security (id INTEGER PRIMARY KEY CHECK (id = 1), generation INTEGER NOT NULL DEFAULT 0, data TEXT NOT NULL DEFAULT '{}')`,
		`INSERT INTO admin_security (id) VALUES (1)`,
		`CREATE TABLE probe_task_tag (task_id INTEGER NOT NULL, tag_id INTEGER NOT NULL, PRIMARY KEY(task_id,tag_id)) WITHOUT ROWID`,
		`CREATE INDEX probe_task_tag_by_tag ON probe_task_tag(tag_id)`,
		`CREATE TABLE alert_rule_tag (rule_id INTEGER NOT NULL, tag_id INTEGER NOT NULL, PRIMARY KEY(rule_id,tag_id)) WITHOUT ROWID`,
		`CREATE INDEX alert_rule_tag_by_tag ON alert_rule_tag(tag_id)`,
		`ALTER TABLE alert_rule ADD COLUMN resource_metric TEXT`,
		`ALTER TABLE alert_rule ADD COLUMN recovery_threshold REAL`,
	)
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		out = append(out,
			"ALTER TABLE "+table+" ADD COLUMN memory_used_pct_sum REAL NOT NULL DEFAULT 0",
			"ALTER TABLE "+table+" ADD COLUMN memory_used_pct_n INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE "+table+" ADD COLUMN disk_used_pct_sum REAL NOT NULL DEFAULT 0",
			"ALTER TABLE "+table+" ADD COLUMN disk_used_pct_n INTEGER NOT NULL DEFAULT 0")
	}
	return out
}()

func TestMigrationFromV19PreservesMetricsAndStartsWithoutFactors(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 19, func(t *testing.T, db *sql.DB) {
		seedMinuteRow(t, db)
		if _, err := db.Exec("INSERT INTO admin VALUES (1,'kept-password',1)"); err != nil {
			t.Fatal(err)
		}
	})
	security, err := s.AdminSecurity(t.Context())
	if err != nil || security.PasswordHash != "kept-password" || security.Generation != 0 || security.Data != "{}" {
		t.Fatalf("security migration: %+v %v", security, err)
	}
	var cpu float64
	var memN, diskN int
	if err := s.r.QueryRow("SELECT cpu_sum, memory_used_pct_n, disk_used_pct_n FROM metric_1m WHERE node_id=7").Scan(&cpu, &memN, &diskN); err != nil {
		t.Fatal(err)
	}
	if cpu != 50 || memN != 0 || diskN != 0 {
		t.Fatalf("metric migration: cpu=%v memory_n=%d disk_n=%d", cpu, memN, diskN)
	}
}
