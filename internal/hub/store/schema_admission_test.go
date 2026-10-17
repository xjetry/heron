package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// admissionCommitReader 先读完真实查询并释放读锁，再提交另一条连接的建库事务。
// 返回的 Row 只重放刚才读到的值，使提交确定发生在首次读取之后、准入继续之前；
// 若准入把版本和对象数拆成两条语句，第二条会读到提交后的对象。
type admissionCommitReader struct {
	t         *testing.T
	db        *sql.DB
	afterRead func()
}

func (r *admissionCommitReader) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	r.t.Helper()
	if r.afterRead == nil {
		return r.db.QueryRowContext(ctx, query, args...)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		r.t.Fatal(err)
	}
	values, dest := make([]any, len(columns)), make([]any, len(columns))
	for i := range values {
		dest[i] = &values[i]
	}
	if !rows.Next() {
		r.t.Fatalf("admission query returned no row: %v", rows.Err())
	}
	if err := rows.Scan(dest...); err != nil {
		r.t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		r.t.Fatal(err)
	}
	r.afterRead()
	r.afterRead = nil
	return r.db.QueryRowContext(ctx, "SELECT "+strings.TrimSuffix(strings.Repeat("?,", len(values)), ","), values...)
}

// 准入的版本号与对象数必须来自同一快照：另一条连接在首次读取后提交完整 schema，
// 本次仍须按读到的空库返回 schemaCreate，由调用方拿到写锁后重新准入。
func TestSchemaAdmissionVersionAndObjectsShareSnapshot(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"delete", "wal"} {
		for _, policy := range []SchemaPolicy{MigrateSchema, RequireCurrentSchema} {
			t.Run(fmt.Sprintf("%s/%d", mode, policy), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "heron.db")
				db, err := sql.Open("sqlite", dsn(path, ""))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Close() })
				if _, err := db.Exec("PRAGMA journal_mode=" + mode); err != nil {
					t.Fatal(err)
				}
				holder := holdSchemaLock(t, path)
				if err := createSchema(t.Context(), holder); err != nil {
					t.Fatal(err)
				}
				reader := &admissionCommitReader{t: t, db: db, afterRead: func() {
					if err := holder.Commit(); err != nil {
						t.Fatal(err)
					}
				}}
				action, version, err := schemaAdmission(t.Context(), reader, policy)
				if err != nil {
					t.Fatalf("admission across concurrent schema commit: %v", err)
				}
				if action != schemaCreate || version != 0 {
					t.Fatalf("admission = (%v, %d), want (schemaCreate, 0) from the empty snapshot", action, version)
				}
				if got := userVersion(t, db); got != schemaVersion {
					t.Fatalf("committed user_version = %d, want %d", got, schemaVersion)
				}
			})
		}
	}
}
