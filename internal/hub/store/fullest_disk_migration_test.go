package store

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/metric"
	"google.golang.org/protobuf/proto"
)

// schemaV53 冻结 v53 的完整 DDL：v52 加上三张指标表按冻结 DDL 重建（末尾追加最满盘使用率一列）。逐字冻结，
// 生产 DDL 后续变化不影响它。回填只改数据，冻结的结构里没有它。
var schemaV53 = func() []string {
	out := slices.Clone(schemaV52)
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		tmp := table + "_new"
		out = append(out,
			metricDDLV53(tmp),
			"DROP TABLE "+table,
			"ALTER TABLE "+tmp+" RENAME TO "+table,
		)
	}
	return out
}()

// fullestDiskColumns 是 schema 53 给三张指标表加的列；伪造早于 53 的指标快照或旧库时先撤掉它们（dropFullestDiskColumns）。
var fullestDiskColumns = []string{"disk_max_used_pct_sum", "disk_max_used_pct_n"}

func dropFullestDiskColumns(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		for _, column := range fullestDiskColumns {
			if _, err := db.Exec("ALTER TABLE " + table + " DROP COLUMN " + column); err != nil {
				t.Fatal(table, column, err)
			}
		}
	}
}

// 最满盘一列随分钟桶写入、按 1m 读回，并随上卷进入 5m 与 1h：和与样本数逐级相加，没有读数的分钟（n=0）不贡献。
// 根盘那一列在同一个桶里各算各的，不被最满盘代替。
func TestFullestDiskSurvivesWriteReadAndRollup(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	base := clk.Now().Truncate(time.Hour).Unix()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	disk := func(rootUsed uint64, fullest float64) *heronv1.Metrics {
		return &heronv1.Metrics{DiskTotal: proto.Uint64(100), DiskUsed: proto.Uint64(rootUsed), DiskMaxUsedPct: proto.Float64(fullest)}
	}
	none := metric.NewBucket()
	none.Add(&heronv1.Metrics{CpuPct: proto.Float64(1)})
	busy := metric.NewBucket()
	busy.Add(disk(20, 80))
	busy.Add(disk(20, 90))
	later := metric.NewBucket()
	later.Add(disk(40, 95))
	rows := []metric.Row{
		{NodeID: id, TS: base, CoverageStart: base, Bucket: none},
		{NodeID: id, TS: base + 60, CoverageStart: base, Bucket: busy},
		{NodeID: id, TS: base + 120, CoverageStart: base, Bucket: later},
	}
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: rows}); err != nil {
		t.Fatal(err)
	}
	fullest, root := metric.Index("disk_max_used_pct"), metric.Index("disk_used_pct")
	minutes := readLevel(t, s, levels[0], id)
	if len(minutes) != 3 || minutes[0].Bucket.N[fullest] != 0 || minutes[1].Bucket.Sum[fullest] != 170 || minutes[1].Bucket.N[fullest] != 2 {
		t.Fatalf("1m rows = %+v, want the second minute to hold 170 over 2 samples and the first none", minutes)
	}
	clk.SetWall(time.Unix(base+3*3600, 0))
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	for _, lv := range []Level{levels[1], levels[2]} {
		got := readLevel(t, s, lv, id)
		if len(got) != 1 || got[0].TS != base {
			t.Fatalf("%v rows = %+v, want one bucket at %d", lv, got, base)
		}
		b := got[0].Bucket
		if b.Sum[fullest] != 265 || b.N[fullest] != 3 {
			t.Fatalf("%v disk_max_used_pct = %v/%d, want 265/3", lv, b.Sum[fullest], b.N[fullest])
		}
		if b.Sum[root] != 80 || b.N[root] != 3 {
			t.Fatalf("%v disk_used_pct = %v/%d, want the root's own 80/3", lv, b.Sum[root], b.N[root])
		}
	}
}

// 迁移 53：旧行的最满盘取同一行根盘使用率的和与样本数（旧版 hub 不接收逐盘读数，根盘就是当时唯一纳入监测的盘，
// 与准入在没有逐盘列表时退回根盘是同一条规则）；没有根盘读数的行仍是 n=0，其余列原样保留。
func TestMigrationFromV52BackfillsFullestDiskFromRoot(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 52, func(t *testing.T, db *sql.DB) {
		seedMinuteRow(t, db)
		for _, q := range []string{
			`INSERT INTO metric_1m (node_id, ts, cpu_sum, cpu_n, cpu_max, disk_used_pct_sum, disk_used_pct_n) VALUES (7, 120, 5, 1, 5, 150, 2)`,
			`INSERT INTO metric_5m (node_id, ts, cpu_sum, cpu_n, cpu_max, disk_used_pct_sum, disk_used_pct_n) VALUES (7, 300, 10, 1, 10, 240, 3)`,
			`INSERT INTO metric_1h (node_id, ts, cpu_sum, cpu_n, cpu_max, disk_used_pct_sum, disk_used_pct_n) VALUES (7, 3600, 20, 1, 20, 400, 5)`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	})
	for _, tc := range []struct {
		table   string
		ts      int64
		sum     float64
		n       int
		cpu     float64
		rootSum float64
	}{
		// seedMinuteRow 的那一分钟没有根盘读数：回填后仍无读数，不是 0%。
		{"metric_1m", 60, 0, 0, 50, 0},
		{"metric_1m", 120, 150, 2, 5, 150},
		{"metric_5m", 300, 240, 3, 10, 240},
		{"metric_1h", 3600, 400, 5, 20, 400},
	} {
		var sum, cpu, rootSum float64
		var n int
		if err := migrated.r.QueryRow("SELECT disk_max_used_pct_sum, disk_max_used_pct_n, cpu_sum, disk_used_pct_sum FROM "+tc.table+" WHERE node_id=7 AND ts=?", tc.ts).
			Scan(&sum, &n, &cpu, &rootSum); err != nil {
			t.Fatal(tc.table, tc.ts, err)
		}
		if sum != tc.sum || n != tc.n || cpu != tc.cpu || rootSum != tc.rootSum {
			t.Fatalf("%s@%d = fullest %v/%d cpu %v root %v, want %v/%d %v %v", tc.table, tc.ts, sum, n, cpu, rootSum, tc.sum, tc.n, tc.cpu, tc.rootSum)
		}
	}
}

// 52 版的指标快照恢复到空库与已有库：恢复与迁移同一步，旧行的最满盘取根盘的和与样本数，cpu 仍在。
func TestRestoreV52MetricsSnapshotBackfillsFullestDisk(t *testing.T) {
	t.Parallel()
	source, clk := open(t)
	ctx := t.Context()
	id, _, err := source.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	b := metric.NewBucket()
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(40), DiskTotal: proto.Uint64(200), DiskUsed: proto.Uint64(50)})
	if _, err := source.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{{NodeID: id, TS: 60, CoverageStart: 60, Bucket: b}}}); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.db")
	metrics := filepath.Join(t.TempDir(), "metrics.db")
	if err := source.SnapshotConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	if err := source.SnapshotMetrics(ctx, metrics); err != nil {
		t.Fatal(err)
	}
	for path, layer := range map[string]string{config: "config", metrics: "metrics"} {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if layer == "metrics" {
			dropSoftirqColumns(t, db)
			dropFullestDiskColumns(t, db)
		}
		if _, err := db.Exec("UPDATE snapshot_meta SET schema_version=52"); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{filepath.Join(t.TempDir(), "empty.db"), existingTarget(t)} {
		t.Run(filepath.Base(target), func(t *testing.T) {
			if _, err := Restore(ctx, target, config, metrics, "", clk.Now(), slog.Default()); err != nil {
				t.Fatalf("restore from schema 52 snapshots: %v", err)
			}
			restored, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			var sum, cpu float64
			var n int
			if err := restored.r.QueryRow("SELECT disk_max_used_pct_sum, disk_max_used_pct_n, cpu_sum FROM metric_1m WHERE node_id=?", id).Scan(&sum, &n, &cpu); err != nil {
				t.Fatal(err)
			}
			if sum != 25 || n != 1 || cpu != 40 {
				t.Fatalf("restored metric = fullest %v/%d cpu %v, want 25/1 and the old cpu 40", sum, n, cpu)
			}
		})
	}
}
