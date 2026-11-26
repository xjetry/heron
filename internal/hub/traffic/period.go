// Package traffic 是流量累计器：每节点一条内存条目，按内核累计计数器差分入账，
// 入账只碰内存，脏条目周期性连同基线一起落盘。
package traffic

import (
	"sync"
	"time"

	"github.com/xjetry/heron/internal/hub/calendar"
	"github.com/xjetry/heron/internal/hub/store"
)

// Schedule 是一个节点的周期规则：重置日、重置时刻与计费时区。账本、告警引擎、流量报告与 api 都经 ScheduleOf 从库里的
// 配置（store.TrafficSchedule）得到它，周期边界只经 PeriodStart / NextResetAfter 算，几处对同一节点得到同一边界。
type Schedule struct {
	// Day 是重置日的配置原值。范围 [store.MinTrafficResetDay, store.MaxTrafficResetDay] 由 store 的写入口与快照恢复预检
	// 保证；boundary 的夹取只让各读者对任何整数得到同一边界，不是这个范围的防线。
	Day int
	// Minute 是重置时刻，Zone 里当地零点起的分钟数。范围 [0, calendar.MinutesPerDay) 同样由 store 的写入口与快照恢复
	// 预检保证；boundary 对越界值同样先夹取。
	Minute int
	// Zone 是计费时区，非 nil：节点没设计费时区时就是 hub 的 --timezone（ScheduleOf）。
	Zone *time.Location
}

// ScheduleOf 把节点的周期配置换成 Schedule，hub 是 hub 生效的 --timezone。
func ScheduleOf(c store.TrafficSchedule, hub *time.Location) Schedule {
	return Schedule{Day: c.Day, Minute: c.Minute, Zone: BillingZone(c.Timezone, hub)}
}

// zones 缓存已加载的计费时区：time.LoadLocation 每次都要读并解析 tzdata，而告警巡检、报告与每次读视图都逐节点解析。
// *time.Location 加载后不可变，按名字共享安全；名字集合以 tzdata 的区域数为界。
var zones sync.Map // string → *time.Location

// BillingZone 是计费时区名对应的时区。空串是缺省——hub 生效的 --timezone，不是放宽；非空的名字经
// store.LoadBillingZone（与写入口、快照恢复预检同一个判定）加载。库里的非空名字都由那两处保证可加载，加载失败只可能
// 来自手改的库，这时同样按 hub 时区：与重置日越界时 boundary 的夹取同一口径——几处读者对同一节点得到同一时区，但这
// 不是名字合法性的防线。
func BillingZone(name string, hub *time.Location) *time.Location {
	if name == "" {
		return hub
	}
	if loc, ok := zones.Load(name); ok {
		return loc.(*time.Location)
	}
	loc, err := store.LoadBillingZone(name)
	if err != nil {
		return hub
	}
	zones.Store(name, loc)
	return loc
}

// 周期边界的不变式：每个自然月恰好一个边界，取该月第 Day 日（夹到 [1, 当月天数]）在 Zone 里本地读数不早于 Minute 的
// 第一个时刻（calendar.Moment；读数不存在或重复时的取法见那里）。重置日大于当月天数时钳到当月最后一日，不顺延进次月
// 一日，所以每年恒为 12 个周期，之后的大月仍按原日号对齐；钳位只在算边界时发生，配置里的日号不变（store 的
// traffic_reset_day 存的是原值）。PeriodStart 与 NextResetAfter 都只经 boundary 取边界，rolled 判断"落盘起点是不是
// 边界"也靠这一点。
//
// 每月恰好一个边界还要求相邻月份的边界严格递增：同一 Schedule 下边界的本地日期逐月后移至少 28 天，而空档只把读数
// 往后挪到跳变时刻，挪动至多是空档的长度（tzdata 里最长的是 Pacific/Apia 2011 年跳过的一整日），挪不过下一个月的边界。

// PeriodStart 返回不晚于 now 的最近一个周期边界。
func PeriodStart(now time.Time, s Schedule) time.Time {
	t := now.In(s.Zone)
	c := boundary(t.Year(), t.Month(), s)
	if c.After(now) {
		c = boundary(t.Year(), t.Month()-1, s)
	}
	return c
}

// NextResetAfter 返回严格晚于 start 的首个周期边界。
func NextResetAfter(start time.Time, s Schedule) time.Time {
	t := start.In(s.Zone)
	c := boundary(t.Year(), t.Month(), s)
	if !c.After(start) {
		c = boundary(t.Year(), t.Month()+1, s)
	}
	return c
}

// boundary 返回 year 年 month 月（month 可越界，按 time.Date 归一）的周期边界。
func boundary(year int, month time.Month, s Schedule) time.Time {
	first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	return calendar.Moment(first.Year(), first.Month(), min(max(s.Day, 1), last), min(max(s.Minute, 0), calendar.MinutesPerDay-1), s.Zone)
}
