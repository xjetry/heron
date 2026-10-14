package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
)

type tracedStatement struct {
	query string
	args  []driver.NamedValue
}

type writeTrace struct {
	mu      sync.Mutex
	commits [][]tracedStatement
}

func (t *writeTrace) snapshot() [][]tracedStatement {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([][]tracedStatement(nil), t.commits...)
}

type traceDriver struct{ trace *writeTrace }
type traceConn struct {
	driver.Conn
	trace      *writeTrace
	statements []tracedStatement
}
type traceTx struct {
	driver.Tx
	conn *traceConn
}

func (d traceDriver) Open(name string) (driver.Conn, error) {
	c, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return &traceConn{Conn: c, trace: d.trace}, nil
}

// 包装 BeginTx 而不是已废弃的 Begin：database/sql 对实现了 ConnBeginTx 的连接只调 BeginTx，包装它才与被包装的驱动走同一条入口。
func (c *traceConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.statements = nil
	return &traceTx{Tx: tx, conn: c}, nil
}
func (c *traceConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.statements = append(c.statements, tracedStatement{q, append([]driver.NamedValue(nil), args...)})
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}
func (c *traceConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}
func (tx *traceTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	tx.conn.trace.mu.Lock()
	tx.conn.trace.commits = append(tx.conn.trace.commits, tx.conn.statements)
	tx.conn.trace.mu.Unlock()
	return nil
}

var traceID atomic.Uint64

// 包装真实驱动，只观察已提交事务及绑定参数，不替换 SQL 的执行与事务语义。
func traceWrites(t *testing.T, s *Store) *writeTrace {
	t.Helper()
	var seq int
	var name, path string
	if err := s.r.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := s.w.Close(); err != nil {
		t.Fatal(err)
	}
	trace := &writeTrace{}
	driverName := fmt.Sprintf("traced-sqlite-%d", traceID.Add(1))
	sql.Register(driverName, traceDriver{trace})
	var err error
	s.w, err = sql.Open(driverName, dsn(path, ""))
	if err != nil {
		t.Fatal(err)
	}
	s.w.SetMaxOpenConns(1)
	return trace
}
