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

// durationMoment 是告警持续时长写进哪一刻的文案：两处都从状态行的 fired_at 量到此刻，只是句式不同。
type durationMoment int

const (
	// atRecovery 是恢复：Engine.apply 在恢复事件上追加，样例的恢复文案同样追加。
	atRecovery durationMoment = iota
	// atUnsilence 是静默或维护解除时仍未恢复的补发（Engine.renotifyUnsilenced）：摘要是当初被静默的触发文案。
	atUnsilence
)

// withFiringDuration 是告警持续时长在文案上的唯一写法，所有带持续时长的摘要都经它，两种句式不各自拼接。
func withFiringDuration(summary string, d time.Duration, at durationMoment) string {
	if at == atUnsilence {
		return summary + "（静默结束时仍未恢复，已持续 " + formatAlertDuration(d) + "）"
	}
	return summary + "，告警持续 " + formatAlertDuration(d)
}
