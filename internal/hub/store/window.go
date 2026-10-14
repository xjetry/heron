package store

import (
	"context"
	"database/sql"
	"time"
)

type Window struct {
	ExpiresAt time.Time
	Remaining int
}

// SetRegisterWindow 只替换当前主体的窗口；owner_id=0 是面板会话共享的窗口。
func (s *Store) SetRegisterWindow(ctx context.Context, keyHash []byte, expiresAt time.Time, maxNodes int) error {
	return s.writeChange(ctx, ChangeTarget{Action: ActionOpenRegisterWindow, ResourceID: OwnerID(ctx)}, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO register_window (owner_id, key_hash, expires_at, remaining) VALUES (?, ?, ?, ?)
			ON CONFLICT (owner_id) DO UPDATE SET key_hash = excluded.key_hash, expires_at = excluded.expires_at, remaining = excluded.remaining`,
			OwnerID(ctx), keyHash, expiresAt.Unix(), maxNodes)
		return err
	})
}

func (s *Store) ClearRegisterWindow(ctx context.Context) error {
	return s.writeChange(ctx, ChangeTarget{Action: ActionCloseRegisterWindow, ResourceID: OwnerID(ctx)}, func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM register_window WHERE owner_id = ?", OwnerID(ctx))
		return err
	})
}

// RegisterWindow 返回窗口原样，不判过期：判定属于 RegisterNode 的事务，
// 展示侧只需要把截止时间给人看。
func (s *Store) RegisterWindow(ctx context.Context) (Window, bool, error) {
	var w Window
	var exp int64
	err := s.r.QueryRowContext(ctx, "SELECT expires_at, remaining FROM register_window WHERE owner_id = ?", OwnerID(ctx)).Scan(&exp, &w.Remaining)
	if err == sql.ErrNoRows {
		return Window{}, false, nil
	}
	if err != nil {
		return Window{}, false, err
	}
	w.ExpiresAt = time.Unix(exp, 0).UTC()
	return w, true, nil
}

// RegisterNode 在一个事务里判定窗口、比对 key、建节点、消耗名额。
//
// ErrNoWindow 覆盖"没有窗口 / 已过期 / 名额用尽"，ErrBadKey 只表示窗口开着
// 但 key 不对：调用方对外把两者映射成同一响应，但只对后者计失败次数——
// 窗口关闭时没有可猜的秘密，计数只会误伤与他人共用出口地址的运维者。
//
// 建节点与 CreateNode 走同一个 insertNode：同样检查继承的任务上限、推进任务版本，返回新节点的探测清单。
func (s *Store) RegisterNode(ctx context.Context, keyHash []byte, name string, tokenHash []byte) (int64, NewNodeTasks, error) {
	var id int64
	var tasks NewNodeTasks
	err := s.write(ctx, func(tx *sql.Tx) error {
		var owner int64
		var exp int64
		var remaining int
		err := tx.QueryRow("SELECT owner_id, expires_at, remaining FROM register_window WHERE key_hash = ?", keyHash).Scan(&owner, &exp, &remaining)
		if err == sql.ErrNoRows {
			var active bool
			if err := tx.QueryRow("SELECT EXISTS (SELECT 1 FROM register_window WHERE expires_at > ? AND remaining > 0)", s.clk.Now().Unix()).Scan(&active); err != nil {
				return err
			}
			if active {
				return ErrBadKey
			}
			return ErrNoWindow
		}
		if err != nil {
			return err
		}
		if s.clk.Now().Unix() >= exp || remaining <= 0 {
			return ErrNoWindow
		}
		if owner != 0 {
			t, err := scanAPIToken(tx.QueryRow(selectAPIToken+" WHERE id = ?", owner).Scan)
			if err == sql.ErrNoRows {
				return ErrNoWindow
			}
			if err != nil {
				return err
			}
			if !t.Allows(PermissionRegister) {
				return ErrNoWindow
			}
		}
		// 注册窗口建出的节点保持不公开、不带计费、不租出（租赁意图不与注册窗口联动）。
		if id, tasks, err = insertNode(tx, NewNode{Name: name}, tokenHash, s.clk.Now().Unix(), false); err != nil {
			return err
		}
		if err := grantNode(tx, owner, id); err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE register_window SET remaining = remaining - 1 WHERE owner_id = ?", owner)
		return err
	})
	return id, tasks, err
}
