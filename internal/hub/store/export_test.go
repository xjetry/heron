package store

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/testwait"
)

// 任意水位只用于构造冻结边界，不能成为绕过聚合即可授权清理的生产入口。
func (s *Store) setRollupWatermark(ctx context.Context, level string, uptoTS int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec("UPDATE rollup_state SET upto_ts = ? WHERE level = ?", uptoTS, level)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// HoldWriterForTest 只阻塞本实例的写队列，不开事务、不占 SQLite 的任何锁，另一实例因此能先提交。写事务在 begin 时
// 就取写锁（writeTxLock），所以不能靠在写协程里挂住一个已开的事务来阻塞队列：那会连另一实例的写一起挡住。这里借走
// 写连接池里唯一的连接（w 的 SetMaxOpenConns(1)），写协程取出下一个请求后停在 inTx 的 Begin 上等连接；release 归还
// 连接，队列按原顺序继续。
func (s *Store) HoldWriterForTest(t testing.TB) (release func(), pending func() int, drain func() error) {
	t.Helper()
	conn, err := s.w.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waits := s.w.Stats().WaitCount
	s.writeAsync(func(*sql.Tx) error { return nil }, nil)
	// database/sql 在请求开始等连接时给 WaitCount 加一；w 只有写协程在用，加一即它已取走上面这个请求、停在 Begin 上，
	// 此后入队的写都留在队列里，pending 数的就是它们。
	testwait.Until(t, time.Millisecond, func() bool { return s.w.Stats().WaitCount > waits }, "writer did not block waiting for the borrowed connection")
	return sync.OnceFunc(func() { conn.Close() }), func() int { return len(s.writes) }, func() error {
		return s.write(context.Background(), func(*sql.Tx) error { return nil })
	}
}
