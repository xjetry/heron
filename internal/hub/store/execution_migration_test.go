package store

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/metric"
	"google.golang.org/protobuf/proto"
)

// schemaV34 冻结 v34 的完整 DDL：v33 加上 node_facts 的 execution、facts_rev，以及三张指标表按冻结 DDL 重建
// （把 load1_per_core 插回覆盖列之前）。逐字冻结，生产 DDL 后续变化不影响它。
var schemaV34 = func() []string {
	out := append(slices.Clone(schemaV33), migrationV34Config...)
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		tmp := table + "_new"
		out = append(out,
			metricDDLV34(tmp),
			"DROP TABLE "+table,
			"ALTER TABLE "+tmp+" RENAME TO "+table,
		)
	}
	return out
}()

// 旧行的新列取缺省：execution 是 'null'（没有上报），facts_rev 是 0（摘要还不能算当前字段集合已确认），
// 按核负载 n=0（这一分钟没有采样）。已有的 cpu 与主机名必须原样留下。
func TestMigrationFromV33AddsExecutionAndPerCoreLoad(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 33, func(t *testing.T, db *sql.DB) {
		seedMinuteRow(t, db)
		for _, q := range []string{
			`INSERT INTO metric_5m (node_id, ts, cpu_sum, cpu_n, cpu_max) VALUES (7, 300, 10, 1, 10)`,
			`INSERT INTO metric_1h (node_id, ts, cpu_sum, cpu_n, cpu_max) VALUES (7, 3600, 20, 1, 20)`,
			`INSERT INTO node_facts (node_id, facts_hash, hostname, os, kernel, arch, virtualization, cpu_model, cpu_cores, agent_version, icmp_available, updated_at, network, diagnostics)
				VALUES (7, 99, 'legacy', 'linux', 'k', 'amd64', '', 'm', 4, 'old', 0, 1, '{}', 'null')`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	})
	var host, execution string
	var cores, rev int
	if err := migrated.r.QueryRow("SELECT hostname, cpu_cores, execution, facts_rev FROM node_facts WHERE node_id=7").Scan(&host, &cores, &execution, &rev); err != nil {
		t.Fatal(err)
	}
	if host != "legacy" || cores != 4 || execution != "null" || rev != 0 {
		t.Fatalf("legacy facts = %s/%d/%q/%d, want legacy/4/null/0", host, cores, execution, rev)
	}
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		var sum, cpu float64
		var n int
		if err := migrated.r.QueryRow("SELECT load1_per_core_sum, load1_per_core_n, cpu_sum FROM "+table+" WHERE node_id=7").Scan(&sum, &n, &cpu); err != nil {
			t.Fatal(table, err)
		}
		if sum != 0 || n != 0 {
			t.Fatalf("%s load1_per_core = %v/%d, want 0/0 (n=0 means no sample)", table, sum, n)
		}
		if cpu == 0 {
			t.Fatalf("%s lost cpu history", table)
		}
	}
}

// v33 的配置与指标快照恢复到空库与已有库：迁移补上 execution、facts_rev 与按核负载列，旧事实与 cpu 仍在，
// 新列取缺省（未上报、未采样）。快照从当前库拷出后先撤列再回填版本号，否则 ADD COLUMN 会撞上重复列。
func TestRestoreV33SnapshotsAddsExecutionColumns(t *testing.T) {
	t.Parallel()
	source, clk := open(t)
	ctx := t.Context()
	id, _, err := source.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	b := metric.NewBucket()
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(40), Load1: proto.Float64(2)})
	if _, err := source.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{{NodeID: id, TS: 60, CoverageStart: 60, Bucket: b}}}); err != nil {
		t.Fatal(err)
	}
	if err := source.UpsertFacts(ctx, id, 7, &heronv1.Facts{Hostname: "kept", Os: "linux", CpuCores: 4}); err != nil {
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
	cfg, err := sql.Open("sqlite", config)
	if err != nil {
		t.Fatal(err)
	}
	removeV44Config(t, cfg)
	removeV41Config(t, cfg)
	removeV40Config(t, cfg)
	removeV39Config(t, cfg)
	if _, err := cfg.Exec("DROP TABLE cleanup_job; ALTER TABLE node DROP COLUMN traffic_quota_bytes; ALTER TABLE node DROP COLUMN traffic_quota_mode; DROP TABLE probe_cert_presented; ALTER TABLE probe_cert DROP COLUMN config_id; ALTER TABLE probe_task DROP COLUMN cert_spki_sha256; ALTER TABLE probe_task DROP COLUMN config_id; ALTER TABLE node_facts DROP COLUMN execution; ALTER TABLE node_facts DROP COLUMN facts_rev; UPDATE snapshot_meta SET schema_version=33"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Close(); err != nil {
		t.Fatal(err)
	}
	met, err := sql.Open("sqlite", metrics)
	if err != nil {
		t.Fatal(err)
	}
	dropTCPRateColumns(t, met)
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		for _, column := range []string{"load1_per_core_sum", "load1_per_core_n"} {
			if _, err := met.Exec("ALTER TABLE " + table + " DROP COLUMN " + column); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := met.Exec("UPDATE snapshot_meta SET schema_version=33"); err != nil {
		t.Fatal(err)
	}
	if err := met.Close(); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{filepath.Join(t.TempDir(), "empty.db"), existingTarget(t)} {
		t.Run(filepath.Base(target), func(t *testing.T) {
			if _, err := Restore(ctx, target, config, metrics, "", clk.Now(), slog.Default()); err != nil {
				t.Fatalf("restore from schema 33 snapshots: %v", err)
			}
			restored, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			var host, execution string
			var cores, rev int
			if err := restored.r.QueryRow("SELECT hostname, cpu_cores, execution, facts_rev FROM node_facts WHERE node_id=?", id).Scan(&host, &cores, &execution, &rev); err != nil {
				t.Fatal(err)
			}
			if host != "kept" || cores != 4 || execution != "null" || rev != 0 {
				t.Fatalf("restored facts = %s/%d/%q/%d, want kept/4/null/0", host, cores, execution, rev)
			}
			var sum, cpu float64
			var n int
			if err := restored.r.QueryRow("SELECT load1_per_core_sum, load1_per_core_n, cpu_sum FROM metric_1m WHERE node_id=?", id).Scan(&sum, &n, &cpu); err != nil {
				t.Fatal(err)
			}
			if sum != 0 || n != 0 || cpu == 0 {
				t.Fatalf("restored metric = per-core %v/%d cpu %v, want 0/0 and the old cpu", sum, n, cpu)
			}
		})
	}
}

// 快照里的 execution 必须是能通过同一校验的 JSON。语法合法但 kind 未指定的对象要拒绝，不能只检查能解开。
func TestRestoreRejectsIllegalExecution(t *testing.T) {
	t.Parallel()
	source, clk := open(t)
	ctx := t.Context()
	id, _, err := source.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := source.UpsertFacts(ctx, id, 1, &heronv1.Facts{Hostname: "kept", Execution: &heronv1.ExecutionScope{
		Kind: heronv1.ScopeKind_SCOPE_KIND_HOST,
		Cpu:  heronv1.ResourceScope_RESOURCE_SCOPE_HOST, Memory: heronv1.ResourceScope_RESOURCE_SCOPE_HOST,
		Swap: heronv1.ResourceScope_RESOURCE_SCOPE_HOST, Load: heronv1.ResourceScope_RESOURCE_SCOPE_HOST,
	}}); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.db")
	if err := source.SnapshotConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", config)
	if err != nil {
		t.Fatal(err)
	}
	const illegal = `{"kind":"SCOPE_KIND_UNSPECIFIED","cpu":"RESOURCE_SCOPE_HOST","memory":"RESOURCE_SCOPE_HOST","swap":"RESOURCE_SCOPE_HOST","load":"RESOURCE_SCOPE_HOST"}`
	if _, err := raw.Exec("UPDATE node_facts SET execution = ? WHERE node_id = ?", illegal, id); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored.db")
	if _, err := Restore(ctx, target, config, "", "", clk.Now(), slog.Default()); err == nil {
		t.Fatal("illegal execution was restored")
	}
}
