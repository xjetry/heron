package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// 离线变更代数的推进是"库外写者"这一角色的属性，不是某个写方法的属性。不变式：带 ExternalWriter 打开的 Store
// 提交的每一个写事务都在同一事务里把代数加一，hub 自己的 Store 永不推进。由此：
//   - 库外写入与它的推进一起提交或一起回滚，hub 读到的代数增长了，就一定能读到这次写入；
//   - 不必逐个方法判断"这次写会不会影响 hub 的缓存"——窗口、API token、密码这类 hub 本就每次读库的写入也推进，
//     代价是 hub 多做一次重载；离线子命令是运维手动执行的，hub 按周期合并它们，成本有界；
//   - hub 自己的写入不推进，所以在线建删节点、换 token、消费安装凭据都不会触发重载。
//
// 推进挂在 runWriter（Store 唯一的写事务入口）上，于是没有绕开它的写路径。不经 runWriter 的写只有两处，都不会落在
// 运行中 hub 的库上：建库与迁移只在 Open 判定库不是当前版本时发生，运行中 hub 的库已是当前版本，离线入口又只用
// RequireCurrentSchema；Restore 要求 hub 已停止，并把代数重新种子为 0。

// advancingOfflineGeneration 在 fn 成功之后、同一事务里推进代数。表缺行时这个写事务失败并回滚：库外写入若不推进，
// 运行中的 hub 永远看不到它，宁可让离线命令报错。
func advancingOfflineGeneration(fn func(*sql.Tx) error) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		res, err := tx.Exec("UPDATE hub_coordination SET offline_generation = offline_generation + 1 WHERE id = 1")
		if err != nil {
			return fmt.Errorf("advance offline generation: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("advance offline generation: %w", err)
		} else if n != 1 {
			return errors.New("advance offline generation: hub_coordination row missing")
		}
		return nil
	}
}

// OfflineGeneration 读离线变更代数（表的含义见 schema.go 的 hub_coordination）。缺行或负值返回错误：调用方据代数
// 判定"自上次确认以来有没有库外写入"，把读不出来当成没有变化，会让离线删除与换发永远不被重载。
func (s *Store) OfflineGeneration(ctx context.Context) (uint64, error) {
	var g int64
	err := s.r.QueryRowContext(ctx, "SELECT offline_generation FROM hub_coordination WHERE id = 1").Scan(&g)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errors.New("hub_coordination: row missing")
	}
	if err != nil {
		return 0, err
	}
	if g < 0 {
		return 0, fmt.Errorf("hub_coordination: offline_generation %d is negative", g)
	}
	return uint64(g), nil
}
