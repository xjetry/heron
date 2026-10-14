package store

import (
	"database/sql"
	"slices"
	"strings"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// schemaV35 冻结 v35 的完整 DDL：v34 重建 probe_task（pin 与 config_id）、probe_cert 加 config_id、
// 新建 probe_cert_presented。逐字冻结，生产 DDL 后续变化不影响它。
// 冻结夹具里没有任务行，INSERT SELECT 不产生行；config_id 的 NOT NULL 只在有行时检查。
var schemaV35 = func() []string {
	ddl := strings.Replace(ddlProbeTaskV35, "CREATE TABLE probe_task (", "CREATE TABLE probe_task_new (", 1)
	return append(slices.Clone(schemaV34),
		ddl,
		`INSERT INTO probe_task_new (id, kind, target, interval_s, timeout_ms, created_at, all_nodes, sort_order, dns_server, cert_spki_sha256, config_id)
			SELECT id, kind, target, interval_s, timeout_ms, created_at, all_nodes, sort_order, dns_server, NULL, randomblob(16) FROM probe_task`,
		`DROP TABLE probe_task`,
		`ALTER TABLE probe_task_new RENAME TO probe_task`,
		`ALTER TABLE probe_cert ADD COLUMN config_id BLOB`,
		ddlProbeCertPresentedV35,
	)
}()

func TestMigrationFromV34AssignsConfigIDAndBumpsVersion(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 34, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec(`INSERT INTO probe_task (id, kind, target, interval_s, timeout_ms, created_at) VALUES (7, 1, 'legacy.example', 60, 1000, 1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`DELETE FROM probe_task WHERE id = 7`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO probe_task (id, kind, target, interval_s, timeout_ms, created_at) VALUES (4, 3, 'https://legacy.example', 60, 1000, 1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO probe_cert (node_id, task_id, not_after, observed_at) VALUES (1, 4, 100, 1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE probe_meta SET version = 42`); err != nil {
			t.Fatal(err)
		}
	})
	var pin []byte
	var cid []byte
	if err := migrated.r.QueryRow(`SELECT cert_spki_sha256, config_id FROM probe_task WHERE id = 4`).Scan(&pin, &cid); err != nil {
		t.Fatal(err)
	}
	if pin != nil || len(cid) != 16 {
		t.Fatalf("migrated task pin=%x config_id len=%d, want NULL pin and 16-byte id", pin, len(cid))
	}
	var certs int
	if err := migrated.r.QueryRow(`SELECT count(*) FROM probe_cert WHERE task_id = 4`).Scan(&certs); err != nil {
		t.Fatal(err)
	}
	if certs != 0 {
		t.Fatalf("old certificate observations = %d, want none without identity", certs)
	}
	var presented int
	if err := migrated.r.QueryRow(`SELECT count(*) FROM probe_cert_presented`).Scan(&presented); err != nil || presented != 0 {
		t.Fatalf("presented rows=%d err=%v, want empty table", presented, err)
	}
	var version int64
	if err := migrated.r.QueryRow(`SELECT version FROM probe_meta WHERE id = 1`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version <= 42 {
		t.Fatalf("probe_meta version = %d, want a bump above 42 so agents refetch tasks that now carry an identity", version)
	}
	var seq int64
	if err := migrated.r.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'probe_task'`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq < 7 {
		t.Fatalf("sqlite_sequence = %d, want at least 7: a deleted task id must not be reused", seq)
	}
	next, _, err := migrated.SaveProbeTask(t.Context(), &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "next.example", IntervalS: 60, TimeoutMs: 1000}, NodeSelector{})
	if err != nil {
		t.Fatal(err)
	}
	if next.Task.GetId() <= 7 {
		t.Fatalf("next task id = %d, want > 7", next.Task.GetId())
	}
}

func TestMigrationFromV34KeepsVersionWhenThereAreNoTasks(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 34, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec(`UPDATE probe_meta SET version = 3`); err != nil {
			t.Fatal(err)
		}
	})
	var version int64
	if err := migrated.r.QueryRow(`SELECT version FROM probe_meta WHERE id = 1`).Scan(&version); err != nil || version != 3 {
		t.Fatalf("version = %d err=%v, want 3: an empty task list did not change", version, err)
	}
}
