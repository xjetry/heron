package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xjetry/heron/internal/clock"
)

// 冻结各版结构，独立于生产迁移中的数据变换。
var schemaV48 = append(slices.Clone(schemaV47),
	`ALTER TABLE traffic ADD COLUMN observed_at INTEGER NOT NULL DEFAULT 0`,
)

var schemaV49 = append(slices.Clone(schemaV48),
	`ALTER TABLE silence ADD COLUMN all_rules INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE silence_rule (
  silence_id INTEGER NOT NULL,
  rule_id INTEGER NOT NULL,
  PRIMARY KEY (silence_id, rule_id)
) WITHOUT ROWID`,
	`CREATE INDEX silence_rule_by_rule ON silence_rule (rule_id)`,
)

func removeV49Config(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("DROP TABLE silence_rule; ALTER TABLE silence DROP COLUMN all_rules"); err != nil {
		t.Fatal(err)
	}
}

func removeV48Config(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("ALTER TABLE traffic DROP COLUMN observed_at"); err != nil {
		t.Fatal(err)
	}
}

func seedNameScopeMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	for i, name := range []string{strings.Repeat("名", 65), strings.Repeat("界", 63) + " x", "kept", " \t\n "} {
		if _, err := db.Exec("INSERT INTO node (id,name,token_hash,created_at) VALUES (?,?,?,1)", i+1, name, hash(byte(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		`INSERT INTO traffic (node_id,boot_id,total_rx,total_tx,period_rx,period_tx,period_start,updated_at) VALUES (1,'',100,200,30,40,1,2)`,
		`INSERT INTO silence (id,name,kind,created_at) VALUES (1,'legacy','once',1)`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
}

func assertMigratedNames(t *testing.T, s *Store) {
	t.Helper()
	for i, want := range []string{strings.Repeat("名", 64), strings.Repeat("界", 63), "kept", "node"} {
		n, err := s.GetNode(t.Context(), int64(i+1))
		if err != nil {
			t.Fatal(err)
		}
		if n.Name != want || utf8.RuneCountInString(n.Name) < 1 || utf8.RuneCountInString(n.Name) > 64 {
			t.Errorf("node %d name = %q, want %q (1-64 runes)", i+1, n.Name, want)
		}
		if clean, err := CleanName(n.Name); err != nil || clean != n.Name {
			t.Errorf("node %d name is not canonical: CleanName = %q, %v", i+1, clean, err)
		}
	}
}

func assertMigratedScopeAndObservation(t *testing.T, db *sql.DB) {
	t.Helper()
	var observed, allRules int
	if err := db.QueryRow("SELECT observed_at FROM traffic WHERE node_id=1").Scan(&observed); err != nil {
		t.Fatal(err)
	}
	if observed != 0 {
		t.Errorf("legacy traffic observed_at = %d, want 0 (unknown)", observed)
	}
	if err := db.QueryRow("SELECT all_rules FROM silence WHERE id=1").Scan(&allRules); err != nil {
		t.Fatal(err)
	}
	if allRules != 1 {
		t.Errorf("legacy silence all_rules = %d, want 1", allRules)
	}
}

func TestMigrationFromV47NormalizesNamesAndPreservesScope(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 47, seedNameScopeMigration)
	assertMigratedNames(t, s)
	assertMigratedScopeAndObservation(t, s.r)
}

func TestMigrationFromV48PreservesSilenceScope(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 48, seedNameScopeMigration)
	assertMigratedScopeAndObservation(t, s.r)
}

// 旧库没有规则关联表；导出它实际拥有的配置表，不在生产快照路径跳过缺表。
func snapshotConfigBefore49(t *testing.T, source *Store, path string) {
	t.Helper()
	tables := slices.DeleteFunc(slices.Clone(configSnapshotTables), func(table string) bool { return table == "silence_rule" })
	if err := source.snapshot(t.Context(), path, "config", tables, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreConfigSnapshotFrom47NormalizesNamesAndPreservesScope(t *testing.T) {
	t.Parallel()
	path, db := frozenSchemaFixture(t, 47)
	seedNameScopeMigration(t, db)
	source := &Store{path: path, clk: clock.NewFake(time.Unix(2_000_000_000, 0))}
	config := filepath.Join(t.TempDir(), "config.db")
	snapshotConfigBefore49(t, source, config)
	target := filepath.Join(t.TempDir(), "target.db")
	if _, err := Restore(t.Context(), target, config, "", "", time.Now(), slog.Default()); err != nil {
		t.Fatal(err)
	}
	s, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertMigratedNames(t, s)
	assertMigratedScopeAndObservation(t, s.r)
}

func TestSnapshotRuleScopeAndTrafficObservationRoundTrip(t *testing.T) {
	t.Parallel()
	source, _ := open(t)
	for _, query := range []string{
		`INSERT INTO node (id,name,token_hash,created_at) VALUES (1,'node',x'01',1)`,
		`INSERT INTO alert_rule (id,name,kind,all_nodes,created_at) VALUES (42,'offline','offline',1,1)`,
		`INSERT INTO silence (id,name,kind,all_nodes,from_at,until_at,created_at,all_rules) VALUES (1,'maintenance','once',1,1,100,1,0)`,
		`INSERT INTO silence_rule (silence_id,rule_id) VALUES (1,42)`,
		`INSERT INTO traffic (node_id,boot_id,total_rx,total_tx,period_rx,period_tx,period_start,updated_at,observed_at) VALUES (1,'',100,200,30,40,1,200,123)`,
	} {
		mustExec(t, source, query)
	}
	config := filepath.Join(t.TempDir(), "config.db")
	if err := source.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.db")
	if _, err := Restore(t.Context(), target, config, "", "", time.Now(), slog.Default()); err != nil {
		t.Fatal(err)
	}
	s, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var observed, ruleID, allRules int
	if err := s.r.QueryRow("SELECT observed_at FROM traffic WHERE node_id=1").Scan(&observed); err != nil || observed != 123 {
		t.Errorf("restored observed_at = %d, %v; want 123", observed, err)
	}
	if err := s.r.QueryRow("SELECT rule_id FROM silence_rule WHERE silence_id=1").Scan(&ruleID); err != nil || ruleID != 42 {
		t.Errorf("restored silence rule = %d, %v; want 42", ruleID, err)
	}
	if err := s.r.QueryRow("SELECT all_rules FROM silence WHERE id=1").Scan(&allRules); err != nil || allRules != 0 {
		t.Errorf("restored all_rules = %d, %v; want 0", allRules, err)
	}
}

// 当前版本快照里的未清洗节点名（绕过写入口直接改出来的）在恢复预检被拒：快照不经写入口，预检是它进库前唯一的检查。
// 旧版快照的同问题由 migrateSnapshot 规范化（TestRestoreConfigSnapshotFrom47NormalizesNamesAndPreservesScope），
// 所以被这条拒绝的只能是当前版本就非法的值。被拒后目标库保持原样。
func TestRestoreRejectsUncleanNodeName(t *testing.T) {
	t.Parallel()
	source, _ := open(t)
	nodeID, _, err := source.CreateNode(t.Context(), NewNode{Name: "ok"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	// 绕过写入口的准入（insertNode 只拦经它的写入），直接改出一个非法名字进快照。
	mustExec(t, source, "UPDATE node SET name = ? WHERE id = ?", "  padded  ", nodeID)
	config := filepath.Join(t.TempDir(), "config.db")
	if err := source.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	source.Close()
	want := fmt.Sprintf("config node %d (\"  padded  \"): name must not have leading or trailing whitespace or control characters; got \"  padded  \"", nodeID)

	t.Run("fresh target", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "target.db")
		_, err := Restore(t.Context(), target, config, "", "", time.Now(), slog.Default())
		if err == nil || err.Error() != want {
			t.Fatalf("err=%v, want %q", err, want)
		}
		var unclean UncleanNameError
		if !errors.As(err, &unclean) {
			t.Fatalf("%T is not an UncleanNameError", err)
		}
		if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("target created despite the rejection")
		}
	})

	t.Run("existing target", func(t *testing.T) {
		seed, target := openAt(t)
		marker, _, err := seed.CreateNode(t.Context(), NewNode{Name: "marker"}, hash(7))
		if err != nil {
			t.Fatal(err)
		}
		seed.Close()
		if _, err := Restore(t.Context(), target, config, "", "", time.Now(), slog.Default()); err == nil || err.Error() != want {
			t.Fatalf("err=%v, want %q", err, want)
		}
		// 目标库保持不变：node 表内容、schema 版本都原样。
		reopened, err := Open(target, clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), slog.Default(), MigrateSchema)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		n, err := reopened.GetNode(t.Context(), marker)
		if err != nil || n.Name != "marker" {
			t.Fatalf("marker node = %+v, %v", n, err)
		}
		if got := nodeCount(t, reopened); got != 1 {
			t.Fatalf("target holds %d nodes after a rejected restore", got)
		}
		db, err := sql.Open("sqlite", dsn(target, "&mode=ro"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if got := userVersion(t, db); got != schemaVersion {
			t.Fatalf("user_version = %d, want %d", got, schemaVersion)
		}
	})
}
