package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// NodeUpdates 是运行簿记；恢复配置不应重放以前排队的安装授权。
func (s *Store) NodeUpdates(ctx context.Context) (map[int64]*heronv1.UpdateStatus, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT u.node_id,u.data FROM node_update u JOIN node n ON n.id=u.node_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]*heronv1.UpdateStatus)
	for rows.Next() {
		var id int64
		var data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		status := new(heronv1.UpdateStatus)
		if err := protojson.Unmarshal([]byte(data), status); err != nil {
			return nil, err
		}
		out[id] = status
	}
	return out, rows.Err()
}

// SaveNodeUpdate 落下后台对账得出的更新状态（下发、确认、超时），不是任何类型化变更的目标。
func (s *Store) SaveNodeUpdate(ctx context.Context, id int64, status *heronv1.UpdateStatus) error {
	return s.saveNodeUpdate(ctx, id, status, s.write)
}

// StartNodeUpdate 把节点排进一次更新，是开始更新的类型化变更目标。
func (s *Store) StartNodeUpdate(ctx context.Context, id int64, status *heronv1.UpdateStatus) error {
	commit := s.changeTarget(ActionStartUpdate, id)
	return s.saveNodeUpdate(ctx, id, status, func(ctx context.Context, fn func(*sql.Tx) error) error {
		return commit(ctx, func(tx *sql.Tx) error {
			if err := rejectRentedUpdate(tx, id); err != nil {
				return err
			}
			return fn(tx)
		})
	})
}

// rejectRentedUpdate 在开始更新的写事务里读目标节点的已出租开关：已出租的机器没有 agent，更新目标
// 不存在。检查放进事务而不是由调用方先读，是为了与并发改开关的 UpdateNodeTasks 串行化，守卫不靠界面不显示
// 入口。节点不存在留给写任务时的 WHERE EXISTS 裁决（ErrNotFound），这里只判已出租。
func rejectRentedUpdate(tx *sql.Tx, id int64) error {
	var name string
	var rented bool
	if err := tx.QueryRow("SELECT name, rented FROM node WHERE id=?", id).Scan(&name, &rented); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if rented {
		return fmt.Errorf("node %d (%s): %w; no agent is expected on it, online updates are unavailable", id, name, ErrNodeRented)
	}
	return nil
}

// CancelNodeUpdate 撤回一次排队中的更新，是取消更新的类型化变更目标。
func (s *Store) CancelNodeUpdate(ctx context.Context, id int64, status *heronv1.UpdateStatus) error {
	return s.saveNodeUpdate(ctx, id, status, s.changeTarget(ActionCancelUpdate, id))
}

func (s *Store) saveNodeUpdate(ctx context.Context, id int64, status *heronv1.UpdateStatus, commit func(context.Context, func(*sql.Tx) error) error) error {
	saved := proto.Clone(status).(*heronv1.UpdateStatus)
	err := commit(ctx, func(tx *sql.Tx) error {
		var owner int64
		var oldData string
		if err := tx.QueryRow("SELECT owner_id,data FROM node_update WHERE node_id=?", id).Scan(&owner, &oldData); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		old := new(heronv1.UpdateStatus)
		if oldData != "" {
			if err := protojson.Unmarshal([]byte(oldData), old); err != nil {
				return err
			}
		}
		if old.GetTask().GetId() != saved.GetTask().GetId() {
			owner = OwnerID(ctx)
		}
		// 排队授权在真正下发的事务内再检查；已下发任务不能假称可以撤回。
		if owner != 0 && old.GetTask().GetState() == "queued" && saved.GetTask().GetState() == "dispatched" {
			t, err := scanAPIToken(tx.QueryRow(selectAPIToken+" WHERE id=?", owner).Scan)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if errors.Is(err, sql.ErrNoRows) || !t.Allows(PermissionUpdate) || !t.AllowsNode(id) {
				saved.Task.State = "cancelled"
				saved.Task.Error = "API token authorization revoked before dispatch"
			}
		}
		data, err := protojson.Marshal(saved)
		if err != nil {
			return err
		}
		// 节点存在性在写事务中裁决，删除后晚到的上报不得重建簿记。
		res, err := tx.Exec("INSERT INTO node_update(node_id,data,owner_id) SELECT ?,?,? WHERE EXISTS (SELECT 1 FROM node WHERE id=?) ON CONFLICT(node_id) DO UPDATE SET data=excluded.data,owner_id=excluded.owner_id", id, string(data), owner, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n == 0 {
			return ErrNotFound
		}
		return err
	})
	if err == nil {
		proto.Reset(status)
		proto.Merge(status, saved)
	}
	return err
}
