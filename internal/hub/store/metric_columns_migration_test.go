package store

import (
	"slices"
	"testing"

	"github.com/xjetry/heron/internal/hub/metric"
)

// schemaV28 冻结 v28 的完整 DDL：v27 加上四个指标在 1m/5m/1h 三张表上的 sum/n/max。逐字冻结。
var schemaV28 = append(slices.Clone(schemaV27),
	`ALTER TABLE metric_1m ADD COLUMN disk_read_bps_sum INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1m ADD COLUMN disk_read_bps_n INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1m ADD COLUMN disk_read_bps_max INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1m ADD COLUMN disk_write_bps_sum INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1m ADD COLUMN disk_write_bps_n INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1m ADD COLUMN disk_write_bps_max INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1m ADD COLUMN cpu_steal_pct_sum REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1m ADD COLUMN cpu_steal_pct_n INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1m ADD COLUMN cpu_steal_pct_max REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1m ADD COLUMN cpu_iowait_pct_sum REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1m ADD COLUMN cpu_iowait_pct_n INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1m ADD COLUMN cpu_iowait_pct_max REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_5m ADD COLUMN disk_read_bps_sum INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_5m ADD COLUMN disk_read_bps_n INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_5m ADD COLUMN disk_read_bps_max INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_5m ADD COLUMN disk_write_bps_sum INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_5m ADD COLUMN disk_write_bps_n INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_5m ADD COLUMN disk_write_bps_max INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_5m ADD COLUMN cpu_steal_pct_sum REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_5m ADD COLUMN cpu_steal_pct_n INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_5m ADD COLUMN cpu_steal_pct_max REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_5m ADD COLUMN cpu_iowait_pct_sum REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_5m ADD COLUMN cpu_iowait_pct_n INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_5m ADD COLUMN cpu_iowait_pct_max REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1h ADD COLUMN disk_read_bps_sum INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1h ADD COLUMN disk_read_bps_n INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1h ADD COLUMN disk_read_bps_max INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1h ADD COLUMN disk_write_bps_sum INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1h ADD COLUMN disk_write_bps_n INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1h ADD COLUMN disk_write_bps_max INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1h ADD COLUMN cpu_steal_pct_sum REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1h ADD COLUMN cpu_steal_pct_n INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1h ADD COLUMN cpu_steal_pct_max REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1h ADD COLUMN cpu_iowait_pct_sum REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1h ADD COLUMN cpu_iowait_pct_n INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE metric_1h ADD COLUMN cpu_iowait_pct_max REAL NOT NULL DEFAULT 0`,
)

// 旧行的新列一律取 0/0/0：n=0 就是这一分钟没有采样，老历史不得被伪装成测到的零速率或零占用。
func TestMigrationFromV27AddsMetricColumns(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 27, seedMinuteRow)
	columns := []string{
		"disk_read_bps_sum", "disk_read_bps_n", "disk_read_bps_max",
		"disk_write_bps_sum", "disk_write_bps_n", "disk_write_bps_max",
		"cpu_steal_pct_sum", "cpu_steal_pct_n", "cpu_steal_pct_max",
		"cpu_iowait_pct_sum", "cpu_iowait_pct_n", "cpu_iowait_pct_max",
	}
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		for _, column := range columns {
			var n int
			if err := migrated.r.QueryRow("SELECT count(*) FROM pragma_table_info(?) WHERE name=?", table, column).Scan(&n); err != nil || n != 1 {
				t.Fatalf("%s.%s present=%d err=%v, want the added column", table, column, n, err)
			}
		}
	}
	var sum, n, max int64
	if err := migrated.r.QueryRow(
		"SELECT disk_read_bps_sum, disk_read_bps_n, disk_read_bps_max FROM metric_1m WHERE node_id = 7 AND ts = 60",
	).Scan(&sum, &n, &max); err != nil {
		t.Fatal(err)
	}
	if sum != 0 || n != 0 || max != 0 {
		t.Fatalf("legacy metric row new columns = %d/%d/%d, want 0/0/0 (n=0 means no sample)", sum, n, max)
	}
}

// 描述表是单一事实源：这里枚举它，确认四个新指标在内存桶与三张指标表的 SQL 列里都有位置，
// 既有的按描述表枚举的用例（如 TestMissingMetricReadsBackAsNoData、上卷与查询）因此自动覆盖新列。
func TestMetricDescriptionCoversFourNewColumns(t *testing.T) {
	t.Parallel()
	wanted := map[string]bool{
		"disk_read_bps": true, "disk_write_bps": true, "cpu_steal_pct": true, "cpu_iowait_pct": true,
	}
	var names []string
	for _, c := range metric.Columns {
		names = append(names, c.Name)
		delete(wanted, c.Name)
	}
	if len(wanted) != 0 {
		t.Fatalf("metric.Columns is missing %v", wanted)
	}
	t.Logf("metric.Columns enumerated: %v", names)

	s, _ := open(t)
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		for _, c := range metric.Columns {
			columns := []string{c.Name + "_sum", c.Name + "_n"}
			if c.Kind == metric.MeanMax {
				columns = append(columns, c.Name+"_max")
			}
			for _, column := range columns {
				var n int
				if err := s.r.QueryRow("SELECT count(*) FROM pragma_table_info(?) WHERE name=?", table, column).Scan(&n); err != nil || n != 1 {
					t.Fatalf("%s.%s present=%d err=%v, description table and schema disagree", table, column, n, err)
				}
			}
		}
	}
}
