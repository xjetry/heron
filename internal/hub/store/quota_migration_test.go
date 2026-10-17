package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"

	"github.com/xjetry/heron/internal/clock"
)

// 冻结节点配额引入后的结构，后续生产 DDL 变化不改变旧版夹具。
var schemaV36 = append(slices.Clone(schemaV35),
	`ALTER TABLE node ADD COLUMN traffic_quota_bytes INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE node ADD COLUMN traffic_quota_mode TEXT NOT NULL DEFAULT 'sum'`,
)

func TestMigrationFromV35AddsTrafficQuota(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 35, seedMinuteRow)
	n, err := s.GetNode(t.Context(), 7)
	if err != nil || n.TrafficQuotaBytes != 0 || n.TrafficQuotaMode != "sum" {
		t.Fatalf("migrated quota = %d/%q, err=%v", n.TrafficQuotaBytes, n.TrafficQuotaMode, err)
	}
	if _, err := s.UpdateNode(t.Context(), 7, NodeEdit{Name: "kept", TrafficResetDay: 1, TrafficQuotaBytes: 1234, TrafficQuotaMode: "max"}); err != nil {
		t.Fatal(err)
	}
	n, err = s.GetNode(t.Context(), 7)
	if err != nil || n.TrafficQuotaBytes != 1234 || n.TrafficQuotaMode != "max" {
		t.Fatalf("updated quota = %d/%q, err=%v", n.TrafficQuotaBytes, n.TrafficQuotaMode, err)
	}
}

func TestTrafficQuotaSnapshotRestore(t *testing.T) {
	t.Parallel()
	for _, version := range []int{35, 36} {
		for _, withMetrics := range []bool{false, true} {
			t.Run(fmt.Sprintf("v%d/metrics=%t", version, withMetrics), func(t *testing.T) {
				s, clk := open(t)
				id, _, err := s.CreateNode(t.Context(), NewNode{Name: "quota"}, hash(1))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.UpdateNode(t.Context(), id, NodeEdit{Name: "quota", TrafficResetDay: 1, TrafficQuotaBytes: 9876, TrafficQuotaMode: "tx"}); err != nil {
					t.Fatal(err)
				}
				config := filepath.Join(t.TempDir(), "config.db")
				if err := s.SnapshotConfig(t.Context(), config); err != nil {
					t.Fatal(err)
				}
				metrics := ""
				if withMetrics {
					metrics = filepath.Join(t.TempDir(), "metrics.db")
					if err := s.SnapshotMetrics(t.Context(), metrics); err != nil {
						t.Fatal(err)
					}
				}
				if version == 35 {
					for _, path := range []string{config, metrics} {
						if path == "" {
							continue
						}
						db, err := sql.Open("sqlite", path)
						if err != nil {
							t.Fatal(err)
						}
						if path == config {
							removeV49Config(t, db)
							removeV48Config(t, db)
							removeV47Config(t, db)
							removeV44Config(t, db)
							removeV41Config(t, db)
							removeV40Config(t, db)
							removeV39Config(t, db)
							if _, err := db.Exec("DROP TABLE cleanup_job"); err != nil {
								t.Fatal(err)
							}
							for _, column := range []string{"traffic_quota_bytes", "traffic_quota_mode"} {
								if _, err := db.Exec("ALTER TABLE node DROP COLUMN " + column); err != nil {
									t.Fatal(err)
								}
							}
						}
						if path == metrics {
							dropTCPRateColumns(t, db)
						}
						if _, err := db.Exec("UPDATE snapshot_meta SET schema_version=35"); err != nil {
							t.Fatal(err)
						}
						if err := db.Close(); err != nil {
							t.Fatal(err)
						}
					}
				}
				target := filepath.Join(t.TempDir(), "restored.db")
				if _, err := Restore(t.Context(), target, config, metrics, "", clk.Now(), slog.Default()); err != nil {
					t.Fatal(err)
				}
				r, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				n, err := r.GetNode(t.Context(), id)
				want, mode := uint64(9876), "tx"
				if version == 35 {
					want, mode = 0, "sum"
				}
				if err != nil || n.TrafficQuotaBytes != want || n.TrafficQuotaMode != mode {
					t.Fatalf("restored quota=%d/%q err=%v; want %d/%s", n.TrafficQuotaBytes, n.TrafficQuotaMode, err, want, mode)
				}
			})
		}
	}
}
