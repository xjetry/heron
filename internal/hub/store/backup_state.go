package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

const backupFailingSinceKey = "backup.config_failing_since"

func (s *Store) RecordBackupSuccess(ctx context.Context, layer string) error {
	name := MaintenanceBackupConfig
	if layer == "metrics" {
		name = MaintenanceBackupMetrics
	} else if layer != "config" {
		return fmt.Errorf("unknown backup layer %q", layer)
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		at := s.clk.Now()
		if _, err := tx.ExecContext(ctx, "INSERT INTO maintenance_state (name, finished_at) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET finished_at = excluded.finished_at", name, at.Unix()); err != nil {
			return err
		}
		ev := AlertEvent{Transition: TransitionBackupSuccess, At: at, Summary: "备份成功（" + layer + "）"}
		return recordAlertEvent(tx, &ev, nil)
	})
}

func (s *Store) BackupSuccessTimes(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT name, finished_at FROM maintenance_state WHERE name IN (?, ?)", MaintenanceBackupConfig, MaintenanceBackupMetrics)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var name string
		var at int64
		if err := rows.Scan(&name, &at); err != nil {
			return nil, err
		}
		out[name] = time.Unix(at, 0).UTC()
	}
	return out, rows.Err()
}

func (s *Store) BackupFailingSince(ctx context.Context) (time.Time, error) {
	var value string
	err := s.r.QueryRowContext(ctx, "SELECT value FROM setting WHERE key = ?", backupFailingSinceKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	at, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid stored %s: %w", backupFailingSinceKey, err)
	}
	return time.Unix(at, 0).UTC(), nil
}

// 失败标记与事件同属配置层，由此事务一起提交或回滚；事件历史被清理也不丢失未恢复状态。
// 触发写入标记，恢复与停用都结束这段故障、删除标记，两者只以事件的 transition 区分。
// 渠道在同一写事务读取，删除渠道也经写队列更新设置，不落下悬空投递。
// 不读取备份数值：设置损坏本身也须能记录故障。事件是系统事件（rule_id、node_id 为 0，种类见 SystemEventKind），
// 不写告警规则的 alert_state。
func (s *Store) RecordBackupEvent(ctx context.Context, transition Transition, summary string, since time.Time) (AlertEvent, error) {
	ev := AlertEvent{Transition: transition, Summary: summary, At: time.Unix(s.clk.Now().Unix(), 0).UTC()}
	err := s.write(ctx, func(tx *sql.Tx) error {
		channels, err := storedChannelIDs(tx, BackupNotifyList)
		if err != nil {
			return err
		}
		switch transition {
		case TransitionBackupFailed:
			if since.IsZero() {
				return fmt.Errorf("backup failure time must be set")
			}
			// 一个库只由一个 backup.Manager 写（serve 装配一个），它先读回标记再判定通知状态，读到有效标记即视为已通知、
			// 不再触发；所以走到这里时库里要么没有这一行，要么是读不懂的坏值。覆盖写让提交后的标记恰为本段故障的
			// 首次失败时刻，坏值随之修复：下一轮读回成功、配置层照常执行，重启后也读得回而不再触发。
			// 保留坏值则配置层每轮卡在读回、始终不执行，每次重启还会再触发一次。
			_, err = tx.ExecContext(ctx, "INSERT INTO setting (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value", backupFailingSinceKey, strconv.FormatInt(since.Unix(), 10))
		case TransitionBackupRecovered, TransitionBackupDisabled:
			_, err = tx.ExecContext(ctx, "DELETE FROM setting WHERE key = ?", backupFailingSinceKey)
		default:
			return fmt.Errorf("invalid backup transition %q", transition)
		}
		if err != nil {
			return err
		}
		return recordAlertEvent(tx, &ev, systemTargets(channels))
	})
	if err != nil {
		return AlertEvent{}, err
	}
	return ev, nil
}
