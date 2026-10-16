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

// 冻结流量基线移出 traffic 行之后的结构，后续生产 DDL 变化不改变旧版夹具。
var schemaV41 = append(slices.Clone(schemaV40),
	`CREATE TABLE traffic_baseline (
  node_id INTEGER NOT NULL,
  iface TEXT NOT NULL,
  rx INTEGER NOT NULL CHECK (rx >= 0),
  tx INTEGER NOT NULL CHECK (tx >= 0),
  PRIMARY KEY (node_id, iface)
) WITHOUT ROWID`,
	`ALTER TABLE traffic DROP COLUMN last_rx`,
	`ALTER TABLE traffic DROP COLUMN last_tx`,
)

// v40 的 traffic 行：合计基线在 last_rx、last_tx；-1 是校正先于首次上报建立的"没有基线"。
const v40TrafficSeed = `INSERT INTO node (id, name, token_hash, created_at) VALUES (1, 'totals', x'01', 1), (2, 'adjusted', x'02', 1), (3, 'stale', x'03', 1), (4, 'half', x'04', 1);
INSERT INTO traffic (node_id, boot_id, last_rx, last_tx, total_rx, total_tx, period_rx, period_tx, period_start, updated_at, net_counter_epoch) VALUES
  (1, 'boot', 10, 20, 100, 200, 30, 40, 0, 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'),
  (2, '', -1, -1, 700, 0, 700, 0, 0, 1, ''),
  (3, 'boot', -1, -1, 5, 6, 5, 6, 0, 1, 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'),
  (4, 'boot', 5, -1, 1, 1, 1, 1, 0, 1, '')`

func seedV40Traffic(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(v40TrafficSeed); err != nil {
		t.Fatal(err)
	}
}

// 迁移后：合计基线成为空名的一行、摘要留在 traffic 行；-1（以及只有一半是 -1）的行没有基线，摘要清空；累计值原样。
func wantMigratedTraffic() []TrafficRecord {
	start := time.Unix(0, 0).UTC()
	return []TrafficRecord{
		{NodeID: 1, BootID: "boot", NetCounterEpoch: strings.Repeat("a", 64), Baseline: []TrafficCounter{{Rx: 10, Tx: 20}}, TotalRx: 100, TotalTx: 200, PeriodRx: 30, PeriodTx: 40, PeriodStart: start},
		{NodeID: 2, TotalRx: 700, PeriodRx: 700, PeriodStart: start},
		{NodeID: 3, BootID: "boot", TotalRx: 5, TotalTx: 6, PeriodRx: 5, PeriodTx: 6, PeriodStart: start},
		{NodeID: 4, BootID: "boot", TotalRx: 1, TotalTx: 1, PeriodRx: 1, PeriodTx: 1, PeriodStart: start},
	}
}

func loadSorted(t *testing.T, st *Store) []TrafficRecord {
	t.Helper()
	got, err := st.LoadTraffic(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(a, b TrafficRecord) int { return int(a.NodeID - b.NodeID) })
	return got
}

// 迁移 41：合计基线搬进 traffic_baseline，哨兵行不当成真实计数；迁移后的库照常接受逐网卡基线的写入。
func TestMigrationFromV40MovesTrafficBaseline(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 40, seedV40Traffic)
	if got, want := loadSorted(t, s), wantMigratedTraffic(); !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated traffic:\n got %+v\nwant %+v", got, want)
	}
	next := wantMigratedTraffic()[0]
	next.NetCounterEpoch, next.Baseline = "", []TrafficCounter{{"eth0", 4, 8}, {"eth1", 6, 12}}
	if _, err := s.WriteTraffic(t.Context(), []TrafficRecord{next}); err != nil {
		t.Fatalf("write after migration: %v", err)
	}
	if got := loadSorted(t, s)[0]; !reflect.DeepEqual(got, next) {
		t.Fatalf("per-interface baseline after migration: %+v, want %+v", got, next)
	}
}

// 迁移失败整步回滚：库仍是 v40、旧列与数据都在；挡路的对象移走后重新开库照常迁移。迁移成功后重复开库不再改动。
func TestTrafficBaselineMigrationRollsBackAndIsIdempotent(t *testing.T) {
	t.Parallel()
	path, raw := frozenSchemaFixture(t, 40)
	seedV40Traffic(t, raw)
	if _, err := raw.Exec("CREATE TABLE traffic_baseline (blocking INTEGER)"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if s, err := Open(path, clk, slog.Default(), MigrateSchema); err == nil {
		s.Close()
		t.Fatal("migration over an existing traffic_baseline table succeeded")
	}
	raw = openRawFixture(t, path)
	if v := userVersion(t, raw); v != 40 {
		t.Fatalf("failed migration left user_version %d, want 40", v)
	}
	var rx, tx int64
	if err := raw.QueryRow("SELECT last_rx, last_tx FROM traffic WHERE node_id = 1").Scan(&rx, &tx); err != nil || rx != 10 || tx != 20 {
		t.Fatalf("failed migration lost the v40 baseline: %d/%d %v", rx, tx, err)
	}
	if _, err := raw.Exec("DROP TABLE traffic_baseline"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	for round := range 2 {
		s, err := Open(path, clk, slog.Default(), MigrateSchema)
		if err != nil {
			t.Fatalf("open round %d: %v", round, err)
		}
		if got, want := loadSorted(t, s), wantMigratedTraffic(); !reflect.DeepEqual(got, want) {
			s.Close()
			t.Fatalf("round %d traffic:\n got %+v\nwant %+v", round, got, want)
		}
		if diff := schemaDifference(describe(t, s.r), freshSchema(t), "重开", "新建"); diff != "" {
			s.Close()
			t.Fatalf("round %d schema differs:\n%s", round, diff)
		}
		s.Close()
	}
}

// v40 的配置快照（合计基线在 traffic 行上）恢复到当前版本：只恢复配置层与连同指标层一起恢复都按迁移 41 搬运基线。
func TestTrafficOldSnapshotRestoresBaseline(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 40, seedV40Traffic)
	config, metrics := filepath.Join(t.TempDir(), "config.db"), filepath.Join(t.TempDir(), "metrics.db")
	if err := s.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if err := s.SnapshotMetrics(t.Context(), metrics); err != nil {
		t.Fatal(err)
	}
	for path, layer := range map[string]string{config: "config", metrics: "metrics"} {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if layer == "config" {
			removeV47Config(t, db)
			removeV44Config(t, db)
			removeV41Config(t, db)
			var rx int64
			if err := db.QueryRow("SELECT last_rx FROM traffic WHERE node_id = 1").Scan(&rx); err != nil || rx != 10 {
				t.Fatalf("v40 snapshot fixture: last_rx %d %v", rx, err)
			}
		}
		if layer == "metrics" {
			dropTCPRateColumns(t, db)
		}
		if _, err := db.Exec("UPDATE snapshot_meta SET schema_version = 40"); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	for _, layers := range [][2]string{{config, ""}, {config, metrics}} {
		target := filepath.Join(t.TempDir(), "restored.db")
		if _, err := Restore(t.Context(), target, layers[0], layers[1], "", time.Now(), slog.Default()); err != nil {
			t.Fatalf("restore %v: %v", layers, err)
		}
		restored, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
		if err != nil {
			t.Fatal(err)
		}
		got := loadSorted(t, restored)
		restored.Close()
		if want := wantMigratedTraffic(); !reflect.DeepEqual(got, want) {
			t.Fatalf("restored %v:\n got %+v\nwant %+v", layers, got, want)
		}
	}
}
