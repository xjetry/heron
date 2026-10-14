package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xjetry/heron/internal/clock"
)

// 冻结离线变更代数表引入后的结构，后续生产 DDL 变化不改变旧版夹具。
var schemaV37 = append(slices.Clone(schemaV36),
	`CREATE TABLE hub_coordination (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  offline_generation INTEGER NOT NULL CHECK (offline_generation >= 0)
)`,
	`INSERT INTO hub_coordination (id, offline_generation) VALUES (1, 0)`,
)

func setOfflineGeneration(t *testing.T, s *Store, g int64) {
	t.Helper()
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE hub_coordination SET offline_generation = ? WHERE id = 1", g)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationFromV36SeedsOfflineGeneration(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 36, seedMinuteRow)
	if g, err := s.OfflineGeneration(t.Context()); err != nil || g != 0 {
		t.Fatalf("migrated offline generation = %d, %v; want 0", g, err)
	}
}

func TestOfflineGenerationOnFreshDatabase(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	if g, err := s.OfflineGeneration(t.Context()); err != nil || g != 0 {
		t.Fatalf("fresh offline generation = %d, %v; want 0", g, err)
	}
}

// 缺行与负值都是库损坏：读侧报错，不能被当成"没有库外变更"而让 hub 永远不重载。负值写不进去（CHECK），
// 夹具关掉约束检查直接写，模拟约束之外改坏的库。
func TestOfflineGenerationRejectsMissingOrNegativeRow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, corrupt, want string
	}{
		{"missing", "DELETE FROM hub_coordination", "row missing"},
		{"negative", "PRAGMA ignore_check_constraints = ON; UPDATE hub_coordination SET offline_generation = -1 WHERE id = 1", "negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "t.db")
			s, err := Open(path, clock.Real(), slog.Default(), MigrateSchema)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", dsn(path, ""))
			if err != nil {
				t.Fatal(err)
			}
			raw.SetMaxOpenConns(1)
			if _, err := raw.Exec(tc.corrupt); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path, clock.Real(), slog.Default(), RequireCurrentSchema)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			g, err := s.OfflineGeneration(t.Context())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("OfflineGeneration on %s row = %d, %v; want an error containing %q", tc.name, g, err, tc.want)
			}
		})
	}
}

// 代数不进任何备份层：36 与 37 的配置层（可带指标层）快照恢复到新建库或已有代数的库，代数都重新种子为 0。
func TestOfflineGenerationSnapshotRestore(t *testing.T) {
	t.Parallel()
	for _, version := range []int{36, 37} {
		for _, withMetrics := range []bool{false, true} {
			for _, existing := range []bool{false, true} {
				t.Run(fmt.Sprintf("v%d/metrics=%t/existing=%t", version, withMetrics, existing), func(t *testing.T) {
					s, clk := open(t)
					setOfflineGeneration(t, s, 5)
					if _, _, err := s.CreateNode(t.Context(), NewNode{Name: "kept"}, hash(1)); err != nil {
						t.Fatal(err)
					}
					config := filepath.Join(t.TempDir(), "config.db")
					if err := s.SnapshotConfig(t.Context(), config); err != nil {
						t.Fatal(err)
					}
					paths := []string{config}
					metrics := ""
					if withMetrics {
						metrics = filepath.Join(t.TempDir(), "metrics.db")
						if err := s.SnapshotMetrics(t.Context(), metrics); err != nil {
							t.Fatal(err)
						}
						paths = append(paths, metrics)
					}
					for _, path := range paths {
						db, err := sql.Open("sqlite", path)
						if err != nil {
							t.Fatal(err)
						}
						var tables int
						if err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name = 'hub_coordination'").Scan(&tables); err != nil || tables != 0 {
							t.Fatalf("snapshot %s carries hub_coordination (count %d, err %v)", path, tables, err)
						}
						if version == 36 {
							if path == metrics {
								dropTCPRateColumns(t, db)
							}
							if path == config {
								removeV44Config(t, db)
								removeV41Config(t, db)
								removeV40Config(t, db)
								removeV39Config(t, db)
							}
							// v36 的配置层还没有清理作业表；指标层本来就没有，IF EXISTS 让两层同一写法。
							if _, err := db.Exec("DROP TABLE IF EXISTS cleanup_job; UPDATE snapshot_meta SET schema_version=36"); err != nil {
								t.Fatal(err)
							}
						}
						if err := db.Close(); err != nil {
							t.Fatal(err)
						}
					}
					target := filepath.Join(t.TempDir(), "restored.db")
					if existing {
						prior, err := Open(target, clock.Real(), slog.Default(), MigrateSchema)
						if err != nil {
							t.Fatal(err)
						}
						setOfflineGeneration(t, prior, 7)
						if err := prior.Close(); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := Restore(t.Context(), target, config, metrics, "", clk.Now(), slog.Default()); err != nil {
						t.Fatal(err)
					}
					r, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
					if err != nil {
						t.Fatal(err)
					}
					defer r.Close()
					if g, err := r.OfflineGeneration(t.Context()); err != nil || g != 0 {
						t.Fatalf("restored offline generation = %d, %v; want 0", g, err)
					}
					if n, err := r.ListNodes(t.Context()); err != nil || len(n) != 1 {
						t.Fatalf("restored nodes = %v, %v; want the snapshot's node", n, err)
					}
				})
			}
		}
	}
}
