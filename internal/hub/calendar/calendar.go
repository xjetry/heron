// Package calendar 把"某个时区里某一日的某个墙钟读数"换成时刻。流量周期边界（traffic）、流量报告的投递时刻与
// 到期扫描的日界（alert）都是这一个问题，三处只经 Moment 求解，夏令时空档与重复时段的规则因此只有一份。
package calendar

import "time"

// Moment 是 loc 里 year-month-day（按 time.Date 归一，月与日可以越界）这一日本地读数不早于 minute（当地零点起的
// 分钟数）的第一个时刻：
//   - 读数存在且只出现一次：就是它。
//   - 读数出现两次（拨慢后的重复时段，如 Europe/London 10 月的 01:00–02:00）：取第一次。日界（minute 为 0）落在
//     重复时段里时，第一次出现的零点之前本地日期还是前一天，所以这一日从第一次开始。
//   - 读数不存在（拨快的空档，如 America/New_York 3 月的 02:00–03:00）：取空档结束的那一刻，即本地读数第一次越过
//     目标的时刻。空档跨过午夜时（America/Santiago 9 月 00:00 直接跳到 01:00）同样如此，这一日从跳变时刻开始。
//
// go1.27.4 的 time.Date 对这两种读数都不保证给哪一侧（它先把墙钟读数当作 UTC 去查偏移，再按换算结果是否越出该时段
// 复核）：空档往回给旧偏移下更早的读数（New York、Santiago）或往前给新偏移下更晚的读数（Australia/Lord_Howe、
// Asia/Beirut），重复时段给第一次（New York 11 月）或第二次（London）。这里按 time.Date 给出的那一侧所在偏移区间的
// 端点（ZoneBounds）改正到上面的规则，两种归一方向得到同一个答案；同一输入恒得同一时刻。
func Moment(year int, month time.Month, day, minute int, loc *time.Location) time.Time {
	d := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	target := d.Add(time.Duration(minute) * time.Minute)
	t := time.Date(d.Year(), d.Month(), d.Day(), minute/60, minute%60, 0, 0, loc)
	if wall := wallClock(t); wall.Equal(target) {
		return firstOccurrence(t)
	} else if wall.Before(target) {
		// time.Date 落在空档之前的区间里：空档从这个区间的终点开始，那一刻的读数已越过目标。
		_, end := t.ZoneBounds()
		return end
	}
	// 落在空档之后的区间里：空档在这个区间的起点结束。
	start, _ := t.ZoneBounds()
	return start
}

// wallClock 是 t 在它自己的时区里的墙钟读数，记成 UTC 里同样读数的时刻，只用于与另一个墙钟读数比先后。
func wallClock(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// firstOccurrence 是与 t 墙钟读数相同的最早时刻：t 落在拨慢后的重复时段里时，前一区间（偏移更大）里同一读数更早。
func firstOccurrence(t time.Time) time.Time {
	start, _ := t.ZoneBounds()
	if start.IsZero() {
		return t
	}
	prev := zoneOffset(start.Add(-time.Second))
	if prev <= zoneOffset(t) {
		return t
	}
	earlier := t.Add(-(prev - zoneOffset(t)))
	if earlier.Before(start) && wallClock(earlier).Equal(wallClock(t)) {
		return earlier
	}
	return t
}

func zoneOffset(t time.Time) time.Duration {
	_, off := t.Zone()
	return time.Duration(off) * time.Second
}

// MinutesPerDay 是一日的分钟数；当地零点起的分钟数取 [0, MinutesPerDay)。
const MinutesPerDay = 24 * 60
