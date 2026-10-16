package store

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

// 快照、指标层与目标库都可能见过已清理的投递行。恢复后新批次必须越过三者的高水位，不能只越过现存行。
func TestRestoreThenRecordTransitionPreservesBatchSequence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		metrics          bool
		targetHigh, want int64
	}{
		{"config_highest", false, 30, 51},
		{"metrics_highest", true, 30, 71},
		{"target_highest", true, 90, 91},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, ids, channels, _ := alertFixture(t)
			rule := saveRule(t, source, AlertRule{Kind: KindOffline})
			event := recordTargets(t, source, rule.ID, ids[0], DeliveryTarget{ChannelID: channels[0].ID})
			retire := func(s *Store, high int64) {
				t.Helper()
				err := s.write(t.Context(), func(tx *sql.Tx) error {
					if _, err := tx.Exec("INSERT INTO alert_delivery (id,event_id,channel_id,batch_id) VALUES (?,?,?,?)", high, event.ID, channels[0].ID, high); err != nil {
						return err
					}
					_, err := tx.Exec("DELETE FROM alert_delivery WHERE id=?", high)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			retire(source, 50)
			config := filepath.Join(t.TempDir(), "config.db")
			if err := source.SnapshotConfig(t.Context(), config); err != nil {
				t.Fatal(err)
			}
			metrics := ""
			if tc.metrics {
				retire(source, 70)
				metrics = filepath.Join(t.TempDir(), "metrics.db")
				if err := source.SnapshotMetrics(t.Context(), metrics); err != nil {
					t.Fatal(err)
				}
			}
			target, _, _, _ := alertFixture(t)
			retire(target, tc.targetHigh)
			if err := target.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Restore(t.Context(), target.path, config, metrics, "", source.clk.Now(), slog.Default()); err != nil {
				t.Fatal(err)
			}
			restored, err := Open(target.path, clock.Real(), slog.Default(), RequireCurrentSchema)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			next, err := restored.RecordTransition(t.Context(), rule.ID, ids[0], StateOK, "", time.Time{}, AlertEvent{Transition: TransitionRecovered, At: source.clk.Now()}, []DeliveryTarget{{ChannelID: channels[0].ID}})
			if err != nil {
				t.Fatalf("record transition after restore: %v", err)
			}
			saved, err := restored.GetAlertEvent(t.Context(), next.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(saved.Deliveries) != 1 || saved.Deliveries[0].ID != tc.want || saved.Deliveries[0].BatchID != tc.want {
				t.Fatalf("delivery after restore=%+v, want new row id and batch %d", saved.Deliveries, tc.want)
			}
		})
	}
}

// 库里每张带 node_id 列的表恰好落在一类里：配置层从属（随 node 行同删）、时序（登记作业后分块清理）、删节点时
// 保留。两份清单各自对齐：时序清单恰是两族的全部表，配置清单恰是其余带 node_id 的表减去保留的表。
func TestNodeDependentTablesComplete(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	actual := tablesWithNodeID(t, s)
	history := append(slices.Clone(metricFamily.tables), probeFamily.tables...)
	if got := slices.Sorted(slices.Values(nodeHistoryTables)); !slices.Equal(got, slices.Sorted(slices.Values(history))) {
		t.Errorf("nodeHistoryTables must be exactly the time-series tables: got=%v want=%v", got, history)
	}
	var config []string
	for _, table := range actual {
		if !slices.Contains(history, table) && !slices.Contains(keptOnNodeDelete, table) {
			config = append(config, table)
		}
	}
	if got := slices.Sorted(slices.Values(nodeConfigTables)); !slices.Equal(got, config) {
		t.Errorf("nodeConfigTables must be every other node_id table except the kept ones: got=%v want=%v", got, config)
	}
	want := slices.Concat(nodeConfigTables, nodeHistoryTables, keptOnNodeDelete)
	slices.Sort(want)
	if !slices.Equal(actual, want) {
		t.Errorf("node-dependent classification must cover each node_id table exactly once: database=%v classified=%v", actual, want)
	}
}

// v16 的完整 DDL：v15 加上主题元数据、启用约束与文件表。
var schemaV16 = append(slices.Clone(schemaV15),
	`CREATE TABLE theme (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  version TEXT NOT NULL,
  preview TEXT NOT NULL,
  uploaded_at INTEGER NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1))
)`,
	`CREATE UNIQUE INDEX theme_enabled ON theme (enabled) WHERE enabled = 1`,
	`CREATE TABLE theme_file (
  theme_id TEXT NOT NULL,
  path TEXT NOT NULL,
  content BLOB NOT NULL,
  PRIMARY KEY (theme_id, path)
)`)

func TestRestoreRecordMigration(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 16, func(t *testing.T, db *sql.DB) {
		seedMinuteRow(t, db)
		if _, err := db.Exec("INSERT INTO theme (id,name,version,preview,uploaded_at,enabled) VALUES ('kept','Kept','1','',1,1)"); err != nil {
			t.Fatal(err)
		}
	})
	if current, _, err := migrated.ThemeSelection(t.Context()); err != nil || current.ID != "" {
		t.Errorf("legacy theme selection = %+v %v, want builtin", current, err)
	}
	var count int
	if err := migrated.r.QueryRow("SELECT count(*) FROM restore_record").Scan(&count); err != nil || count != 0 {
		t.Errorf("restore record migration: count=%d err=%v", count, err)
	}
	fresh, _ := open(t)
	for _, s := range []*Store{migrated, fresh} {
		var primary int
		if err := s.r.QueryRow("SELECT coalesce(sum(pk),0) FROM pragma_table_info('restore_record') WHERE name='id' AND type='TEXT'").Scan(&primary); err != nil {
			t.Fatal(err)
		}
		if primary != 1 {
			t.Errorf("restore record id must be TEXT primary key: got pk=%d", primary)
		}
	}
}

func TestRestoreRecordUnion(t *testing.T) {
	t.Parallel()
	source, _ := open(t)
	target, _ := open(t)
	const a = "00000000000000000000000000000001"
	const b = "00000000000000000000000000000002"
	const c = "00000000000000000000000000000003"
	for _, seed := range []struct {
		s   *Store
		ids []string
	}{{source, []string{a, b}}, {target, []string{a, c}}} {
		for _, id := range seed.ids {
			if _, err := seed.s.w.Exec("INSERT INTO restore_record (id,restored_at,config_taken_at,metrics_taken_at,orphans) VALUES (?,7,6,NULL,'{}')", id); err != nil {
				t.Fatal(err)
			}
		}
	}
	config := filepath.Join(t.TempDir(), "config.db")
	if err := source.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := Restore(t.Context(), target.path, config, "", "", time.Unix(9000, 0), slog.Default())
		if err != nil {
			t.Fatalf("restore must retain independent records even at the same second: %v", err)
		}
		var actual []string
		for table := range result.Orphans {
			actual = append(actual, table)
		}
		want := slices.Concat(nodeConfigTables, nodeHistoryTables)
		slices.Sort(actual)
		slices.Sort(want)
		if !slices.Equal(actual, want) {
			t.Errorf("restore orphan summary must enumerate both node lists: got=%v want=%v", actual, want)
		}
	}
	db, err := sql.Open("sqlite", target.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var oldIDs string
	if err := db.QueryRow("SELECT group_concat(id, ',') FROM (SELECT id FROM restore_record WHERE restored_at=7 ORDER BY id)").Scan(&oldIDs); err != nil {
		t.Fatal(err)
	}
	if want := a + "," + b + "," + c; oldIDs != want {
		t.Errorf("audit union must retain snapshot and target IDs once, including equal business fields: got=%s want=%s", oldIDs, want)
	}
	rows, err := db.Query("SELECT id FROM restore_record WHERE restored_at=9000")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	idPattern := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if !idPattern.MatchString(id) {
			t.Errorf("new audit id must be 128-bit lowercase hex: %q", id)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Errorf("two restores in the same second must retain two distinct records: %v", ids)
	}
}

// 恢复的写事务在开始时就拿目标库的写锁：目标库此刻被另一个写者持着锁时，恢复等它提交后再校验与写入，而不是
// 先读、到第一次写时才发现锁被占或快照已过时而失败。
func TestRestoreWaitsForConcurrentTargetWriter(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	source, _ := open(t)
	config := filepath.Join(t.TempDir(), "config.db")
	if err := source.SnapshotConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	target, path := openAt(t)
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	holder, err := sql.Open("sqlite", dsn(path, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	conn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	restored := make(chan error, 1)
	go func() {
		_, err := Restore(ctx, path, config, "", "", source.clk.Now(), slog.Default())
		restored <- err
	}()
	// 持锁一段远短于 busyTimeout 的时间：恢复走到写事务时锁仍被占着，放锁之后它仍在自己的 busyTimeout 之内。
	select {
	case err := <-restored:
		t.Fatalf("restore returned while the target's write lock was held: %v", err)
	case <-time.After(busyTimeout / 5):
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := <-restored; err != nil {
		t.Fatalf("restore after the concurrent writer committed: %v", err)
	}
}
