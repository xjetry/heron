package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// TrafficReportSettings 是周期流量报告的设置。Channels 是选定的渠道，登记在 NotifyLists 里（键
// TrafficReportNotifyList），删渠道时与其它选择列表一样在同一事务里摘除。取值恒合法：readSettings 对库里的每个键按
// 下面同一张取值表核对，不合即报错。
type TrafficReportSettings struct {
	Enabled, Daily, Weekly, Monthly bool
	// Hour 是 hub 时区的投递整点，[0, TrafficReportMaxHour]。
	Hour     uint32
	Channels []int64
}

// TrafficReportUpdate 是一次 UpdateSettings 里给出的 traffic_report。整组替换：给出即五项与渠道列表一起写；nil 表示
// 请求里没有这一组，不写任何 traffic_report.* 键，也不动渠道列表。
type TrafficReportUpdate struct {
	Enabled, Daily, Weekly, Monthly bool
	Hour                            uint32
	Channels                        []int64
}

// TrafficReportMaxHour 是投递时刻的上界。写侧（saveTrafficReport）与读侧（parseStoredReportHour）都按它裁决。
const TrafficReportMaxHour = 23

// 键名是库里的持久标识，改名要迁移。四个开关与总闸同一编码（flagField / parseFlag）；新增键要同时在 readSettings 的
// GLOB 与这里登记。
const (
	trafficReportEnabledKey = "traffic_report.enabled"
	trafficReportDailyKey   = "traffic_report.daily"
	trafficReportWeeklyKey  = "traffic_report.weekly"
	trafficReportMonthlyKey = "traffic_report.monthly"
	trafficReportHourKey    = "traffic_report.hour"
)

type flagRef struct {
	key string
	on  *bool
}

// flags 是四个开关的键与落点，readSettings 与 saveTrafficReport 都按它读写，开关只有这一张表。
func (r *TrafficReportSettings) flags() []flagRef {
	return []flagRef{
		{trafficReportEnabledKey, &r.Enabled},
		{trafficReportDailyKey, &r.Daily},
		{trafficReportWeeklyKey, &r.Weekly},
		{trafficReportMonthlyKey, &r.Monthly},
	}
}

// TrafficReportError 是写侧给出的这一组不合约束，api 把它映射为点名 Field 的 InvalidArgument。
type TrafficReportError struct {
	Field, Reason string
}

func (e TrafficReportError) Error() string { return e.Field + " " + e.Reason }

func parseStoredReportHour(v string) (uint32, error) {
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil || n > TrafficReportMaxHour {
		return 0, fmt.Errorf("invalid stored %s: must be an integer in [0, %d]", trafficReportHourKey, TrafficReportMaxHour)
	}
	return uint32(n), nil
}

// saveTrafficReport 在校验之后整体写入这一组。启用而一种周期都没选的组合永远不会发出报告，却会让面板显示"已启用"，
// 所以拒绝而不是存下；关闭时保留周期与时刻的选择。渠道列表经 saveChannelIDs，存在性在同一写事务里核对。
func saveTrafficReport(tx *sql.Tx, u *TrafficReportUpdate) error {
	if u.Enabled && !u.Daily && !u.Weekly && !u.Monthly {
		return TrafficReportError{Field: "settings.traffic_report", Reason: "must select at least one of daily, weekly or monthly when enabled"}
	}
	if u.Hour > TrafficReportMaxHour {
		return TrafficReportError{Field: "settings.traffic_report.hour", Reason: fmt.Sprintf("must be in [0, %d]; got %d", TrafficReportMaxHour, u.Hour)}
	}
	saved := TrafficReportSettings{Enabled: u.Enabled, Daily: u.Daily, Weekly: u.Weekly, Monthly: u.Monthly}
	for _, f := range saved.flags() {
		if err := putSetting(tx, f.key, *flagField(f.key, *f.on).value); err != nil {
			return err
		}
	}
	if err := putSetting(tx, trafficReportHourKey, strconv.FormatUint(uint64(u.Hour), 10)); err != nil {
		return err
	}
	return saveChannelIDs(tx, TrafficReportNotifyList, u.Channels)
}

// ReportCadence 是流量报告的一种周期。取值是它在 maintenance_state 里的 name（health.go 的 Maintenance 系列），是库里
// 的持久标识，改名要迁移。
type ReportCadence string

const (
	ReportDaily   ReportCadence = MaintenanceTrafficReportDaily
	ReportWeekly  ReportCadence = MaintenanceTrafficReportWeekly
	ReportMonthly ReportCadence = MaintenanceTrafficReportMonthly
)

// ReportCadences 是全部周期，按周期由短到长。"对每种周期都要做"的事都遍历它。
var ReportCadences = []ReportCadence{ReportDaily, ReportWeekly, ReportMonthly}

// ReportPeriod 是一期报告：周期与这一期的周期键。Key 是这一期在 hub 时区里的日历日（日报是当天，周报是周一，月报是
// 1 日），表示为该日 UTC 零点的 time.Time（与 alert.Today 的日期值同一表示），与投递时刻、夏令时、时区偏移都无关：
// 改投递时刻不会让同一期再发一次，键的先后就是日期的先后。
type ReportPeriod struct {
	Cadence ReportCadence
	Key     time.Time
}

// ErrTrafficReportSent 表示给出的某一期不晚于库里已记下的那一期：这一期（或更晚的一期）已经发过，什么都没写。
var ErrTrafficReportSent = errors.New("traffic report period already sent")

// TrafficReportSent 读出每种周期最近一次已发的周期键；没有行的周期不在映射里（从未发过，或标记随只恢复配置层而丢失）。
func (s *Store) TrafficReportSent(ctx context.Context) (map[ReportCadence]time.Time, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT name, finished_at FROM maintenance_state WHERE name IN (?, ?, ?)", ReportDaily, ReportWeekly, ReportMonthly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[ReportCadence]time.Time{}
	for rows.Next() {
		var name string
		var at int64
		if err := rows.Scan(&name, &at); err != nil {
			return nil, err
		}
		out[ReportCadence(name)] = time.Unix(at, 0).UTC()
	}
	return out, rows.Err()
}

// RecordTrafficReportEvent 记一条流量报告事件（系统事件，rule_id、node_id 为 0，种类见 SystemEventKind），给设置里
// 选定的每个渠道各建一条投递，并把 periods 里每种周期的周期键记进 maintenance_state——三者在同一个写事务里提交：
// 事件落库了标记就一定在，hub 重启后读回标记、不重发；标记写不进去事件也不落库，下一轮再发。
//
// 写之前在同一事务里核对每一期都晚于库里已记的那一期，否则返回 ErrTrafficReportSent、什么都不写。"同一期至多一条
// 事件"由这条检查承载，不依赖调用方只有一个：两个调用方读到同一份旧标记、各自拼好同一期的报告，后提交的那个在这里
// 被拒。墙钟回拨让调用方算出更早的一期时同样被拒，不会补发已经过去的一期。
//
// 渠道列表与登录通知的列表一样在这个事务里读，与改设置、删渠道由写协程串行（交错的结果见 RecordLoginEvent）。
func (s *Store) RecordTrafficReportEvent(ctx context.Context, periods []ReportPeriod, summary string) (AlertEvent, error) {
	if len(periods) == 0 {
		return AlertEvent{}, fmt.Errorf("traffic report must cover at least one period")
	}
	ev := AlertEvent{Transition: TransitionTrafficReport, Summary: summary, At: time.Unix(s.clk.Now().Unix(), 0).UTC()}
	err := s.write(ctx, func(tx *sql.Tx) error {
		for _, p := range periods {
			if !isReportCadence(p.Cadence) {
				return fmt.Errorf("unknown traffic report cadence %q", p.Cadence)
			}
			var stored int64
			err := tx.QueryRowContext(ctx, "SELECT finished_at FROM maintenance_state WHERE name = ?", p.Cadence).Scan(&stored)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return err
			case stored >= p.Key.Unix():
				return ErrTrafficReportSent
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO maintenance_state (name, finished_at) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET finished_at = excluded.finished_at", p.Cadence, p.Key.Unix()); err != nil {
				return err
			}
		}
		channels, err := storedChannelIDs(tx, TrafficReportNotifyList)
		if err != nil {
			return err
		}
		return recordAlertEvent(tx, &ev, systemTargets(channels))
	})
	if err != nil {
		return AlertEvent{}, err
	}
	return ev, nil
}

func isReportCadence(c ReportCadence) bool { return slices.Contains(ReportCadences, c) }
