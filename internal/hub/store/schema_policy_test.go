package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/xjetry/heron/internal/clock"
)

func schemaPolicyFixture(t *testing.T, statements []string, version int) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.db")
	if err := buildSchemaFile(path, statements, version); err != nil {
		t.Fatal(err)
	}
	return path, openRawFixture(t, path)
}

// buildSchemaFile 在 path 上逐条执行 statements，再把 user_version 写成 version，最后关掉连接。库按 dsn 的默认
// 日志模式（rollback journal）写，关掉之后主文件就是完整的库，可以整文件复制。
func buildSchemaFile(path string, statements []string, version int) (err error) {
	db, err := sql.Open("sqlite", dsn(path, ""))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("%w in %q", err, stmt)
		}
	}
	_, err = db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version))
	return err
}

// openRawFixture 以与 Store 相同的 DSN 打开 path，不经 Open 的 schema 准入，用例借它在建好的库上直接改结构或数据。
func openRawFixture(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// frozenTemplate 是一个冻结版本的只读模板库，每个版本在本包进程里只建一次。
type frozenTemplate struct {
	once sync.Once
	path string
	err  error
}

var frozenTemplates sync.Map // version → *frozenTemplate

// frozenSchemaFixture 建一个停在指定版本的库，DDL 按版本号取自 frozenSchemas。冻结 DDL 逐条执行、每条自成一个
// 事务，耗时随版本增长（同机 -race、负载约 20 时实测 v8 约 80ms、v20 约 0.7s、v37 约 2.7s），迁移用例与
// TestFrozenSchemasFollowMigrations 又反复要同几个版本，所以每个版本只在 fixtureDir 里建一次模板，用例拿到的是
// 模板的副本：库的内容与逐条执行的结果相同，用例可以随意写。
func frozenSchemaFixture(t *testing.T, version int) (string, *sql.DB) {
	t.Helper()
	statements := frozenSchema(t, version)
	v, _ := frozenTemplates.LoadOrStore(version, &frozenTemplate{})
	tpl := v.(*frozenTemplate)
	tpl.once.Do(func() {
		path := filepath.Join(fixtureDir, fmt.Sprintf("frozen-v%d.db", version))
		if tpl.err = buildSchemaFile(path, statements, version); tpl.err == nil {
			tpl.path = path
		}
	})
	if tpl.err != nil {
		t.Fatalf("frozen v%d template: %v", version, tpl.err)
	}
	path := filepath.Join(t.TempDir(), "schema.db")
	copyFile(t, tpl.path, path)
	return path, openRawFixture(t, path)
}

func assertSchemaLogs(t *testing.T, buf *bytes.Buffer, want ...map[string]any) {
	t.Helper()
	var got []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		delete(record, "time")
		got = append(got, record)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("schema logs = %v, want %v", got, want)
	}
}

func TestSchemaPolicyRequiresCurrentWithoutChangingV8(t *testing.T) {
	t.Parallel()
	path, raw := frozenSchemaFixture(t, 8)
	seedMinuteRow(t, raw)
	before := describe(t, raw)
	var logs bytes.Buffer
	st, err := Open(path, clock.Real(), slog.New(slog.NewJSONHandler(&logs, nil)), RequireCurrentSchema)
	if st != nil {
		st.Close()
	}
	want := fmt.Sprintf("database schema version 8 is older than this binary (%d); start the new heron-hub serve once to upgrade it (back up the database first)", schemaVersion)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("RequireCurrentSchema error = %v, want %q", err, want)
	}
	if got := userVersion(t, raw); got != 8 {
		t.Errorf("rejected database user_version = %d, want 8", got)
	}
	after := describe(t, raw)
	if diff := schemaDifference(after, before, "打开后", "打开前"); diff != "" {
		t.Errorf("rejected database schema changed:\n%s", diff)
	}
	var name string
	if err := raw.QueryRow("SELECT name FROM node WHERE id = 7").Scan(&name); err != nil || name != "kept" {
		t.Errorf("rejected database node = %q, %v, want kept", name, err)
	}
	assertSchemaLogs(t, &logs)
}

// 迁移成功时日志恰好三行：取写锁之前一行（from 为锁外读到的版本，to 为当前版本），迁移前备份一行，整体提交之后
// 迁移一行（from 为迁移前版本，to 为当前版本）。
func TestSchemaPolicyMigratesAndLogsBackupThenMigration(t *testing.T) {
	t.Parallel()
	for _, version := range []int{7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			t.Parallel()
			path, raw := frozenSchemaFixture(t, version)
			var logs bytes.Buffer
			st, err := Open(path, clock.Real(), slog.New(slog.NewJSONHandler(&logs, nil)), MigrateSchema)
			if err != nil {
				t.Fatalf("MigrateSchema failed: %v", err)
			}
			defer st.Close()
			if got := userVersion(t, raw); got != schemaVersion {
				t.Errorf("migrated user_version = %d, want %d", got, schemaVersion)
			}
			assertSchemaLogs(t, &logs,
				map[string]any{"level": "INFO", "msg": schemaUpgradeLockMsg, "from": float64(version), "to": float64(schemaVersion)},
				backupLog(t, backupPath(path, version, 1), version, 1),
				map[string]any{"level": "INFO", "msg": "database schema migrated", "from": float64(version), "to": float64(schemaVersion)})
		})
	}
}

func TestSchemaPolicyCreatesAndReopensWithoutMigration(t *testing.T) {
	t.Parallel()
	for _, policy := range []SchemaPolicy{MigrateSchema, RequireCurrentSchema} {
		t.Run(fmt.Sprint(policy), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "empty.db")
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&logs, nil))
			st, err := Open(path, clock.Real(), log, policy)
			if err != nil {
				t.Fatalf("empty database open failed: %v", err)
			}
			if got := userVersion(t, st.r); got != schemaVersion {
				t.Errorf("created user_version = %d, want %d", got, schemaVersion)
			}
			if _, _, err := st.CreateNode(t.Context(), NewNode{Name: "new"}, hash(1)); err != nil {
				t.Errorf("created schema is not usable: %v", err)
			}
			st.Close()
			assertSchemaLogs(t, &logs,
				map[string]any{"level": "INFO", "msg": schemaCreateLockMsg, "version": float64(schemaVersion)},
				map[string]any{"level": "INFO", "msg": "database schema created", "version": float64(schemaVersion)})
			logs.Reset()
			st, err = Open(path, clock.Real(), log, policy)
			if err != nil {
				t.Fatalf("current database reopen failed: %v", err)
			}
			st.Close()
			assertSchemaLogs(t, &logs)
		})
	}
}

// 非本项目建的库通常从没调用过 PRAGMA user_version，读出来正好是 0；如果只看版本号
// 就当空库处理，CREATE TABLE 会把 schemaStatements 的全部对象叠进陌生库已有的数据上。
// 夹具不经 dsn 的任何 pragma 建库（sqlite3 建库的默认日志模式就是 DELETE），拒绝时
// 逐字节比较而不只比较表结构：journal_mode(WAL) 这类 pragma 一旦在某个连接上生效就
// 立即改写文件头（第 18—19 字节标出日志模式），比对表结构看不出这种改写。
func TestSchemaPolicyRejectsDatabaseWithTablesButNoVersion(t *testing.T) {
	t.Parallel()
	for _, policy := range []SchemaPolicy{MigrateSchema, RequireCurrentSchema} {
		t.Run(fmt.Sprint(policy), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "foreign.db")
			raw, err := sql.Open("sqlite", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { raw.Close() })
			if _, err := raw.Exec("CREATE TABLE unrelated (id INTEGER PRIMARY KEY, note TEXT)"); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec("INSERT INTO unrelated (note) VALUES ('not a Heron database')"); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			st, err := Open(path, clock.Real(), slog.Default(), policy)
			if st != nil {
				st.Close()
			}
			want := "not a Heron database"
			if err == nil {
				t.Fatalf("foreign database open error = <nil>, want to contain %q", want)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("foreign database open error = %v, want to contain %q", err, want)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Errorf("foreign database bytes changed: before %d bytes, after %d bytes", len(before), len(after))
			}
		})
	}
}

// 负数版本落进旧库分支会去找不存在的 migrations[0]（MigrateSchema）或建议改跑
// serve 升级（RequireCurrentSchema）——两条提示都假定这是本项目的旧库，跟着做都走不通。
// 逐字节比较的理由同上一条；夹具在下面独立建成 DELETE 模式库。
func TestSchemaPolicyRejectsNegativeVersionWithoutSuggestingServe(t *testing.T) {
	t.Parallel()
	for _, policy := range []SchemaPolicy{MigrateSchema, RequireCurrentSchema} {
		t.Run(fmt.Sprint(policy), func(t *testing.T) {
			// 不经 schemaPolicyFixture：它建夹具用的是 dsn，dsn 的 pragma 列表一旦重新
			// 带回 journal_mode(WAL)，"改写前"的快照会在夹具建立的那一刻就已经被写成
			// WAL，测不出 Open 自己有没有再碰这个文件。这条用例要单独验证 Open，
			// 夹具必须独立于 dsn 的实现。
			path := filepath.Join(t.TempDir(), "negative.db")
			raw, err := sql.Open("sqlite", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { raw.Close() })
			if _, err := raw.Exec("PRAGMA user_version = -1"); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			st, err := Open(path, clock.Real(), slog.Default(), policy)
			if st != nil {
				st.Close()
			}
			want := "not a Heron database"
			if err == nil {
				t.Fatalf("negative version open error = <nil>, want to contain %q", want)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("negative version open error = %v, want to contain %q", err, want)
			}
			if strings.Contains(err.Error(), "serve") {
				t.Errorf("negative version error = %v, must not suggest running serve", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Errorf("negative version database bytes changed: before %d bytes, after %d bytes", len(before), len(after))
			}
		})
	}
}

// 守卫挪到 openStore 之后，panic 仍会发生：能观察到挪动的不是 panic 本身，而是
// panic 之前多做的那部分工作留下的副作用。用一个已存在的 v8 库：挪动后的守卫会先
// 把它迁到 v9 再 panic，这正是 store.go 的 SchemaPolicy 注释所说守卫要防的
// "遗漏选择时意外迁移"。这条钉住"库被迁走"这一种副作用；空路径上的另一种副作用见
// 下一条用例。
func TestSchemaPolicyRejectsInvalidPolicy(t *testing.T) {
	t.Parallel()
	for _, policy := range []SchemaPolicy{0, -1, 3} {
		t.Run(fmt.Sprint(policy), func(t *testing.T) {
			path, raw := frozenSchemaFixture(t, 8)
			before := describe(t, raw)
			defer func() {
				if got := recover(); got != "store.Open requires a valid SchemaPolicy" {
					t.Errorf("invalid policy panic = %v, want explicit SchemaPolicy panic", got)
				}
				if got := userVersion(t, raw); got != 8 {
					t.Errorf("invalid policy changed user_version to %d before panicking, want 8", got)
				}
				if diff := schemaDifference(describe(t, raw), before, "打开后", "打开前"); diff != "" {
					t.Errorf("invalid policy changed schema before panicking:\n%s", diff)
				}
			}()
			st, _ := Open(path, clock.Real(), slog.Default(), policy)
			if st != nil {
				st.Close()
			}
		})
	}
}

// 同一种挪动在空路径上留下另一种副作用：挪到 openStore 之后会先建出这个文件再
// panic。这条钉住"文件被建出"，与上一条钉住的"库被迁走"是两种不同的副作用，各自
// 只覆盖自己这条路径上的挪动。
func TestSchemaPolicyRejectsInvalidPolicyBeforeTouchingAMissingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "invalid.db")
	defer func() {
		if got := recover(); got != "store.Open requires a valid SchemaPolicy" {
			t.Errorf("invalid policy panic = %v, want explicit SchemaPolicy panic", got)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("invalid policy created a file at %s before panicking: %v", path, err)
		}
	}()
	st, _ := Open(path, clock.Real(), slog.Default(), SchemaPolicy(0))
	if st != nil {
		st.Close()
	}
}
