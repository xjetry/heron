package store

import (
	"database/sql"
	"slices"
	"testing"
)

// schemaV29 冻结 v29 的完整 DDL：v28 加上 probe_task.dns_server。逐字冻结，生产 DDL 后续变化不影响它。
var schemaV29 = append(slices.Clone(schemaV28),
	`ALTER TABLE probe_task ADD COLUMN dns_server TEXT NOT NULL DEFAULT ''`,
)

// 旧行的 dns_server 一律取空串：升级前没有 DNS 任务，空串就是"不携带解析器"的既有值，不能迁出别的内容。
func TestMigrationFromV28AddsDNSServerColumn(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 28, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec("INSERT INTO probe_task (kind, target, interval_s, timeout_ms, created_at) VALUES (1, 'legacy.example', 60, 1000, 0)"); err != nil {
			t.Fatal(err)
		}
	})
	var n int
	if err := migrated.r.QueryRow("SELECT count(*) FROM pragma_table_info('probe_task') WHERE name='dns_server'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("probe_task.dns_server present=%d err=%v, want the added column", n, err)
	}
	var dnsServer string
	if err := migrated.r.QueryRow("SELECT dns_server FROM probe_task WHERE target='legacy.example'").Scan(&dnsServer); err != nil {
		t.Fatal(err)
	}
	if dnsServer != "" {
		t.Fatalf("legacy probe task dns_server = %q, want empty", dnsServer)
	}
}
