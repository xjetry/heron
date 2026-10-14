package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func TestSnapshotClassificationComplete(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	// 会话与注册窗口不进快照，避免复活已撤销的授权；恢复记录不能自愈，随配置备份。
	// 主题文件与原包按变更单独备份；sqlite_sequence 是每层都携带的分配簿记，不计入数据表分层等式。
	// hub_coordination 是运行中的 hub 与离线写者之间的协调状态，只对读它的那个进程有意义，恢复时重新种子。
	excluded := []string{"admin_session", "register_window", "theme_file", "theme_package", "sqlite_sequence", "node_update", "hub_coordination"}
	classified := append(append(slices.Clone(configSnapshotTables), metricsSnapshotTables...), excluded...)
	slices.Sort(classified)
	if actual := tableNames(t, s.r); !reflect.DeepEqual(actual, classified) {
		t.Fatalf("snapshot classification must cover each table exactly once: database=%v classified=%v", actual, classified)
	}
}

func TestSnapshotFiles(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := context.Background()
	if err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO node (id,name,token_hash,created_at) VALUES (42,'deleted',x'01',0); DELETE FROM node")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, layer := range []string{"config", "metrics"} {
		t.Run(layer, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snapshot.db")
			fn, tables := s.SnapshotConfig, configSnapshotTables
			if layer == "metrics" {
				fn, tables = s.SnapshotMetrics, metricsSnapshotTables
			}
			if err := fn(ctx, path); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", dsn(path, "&mode=ro"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			want := append(slices.Clone(tables), "snapshot_meta", "sqlite_sequence")
			if layer == "config" {
				want = append(want, "snapshot_theme")
			}
			slices.Sort(want)
			if got := tableNames(t, db); !reflect.DeepEqual(got, want) {
				t.Fatalf("snapshot tables=%v want=%v", got, want)
			}
			var version int
			var at int64
			var gotLayer string
			if err := db.QueryRow("SELECT schema_version,taken_at,layer FROM snapshot_meta").Scan(&version, &at, &gotLayer); err != nil {
				t.Fatal(err)
			}
			if version != schemaVersion || at != clk.Now().Unix() || gotLayer != layer {
				t.Fatalf("snapshot meta=(%d,%d,%s)", version, at, gotLayer)
			}
			var seq int
			if err := db.QueryRow("SELECT seq FROM sqlite_sequence WHERE name='node'").Scan(&seq); err != nil {
				t.Fatal(err)
			}
			if seq != 42 {
				t.Fatalf("node sequence=%d want=42", seq)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatalf("snapshot mode=%o want=600", info.Mode().Perm())
			}
			out, err := exec.Command("sqlite3", path, "PRAGMA integrity_check;").CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != "ok" {
				t.Fatalf("sqlite3 integrity=%q err=%v", out, err)
			}
			if err := fn(ctx, path); err == nil {
				t.Fatal("snapshot overwrote existing destination")
			}
		})
	}
}

func TestSnapshotCrossTableConsistency(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		for ctx.Err() == nil {
			err := s.write(ctx, func(tx *sql.Tx) error {
				_, err := tx.Exec(`DELETE FROM node; DELETE FROM alert_rule;
				INSERT INTO node (name,token_hash,created_at) VALUES ('paired',x'01',0);
				INSERT INTO alert_rule (id,name,kind,created_at) SELECT id,'paired','offline',0 FROM node;`)
				return err
			})
			if err != nil && ctx.Err() == nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	dir := t.TempDir()
	for i := 0; i < 100; i++ {
		path := filepath.Join(dir, fmt.Sprintf("%d.db", i))
		if err := s.SnapshotConfig(ctx, path); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", dsn(path, "&mode=ro"))
		if err != nil {
			t.Fatal(err)
		}
		var mismatch int
		err = db.QueryRow(`SELECT (SELECT count(*) FROM (SELECT id FROM node EXCEPT SELECT id FROM alert_rule)) +
			(SELECT count(*) FROM (SELECT id FROM alert_rule EXCEPT SELECT id FROM node))`).Scan(&mismatch)
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		if mismatch != 0 {
			t.Fatalf("snapshot contains node/rule from different transactions: mismatch=%d", mismatch)
		}
	}
}
