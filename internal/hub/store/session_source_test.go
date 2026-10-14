package store

import (
	"database/sql"
	"slices"
	"testing"
	"time"
)

// 冻结会话来源列引入后的结构，后续生产 DDL 变化不改变旧版夹具。
var schemaV42 = append(slices.Clone(schemaV41),
	`ALTER TABLE admin_session ADD COLUMN source TEXT NOT NULL DEFAULT ''`,
)

// 迁移 42：旧库已有的会话没有来源记录，source 取空串（= 未知），不回填别的值；行内其余列原样保留。
func TestMigrationFromV41AddsSessionSource(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 41, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec("INSERT INTO admin_session (token_hash, created_at, last_used_at, expires_at) VALUES (x'00000000000000000000000000000000000000000000000000000000000000ff', 10, 20, 4102444800)"); err != nil {
			t.Fatal(err)
		}
	})
	rows, err := s.Sessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Source != "" {
		t.Fatalf("sessions after migration = %+v, want one row with empty source", rows)
	}
	if got := rows[0]; got.TokenHash[31] != 0xff || !got.CreatedAt.Equal(time.Unix(10, 0).UTC()) || !got.LastUsedAt.Equal(time.Unix(20, 0).UTC()) {
		t.Fatalf("session row content changed across migration: %+v", got)
	}
}

// 写入与读取同一口径：CommitAdminSecurity 写入的 source 由 Sessions 原样读回；空串保持空串，不被默认值替换。
func TestCommitAdminSecurityStoresSessionSource(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	if err := s.SetAdminPassword(t.Context(), "original"); err != nil {
		t.Fatal(err)
	}
	var h1, h2 [32]byte
	h1[0], h2[0] = 1, 2
	seedSession(t, s, h1, clk.Now(), clk.Now().Add(time.Hour), "203.0.113.10")
	seedSession(t, s, h2, clk.Now(), clk.Now().Add(time.Hour), "")
	if got, ok := lookupSession(t, s, h1); !ok || got.Source != "203.0.113.10" {
		t.Fatalf("source = %+v %v, want 203.0.113.10", got, ok)
	}
	if got, ok := lookupSession(t, s, h2); !ok || got.Source != "" {
		t.Fatalf("empty source = %+v %v, want empty", got, ok)
	}
}
