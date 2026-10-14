package api

import (
	"context"
	"maps"
	"slices"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

// 与 proto 注释同一口径：0 取默认页长，超过上限拒绝而不是截断——恢复记录没有翻页游标，
// 截断会让调用方误以为已经拿全。
const (
	restoreRecordsDefaultLimit = 20
	restoreRecordsMaxLimit     = 100
)

// ListRestoreRecords 读离线恢复写下的审计记录。恢复只在停机时发生，这里只读 store，不触碰备份循环。
func (s *Service) ListRestoreRecords(ctx context.Context, req *connect.Request[heronv1.ListRestoreRecordsRequest]) (*connect.Response[heronv1.ListRestoreRecordsResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit == 0 {
		limit = restoreRecordsDefaultLimit
	}
	if limit > restoreRecordsMaxLimit {
		return nil, invalid("limit must be between 1 and %d, got %d", restoreRecordsMaxLimit, limit)
	}
	records, err := s.store.ListRestoreRecords(ctx, limit)
	if err != nil {
		s.log.Error("restore records failed", "err", err)
		return nil, internalError("restore records failed")
	}
	out := &heronv1.ListRestoreRecordsResponse{}
	for _, r := range records {
		out.Records = append(out.Records, restoreRecordProto(r))
	}
	return connect.NewResponse(out), nil
}

func restoreRecordProto(r store.RestoreRecord) *heronv1.RestoreRecord {
	out := &heronv1.RestoreRecord{
		Id: r.ID, RestoredAt: r.RestoredAt, ConfigTakenAt: r.ConfigTakenAt,
		Themes: &heronv1.RestoreThemes{Restored: r.Themes.Restored, Missing: r.Themes.Missing, Ignored: r.Themes.Ignored},
	}
	if r.MetricsTakenAt != nil {
		out.MetricsTakenAt = r.MetricsTakenAt
	}
	// Orphans 在库里是 map，wire 上是重复字段；按表名排序，同一条记录的字段顺序稳定。
	for _, table := range slices.Sorted(maps.Keys(r.Orphans)) {
		out.Orphans = append(out.Orphans, &heronv1.OrphanRows{Table: table, Rows: r.Orphans[table]})
	}
	return out
}
