package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// RestoreRecord 是 restore_record 一行的读出形：Restore 落库的审计内容加上创建时随机生成的主键。
// 这张表的写者只有离线恢复，读侧没有别的；字段是恢复那一刻的事实快照，不随之后的变化改写。
type RestoreRecord struct {
	ID string
	RestoreResult
}

// ListRestoreRecords 按恢复时刻倒序、同刻按 id 升序返回至多 limit 条恢复记录。orphans 与 themes 两列
// 由 Restore 以 json.Marshal 写入；解析失败返回错误而不是跳过该行——坏 JSON 只能来自库损坏，静默跳过
// 会把一次恢复从审计历史里抹掉。limit 由调用方（api 层）约束在 1..100，这里不重复裁决。
func (s *Store) ListRestoreRecords(ctx context.Context, limit int) ([]RestoreRecord, error) {
	rows, err := s.r.QueryContext(ctx,
		"SELECT id,restored_at,config_taken_at,metrics_taken_at,orphans,themes FROM restore_record ORDER BY restored_at DESC, id LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RestoreRecord
	for rows.Next() {
		var r RestoreRecord
		var metrics sql.NullInt64
		var orphans, themes string
		if err := rows.Scan(&r.ID, &r.RestoreResult.RestoredAt, &r.RestoreResult.ConfigTakenAt, &metrics, &orphans, &themes); err != nil {
			return nil, err
		}
		if metrics.Valid {
			at := metrics.Int64
			r.RestoreResult.MetricsTakenAt = &at
		}
		if err := json.Unmarshal([]byte(orphans), &r.RestoreResult.Orphans); err != nil {
			return nil, fmt.Errorf("restore record %s orphans: %w", r.ID, err)
		}
		if err := json.Unmarshal([]byte(themes), &r.RestoreResult.Themes); err != nil {
			return nil, fmt.Errorf("restore record %s themes: %w", r.ID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
