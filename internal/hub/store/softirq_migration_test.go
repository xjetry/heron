package store

import (
	"database/sql"
	"fmt"
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

// schemaV55 冻结 v55 的完整 DDL：v54 加上三张指标表按冻结 DDL 重建（末尾追加 softirq 占比一列）。逐字冻结，
// 生产 DDL 后续变化不影响它。
var schemaV55 = func() []string {
	out := slices.Clone(schemaV54)
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		tmp := table + "_new"
		out = append(out,
			metricDDLV55(tmp),
			"DROP TABLE "+table,
			"ALTER TABLE "+tmp+" RENAME TO "+table,
		)
	}
	return out
}()

// softirqColumns 是 schema 55 给三张指标表加的列；伪造早于 55 的指标快照或旧库时先撤掉它们（dropSoftirqColumns）。
var softirqColumns = []string{"cpu_softirq_pct_sum", "cpu_softirq_pct_n", "cpu_softirq_pct_max"}

func dropSoftirqColumns(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		for _, column := range softirqColumns {
			if _, err := db.Exec("ALTER TABLE " + table + " DROP COLUMN " + column); err != nil {
				t.Fatal(table, column, err)
			}
		}
	}
}

// softirq 一列随分钟桶写入、按 1m 读回，并随上卷进入 5m 与 1h：和与样本数逐级相加、峰值取各分钟峰值的最大者，
// 没有读数的分钟（n=0）既不贡献和也不贡献峰值。cpu 列在同一个桶里各算各的。
func TestSoftirqSurvivesWriteReadAndRollup(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	base := clk.Now().Truncate(time.Hour).Unix()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	sample := func(cpu, softirq float64) *heronv1.Metrics {
		return &heronv1.Metrics{CpuPct: proto.Float64(cpu), CpuSoftirqPct: proto.Float64(softirq)}
	}
	none := metric.NewBucket()
	none.Add(&heronv1.Metrics{CpuPct: proto.Float64(90)})
	busy := metric.NewBucket()
	busy.Add(sample(40, 12))
	busy.Add(sample(60, 30))
	later := metric.NewBucket()
	later.Add(sample(20, 6))
	rows := []metric.Row{
		{NodeID: id, TS: base, CoverageStart: base, Bucket: none},
		{NodeID: id, TS: base + 60, CoverageStart: base, Bucket: busy},
		{NodeID: id, TS: base + 120, CoverageStart: base, Bucket: later},
	}
	clk.SetWall(time.Unix(base+3*3600, 0))
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: rows}); err != nil {
		t.Fatal(err)
	}
	softirq, cpu := metric.Index("cpu_softirq_pct"), metric.Index("cpu")
	minutes := readLevel(t, s, levels[0], id)
	if len(minutes) != 3 || minutes[0].Bucket.N[softirq] != 0 ||
		minutes[1].Bucket.Sum[softirq] != 42 || minutes[1].Bucket.N[softirq] != 2 || minutes[1].Bucket.Max[softirq] != 30 {
		var got []string
		for _, r := range minutes {
			got = append(got, fmt.Sprintf("@%d %v/%d max %v", r.TS-base, r.Bucket.Sum[softirq], r.Bucket.N[softirq], r.Bucket.Max[softirq]))
		}
		t.Fatalf("1m softirq = %v, want the second minute to hold 42 over 2 samples peaking at 30 and the first none", got)
	}
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	for _, lv := range []Level{levels[1], levels[2]} {
		got := readLevel(t, s, lv, id)
		if len(got) != 1 || got[0].TS != base {
			t.Fatalf("%v rows = %+v, want one bucket at %d", lv, got, base)
		}
		b := got[0].Bucket
		if b.Sum[softirq] != 48 || b.N[softirq] != 3 || b.Max[softirq] != 30 {
			t.Fatalf("%v cpu_softirq_pct = %v/%d max %v, want 48/3 max 30", lv, b.Sum[softirq], b.N[softirq], b.Max[softirq])
		}
		if b.Sum[cpu] != 210 || b.N[cpu] != 4 || b.Max[cpu] != 90 {
			t.Fatalf("%v cpu = %v/%d max %v, want its own 210/4 max 90", lv, b.Sum[cpu], b.N[cpu], b.Max[cpu])
		}
	}
	// 查询入口按同一张描述表读列：上卷后的 1h 级别里 softirq 的和、样本数与峰值原样到达。
	got, err := s.QueryMetrics(ctx, id, base, base+3600, levels[2], 3600)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("QueryMetrics 1h = %d rows, want one with softirq 48/3 max 30", len(got))
	}
	if b := got[0].Bucket; b.Sum[softirq] != 48 || b.N[softirq] != 3 || b.Max[softirq] != 30 {
		t.Fatalf("QueryMetrics 1h softirq = %v/%d max %v, want 48/3 max 30", b.Sum[softirq], b.N[softirq], b.Max[softirq])
	}
}

// 迁移 55：三张表按冻结 DDL 重建，旧行的 softirq 列 n=0（升级前的 agent 不采集，无读数不是 0%），其余列原样保留。
func TestMigrationFromV54LeavesSoftirqUnread(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 54, func(t *testing.T, db *sql.DB) {
		seedMinuteRow(t, db)
		for _, q := range []string{
			`INSERT INTO metric_5m (node_id, ts, cpu_sum, cpu_n, cpu_max, cpu_steal_pct_sum, cpu_steal_pct_n, cpu_steal_pct_max) VALUES (7, 300, 10, 1, 10, 3, 1, 3)`,
			`INSERT INTO metric_1h (node_id, ts, cpu_sum, cpu_n, cpu_max, cpu_steal_pct_sum, cpu_steal_pct_n, cpu_steal_pct_max) VALUES (7, 3600, 20, 1, 20, 4, 1, 4)`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	})
	for _, tc := range []struct {
		table string
		ts    int64
		cpu   float64
	}{
		{"metric_1m", 60, 50},
		{"metric_5m", 300, 10},
		{"metric_1h", 3600, 20},
	} {
		var sum, peak, cpu float64
		var n int
		if err := migrated.r.QueryRow("SELECT cpu_softirq_pct_sum, cpu_softirq_pct_n, cpu_softirq_pct_max, cpu_sum FROM "+tc.table+" WHERE node_id=7 AND ts=?", tc.ts).
			Scan(&sum, &n, &peak, &cpu); err != nil {
			t.Fatal(tc.table, tc.ts, err)
		}
		if sum != 0 || n != 0 || peak != 0 || cpu != tc.cpu {
			t.Fatalf("%s@%d = softirq %v/%d max %v cpu %v, want no softirq reading and the old cpu %v", tc.table, tc.ts, sum, n, peak, cpu, tc.cpu)
		}
	}
}

// 54 版的指标快照恢复到空库与已有库：恢复与迁移同一步，旧行的 softirq 无读数，cpu 仍在。
func TestRestoreV54MetricsSnapshotAddsSoftirqColumns(t *testing.T) {
	t.Parallel()
	source, clk := open(t)
	ctx := t.Context()
	id, _, err := source.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	b := metric.NewBucket()
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(40)})
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
		}
		if _, err := db.Exec("UPDATE snapshot_meta SET schema_version=54"); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{filepath.Join(t.TempDir(), "empty.db"), existingTarget(t)} {
		t.Run(filepath.Base(target), func(t *testing.T) {
			if _, err := Restore(ctx, target, config, metrics, "", clk.Now(), slog.Default()); err != nil {
				t.Fatalf("restore from schema 54 snapshots: %v", err)
			}
			restored, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			var sum, cpu float64
			var n int
			if err := restored.r.QueryRow("SELECT cpu_softirq_pct_sum, cpu_softirq_pct_n, cpu_sum FROM metric_1m WHERE node_id=?", id).Scan(&sum, &n, &cpu); err != nil {
				t.Fatal(err)
			}
			if sum != 0 || n != 0 || cpu != 40 {
				t.Fatalf("restored metric = softirq %v/%d cpu %v, want no softirq reading and the old cpu 40", sum, n, cpu)
			}
		})
	}
}
