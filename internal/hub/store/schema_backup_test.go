package store

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/xjetry/heron/internal/clock"
)

// hookedBackupFiles 是真实文件系统加两个钩子：before 在每一步真实操作之前调用，返回错误即该步失败、真实操作不执行；
// after 在真实操作成功之后调用。步骤名：create、vacuum、syncfile、link、syncdir（各对应 backupFiles 的一个方法）。
type hookedBackupFiles struct {
	osBackupFiles
	before func(step, path string) error
	after  func(step, path string)
}

func (h hookedBackupFiles) run(step, path string, op func() error) error {
	if h.before != nil {
		if err := h.before(step, path); err != nil {
			return err
		}
	}
	if err := op(); err != nil {
		return err
	}
	if h.after != nil {
		h.after(step, path)
	}
	return nil
}

func (h hookedBackupFiles) CreateExclusive(path string) error {
	return h.run("create", path, func() error { return h.osBackupFiles.CreateExclusive(path) })
}

func (h hookedBackupFiles) Vacuum(ctx context.Context, db *sql.DB, path string) error {
	return h.run("vacuum", path, func() error { return h.osBackupFiles.Vacuum(ctx, db, path) })
}

func (h hookedBackupFiles) SyncFile(path string) error {
	return h.run("syncfile", path, func() error { return h.osBackupFiles.SyncFile(path) })
}

func (h hookedBackupFiles) Link(oldname, newname string) error {
	return h.run("link", newname, func() error { return h.osBackupFiles.Link(oldname, newname) })
}

func (h hookedBackupFiles) SyncDir(dir string) error {
	return h.run("syncdir", dir, func() error { return h.osBackupFiles.SyncDir(dir) })
}

// schemaFixtureAt 把 version 版的冻结库放到 path（库按回滚日志模式建成，主文件就是完整的库）。
func schemaFixtureAt(t *testing.T, version int, path string) {
	t.Helper()
	src, raw := frozenSchemaFixture(t, version)
	raw.Close()
	copyFile(t, src, path)
}

func rawUserVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path, "&mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	return userVersion(t, db)
}

func backupPath(dbPath string, version int, seq uint64) string {
	return fmt.Sprintf("%s%s%d.%d", dbPath, schemaBackupInfix, version, seq)
}

// backupNames 列出 dir 里名字含 ".before-schema-" 的条目，排序后返回。
func backupNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), schemaBackupInfix) {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out
}

func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		delete(record, "time")
		out = append(out, record)
	}
	return out
}

func recordsWithMsg(records []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, r := range records {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

// backupLog 是一条 "database backed up before migration" 的期望记录，bytes 取自文件本身。
func backupLog(t *testing.T, path string, version int, seq uint64) map[string]any {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"level": "INFO", "msg": "database backed up before migration", "path": path,
		"schema_version": float64(version), "bytes": float64(info.Size()), "seq": float64(seq)}
}

// readBackup 用 database/sql 只读打开一份备份，返回 user_version、integrity_check 的结果与 node 表行数。
func readBackup(t *testing.T, path string) (version int, integrity string, nodes int) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path, "&mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	version = userVersion(t, db)
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM node").Scan(&nodes); err != nil {
		t.Fatal(err)
	}
	return version, integrity, nodes
}

func insertNodes(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	for i := range n {
		if _, err := db.Exec("INSERT INTO node (name, token_hash, created_at) VALUES (?, ?, 1)", fmt.Sprintf("n%d", i), []byte(fmt.Sprintf("token-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 备份必须是迁移前库的一致快照，包括已提交但还在 -wal 里、没有检查点进主文件的行：直接复制主文件会丢掉它们。
// 夹具把 v40 库切到 WAL、关掉自动检查点再写行，并证明这些行确实只在 WAL 里（单独复制主文件读不到）；写行的连接
// 在 Open 期间保持打开，最后一个连接关闭时的检查点不会提前发生。路径含单引号的一例证明目标路径经参数绑定。
func TestSchemaBackupCapturesUncheckpointedWAL(t *testing.T) {
	t.Parallel()
	const rows = 50
	for _, dirName := range []string{"plain", "it's"} {
		t.Run(dirName, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), dirName)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "heron.db")
			schemaFixtureAt(t, 40, path)
			raw, err := sql.Open("sqlite", dsn(path, ""))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			raw.SetMaxOpenConns(1)
			var mode string
			if err := raw.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil || mode != "wal" {
				t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
			}
			if _, err := raw.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
				t.Fatal(err)
			}
			insertNodes(t, raw, rows)
			mainOnly := filepath.Join(t.TempDir(), "main-only.db")
			copyFile(t, path, mainOnly)
			if _, _, n := readBackup(t, mainOnly); n != 0 {
				t.Fatalf("fixture main file already holds %d nodes; the rows must be only in the WAL", n)
			}

			var logs bytes.Buffer
			st, err := Open(path, clock.Real(), slog.New(slog.NewJSONHandler(&logs, nil)), MigrateSchema)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer st.Close()
			backup := backupPath(path, 40, 1)
			version, integrity, nodes := readBackup(t, backup)
			if version != 40 || integrity != "ok" || nodes != rows {
				t.Errorf("backup user_version=%d integrity=%q nodes=%d, want 40, ok, %d", version, integrity, nodes, rows)
			}
			info, err := os.Stat(backup)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != 0600 {
				t.Errorf("backup mode = %v, want 0600", perm)
			}
			if got, want := backupNames(t, dir), []string{"heron.db.before-schema-40.1"}; !slices.Equal(got, want) {
				t.Errorf("backup files = %v, want %v (no temporary file left)", got, want)
			}
			if got := userVersion(t, st.r); got != schemaVersion {
				t.Errorf("migrated user_version = %d, want %d", got, schemaVersion)
			}
			records := logRecords(t, &logs)
			wantBackup := backupLog(t, backup, 40, 1)
			if got := recordsWithMsg(records, "database backed up before migration"); len(got) != 1 || !mapsEqual(got[0], wantBackup) {
				t.Errorf("backup log = %v, want %v", got, wantBackup)
			}
		})
	}
}

func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// errInjected 是失败用例注入的错误，用例据此确认 Open 的错误来自被注入的那一步。
var errInjected = errors.New("injected backup failure")

// 备份任一步失败：Open 报错且错误来自备份（不是打开原库）；不迁移（user_version 不变）；临时文件不留；
// 已有备份字节不变。目录同步失败那一例正式文件已发布，保留它。注入前先断言原库可读且准入判定为迁移，
// 证明失败确实发生在备份层而不是更早。
//
// 已有备份放三份：保留策略只留两份，若新备份发布之前就清理旧备份（无论按留两份还是留一份算），至少一份会被删，
// 用例据此能抓住"先删旧备份再写"。
func TestSchemaBackupFailureLeavesDatabaseAndBackupsUntouched(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// files 返回注入用的文件操作；path 是库路径。
		files func(t *testing.T, path string) backupFiles
		// wantErr 是错误里必须出现的片段，injected 为真时还要求 errors.Is(err, errInjected)。
		wantErr  string
		injected bool
		// kept 是失败之后应当存在的本次正式名（目录同步失败时保留；发布 EEXIST 时是抢先占用的文件）。
		kept bool
	}{
		{name: "create", wantErr: "create pre-migration backup", injected: true, files: failAt("create")},
		{name: "vacuum", wantErr: "write pre-migration backup", injected: true, files: failAt("vacuum")},
		{name: "syncfile", wantErr: "sync pre-migration backup", injected: true, files: failAt("syncfile")},
		{name: "syncdir", wantErr: "was written but its directory was not synced", injected: true, kept: true, files: failAt("syncdir")},
		{name: "link-exists", wantErr: "publish pre-migration backup", kept: true, files: func(t *testing.T, path string) backupFiles {
			// 扫描之后、发布之前有人占用了本次的正式名：真实的 link 返回 EEXIST。
			return hookedBackupFiles{before: func(step, p string) error {
				if step == "link" {
					if err := os.WriteFile(p, []byte("squatter"), 0600); err != nil {
						t.Error(err)
					}
				}
				return nil
			}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "heron.db")
			schemaFixtureAt(t, 40, path)
			old := map[string][]byte{
				backupPath(path, 38, 1): []byte("backup one"),
				backupPath(path, 39, 2): []byte("backup two"),
				backupPath(path, 39, 3): []byte("backup three"),
			}
			for p, b := range old {
				if err := os.WriteFile(p, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw := openRawFixture(t, path)
			if action, v, err := schemaAdmission(t.Context(), raw, MigrateSchema); err != nil || action != schemaMigrate || v != 40 {
				t.Fatalf("admission = %v, %d, %v; want schemaMigrate at 40", action, v, err)
			}
			raw.Close()

			st, err := Open(path, clock.Real(), slog.New(slog.NewTextHandler(io.Discard, nil)), MigrateSchema, withBackupFiles(c.files(t, path)))
			if st != nil {
				st.Close()
			}
			if err == nil {
				t.Fatal("Open succeeded, want a backup error")
			}
			if !strings.Contains(err.Error(), "back up database before migrating schema 40") || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("Open error = %v, want a backup error containing %q", err, c.wantErr)
			}
			if c.injected && !errors.Is(err, errInjected) {
				t.Errorf("Open error = %v, want it to wrap the injected failure", err)
			}
			if got := rawUserVersion(t, path); got != 40 {
				t.Errorf("user_version after failed backup = %d, want 40 (no migration)", got)
			}
			for p, b := range old {
				if got, err := os.ReadFile(p); err != nil || !bytes.Equal(got, b) {
					t.Errorf("existing backup %s = %q, %v; want %q unchanged", filepath.Base(p), got, err, b)
				}
			}
			want := []string{"heron.db.before-schema-38.1", "heron.db.before-schema-39.2", "heron.db.before-schema-39.3"}
			if c.kept {
				want = append(want, "heron.db.before-schema-40.4")
			}
			if got := backupNames(t, dir); !slices.Equal(got, want) {
				t.Errorf("backup files = %v, want %v (no temporary file left)", got, want)
			}
			if c.name == "link-exists" {
				if got := fileBytes(t, backupPath(path, 40, 4)); string(got) != "squatter" {
					t.Errorf("publish overwrote the existing backup name: content %q, want squatter", got)
				}
			}
			if c.name == "syncdir" {
				if version, integrity, _ := readBackup(t, backupPath(path, 40, 4)); version != 40 || integrity != "ok" {
					t.Errorf("kept backup user_version=%d integrity=%q, want 40, ok", version, integrity)
				}
			}
		})
	}
}

// 备份期间迁移者一直持着写锁：持锁者之外的写者在备份进行中拿不到排他锁（等满它自己的 busy_timeout 后得到
// SQLITE_BUSY），备份与迁移照常完成，Open 返回之后锁随提交释放，那个写者再试就拿得到。迁移前备份的 seq 与临时
// 文件清理都以"同一时刻只有持锁者在备份"为前提（schema_backup.go 文件头），这条用例钉住备份确实发生在锁内：
// 备份若挪到锁外，钩子里的 BEGIN EXCLUSIVE 会成功。
func TestSchemaBackupRunsUnderWriteLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "heron.db")
	schemaFixtureAt(t, 40, path)
	insertNodes(t, openRawFixture(t, path), 3)
	// 另一个写者只等 100ms：它要证明的是"拿不到"，不必等满 5 秒。
	other, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(100)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var during error
	tried := false
	files := hookedBackupFiles{before: func(step, _ string) error {
		if step != "vacuum" {
			return nil
		}
		tried = true
		conn, err := other.Conn(context.Background())
		if err != nil {
			return err
		}
		defer conn.Close()
		if _, during = conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); during == nil {
			_, err = conn.ExecContext(context.Background(), "ROLLBACK")
			return err
		}
		return nil
	}}
	var logs bytes.Buffer
	st, err := Open(path, clock.Real(), slog.New(slog.NewJSONHandler(&logs, nil)), MigrateSchema, withBackupFiles(files))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if !tried {
		t.Fatal("the backup never reached VACUUM INTO; the case did not exercise the lock")
	}
	var se *sqlite.Error
	if during == nil {
		t.Error("another writer got an exclusive lock while the pre-migration backup was running; the backup is not under the migration's write lock")
	} else if !errors.As(during, &se) || se.Code()&0xff != sqlite3.SQLITE_BUSY {
		t.Errorf("another writer's BEGIN EXCLUSIVE during the backup = %v, want SQLITE_BUSY", during)
	}
	if got := userVersion(t, st.r); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
	if version, integrity, nodes := readBackup(t, backupPath(path, 40, 1)); version != 40 || integrity != "ok" || nodes != 3 {
		t.Errorf("backup user_version=%d integrity=%q nodes=%d, want 40, ok, 3", version, integrity, nodes)
	}
	if got := recordsWithMsg(logRecords(t, &logs), "database schema migrated"); len(got) != 1 {
		t.Errorf("migrated logs = %v, want one", got)
	}
	tx, err := other.Begin()
	if err != nil {
		t.Fatalf("another writer after Open returned: %v", err)
	}
	if _, err := tx.Exec("INSERT INTO setting (key, value) VALUES ('lock-released', '1')"); err != nil {
		t.Errorf("another writer could not write after Open returned: %v", err)
	}
	tx.Rollback()
}

func failAt(step string) func(*testing.T, string) backupFiles {
	return func(*testing.T, string) backupFiles {
		return hookedBackupFiles{before: func(s, _ string) error {
			if s == step {
				return errInjected
			}
			return nil
		}}
	}
}

// 真实文件系统上不能写库所在目录时，备份在创建临时文件那一步失败，不迁移。root 无视目录权限，测不出这一条。
func TestSchemaBackupReadOnlyDirectoryRefusesMigration(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permission; this case needs a non-root user")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "heron.db")
	schemaFixtureAt(t, 40, path)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	st, err := Open(path, clock.Real(), slog.New(slog.NewTextHandler(io.Discard, nil)), MigrateSchema)
	if st != nil {
		st.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "create pre-migration backup") || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Open error = %v, want a permission error from creating the backup", err)
	}
	if got := rawUserVersion(t, path); got != 40 {
		t.Errorf("user_version = %d, want 40 (no migration)", got)
	}
	if got := backupNames(t, dir); len(got) != 0 {
		t.Errorf("backup files = %v, want none", got)
	}
}

// 新旧只看 seq：同一进程里连续三次迁移（同一秒内）seq 依次递增，保留 seq 最大的两份；已有备份的 schema 版本比新备份
// 高、mtime 比新备份新（墙钟回拨）都不影响谁被留下。
func TestSchemaBackupRetentionFollowsSequence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "heron.db")
	// 回退过的库：先前从 v40 升过一次（seq 1），又从 v39 升过一次（seq 2）。两份的 mtime 都在未来，schema 都比下面的
	// v38 高：按 mtime 或按 schema 保留都会删掉新备份。
	future := time.Now().Add(48 * time.Hour)
	for _, b := range []struct {
		version int
		seq     uint64
	}{{40, 1}, {39, 2}} {
		p := backupPath(path, b.version, b.seq)
		if err := os.WriteFile(p, []byte(filepath.Base(p)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatal(err)
		}
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	for round, version := range []int{38, 40, 40} {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(path + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
		}
		schemaFixtureAt(t, version, path)
		st, err := Open(path, clock.Real(), log, MigrateSchema)
		if err != nil {
			t.Fatalf("round %d: Open: %v", round, err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"heron.db.before-schema-40.4", "heron.db.before-schema-40.5"}
	if got := backupNames(t, dir); !slices.Equal(got, want) {
		t.Errorf("backups after three migrations = %v, want %v", got, want)
	}
	var removed []string
	for _, r := range recordsWithMsg(logRecords(t, &logs), "removed an older pre-migration backup") {
		removed = append(removed, filepath.Base(r["path"].(string)))
	}
	if want := []string{"heron.db.before-schema-40.1", "heron.db.before-schema-39.2", "heron.db.before-schema-38.3"}; !slices.Equal(removed, want) {
		t.Errorf("removal logs = %v, want %v", removed, want)
	}
}

// 清理只删常规文件：正式名上的符号链接与目录既不删除（链接指向的文件也不动），也不算进保留的两份；跳过时记日志。
func TestSchemaBackupPruneSkipsNonRegularFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "heron.db")
	for _, seq := range []uint64{1, 2} {
		if err := os.WriteFile(backupPath(path, 40, seq), []byte("regular"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("not a backup"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, backupPath(path, 40, 3)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(backupPath(path, 40, 4), 0700); err != nil {
		t.Fatal(err)
	}
	schemaFixtureAt(t, 40, path)
	var logs bytes.Buffer
	st, err := Open(path, clock.Real(), slog.New(slog.NewJSONHandler(&logs, nil)), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	want := []string{"heron.db.before-schema-40.2", "heron.db.before-schema-40.3", "heron.db.before-schema-40.4", "heron.db.before-schema-40.5"}
	if got := backupNames(t, dir); !slices.Equal(got, want) {
		t.Errorf("backups after migration = %v, want %v", got, want)
	}
	if got := fileBytes(t, target); string(got) != "not a backup" {
		t.Errorf("symlink target changed to %q", got)
	}
	if info, err := os.Lstat(backupPath(path, 40, 3)); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("symlink backup name = %v, %v; want it kept as a symlink", info, err)
	}
	var skipped []string
	for _, r := range recordsWithMsg(logRecords(t, &logs), "skipped a pre-migration backup name that is not a regular file") {
		skipped = append(skipped, filepath.Base(r["path"].(string)))
	}
	if want := []string{"heron.db.before-schema-40.3", "heron.db.before-schema-40.4"}; !slices.Equal(skipped, want) {
		t.Errorf("skip logs = %v, want %v", skipped, want)
	}
}

// 只在要迁移时备份：当前版本、新建、外来零版本库、较新版本、RequireCurrentSchema 下的旧库，都不产生备份文件。
func TestSchemaBackupOnlyWhenMigrating(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		prepare func(t *testing.T, path string)
		policy  SchemaPolicy
		wantErr bool
	}{
		{name: "current", policy: MigrateSchema, prepare: func(t *testing.T, path string) {
			st, err := Open(path, clock.Real(), slog.New(slog.NewTextHandler(io.Discard, nil)), MigrateSchema)
			if err != nil {
				t.Fatal(err)
			}
			st.Close()
		}},
		{name: "create", policy: MigrateSchema, prepare: func(*testing.T, string) {}},
		{name: "foreign", policy: MigrateSchema, wantErr: true, prepare: func(t *testing.T, path string) {
			db, err := sql.Open("sqlite", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec("CREATE TABLE unrelated (id INTEGER PRIMARY KEY)"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "newer", policy: MigrateSchema, wantErr: true, prepare: func(t *testing.T, path string) {
			schemaFixtureAt(t, 40, path)
			db := openRawFixture(t, path)
			if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion+1)); err != nil {
				t.Fatal(err)
			}
			db.Close()
		}},
		{name: "require-current", policy: RequireCurrentSchema, wantErr: true, prepare: func(t *testing.T, path string) {
			schemaFixtureAt(t, 40, path)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "heron.db")
			c.prepare(t, path)
			st, err := Open(path, clock.Real(), slog.New(slog.NewTextHandler(io.Discard, nil)), c.policy)
			if st != nil {
				st.Close()
			}
			if (err != nil) != c.wantErr {
				t.Fatalf("Open error = %v, want error %v", err, c.wantErr)
			}
			if got := backupNames(t, dir); len(got) != 0 {
				t.Errorf("backup files = %v, want none", got)
			}
		})
	}
}

// 迁移整体一个事务：中途一步失败时库停在迁移前的版本、没有迁移日志。hub 被反复拉起、每次迁移都失败时，备份恒为两份，
// 最新一份就是原状（旧二进制能打开它）。夹具让迁移 9 的第一条 ALTER 撞上已存在的同名列，使 7→8 成功、8→9 失败：
// 分步提交时库会停在 8。
func TestSchemaMigrationFailureLeavesDatabaseUnchanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "heron.db")
	schemaFixtureAt(t, 7, path)
	raw := openRawFixture(t, path)
	if _, err := raw.Exec("ALTER TABLE node ADD COLUMN price TEXT NOT NULL DEFAULT ''"); err != nil {
		t.Fatal(err)
	}
	original := describe(t, raw)
	raw.Close()
	for attempt := 1; attempt <= 3; attempt++ {
		var logs bytes.Buffer
		st, err := Open(path, clock.Real(), slog.New(slog.NewJSONHandler(&logs, nil)), MigrateSchema)
		if st != nil {
			st.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "duplicate column name: price") {
			t.Fatalf("attempt %d: Open error = %v, want the duplicate column failure", attempt, err)
		}
		if got := rawUserVersion(t, path); got != 7 {
			t.Errorf("attempt %d: user_version = %d, want 7 (whole migration rolled back)", attempt, got)
		}
		records := logRecords(t, &logs)
		if got := recordsWithMsg(records, "database schema migrated"); len(got) != 0 {
			t.Errorf("attempt %d: migration logs = %v, want none", attempt, got)
		}
		if got, want := recordsWithMsg(records, "database backed up before migration"), backupLog(t, backupPath(path, 7, uint64(attempt)), 7, uint64(attempt)); len(got) != 1 || !mapsEqual(got[0], want) {
			t.Errorf("attempt %d: backup logs = %v, want %v", attempt, got, want)
		}
	}
	if got, want := backupNames(t, dir), []string{"heron.db.before-schema-7.2", "heron.db.before-schema-7.3"}; !slices.Equal(got, want) {
		t.Errorf("backups after three failed starts = %v, want %v", got, want)
	}
	newest, err := sql.Open("sqlite", dsn(backupPath(path, 7, 3), "&mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer newest.Close()
	if got := userVersion(t, newest); got != 7 {
		t.Errorf("newest backup user_version = %d, want 7", got)
	}
	if diff := schemaDifference(describe(t, newest), original, "最新备份", "原状"); diff != "" {
		t.Errorf("newest backup differs from the original database:\n%s", diff)
	}
}

// 进程在发布之前被 SIGKILL（VACUUM INTO 写完或 fsync 进行中）留下的临时文件：重启后迁移照常进行、生成新备份、
// 已有备份字节不变、遗留临时文件被清掉。子进程是本测试二进制自己，经 TestSchemaBackupInterruptedHelper 在指定的
// 一步停住，父进程看到它停住后发 SIGKILL。
func TestSchemaBackupSurvivesSIGKILL(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"vacuum", "syncfile"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "heron.db")
			schemaFixtureAt(t, 40, path)
			insertNodes(t, openRawFixture(t, path), 3)
			older := backupPath(path, 39, 1)
			if err := os.WriteFile(older, []byte("older backup"), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestSchemaBackupInterruptedHelper$", "-test.count=1")
			// 子进程的 TestMain 会建包级夹具目录，被 SIGKILL 后来不及删：放进本用例的临时目录。
			cmd.Env = append(os.Environ(), "HERON_SCHEMA_BACKUP_INTERRUPT="+phase, "HERON_SCHEMA_BACKUP_DB="+path, "TMPDIR="+t.TempDir())
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			blocked := make(chan bool, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					if scanner.Text() == "blocked "+phase {
						blocked <- true
						io.Copy(io.Discard, stdout)
						return
					}
				}
				blocked <- false
			}()
			select {
			case ok := <-blocked:
				if !ok {
					cmd.Wait()
					t.Fatal("helper exited without reaching the blocking step")
				}
			case <-time.After(2 * time.Minute):
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("helper did not reach the blocking step")
			}
			if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			cmd.Wait()
			var leftovers []string
			for _, n := range backupNames(t, dir) {
				if strings.Contains(n, ".tmp-") {
					leftovers = append(leftovers, n)
				}
			}
			if len(leftovers) == 0 {
				t.Fatalf("killed helper left no temporary file (backups %v); the case does not exercise recovery", backupNames(t, dir))
			}

			var logs bytes.Buffer
			st, err := Open(path, clock.Real(), slog.New(slog.NewJSONHandler(&logs, nil)), MigrateSchema)
			if err != nil {
				t.Fatalf("restart Open: %v", err)
			}
			defer st.Close()
			if got := userVersion(t, st.r); got != schemaVersion {
				t.Errorf("user_version after restart = %d, want %d", got, schemaVersion)
			}
			if got, want := backupNames(t, dir), []string{"heron.db.before-schema-39.1", "heron.db.before-schema-40.2"}; !slices.Equal(got, want) {
				t.Errorf("backups after restart = %v, want %v", got, want)
			}
			if got := fileBytes(t, older); string(got) != "older backup" {
				t.Errorf("older backup changed to %q", got)
			}
			if version, integrity, nodes := readBackup(t, backupPath(path, 40, 2)); version != 40 || integrity != "ok" || nodes != 3 {
				t.Errorf("new backup user_version=%d integrity=%q nodes=%d, want 40, ok, 3", version, integrity, nodes)
			}
			var cleaned []string
			for _, r := range recordsWithMsg(logRecords(t, &logs), "removed an incomplete pre-migration backup") {
				cleaned = append(cleaned, filepath.Base(r["path"].(string)))
			}
			if !slices.Equal(cleaned, leftovers) {
				t.Errorf("cleanup logs = %v, want %v", cleaned, leftovers)
			}
		})
	}
}

// TestSchemaBackupInterruptedHelper 只在 TestSchemaBackupSurvivesSIGKILL 起的子进程里运行：迁移库，在
// HERON_SCHEMA_BACKUP_INTERRUPT 指定的一步真实操作完成之后打印一行并停住，等父进程 SIGKILL。
func TestSchemaBackupInterruptedHelper(t *testing.T) {
	phase := os.Getenv("HERON_SCHEMA_BACKUP_INTERRUPT")
	if phase == "" {
		t.Skip("helper process for TestSchemaBackupSurvivesSIGKILL")
	}
	files := hookedBackupFiles{after: func(step, _ string) {
		if step == phase {
			fmt.Println("blocked " + step)
			time.Sleep(time.Hour)
		}
	}}
	st, err := Open(os.Getenv("HERON_SCHEMA_BACKUP_DB"), clock.Real(), slog.New(slog.NewTextHandler(io.Discard, nil)), MigrateSchema, withBackupFiles(files))
	if st != nil {
		st.Close()
	}
	t.Fatalf("Open returned (%v) instead of blocking at %s", err, phase)
}

type readDirFails struct{ osBackupFiles }

func (readDirFails) ReadDir(string) ([]fs.DirEntry, error) { return nil, errInjected }

// 存储统计列出库旁的迁移前备份：只认本库文件名、规范写法、常规文件的正式名，按 seq 从新到旧；临时名、符号链接、
// 目录、别的库的备份都不列。列表每次调用独立读取，不随 60 秒复用的 SQL 统计停住。列目录失败时给出错误文本、
// 列表为空，其余读数照常。
func TestStorageStatsListsSchemaBackups(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "heron.db")
	st, err := Open(path, clock.Real(), slog.New(slog.NewTextHandler(io.Discard, nil)), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	older := time.Unix(1_700_000_000, 0)
	newer := time.Unix(1_800_000_000, 0)
	write := func(name, content string, mtime time.Time) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	// seq 大的 mtime 反而更早：顺序只看 seq。
	write("heron.db.before-schema-40.1", "a", newer)
	write("heron.db.before-schema-39.3", "bbb", older)
	write("heron.db.before-schema-40.6.tmp-1-abcdef01", "temporary", newer)
	write("heron.db.before-schema-40.07", "not canonical", newer)
	write("other.db.before-schema-40.9", "another database", newer)
	if err := os.Symlink(filepath.Join(dir, "heron.db.before-schema-40.1"), filepath.Join(dir, "heron.db.before-schema-38.5")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "heron.db.before-schema-37.8"), 0700); err != nil {
		t.Fatal(err)
	}
	stats, err := st.StorageStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []SchemaBackup{
		{Path: filepath.Join(dir, "heron.db.before-schema-39.3"), SchemaVersion: 39, Seq: 3, Bytes: 3, ModifiedAt: older.Unix()},
		{Path: filepath.Join(dir, "heron.db.before-schema-40.1"), SchemaVersion: 40, Seq: 1, Bytes: 1, ModifiedAt: newer.Unix()},
	}
	if !slices.Equal(stats.SchemaBackups, want) || stats.SchemaBackupsError != "" {
		t.Errorf("schema backups = %+v, error %q; want %+v", stats.SchemaBackups, stats.SchemaBackupsError, want)
	}
	if stats.WALSizeLimit != walSizeLimit {
		t.Errorf("WALSizeLimit = %d, want %d", stats.WALSizeLimit, walSizeLimit)
	}

	write("heron.db.before-schema-41.10", "cccc", newer)
	again, err := st.StorageStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if again.SQLObservedAt != stats.SQLObservedAt {
		t.Fatalf("SQL stats were recomputed (%d then %d); the case needs the reused snapshot", stats.SQLObservedAt, again.SQLObservedAt)
	}
	if len(again.SchemaBackups) != 3 || again.SchemaBackups[0].Seq != 10 {
		t.Errorf("schema backups on the reused SQL snapshot = %+v, want the new seq 10 first", again.SchemaBackups)
	}

	st.backups = readDirFails{}
	failed, err := st.StorageStats(t.Context())
	if err != nil {
		t.Fatalf("StorageStats with an unreadable directory: %v", err)
	}
	if failed.SchemaBackups != nil || !strings.Contains(failed.SchemaBackupsError, errInjected.Error()) {
		t.Errorf("schema backups = %+v, error %q; want none with the listing error", failed.SchemaBackups, failed.SchemaBackupsError)
	}
	if failed.DBBytes == 0 || len(failed.Tables) == 0 || failed.WALSizeLimit != walSizeLimit {
		t.Errorf("other readings missing when listing fails: %+v", failed)
	}
}
