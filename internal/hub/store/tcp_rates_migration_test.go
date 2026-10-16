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

// schemaV43 冻结 v43 的完整 DDL：v42 加上三张指标表按冻结 DDL 重建（末尾追加 TCP 速率四列）。逐字冻结，
// 生产 DDL 后续变化不影响它。
var schemaV43 = func() []string {
	out := slices.Clone(schemaV42)
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		tmp := table + "_new"
		out = append(out,
			metricDDLV43(tmp),
			"DROP TABLE "+table,
			"ALTER TABLE "+tmp+" RENAME TO "+table,
		)
	}
	return out
}()

// 一个 5 分钟桶里混着没有 TCP 读数的分钟（n=0）与有采样的分钟（多次采样、两分钟的重传比例不同）：没有读数的分钟不贡献
// 和与样本数，桶的重传率 = Σretrans ÷ Σout；建连峰值取各分钟峰值的最大值。上卷到 1 小时仍是同一组和。
// 按分钟比例求平均得 (2.5% + 0%) ÷ 2 = 1.25%，与求和相除的 1% 不同，这组数据能区分两种口径。
func TestRollupCarriesTCPRatesAcrossMixedMinutes(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	base := clk.Now().Truncate(time.Hour).Unix()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	tcp := func(active, out, retrans float64) *heronv1.Metrics {
		return &heronv1.Metrics{TcpActiveOpensPs: proto.Float64(active), TcpPassiveOpensPs: proto.Float64(1),
			TcpOutSegsPs: proto.Float64(out), TcpRetransSegsPs: proto.Float64(retrans)}
	}
	old := metric.NewBucket()
	old.Add(&heronv1.Metrics{CpuPct: proto.Float64(1)})
	busy := metric.NewBucket()
	busy.Add(tcp(2, 100, 1))
	busy.Add(tcp(8, 300, 9))
	quiet := metric.NewBucket()
	quiet.Add(tcp(5, 600, 0))
	rows := []metric.Row{
		{NodeID: id, TS: base, CoverageStart: base, Bucket: old},
		{NodeID: id, TS: base + 60, CoverageStart: base, Bucket: busy},
		{NodeID: id, TS: base + 120, CoverageStart: base, Bucket: quiet},
	}
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: rows}); err != nil {
		t.Fatal(err)
	}
	clk.SetWall(time.Unix(base+3*3600, 0))
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	if got := watermark(t, s, "1h"); got < base+3600 {
		t.Fatalf("1h watermark = %d, want the first hour rolled up", got)
	}
	active, out, retrans := metric.Index("tcp_active_opens_ps"), metric.Index("tcp_out_segs_ps"), metric.Index("tcp_retrans_segs_ps")
	for _, lv := range []Level{levels[1], levels[2]} {
		got := readLevel(t, s, lv, id)
		if len(got) != 1 || got[0].TS != base {
			t.Fatalf("%v rows = %+v, want one bucket at %d", lv, got, base)
		}
		b := got[0].Bucket
		if b.N[out] != 3 || b.Sum[out] != 1000 || b.N[retrans] != 3 || b.Sum[retrans] != 10 || b.Max[active] != 8 {
			t.Fatalf("%v: out %v/%d retrans %v/%d active max %v, want 1000/3 10/3 8", lv, b.Sum[out], b.N[out], b.Sum[retrans], b.N[retrans], b.Max[active])
		}
		if ratio := b.Sum[retrans] / b.Sum[out] * 100; ratio != 1 {
			t.Fatalf("%v retrans ratio = %v, want 1", lv, ratio)
		}
	}
}

// tcpRateColumns 是 schema 43 给三张指标表加的列；伪造早于 43 的指标快照时先撤掉它们（dropTCPRateColumns）。
var tcpRateColumns = []string{"tcp_active_opens_ps_sum", "tcp_active_opens_ps_n", "tcp_active_opens_ps_max",
	"tcp_passive_opens_ps_sum", "tcp_passive_opens_ps_n", "tcp_passive_opens_ps_max",
	"tcp_out_segs_ps_sum", "tcp_out_segs_ps_n", "tcp_retrans_segs_ps_sum", "tcp_retrans_segs_ps_n"}

func dropTCPRateColumns(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		for _, column := range tcpRateColumns {
			if _, err := db.Exec("ALTER TABLE " + table + " DROP COLUMN " + column); err != nil {
				t.Fatal(table, column, err)
			}
		}
	}
}

// 迁移 43：旧行的四个 TCP 速率 n=0（这一分钟没有采样），已有的 cpu 与按核负载原样保留。
func TestMigrationFromV42AddsTCPRateColumns(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 42, func(t *testing.T, db *sql.DB) {
		seedMinuteRow(t, db)
		for _, q := range []string{
			`INSERT INTO metric_5m (node_id, ts, cpu_sum, cpu_n, cpu_max, load1_per_core_sum, load1_per_core_n) VALUES (7, 300, 10, 1, 10, 2, 1)`,
			`INSERT INTO metric_1h (node_id, ts, cpu_sum, cpu_n, cpu_max, load1_per_core_sum, load1_per_core_n) VALUES (7, 3600, 20, 1, 20, 3, 1)`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	})
	for i, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		var active, passive, out, retrans, loadN int
		var cpu, load float64
		if err := migrated.r.QueryRow("SELECT tcp_active_opens_ps_n, tcp_passive_opens_ps_n, tcp_out_segs_ps_n, tcp_retrans_segs_ps_n, cpu_sum, load1_per_core_sum, load1_per_core_n FROM "+table+" WHERE node_id=7").
			Scan(&active, &passive, &out, &retrans, &cpu, &load, &loadN); err != nil {
			t.Fatal(table, err)
		}
		if active != 0 || passive != 0 || out != 0 || retrans != 0 {
			t.Fatalf("%s tcp n = %d/%d/%d/%d, want 0 (no sample)", table, active, passive, out, retrans)
		}
		if cpu != []float64{50, 10, 20}[i] || load != []float64{0, 2, 3}[i] || loadN != []int{0, 1, 1}[i] {
			t.Fatalf("%s changed history: cpu %v load %v/%d", table, cpu, load, loadN)
		}
	}
}

// 42 版的指标快照恢复到空库与已有库：恢复按冻结 DDL 重建三张表后按列名搬运，旧行的四个 TCP 速率 n=0，cpu 仍在。
func TestRestoreV42MetricsSnapshotAddsTCPRateColumns(t *testing.T) {
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
			dropTCPRateColumns(t, db)
		}
		if layer == "config" {
			removeV47Config(t, db)
			removeV44Config(t, db)
		}
		if _, err := db.Exec("UPDATE snapshot_meta SET schema_version=42"); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{filepath.Join(t.TempDir(), "empty.db"), existingTarget(t)} {
		t.Run(filepath.Base(target), func(t *testing.T) {
			if _, err := Restore(ctx, target, config, metrics, "", clk.Now(), slog.Default()); err != nil {
				t.Fatalf("restore from schema 42 snapshots: %v", err)
			}
			restored, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			var n int
			var cpu float64
			if err := restored.r.QueryRow("SELECT tcp_out_segs_ps_n, cpu_sum FROM metric_1m WHERE node_id=?", id).Scan(&n, &cpu); err != nil {
				t.Fatal(err)
			}
			if n != 0 || cpu == 0 {
				t.Fatalf("restored metric = tcp n %d cpu %v, want 0 and the old cpu", n, cpu)
			}
		})
	}
}
