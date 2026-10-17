package alert

import (
	"fmt"
	"time"
)

// formatAlertDuration 按完整分钟或小时展示；墙钟回拨产生的负值与零都归为“不到 1 分钟”，不展示负时长。
func formatAlertDuration(d time.Duration) string {
	if d < time.Minute {
		return "不到 1 分钟"
	}
	if d >= 24*time.Hour {
		days, hours := int64(d/(24*time.Hour)), int64(d/time.Hour)%24
		if hours == 0 {
			return fmt.Sprintf("%d 天", days)
		}
		return fmt.Sprintf("%d 天 %d 小时", days, hours)
	}
	hours, minutes := int64(d/time.Hour), int64(d/time.Minute)%60
	if hours == 0 {
		return fmt.Sprintf("%d 分钟", minutes)
	}
	if minutes == 0 {
		return fmt.Sprintf("%d 小时", hours)
	}
	return fmt.Sprintf("%d 小时 %d 分钟", hours, minutes)
}
