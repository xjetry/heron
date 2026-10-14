package alert

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
)

// ReportCheckEvery 是流量报告调度的检查间隔：投递时刻是整点，一期报告在它到来之后至多这么久发出；设置的改动（开关、
// 周期、时刻）也在这么久之内生效。
const ReportCheckEvery = time.Minute

// ReportStorage 是流量报告对 store 的全部读写，*store.Store 满足它。
type ReportStorage interface {
	Settings(ctx context.Context) (store.Settings, error)
	TrafficReportSent(ctx context.Context) (map[store.ReportCadence]time.Time, error)
	RecordTrafficReportEvent(ctx context.Context, periods []store.ReportPeriod, summary string) (store.AlertEvent, error)
	ListMonitoringNodes(ctx context.Context) ([]store.Node, error)
}

// ReportDeps 是 Reporter 的协作者，全部必需：NewReporter 逐字段核对非 nil。
type ReportDeps struct {
	Store ReportStorage
	// Traffic 是流量账本，报告只读它的已提交观测（Committed）。
	Traffic *traffic.Book
	// Sender 投递报告事件；生产装配传告警的投递队列，报告与告警共用队列与重试。
	Sender Sender
	// Location 是 hub 的 --timezone：周期边界、投递时刻与距重置的天数都按它的日历算。
	Location *time.Location
	Clock    clock.Clock
	Log      *slog.Logger
}

// Reporter 是周期流量报告的调度：按 hub 时区的日历，在每种启用的周期到期时把全部节点的已提交周期用量汇成一条
// 消息，记为系统事件并交给投递队列。它放在 alert 包而不是 store 的维护循环里：投递要经告警的队列，store 见不到它。
type Reporter struct {
	st     ReportStorage
	book   *traffic.Book
	sender Sender
	loc    *time.Location
	clk    clock.Clock
	log    *slog.Logger
}

// NewReporter 对依赖缺失 panic，口径与理由见 api.New。
func NewReporter(deps ReportDeps) *Reporter {
	switch {
	case deps.Store == nil:
		panic("alert.ReportDeps.Store must be set")
	case deps.Traffic == nil:
		panic("alert.ReportDeps.Traffic must be set")
	case deps.Sender == nil:
		panic("alert.ReportDeps.Sender must be set")
	case deps.Location == nil:
		panic("alert.ReportDeps.Location must be set")
	case deps.Clock == nil:
		panic("alert.ReportDeps.Clock must be set")
	case deps.Log == nil:
		panic("alert.ReportDeps.Log must be set")
	}
	return &Reporter{st: deps.Store, book: deps.Traffic, sender: deps.Sender, loc: deps.Location, clk: deps.Clock, log: deps.Log}
}

// Run 启动时先检查一次（hub 停机跨过的投递时刻由这一次补上），此后每 ReportCheckEvery 检查一次。检查出错只记日志：
// 没发成的那一期没有记下标记，下一次检查照样到期。
func (r *Reporter) Run(ctx context.Context) {
	ticker := time.NewTicker(ReportCheckEvery)
	defer ticker.Stop()
	for {
		if err := r.Check(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("traffic report check failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Check 做一次检查：每种启用的周期取投递时刻已过的最近一期，晚于库里已记的那一期就到期；到期的各种周期合成一条报告
// （正文是当前的已提交用量，不随周期变化，同一时刻分发几条只有标题里的周期名不同）。
//
// "重启不重发"与"停机跨过投递时刻只补发最近一期"都由同一条比较承载：标记是最近一次已发的那一期，hub 重启后从库里
// 读回；停机跨过几期时，这里只看得到投递时刻已过的最近一期，更早的几期不会逐期补发。标记没有（从未发过，或只恢复了
// 配置层、指标层的标记丢了）时最近一期即到期，所以首次启用或标记丢失后会立即发一次。
func (r *Reporter) Check(ctx context.Context) error {
	st, err := r.st.Settings(ctx)
	if err != nil {
		return err
	}
	cfg := st.TrafficReport
	if !cfg.Enabled {
		return nil
	}
	now := r.clk.Now()
	sent, err := r.st.TrafficReportSent(ctx)
	if err != nil {
		return err
	}
	var due []store.ReportPeriod
	for _, c := range ReportCadencesOf(cfg) {
		p := LatestReportPeriod(c, now, int(cfg.Hour), r.loc)
		if last, ok := sent[c]; ok && !last.Before(p.Key) {
			continue
		}
		due = append(due, p)
	}
	if len(due) == 0 {
		return nil
	}
	nodes, err := r.st.ListMonitoringNodes(ctx)
	if err != nil {
		return err
	}
	ev, err := r.st.RecordTrafficReportEvent(ctx, due, TrafficReportText(due, ReportRows(nodes, r.book.Committed(), now, r.loc), now, r.loc))
	if errors.Is(err, store.ErrTrafficReportSent) {
		// 读标记与写事件之间别处已记下这一期（或墙钟回拨后算出了更早的一期）：已发过，不是故障。
		return nil
	}
	if err != nil {
		return err
	}
	r.log.Info("traffic report recorded", "event_id", ev.ID, "periods", len(due), "nodes", len(nodes), "deliveries", len(ev.Deliveries))
	r.sender.Enqueue(ev)
	return nil
}

// ReportCadencesOf 是设置里选中的周期，按 store.ReportCadences 的顺序。
func ReportCadencesOf(cfg store.TrafficReportSettings) []store.ReportCadence {
	on := map[store.ReportCadence]bool{store.ReportDaily: cfg.Daily, store.ReportWeekly: cfg.Weekly, store.ReportMonthly: cfg.Monthly}
	var out []store.ReportCadence
	for _, c := range store.ReportCadences {
		if on[c] {
			out = append(out, c)
		}
	}
	return out
}

// periodDay 是 today 所在这一期的投递日（日报是当天、周报是本周一、月报是本月 1 日）；日期值的表示见 Today。
func periodDay(c store.ReportCadence, today time.Time) time.Time {
	switch c {
	case store.ReportWeekly:
		return today.AddDate(0, 0, -(int(today.Weekday())+6)%7)
	case store.ReportMonthly:
		return time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return today
}

// previousPeriodDay 是投递日 day 的上一期的投递日。day 由 periodDay 给出：月报的 day 是 1 日，减一个月不会钳日。
func previousPeriodDay(c store.ReportCadence, day time.Time) time.Time {
	switch c {
	case store.ReportWeekly:
		return day.AddDate(0, 0, -7)
	case store.ReportMonthly:
		return day.AddDate(0, -1, 0)
	}
	return day.AddDate(0, 0, -1)
}

// LatestReportPeriod 是周期 c 里投递时刻不晚于 now 的最近一期：本期的投递时刻已过就是本期，否则是上一期。
func LatestReportPeriod(c store.ReportCadence, now time.Time, hour int, loc *time.Location) store.ReportPeriod {
	day := periodDay(c, Today(now, loc))
	if now.Before(ReportSendMoment(day, hour, loc)) {
		day = previousPeriodDay(c, day)
	}
	return store.ReportPeriod{Cadence: c, Key: day}
}

// ReportSendMoment 是投递日 day（日期值，见 Today）在 loc 里本地时间不小于 hour:00 的第一个存在时刻，即到期扫描
// "零点不存在取新一天第一个时刻"（nextDayStart）的推广。go1.27.1 的 time.Date 对不存在与重复的墙钟读数都不保证给哪一侧：
//   - 拨快的间隙（America/New_York 3 月的 02:00）：它可能往回给旧偏移下更早的读数（01:00 EST），也可能往前给新偏移下
//     更晚的读数（Australia/Lord_Howe 的 02:30、Asia/Beirut 的 01:00）。往回时第一个存在时刻是这一时段的结束处，往前时
//     是这一时段的开始处，两者都是拨快的那一刻。间隙跨过午夜（America/Santiago 9 月的 00:00 往回到前一天 23:00）同样
//     取结束处。
//   - 拨慢的重复（Europe/London 10 月的 01:00 给第二次的 GMT，America/New_York 11 月的 01:00 给第一次的 EDT）：取第一次。
//
// 一期报告由投递日（周期键）标识而不是由这个时刻标识，所以这里给出的时刻只决定"何时算到期"，不会让一期发两次。
func ReportSendMoment(day time.Time, hour int, loc *time.Location) time.Time {
	y, m, d := day.Date()
	t := time.Date(y, m, d, hour, 0, 0, 0, loc)
	ty, tm, td := t.Date()
	if ty == y && tm == m && td == d && t.Hour() == hour && t.Minute() == 0 {
		return firstOccurrence(t)
	}
	start, end := t.ZoneBounds()
	if wallClock(t).Before(time.Date(y, m, d, hour, 0, 0, 0, time.UTC)) {
		return end
	}
	return start
}

// wallClock 是 t 在它自己的时区里的墙钟读数，记成 UTC 里同样读数的时刻，只用于与另一个墙钟读数比先后。
func wallClock(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// firstOccurrence 是与 t 墙钟读数相同的最早时刻：t 落在拨慢后的重复时段里时，前一时段（偏移更大）里同一读数更早。
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

// ReportRow 是报告里的一个节点。
type ReportRow struct {
	Name string
	// UsedBytes 按节点的配额口径（traffic.Quota）计；QuotaBytes 为 0 表示未设配额，此时 Percent 无意义。
	UsedBytes, QuotaBytes uint64
	Mode                  string
	Percent               float64
	// DaysLeft 是 hub 时区的今天到下一个重置日的天数。
	DaysLeft int
}

// ReportRows 由已提交观测算出每个节点这一行，口径与面板相同：用量与百分比经 traffic.Quota，周期边界经
// traffic.PeriodStart / NextResetAfter。已提交的观测不重算；它的周期已经结束（账本还没刷出这次滚动，或这台节点自上个
// 周期起再没有入账）时按账本读侧（Book.Get 的滚动）同样的规则看作新周期、用量为零，而不是把上个周期的整期用量报成本期的。
// 没有已提交观测的节点同样是新周期的零用量（与 Book.View 一致）。
func ReportRows(nodes []store.Node, committed map[int64]traffic.State, now time.Time, loc *time.Location) []ReportRow {
	today := Today(now, loc)
	rows := make([]ReportRow, 0, len(nodes))
	for _, n := range nodes {
		s, ok := committed[n.ID]
		if !ok || !now.Before(traffic.NextResetAfter(s.PeriodStart, n.TrafficResetDay, loc)) {
			s = traffic.State{PeriodStart: traffic.PeriodStart(now, n.TrafficResetDay, loc)}
		}
		used, pct, _ := traffic.Quota(traffic.Entry{State: s}, n.TrafficQuotaBytes, n.TrafficQuotaMode)
		next := traffic.NextResetAfter(s.PeriodStart, n.TrafficResetDay, loc)
		rows = append(rows, ReportRow{Name: n.Name, UsedBytes: used, QuotaBytes: n.TrafficQuotaBytes, Mode: n.TrafficQuotaMode, Percent: pct,
			DaysLeft: daysBetween(today, Today(next, loc))})
	}
	return rows
}

// reportCadenceLabels 是各周期在报告标题里的名字。
var reportCadenceLabels = map[store.ReportCadence]string{store.ReportDaily: "日报", store.ReportWeekly: "周报", store.ReportMonthly: "月报"}

// TrafficReportText 是一条报告的全文，也是事件的摘要：Telegram 发它，Webhook 的 summary 字段是它。首行写周期、hub 时区的
// 日期与节点总数；其后每行一个节点，按配额占比降序（未设配额的排在设了配额的之后）、再按用量降序、再按名字，至多
// maxListedNodes 行（与合并告警消息同一上限），其余写"另外 N 台"。
func TrafficReportText(periods []store.ReportPeriod, rows []ReportRow, now time.Time, loc *time.Location) string {
	labels := make([]string, len(periods))
	for i, p := range periods {
		labels[i] = reportCadenceLabels[p.Cadence]
	}
	sorted := slices.Clone(rows)
	slices.SortStableFunc(sorted, func(a, b ReportRow) int {
		return cmp.Or(
			-cmp.Compare(reportSortPercent(a), reportSortPercent(b)),
			-cmp.Compare(a.UsedBytes, b.UsedBytes),
			cmp.Compare(a.Name, b.Name),
		)
	})
	lines := []string{fmt.Sprintf("流量报告（%s）%s %s，共 %d 台节点", strings.Join(labels, "、"), now.In(loc).Format(time.DateOnly), loc.String(), len(rows))}
	listed := sorted[:min(len(sorted), maxListedNodes)]
	for _, r := range listed {
		lines = append(lines, reportLine(r))
	}
	if rest := len(sorted) - len(listed); rest > 0 {
		lines = append(lines, fmt.Sprintf("另外 %d 台", rest))
	}
	return strings.Join(lines, "\n")
}

// reportSortPercent 让未设配额的节点排在全部设了配额的节点之后（含占比为 0 的）。
func reportSortPercent(r ReportRow) float64 {
	if r.QuotaBytes == 0 {
		return -1
	}
	return r.Percent
}

func reportLine(r ReportRow) string {
	reset := fmt.Sprintf("%d 天后重置", r.DaysLeft)
	if r.QuotaBytes == 0 {
		return fmt.Sprintf("%s：%s，未设配额，%s", r.Name, quotaBytesText(r.UsedBytes), reset)
	}
	return fmt.Sprintf("%s：%s / %s（%.2f%%，%s），%s", r.Name, quotaBytesText(r.UsedBytes), quotaBytesText(r.QuotaBytes), r.Percent, quotaModeText[r.Mode], reset)
}
