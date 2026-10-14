package store

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

// 冻结证书身份约束加入后的完整结构，不随生产 DDL 演进。
var schemaV45 = append(slices.Clone(schemaV44),
	`DELETE FROM probe_cert WHERE config_id IS NULL`,
	`CREATE TABLE probe_cert_new (
  node_id INTEGER NOT NULL,
  task_id INTEGER NOT NULL,
  not_after INTEGER NOT NULL,
  observed_at INTEGER NOT NULL,
  config_id BLOB NOT NULL,
  PRIMARY KEY (node_id, task_id)
) WITHOUT ROWID`,
	`INSERT INTO probe_cert_new SELECT node_id, task_id, not_after, observed_at, config_id FROM probe_cert`,
	`DROP TABLE probe_cert`,
	`ALTER TABLE probe_cert_new RENAME TO probe_cert`,
)

type identityCertRow struct {
	Node, Task, NotAfter, Observed int64
	Config                         []byte
}

func identityCertRows(t *testing.T, db *sql.DB) []identityCertRow {
	t.Helper()
	rows, err := db.Query("SELECT node_id, task_id, not_after, observed_at, config_id FROM probe_cert ORDER BY node_id, task_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []identityCertRow
	for rows.Next() {
		var row identityCertRow
		if err := rows.Scan(&row.Node, &row.Task, &row.NotAfter, &row.Observed, &row.Config); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func seedIdentityCerts(t *testing.T, db *sql.DB) {
	t.Helper()
	seedMinuteRow(t, db)
	for _, q := range []string{
		`INSERT INTO metric_5m (node_id, ts, cpu_sum, cpu_n, cpu_max, tcp_active_opens_ps_sum, tcp_active_opens_ps_n) VALUES (7, 300, 91, 2, 49, 3, 1)`,
		`INSERT INTO metric_1h (node_id, ts, cpu_sum, cpu_n, cpu_max, tcp_out_segs_ps_sum, tcp_out_segs_ps_n) VALUES (7, 3600, 194, 4, 51, 7, 2)`,
		`INSERT INTO probe_task (id, kind, target, interval_s, timeout_ms, created_at, all_nodes, config_id) VALUES (4, 3, 'https://a.example', 60, 1000, 1, 1, X'0102'), (5, 3, 'https://b.example', 60, 1000, 2, 1, X'0304'), (6, 3, 'https://c.example', 60, 1000, 3, 1, X'0506')`,
		`INSERT INTO probe_cert (node_id, task_id, not_after, observed_at, config_id) VALUES (7, 4, 101, 11, NULL), (7, 5, 202, 22, X'0304'), (7, 6, 303, 33, X'0506')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func assertIdentityCerts(t *testing.T, db *sql.DB, want []identityCertRow) {
	t.Helper()
	if got := identityCertRows(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("certificate rows = %+v, want %+v", got, want)
	}
	_, err := db.Exec(`INSERT INTO probe_cert (node_id, task_id, not_after, observed_at, config_id) VALUES (8, 4, 404, 44, NULL)`)
	if err == nil || !strings.Contains(err.Error(), "NOT NULL constraint failed: probe_cert.config_id") {
		t.Fatalf("NULL identity insert = %v, want probe_cert.config_id NOT NULL failure", err)
	}
}

func TestMigration45DropsNullIdentity(t *testing.T) {
	t.Parallel()
	var want []identityCertRow
	s := migrateFrom(t, 44, func(t *testing.T, db *sql.DB) {
		seedIdentityCerts(t, db)
		want = identityCertRows(t, db)[1:]
	})
	assertIdentityCerts(t, s.w, want)
}

func TestMigration45MatchesFreshSchema(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 44, seedIdentityCerts)
	if got := userVersion(t, s.r); got != 45 {
		t.Fatalf("schema version = %d, want 45", got)
	}
}

func TestMigration45RollsBackOnFailure(t *testing.T) {
	t.Parallel()
	path, db := frozenSchemaFixture(t, 44)
	seedIdentityCerts(t, db)
	want := identityCertRows(t, db)
	if _, err := db.Exec(`CREATE TABLE probe_cert_new (conflict INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path, clock.Real(), slog.Default(), MigrateSchema); err == nil {
		s.Close()
		t.Fatal("migration succeeded despite conflicting rebuild table")
	} else if !strings.Contains(err.Error(), "probe_cert_new already exists") {
		t.Fatalf("unexpected migration failure: %v", err)
	}
	if got := userVersion(t, db); got != 44 {
		t.Fatalf("failed migration version = %d, want 44", got)
	}
	if got := identityCertRows(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("failed migration changed rows: %+v, want %+v", got, want)
	}
}

// 快照从冻结版本的真实库导出，版本元数据和表结构由同一份夹具保证。
func identitySnapshots44(t *testing.T) (string, string, []identityCertRow) {
	t.Helper()
	path, db := frozenSchemaFixture(t, 44)
	seedIdentityCerts(t, db)
	want := identityCertRows(t, db)[1:]
	source := &Store{path: path, clk: clock.NewFake(time.Unix(2_000_000_000, 0))}
	config, metrics := filepath.Join(t.TempDir(), "config.db"), filepath.Join(t.TempDir(), "metrics.db")
	if err := source.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if err := source.SnapshotMetrics(t.Context(), metrics); err != nil {
		t.Fatal(err)
	}
	return config, metrics, want
}

func TestRestoreConfigSnapshotFrom44(t *testing.T) {
	t.Parallel()
	config, _, want := identitySnapshots44(t)
	for _, target := range []string{filepath.Join(t.TempDir(), "empty.db"), existingTarget(t)} {
		if _, err := Restore(t.Context(), target, config, "", "", time.Now(), slog.Default()); err != nil {
			t.Fatal(err)
		}
		s, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
		if err != nil {
			t.Fatal(err)
		}
		assertIdentityCerts(t, s.w, want)
		s.Close()
	}
}

func TestRestoreMetricsSnapshotFrom44(t *testing.T) {
	t.Parallel()
	_, metrics, _ := identitySnapshots44(t)
	old, err := sql.Open("sqlite", metrics)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	source, _ := open(t)
	if err := source.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO node (id, name, token_hash, created_at) VALUES (7, 'kept', x'00', 1)")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "current-config.db")
	if err := source.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{filepath.Join(t.TempDir(), "empty.db"), existingTarget(t)} {
		if _, err := Restore(t.Context(), target, config, metrics, "", time.Now(), slog.Default()); err != nil {
			t.Fatal(err)
		}
		s, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
		if err != nil {
			t.Fatal(err)
		}
		var cpu float64
		var n int
		if err := s.r.QueryRow(`SELECT cpu_sum, cpu_n FROM metric_1m WHERE node_id=7`).Scan(&cpu, &n); err != nil {
			t.Fatal(err)
		}
		if cpu != 50 || n != 1 {
			t.Fatalf("restored metric = %v/%d, want 50/1", cpu, n)
		}
		for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
			if got, want := identityMetricRows(t, s.r, table), identityMetricRows(t, old, table); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s changed during restore: got %v want %v", table, got, want)
			}
		}
		if got := userVersion(t, s.r); got != 45 {
			t.Fatalf("restored version = %d, want 45", got)
		}
		s.Close()
	}
}

func identityMetricRows(t *testing.T, db *sql.DB, table string) [][]any {
	t.Helper()
	rows, err := db.Query("SELECT * FROM " + table + " ORDER BY node_id, ts")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var result [][]any
	for rows.Next() {
		values, pointers := make([]any, len(cols)), make([]any, len(cols))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
