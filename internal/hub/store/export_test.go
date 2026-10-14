package store

import (
	"context"
	"database/sql"
	"sync"
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

// HoldWriterForTest 仅阻塞本实例的队列，不获取 SQLite 写锁，允许另一实例先提交撤销。
func (s *Store) HoldWriterForTest() (release func(), pending func() int, drain func() error) {
	entered, gate := make(chan struct{}), make(chan struct{})
	s.writeAsync(func(*sql.Tx) error { close(entered); <-gate; return nil }, nil)
	<-entered
	return sync.OnceFunc(func() { close(gate) }), func() int { return len(s.writes) }, func() error {
		return s.write(context.Background(), func(*sql.Tx) error { return nil })
	}
}
