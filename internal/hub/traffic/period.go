// Package traffic 是流量累计器：每节点一条内存条目，按内核累计计数器差分入账，
// 入账只碰内存，脏条目周期性连同基线一起落盘。
package traffic

import "time"

// 周期边界的不变式：每个自然月恰好一个边界，取该月第 day 日（夹到 [1, 当月天数]）在 tz 里的第一个时刻（boundary）。
// 重置日大于当月天数时钳到当月最后一日，不顺延进次月一日，所以每年恒为 12 个周期，之后的大月仍按原日号对齐；
// 钳位只在算边界时发生，配置里的日号不变（store 的 traffic_reset_day 存的是原值）。
// PeriodStart 与 NextResetAfter 都只经 boundary 取边界，rolled 判断"落盘起点是不是边界"也靠这一点。
// 账本、告警引擎、流量报告都把库里的日号原样传进来；夹取让三者对任何整数（含绕过准入写进库的值）得到同一边界。

// MinResetDay、MaxResetDay 是重置日的准入范围，由管理接口（api.UpdateNode，ExecuteChange 也经它）裁决，越界值点名
// 字段拒绝。上界取 31 是因为 boundary 把超出当月天数的日号钳到月末，任何日号每月都恰好有一个边界。
const (
	MinResetDay = 1
	MaxResetDay = 31
)

// PeriodStart 返回不晚于 now 的最近一个周期边界（tz 时区）。
func PeriodStart(now time.Time, day int, tz *time.Location) time.Time {
	t := now.In(tz)
	c := boundary(t.Year(), t.Month(), day, tz)
	if c.After(now) {
		c = boundary(t.Year(), t.Month()-1, day, tz)
	}
	return c
}

// NextResetAfter 返回严格晚于 start 的首个周期边界。
func NextResetAfter(start time.Time, day int, tz *time.Location) time.Time {
	t := start.In(tz)
	c := boundary(t.Year(), t.Month(), day, tz)
	if !c.After(start) {
		c = boundary(t.Year(), t.Month()+1, day, tz)
	}
	return c
}

// boundary 返回 year 年 month 月（month 可越界，按 time.Date 归一）的周期边界。
func boundary(year int, month time.Month, day int, tz *time.Location) time.Time {
	first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	return dayStart(first.Year(), first.Month(), min(max(day, 1), last), tz)
}

// dayStart 返回 tz 里这一日的第一个时刻。零点落在夏令时跳变的空档里时（如 America/Santiago 2026-09-06 00:00 直接
// 跳到 01:00），time.Date 可能按跳变前的偏移归一到前一天 23:00；这时该日从跳变时刻开始，即那个时刻所在偏移区间的
// 终点（ZoneBounds）。零点重复出现时 time.Date 取其中之一，两个都在这一日内，且同一输入恒得同一结果，边界仍稳定。
func dayStart(year int, month time.Month, day int, tz *time.Location) time.Time {
	t := time.Date(year, month, day, 0, 0, 0, 0, tz)
	if t.Day() != day {
		_, end := t.ZoneBounds()
		return end
	}
	return t
}
