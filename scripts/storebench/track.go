package main

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// 写者占用的计时。store 不暴露每个写事务的耗时，生产代码也不该为测量改；这里在驱动层挂钩：modernc.org/sqlite 的
// 连接钩子在每条新连接建好后调用，对写连接注册 preupdate 与 commit 钩子。
//
// 哪条是写连接：store 的三个读池与统计连接的 DSN 都带 query_only(1)，写连接不带（store.go 的 openStore），钩子按此区分。
// 写连接只有一条（SetMaxOpenConns(1)），所有写事务都在写协程里串行执行，所以同一时刻至多一个事务在这条连接上。
//
// 每个写事务记一段 txSpan：起点是事务里第一行改动的 preupdate，终点是 commit 钩子。它是写者占用的下界——不含 BEGIN、
// 第一行改动之前的读（例如 Prune 分块的主键定位）、以及 commit 钩子之后的 WAL 追加。commit 钩子的时刻序列另行保存：
// 只有 Prune 在写时，相邻两次提交之间的间隔是该块占用的上界（多出的是块间在读池上的 min(ts) 读与写请求的交接）。
// preupdate 钩子逐行触发，带来每行一次回调的开销；time 对 Prune 另跑不挂钩子的重复，用来给出不含这份开销的总耗时。
type txSpan struct {
	Start time.Time
	End   time.Time
	Table string
	Op    string
}

type writerTracker struct {
	enabled atomic.Bool
	mu      sync.Mutex
	spans   []txSpan
	commits []time.Time
}

var tracker writerTracker

func init() {
	sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, dsn string) error {
		if !tracker.enabled.Load() || strings.Contains(dsn, "query_only") {
			return nil
		}
		h, ok := conn.(sqlite.HookRegisterer)
		if !ok {
			return errors.New("modernc.org/sqlite connection does not implement HookRegisterer")
		}
		var c txState
		h.RegisterPreUpdateHook(func(d sqlite.SQLitePreUpdateData) {
			if !c.open {
				c.open, c.start, c.table, c.op = true, time.Now(), d.TableName, opName(d.Op)
			}
		})
		h.RegisterCommitHook(func() int32 {
			tracker.commit(time.Now(), &c)
			return 0
		})
		h.RegisterRollbackHook(func() { c.open = false })
		return nil
	})
}

// txState 只在持有写连接的那个协程里读写（钩子在 SQLite 调用里同步执行），不需要锁。
type txState struct {
	open  bool
	start time.Time
	table string
	op    string
}

func opName(op int32) string {
	switch op {
	case sqlite3.SQLITE_INSERT:
		return "insert"
	case sqlite3.SQLITE_DELETE:
		return "delete"
	case sqlite3.SQLITE_UPDATE:
		return "update"
	}
	return "other"
}

func (t *writerTracker) commit(at time.Time, c *txState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.commits = append(t.commits, at)
	if c.open {
		t.spans = append(t.spans, txSpan{Start: c.start, End: at, Table: c.table, Op: c.op})
	}
	c.open = false
}

// reset 清空记录并设置是否给此后新开的写连接挂钩子；已打开的连接不受影响，所以要在 store.Open 之前调用。
func (t *writerTracker) reset(enabled bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spans, t.commits = nil, nil
	t.enabled.Store(enabled)
}

func (t *writerTracker) snapshot() ([]txSpan, []time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]txSpan(nil), t.spans...), append([]time.Time(nil), t.commits...)
}
