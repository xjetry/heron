// Package traffic 是流量累计器：每节点一条内存条目，按内核累计计数器差分入账，
// 入账只碰内存，脏条目周期性连同基线一起落盘。
package traffic

import "time"

// PeriodStart 返回不晚于 now 的最近一个重置日零点（tz 时区）。day 限定在 1–28，每个月都有
// 这一天，所以不存在"本月没有这一天"的情形；time.Date 对月份 0 自动归一到上一年 12 月。
func PeriodStart(now time.Time, day int, tz *time.Location) time.Time {
	t := now.In(tz)
	c := time.Date(t.Year(), t.Month(), day, 0, 0, 0, 0, tz)
	if c.After(t) {
		c = time.Date(t.Year(), t.Month()-1, day, 0, 0, 0, 0, tz)
	}
	return c
}

// NextResetAfter 返回严格晚于 start 的首个重置日零点。
func NextResetAfter(start time.Time, day int, tz *time.Location) time.Time {
	t := start.In(tz)
	c := time.Date(t.Year(), t.Month(), day, 0, 0, 0, 0, tz)
	if !c.After(t) {
		c = time.Date(t.Year(), t.Month()+1, day, 0, 0, 0, 0, tz)
	}
	return c
}
