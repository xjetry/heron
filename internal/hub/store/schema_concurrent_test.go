package store

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/xjetry/heron/internal/clock"
)

// 同一个库被两个打开者并发建库或迁移：先到的那个以 BEGIN IMMEDIATE 持着写锁做完全部改动、还没提交时，后到的
// Open 已经在锁外看到"空库"或"旧版本"。后到者必须在 begin 处等对方提交，拿到锁后重新准入、发现已是当前版本就
// 既不备份也不建表或迁移；准入若留在锁外，它会照旧去建表或迁移，撞上对方已建好的对象；事务若以 DEFERRED 开始，
// 它不在 begin 处等，读到旧版本后第一次写就得到 SQLITE_BUSY。
//
// 先到者用 _txlock=immediate 的连接模拟另一个进程里的 migrate：SQLite 的文件锁对同一进程里的两条连接与两个进程
// 一样生效（不用 cache=shared，见 store.go 的连接笔记）。
//
// 先到者什么时候提交，以后到者的日志为准：migrateLocked 在锁外准入决定建库或迁移之后、begin 之前记一行取锁日志
// （logSchemaLockTaking 的不变式），那一行出现就意味着后到者已经读到空库或旧版本，接下来只会在 begin 处等锁；
// 先到者在那之后才提交，后到者必然在锁内重新准入、读到当前版本、记恰好一行 already current。按固定时长持锁不行：
// 后到者在锁外走到那次读之前要开连接、设 pragma，负载高时超过固定时长，它在提交之后才读、直接读到当前版本、
// 一行日志也不记，用例红在"没走到等锁路径"而不是缺陷上。

const alreadyCurrentMsg = "database schema already current: another process created or migrated it while this one waited for the write lock"

// lockReachGuard 只防后到者挂死，不是用例的时序前提：后到者在锁外的那次读若撞上对方的 EXCLUSIVE 锁，会先等满
// busy_timeout 再以错误返回（opened 先触发），所以这个上限要比 busy_timeout 长得多。
const lockReachGuard = 6 * busyTimeout

// lockSignal 包住 JSON handler：Open 记下取锁那一行时关闭 reached，其余照常写进缓冲。WithAttrs / WithGroup 要保住
// 包装，否则 Open 一旦派生 logger，取锁那一行就从包装外面写过去、用例等不到信号。
type lockSignal struct {
	slog.Handler
	state *lockSignalState
}

type lockSignalState struct {
	once    sync.Once
	reached chan struct{}
}

func (h *lockSignal) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == schemaCreateLockMsg || r.Message == schemaUpgradeLockMsg {
		h.state.once.Do(func() { close(h.state.reached) })
	}
	return h.Handler.Handle(ctx, r)
}

func (h *lockSignal) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &lockSignal{Handler: h.Handler.WithAttrs(attrs), state: h.state}
}

func (h *lockSignal) WithGroup(name string) slog.Handler {
	return &lockSignal{Handler: h.Handler.WithGroup(name), state: h.state}
}

// holdSchemaLock 在 path 上开一个 BEGIN IMMEDIATE 事务并返回它，调用方在事务里做改动、决定何时提交或回滚。
func holdSchemaLock(t *testing.T, path string) *sql.Tx {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path, "&_txlock=immediate"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx
}

type openResult struct {
	st  *Store
	err error
}

// openAsync 在另一个 goroutine 里以 MigrateSchema 打开 path：opened 收 Open 的结果，reached 在它记下取锁那一行时关闭。
func openAsync(path string, logs *bytes.Buffer) (opened <-chan openResult, reached <-chan struct{}) {
	done := make(chan openResult, 1)
	state := &lockSignalState{reached: make(chan struct{})}
	handler := &lockSignal{Handler: slog.NewJSONHandler(logs, nil), state: state}
	go func() {
		st, err := Open(path, clock.Real(), slog.New(handler), MigrateSchema)
		done <- openResult{st, err}
	}()
	return done, state.reached
}

// commitAfterHold 等后到的 Open 记下取锁那一行再提交 holder。Open 在那之前就返回，说明它没有在锁内重新准入；
// 迟迟不记那一行，说明它卡在锁外（lockReachGuard）。
func commitAfterHold(t *testing.T, holder *sql.Tx, opened <-chan openResult, reached <-chan struct{}) openResult {
	t.Helper()
	select {
	case r := <-opened:
		if r.st != nil {
			r.st.Close()
		}
		t.Fatalf("Open returned while another connection held the write lock with uncommitted schema changes: %v", r.err)
	case <-reached:
	case <-time.After(lockReachGuard):
		t.Fatalf("Open did not log that it is taking the write lock within %v", lockReachGuard)
	}
	if err := holder.Commit(); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	return <-opened
}

// 新库：先到者建好全部表、写好版本但未提交，后到的 Open 等它提交后成功返回，不再建表、不记 created。
func TestSchemaCreateWaitsForConcurrentCreator(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "heron.db")
	holder := holdSchemaLock(t, path)
	if err := createSchema(t.Context(), holder); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	opened, reached := openAsync(path, &logs)
	r := commitAfterHold(t, holder, opened, reached)
	if r.err != nil {
		t.Fatalf("Open after the concurrent creator committed: %v", r.err)
	}
	defer r.st.Close()
	if got := userVersion(t, r.st.r); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
	if _, _, err := r.st.CreateNode(t.Context(), NewNode{Name: "after-race"}, hash(1)); err != nil {
		t.Errorf("database is not usable after the concurrent create: %v", err)
	}
	records := logRecords(t, &logs)
	if got := recordsWithMsg(records, "database schema created"); len(got) != 0 {
		t.Errorf("the waiting Open logged %v; the concurrent creator built the schema, not it", got)
	}
	if got := recordsWithMsg(records, alreadyCurrentMsg); len(got) != 1 {
		t.Errorf("logs = %v, want exactly one %q", records, alreadyCurrentMsg)
	}
}

// 旧库（v40）：先到者在 BEGIN IMMEDIATE 里迁到当前版本、未提交；后到的 Open 在锁外准入为迁移，在 begin 处等。
// 先到者提交后它在锁内重新准入，读到当前版本：不备份、不迁移、不记 migrated。备份只由持锁的那个做（先到者在这里
// 模拟的是另一个已备份过的进程，所以库旁一份备份都不该有）。
func TestSchemaMigrateWaitsForConcurrentMigrator(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "heron.db")
	schemaFixtureAt(t, 40, path)
	insertNodes(t, openRawFixture(t, path), 3)
	holder := holdSchemaLock(t, path)
	for next := 41; next <= schemaVersion; next++ {
		if err := migrations[next](holder); err != nil {
			t.Fatalf("holder migrate to %d: %v", next, err)
		}
	}
	if _, err := holder.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	opened, reached := openAsync(path, &logs)
	r := commitAfterHold(t, holder, opened, reached)
	if r.err != nil {
		t.Fatalf("Open after the concurrent migrator committed: %v", r.err)
	}
	defer r.st.Close()
	if got := userVersion(t, r.st.r); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
	records := logRecords(t, &logs)
	if got := recordsWithMsg(records, "database schema migrated"); len(got) != 0 {
		t.Errorf("the waiting Open logged %v; the concurrent migrator migrated, not it", got)
	}
	if got := recordsWithMsg(records, alreadyCurrentMsg); len(got) != 1 {
		t.Errorf("logs = %v, want exactly one %q", records, alreadyCurrentMsg)
	}
	if got := backupNames(t, dir); len(got) != 0 {
		t.Errorf("backups = %v, want none: the waiting Open found the schema current under the lock", got)
	}
}

// 刚建好、提交、还是回滚日志模式的库：另一条连接随即 BEGIN IMMEDIATE 持着写锁（并发打开时等锁的一方重新准入就是
// 这样），这时 Open 读到当前版本，不取锁，但切 WAL 仍须成功——改 journal_mode 时驱动不调 busy handler，立即
// SQLITE_BUSY，Open 要自己等对方放锁。持锁者在固定时长后才提交：Open 若在提交之前就返回，说明切换没有撞上锁，
// 用例没有考到要考的东西，同样判红。
func TestSchemaWALSwitchWaitsForConcurrentLock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "heron.db")
	if err := buildSchemaFile(path, schemaStatements(), schemaVersion); err != nil {
		t.Fatal(err)
	}
	holder := holdSchemaLock(t, path)
	type commit struct {
		start time.Time
		err   error
	}
	committed := make(chan commit, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		start := time.Now()
		committed <- commit{start, holder.Commit()}
	}()
	var logs bytes.Buffer
	opened, _ := openAsync(path, &logs)
	r := <-opened
	returned := time.Now()
	c := <-committed
	if c.err != nil {
		t.Fatalf("holder commit: %v", c.err)
	}
	if returned.Before(c.start) {
		t.Error("Open returned before the holder started to commit; the WAL switch never met the lock")
	}
	if r.err != nil {
		t.Fatalf("Open while another connection briefly held the write lock: %v", r.err)
	}
	defer r.st.Close()
	var mode string
	if err := r.st.r.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v; want wal", mode, err)
	}
	if records := logRecords(t, &logs); len(records) != 0 {
		t.Errorf("logs = %v, want none (the schema was already current)", records)
	}
}

// 先到者一直不提交：后到的 Open 等满 busy_timeout 后失败，错误点名另一个进程正在建库或迁移，同时保留驱动的
// SQLITE_BUSY 以便调用方按类别判断；库原样不动。日志只有取锁那一行：失败前它在等什么，运维由此看得出。
func TestSchemaLockHeldPastBusyTimeoutNamesConcurrentWriter(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "heron.db")
	holder := holdSchemaLock(t, path)
	if err := createSchema(t.Context(), holder); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	opened, _ := openAsync(path, &logs)
	r := <-opened
	if r.st != nil {
		r.st.Close()
	}
	if r.err == nil {
		t.Fatal("Open succeeded while another connection held the write lock")
	}
	if want := "another process is creating or migrating the same database"; !strings.Contains(r.err.Error(), want) {
		t.Errorf("Open error = %v, want it to contain %q", r.err, want)
	}
	var se *sqlite.Error
	if !errors.As(r.err, &se) || se.Code()&0xff != sqlite3.SQLITE_BUSY {
		t.Errorf("Open error = %v, want it to wrap SQLITE_BUSY", r.err)
	}
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := rawUserVersion(t, path); got != 0 {
		t.Errorf("user_version after the failed Open = %d, want 0 (nothing created)", got)
	}
	records := logRecords(t, &logs)
	if got := recordsWithMsg(records, schemaCreateLockMsg); len(got) != 1 || len(records) != 1 {
		t.Errorf("logs = %v, want exactly one %q", records, schemaCreateLockMsg)
	}
}

// 两个真实进程同时首次打开同一个库：都成功，库的结构与新建库相同，两份日志合起来恰好一行 created（新库）或恰好一行
// migrated 且库旁恰好一份新备份（v40 夹具）。迁移一例钉住备份在锁内：两个进程若在锁外各自备份，会取到同一个 seq、
// 发布时 EEXIST，或一方把另一方正在写的临时文件当遗留删掉，其中一个 Open 失败。
//
// 子进程是本测试二进制自己（TestSchemaConcurrentOpenHelper），都在读到 stdin 关闭后才 Open，起跑尽量对齐；
// 每轮的另一个进程是否真的在锁上等过取决于调度，等过的那一个记 already current。等过的轮数只记在日志里，不作断言：
// 对齐不了的一轮仍要满足同一组断言，只是没有考到并发。
func TestSchemaConcurrentProcessesCreateOrMigrateOnce(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// prepare 在 path 上放好起跑前的库；为 nil 时 path 不存在，由 Open 新建。
		prepare func(t *testing.T, path string)
		// msg 是只该由一个进程记下的那一行；backups 是结束后库旁应有的正式备份名。
		msg     string
		backups []string
	}{
		{name: "create", msg: "database schema created"},
		{name: "migrate", prepare: func(t *testing.T, path string) { schemaFixtureAt(t, 40, path) },
			msg: "database schema migrated", backups: []string{"heron.db.before-schema-40.1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			const rounds = 8
			waited := 0
			for round := range rounds {
				dir := t.TempDir()
				path := filepath.Join(dir, "heron.db")
				if c.prepare != nil {
					c.prepare(t, path)
				}
				type child struct {
					cmd    *exec.Cmd
					stdin  io.WriteCloser
					stdout bytes.Buffer
				}
				children := make([]*child, 2)
				for i := range children {
					ch := &child{cmd: exec.Command(os.Args[0], "-test.run=^TestSchemaConcurrentOpenHelper$", "-test.count=1")}
					// 子进程的 TestMain 会建包级夹具目录：放进本用例的临时目录。
					ch.cmd.Env = append(os.Environ(), "HERON_SCHEMA_CONCURRENT_DB="+path, "TMPDIR="+t.TempDir())
					ch.cmd.Stdout = &ch.stdout
					ch.cmd.Stderr = os.Stderr
					var err error
					if ch.stdin, err = ch.cmd.StdinPipe(); err != nil {
						t.Fatal(err)
					}
					if err := ch.cmd.Start(); err != nil {
						t.Fatal(err)
					}
					children[i] = ch
				}
				for _, ch := range children {
					ch.stdin.Close()
				}
				var once, current int
				for i, ch := range children {
					if err := ch.cmd.Wait(); err != nil {
						t.Fatalf("round %d child %d: %v\n%s", round, i, err, ch.stdout.String())
					}
					scanner := bufio.NewScanner(&ch.stdout)
					for scanner.Scan() {
						line := scanner.Text()
						switch {
						case strings.Contains(line, `"msg":"`+c.msg+`"`):
							once++
						case strings.Contains(line, `"msg":"`+alreadyCurrentMsg+`"`):
							current++
						}
					}
				}
				if once != 1 {
					t.Errorf("round %d: %d %q lines across both processes, want 1", round, once, c.msg)
				}
				waited += current
				if got := backupNames(t, dir); !slices.Equal(got, c.backups) {
					t.Errorf("round %d: backups = %v, want %v", round, got, c.backups)
				}
				db, err := sql.Open("sqlite", dsn(path, "&mode=ro"))
				if err != nil {
					t.Fatal(err)
				}
				if diff := schemaDifference(describe(t, db), freshSchema(t), "并发打开", "新建"); diff != "" {
					t.Errorf("round %d: schema differs from a freshly created database:\n%s", round, diff)
				}
				db.Close()
			}
			t.Logf("%d of %d rounds had one process wait for the other's lock", waited, rounds)
		})
	}
}

// TestSchemaConcurrentOpenHelper 只在 TestSchemaConcurrentProcessesCreateOrMigrateOnce 起的子进程里运行：读到 stdin
// 关闭后以 MigrateSchema 打开 HERON_SCHEMA_CONCURRENT_DB，日志按 JSON 写到 stdout，失败时以非零退出。
func TestSchemaConcurrentOpenHelper(t *testing.T) {
	path := os.Getenv("HERON_SCHEMA_CONCURRENT_DB")
	if path == "" {
		t.Skip("helper process for TestSchemaConcurrentProcessesCreateOrMigrateOnce")
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path, clock.Real(), slog.New(slog.NewJSONHandler(os.Stdout, nil)), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}
