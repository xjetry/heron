package update

import (
	"errors"
	"unicode/utf8"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

func TaskProto(j *Job) *heronv1.UpdateTask {
	if j == nil {
		return nil
	}
	return &heronv1.UpdateTask{Id: j.ID, Version: j.Version, ExpiresAt: j.ExpiresAt, State: j.State, Error: bounded(j.Error, 2048), UpdatedAt: j.UpdatedAt}
}

func StatusProto(s Status, version string) *heronv1.UpdateStatus {
	return &heronv1.UpdateStatus{Supported: s.Supported, Reason: bounded(s.Reason, 2048), Version: bounded(version, 64), Task: TaskProto(s.Job), Source: s.Source}
}

func bounded(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func ActiveState(state string) bool {
	return state == "dispatched" || (Job{State: state}).Active()
}

func ValidateStatus(s *heronv1.UpdateStatus) error {
	if s == nil {
		return nil
	}
	if len(s.Reason) > 2048 || len(s.Version) > 64 || !utf8.ValidString(s.Reason) || !utf8.ValidString(s.Version) {
		return errors.New("update status exceeds string bounds")
	}
	// 空串放行：旧更新器不报这个字段。它不是放宽——来源只用于展示，不参与任何判定。
	switch s.Source {
	case "", "github", "hub":
	default:
		return errors.New("invalid update source")
	}
	if t := s.Task; t != nil {
		if !idPattern.MatchString(t.Id) || !ValidVersion(t.Version) || len(t.Error) > 2048 || !utf8.ValidString(t.Error) || t.ExpiresAt <= 0 || t.UpdatedAt < 0 {
			return errors.New("invalid update task status")
		}
		switch t.State {
		case "queued", "dispatched", "downloading", "stopping", "installing", "verifying", "rolling_back", "succeeded", "failed", "rolled_back", "cancelled", "expired":
		default:
			return errors.New("invalid update task state")
		}
	}
	return nil
}
