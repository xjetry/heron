package store

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/metric"
)

// schemaV33 冻结 v33 的完整 DDL：v32 加上三张探测表的 (task_id, node_id, ts) 索引。逐字冻结，
// 生产 DDL 后续变化不影响它。
var schemaV33 = append(slices.Clone(schemaV32),
	`CREATE INDEX probe_1m_by_task ON probe_1m (task_id, node_id, ts)`,
	`CREATE INDEX probe_5m_by_task ON probe_5m (task_id, node_id, ts)`,
	`CREATE INDEX probe_1h_by_task ON probe_1h (task_id, node_id, ts)`,
)

// seedProbeByTask 给 32 版旧库留下一行探测历史：迁移只是加索引，旧行必须原样保留。节点与任务都存在，
// 这行不是孤儿，迁移后的查询能读到它。
func seedProbeByTask(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		"INSERT INTO node (id, name, token_hash, created_at) VALUES (7, 'kept', x'00', 1)",
		"INSERT INTO probe_task (id, kind, target, interval_s, timeout_ms, created_at) VALUES (3, 1, 'example.com', 60, 1000, 1)",
		"INSERT INTO probe_1m (node_id, ts, task_id, sent, lost, rtt_sum_us) VALUES (7, 60, 3, 2, 1, 300)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrationFromV32AddsProbeByTaskIndexes(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 32, seedProbeByTask)
	for _, table := range probeTables {
		var n int
		if err := migrated.r.QueryRow("SELECT count(*) FROM pragma_index_list(?) WHERE name = ?", table, table+"_by_task").Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s_by_task present=%d err=%v, want the index", table, n, err)
		}
	}
	var sent int64
	if err := migrated.r.QueryRow("SELECT sent FROM probe_1m WHERE node_id = 7 AND ts = 60 AND task_id = 3").Scan(&sent); err != nil || sent != 2 {
		t.Fatalf("probe history across the index migration: sent=%d err=%v, want 2", sent, err)
	}
	lv, _ := LevelByName("1m")
	rows, err := migrated.QueryProbeComparison(t.Context(), 3, []int64{7}, 0, 120, lv, 60)
	if err != nil || len(rows) != 1 || rows[0].Bucket.Sent != 2 {
		t.Fatalf("comparison after migration: rows=%v err=%v, want one bucket with sent=2", rows, err)
	}
}

// v32 的指标快照恢复到空库与已有库：迁移 33 在快照副本上补建索引（快照本身不带索引，变化只是
// 版本号与副本结构），恢复后的目标库建到 v33、索引存在，对比查询走索引。
func TestRestoreMigratesV32MetricsSnapshotAndKeepsProbeByTaskIndexes(t *testing.T) {
	t.Parallel()
	source, clk := open(t)
	ctx := t.Context()
	id, _, err := source.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	seedProbeTasks(t, source, 5)
	if _, err := source.WriteMinuteBatch(ctx, metric.Batch{Probes: []metric.ProbeRow{probeRow(id, 60, 5, []uint32{300}, 1, 0)}}); err != nil {
		t.Fatal(err)
	}
	metrics := filepath.Join(t.TempDir(), "metrics.db")
	if err := source.SnapshotMetrics(ctx, metrics); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", metrics)
	if err != nil {
		t.Fatal(err)
	}
	// 快照从当前库拷出，已带按核负载列；回填 32 之前必须撤掉，否则重建仍会把新列的值当作旧历史。
	dropTCPRateColumns(t, old)
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		for _, column := range []string{"load1_per_core_sum", "load1_per_core_n"} {
			if _, err := old.Exec("ALTER TABLE " + table + " DROP COLUMN " + column); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := old.Exec("DROP TABLE IF EXISTS cleanup_job; UPDATE snapshot_meta SET schema_version = 32"); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.db")
	if err := source.SnapshotConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{filepath.Join(t.TempDir(), "empty.db"), existingTarget(t)} {
		t.Run(filepath.Base(target), func(t *testing.T) {
			if _, err := Restore(ctx, target, config, metrics, "", clk.Now(), slog.Default()); err != nil {
				t.Fatalf("restore from a schema 32 metrics snapshot: %v", err)
			}
			restored, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			for _, table := range probeTables {
				var n int
				if err := restored.r.QueryRow("SELECT count(*) FROM pragma_index_list(?) WHERE name = ?", table, table+"_by_task").Scan(&n); err != nil || n != 1 {
					t.Fatalf("restored %s_by_task present=%d err=%v, want the index", table, n, err)
				}
			}
			lv, _ := LevelByName("1m")
			rows, err := restored.QueryProbeComparison(ctx, 5, []int64{id}, 0, 120, lv, 60)
			if err != nil || len(rows) != 1 || rows[0].Bucket.Lost != 1 {
				t.Fatalf("comparison after restore: rows=%v err=%v, want one row with lost=1", rows, err)
			}
			plan := strings.Join(queryPlans(t, restored.r,
				probeFamily.rangeSQL(0, queryShape{keyWhere: "task_id = ? AND node_id IN (?,?)", seriesLimit: MaxComparisonNodes, byTaskIndex: true}), 5, id, id+1, 0, 120), "\n")
			if !strings.Contains(plan, "INDEX probe_1m_by_task") {
				t.Fatalf("restored comparison lookup must use the by-task index: %s", plan)
			}
		})
	}
}

func existingTarget(t *testing.T) string {
	t.Helper()
	st, target := openAt(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return target
}
