package store

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

// schemaV47 冻结 v47 的完整 DDL：v46 加上 token 失效时刻列与推进节点资料版本的 token 触发器（迁移 47 的两组语句）。
var schemaV47 = append(append(slices.Clone(schemaV46), migrationV47Config...), migrationV47Runtime...)

// 已有 token 迁移后不过期，行为不变；迁移建的触发器随即生效。
func TestMigrationFromV46AddsTokenExpiry(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 46, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec("INSERT INTO api_token (name, token_hash, created_at, permissions, all_nodes) VALUES ('legacy', ?, 1, '[]', 0)", hash(1)); err != nil {
			t.Fatal(err)
		}
	})
	list, err := s.ListAPITokens(t.Context())
	if err != nil || len(list) != 1 {
		t.Fatalf("tokens after migration: %+v %v", list, err)
	}
	if !list[0].ExpiresAt.IsZero() {
		t.Fatalf("legacy token gained an expiry: %v", list[0].ExpiresAt)
	}
	before := revisionOf(t, s)
	mustExec(t, s, "UPDATE api_token SET all_nodes = 1 WHERE id = ?", list[0].ID)
	if revisionOf(t, s) == before {
		t.Fatal("migrated database does not bump the node list revision when a token's scope flips")
	}
}

// 46 的配置快照恢复到 47：快照迁移只给配置层加列，旧 token 原样回来、不过期。
func TestRestoreConfigSnapshotFrom46KeepsTokens(t *testing.T) {
	t.Parallel()
	path, db := frozenSchemaFixture(t, 46)
	if _, err := db.Exec(`INSERT INTO api_token (name, token_hash, created_at, permissions, all_nodes) VALUES ('kept', ?, 1, '["configure"]', 1)`, hash(1)); err != nil {
		t.Fatal(err)
	}
	source := &Store{path: path, clk: clock.NewFake(time.Unix(2_000_000_000, 0))}
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
	list, err := s.ListAPITokens(t.Context())
	if err != nil || len(list) != 1 || list[0].Name != "kept" || !list[0].ExpiresAt.IsZero() ||
		!slices.Equal(list[0].Permissions, []Permission{PermissionConfigure}) {
		t.Fatalf("restored tokens: %+v %v", list, err)
	}
}
