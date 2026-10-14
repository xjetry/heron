package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"
	"modernc.org/sqlite"
)

// 包装真实驱动，在版本行读完后提交保存，使后续读取能否维持快照不依赖调度时机。
type snapshotDriver struct{ afterVersion func() error }
type snapshotConn struct {
	driver.Conn
	afterVersion func() error
}
type snapshotRows struct {
	driver.Rows
	afterVersion func() error
	once         sync.Once
	err          error
}

func (d snapshotDriver) Open(name string) (driver.Conn, error) {
	c, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return &snapshotConn{Conn: c, afterVersion: d.afterVersion}, nil
}
func (c *snapshotConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *snapshotConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	if q == "SELECT version FROM probe_meta WHERE id = 1" {
		return &snapshotRows{Rows: rows, afterVersion: c.afterVersion}, nil
	}
	return rows, nil
}
func (r *snapshotRows) Close() error {
	if err := r.Rows.Close(); err != nil {
		return err
	}
	r.once.Do(func() { r.err = r.afterVersion() })
	return r.err
}

func TestLoadProbeTasksReadsOneSnapshot(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	a, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	b, _, _ := s.CreateNode(ctx, NewNode{Name: "b"}, hash(2))
	saved, savedVersion, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: false, NodeIDs: []int64{a}})
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	driverName := fmt.Sprintf("snapshot-sqlite-%d", traceID.Add(1))
	sql.Register(driverName, snapshotDriver{afterVersion: func() error {
		next := taskForTest()
		next.Id = saved.Task.Id
		next.Target = "changed"
		_, _, err := s.SaveProbeTask(ctx, next, NodeSelector{AllNodes: false, NodeIDs: []int64{b}})
		changed = err == nil
		return err
	}})
	reopenReadPools(t, s, driverName)
	version, tasks, err := s.LoadProbeTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("concurrent save did not run")
	}
	if version != savedVersion || len(tasks) != 1 || !proto.Equal(tasks[0].Task, saved.Task) || !reflect.DeepEqual(tasks[0].NodeIDs, []int64{a}) {
		t.Fatalf("mixed snapshot: version=%d tasks=%v, want %v/[%d] at saved version", version, tasks, saved, a)
	}
	var current int
	if err := s.r.QueryRow("SELECT version FROM probe_meta").Scan(&current); err != nil || uint64(current) != savedVersion+1 {
		t.Fatalf("concurrent save not persisted: version=%d err=%v", current, err)
	}
}
