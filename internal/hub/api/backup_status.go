package api

import (
	"context"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/backup"
)

func (s *Service) GetBackupStatus(ctx context.Context, _ *connect.Request[heronv1.GetBackupStatusRequest]) (*connect.Response[heronv1.GetBackupStatusResponse], error) {
	status, err := s.cfg.Backups.Status(ctx)
	if err != nil {
		s.log.Error("backup status failed", "err", err)
		return nil, internalError("backup status failed")
	}
	return connect.NewResponse(&heronv1.GetBackupStatusResponse{
		Enabled: status.Enabled, Config: backupLayerProto(status.Config), Metrics: backupLayerProto(status.Metrics),
		ThemesWithoutPackage: status.ThemesWithoutPackage,
	}), nil
}

func backupLayerProto(s backup.LayerStatus) *heronv1.BackupLayerStatus {
	out := &heronv1.BackupLayerStatus{}
	if !s.LastSuccess.IsZero() {
		at := s.LastSuccess.Unix()
		out.LastSuccessAt = &at
	}
	if s.Failure != "" {
		out.Failure = &heronv1.BackupFailure{Category: s.Failure, SinceAt: s.Since.Unix(), StatusCode: int32(s.StatusCode)}
	}
	return out
}
