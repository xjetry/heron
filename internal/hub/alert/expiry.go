package alert

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
)

// 到期日按日历日计。日期一律表示为该日 UTC 零点的 time.Time：解析、"今天"与相减都在这种值上做，两个日期
// 相差几天与任何时区的夏令时无关。日期值里 hub 时区（--timezone）只经 Today 进入：把一个时刻换成那里的日历日；
// 下一个日界的时刻由 nextDayStart 按同一时区算（RunExpirySweep 传入 cfg.Location）。

// ParseDate 解析 YYYY-MM-DD。写侧（api 的 UpdateNode）与读侧（到期扫描、days_left）都用它，"合法日期"只有这一个
// 口径：time.Parse 按 time.DateOnly 要求四位年、两位月日，并拒绝不存在的日子（2026-02-30、非闰年的 02-29）。
func ParseDate(s string) (time.Time, error) {
	return time.Parse(time.DateOnly, s)
}

// Today 是 now 在 loc 里的日历日。
func Today(now time.Time, loc *time.Location) time.Time {
	y, m, d := now.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// daysBetween 用 Unix 秒相减：time.Time.Sub 相差约 292 年以上就饱和，ParseDate 接受的 0000–9999 年会算错。
func daysBetween(from, to time.Time) int {
	return int((to.Unix() - from.Unix()) / 86400)
}

// DaysLeft 是到期日减 today 的天数，负数是已过期天数；expiresOn 为空或不是合法日期时 ok 为假。
func DaysLeft(expiresOn string, today time.Time) (int, bool) {
	if expiresOn == "" {
		return 0, false
	}
	d, err := ParseDate(expiresOn)
	if err != nil {
		return 0, false
	}
	return daysBetween(today, d), true
}

// cycleMonths 是自动续期每次推后的月数。store.CycleNone 与表外的值（写入口会拒绝，只可能来自手改的库）不推后：
// 扫描自己守这一条，不依赖保存入口。
func cycleMonths(c store.BillingCycle) (int, bool) {
	switch c {
	case store.CycleMonthly:
		return 1, true
	case store.CycleQuarterly:
		return 3, true
	case store.CycleSemiannual:
		return 6, true
	case store.CycleYearly:
		return 12, true
	case store.CycleBiennial:
		return 24, true
	case store.CycleTriennial:
		return 36, true
	case store.CycleQuinquennial:
		return 60, true
	}
	return 0, false
}

// addMonths 把日期推后 n 个月；日号超过目标月的天数时钳到月末（1 月 31 日 + 1 个月 = 2 月 28 或 29 日）。
func addMonths(d time.Time, n int) time.Time {
	first := time.Date(d.Year(), d.Month()+time.Month(n), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	return time.Date(first.Year(), first.Month(), min(d.Day(), last), 0, 0, 0, 0, time.UTC)
}

// stepExpiry 从 d 起按 months 个月一步步推后，直到不早于 today。每一步从上一步的结果推后，钳到月末之后日号停在
// 钳后的值（1 月 31 日按月推后：2 月 28 日、3 月 28 日……），这是按月推后接受的漂移。自动续期（renewedExpiry）与
// 手动续期（RenewedExpiry）都只经这里步进，二者的推后规则因此只有一份。
func stepExpiry(d time.Time, months int, today time.Time) time.Time {
	for d.Before(today) {
		d = addMonths(d, months)
	}
	return d
}

// renewedExpiry 是自动续期推后的到期日：到期日早于 today 就按周期步进到不早于 today（stepExpiry）。
// ok 为假表示不推后：没开自动续期、没有可用的周期、没有到期日或不是合法日期、到期日不早于 today。
func renewedExpiry(b store.Billing, today time.Time) (string, bool) {
	if !b.AutoRenew {
		return "", false
	}
	months, ok := cycleMonths(b.Cycle)
	if !ok {
		return "", false
	}
	d, err := ParseDate(b.ExpiresOn)
	if err != nil || !d.Before(today) {
		return "", false
	}
	return stepExpiry(d, months, today).Format(time.DateOnly), true
}

// 手动续期（RenewedExpiry）拒绝的两种计费：推后需要周期与到期日，缺哪一项就没有可推后的对象。
var (
	ErrRenewNoCycle  = errors.New("node has no billing cycle")
	ErrRenewNoExpiry = errors.New("node has no expiry date")
)

// RenewedExpiry 是"已续费"把到期日推后之后的日期。规则与自动续期相同：先推后一个周期，仍早于 today 就继续
// 步进到不早于 today。自动续期从到期日 d 步进经过的是 addMonths(d)、addMonths(addMonths(d))……，先走的一步就是这
// 个序列的第一项，所以已过期的节点得到与自动续期同一个日期；未过期的节点第一步之后已不早于 today，恰好推后一个周期。与 renewedExpiry 不同，不要求开着自动续期：手动续期的
// 主要对象正是没开它的节点。
//
// 周期不可用（store.CycleNone，或写入口会拒绝、只可能来自手改的库的表外值）返回 ErrRenewNoCycle；没有到期日返回
// ErrRenewNoExpiry；到期日不是合法日期返回 ParseDate 的错误。
func RenewedExpiry(b store.Billing, today time.Time) (string, error) {
	months, ok := cycleMonths(b.Cycle)
	if !ok {
		return "", ErrRenewNoCycle
	}
	if b.ExpiresOn == "" {
		return "", ErrRenewNoExpiry
	}
	d, err := ParseDate(b.ExpiresOn)
	if err != nil {
		return "", err
	}
	return stepExpiry(addMonths(d, months), months, today).Format(time.DateOnly), nil
}

// nextDayStart 是 now 之后 loc 里下一个日历日开始的时刻。一般就是 time.Date(y, m, d+1, 0, 0, 0, 0, loc)。
// 夏令时在零点开始的时区当天的零点不存在，go1.27.1 的 time.Date 对它的归一方向随 UTC 偏移的正负而异（它先把墙钟
// 读数当作 UTC 去查偏移，再按换算结果是否越出该时段复核）：
// 偏移为负的时区（America/Santiago、America/Havana）往回给前一天的 23:00（仍按旧偏移），本地日期没变；拿它定时会在旧的一天里
// 触发，之后每一轮算出的都是这个已经过去的时刻，定时器立即触发，循环空转到夏令时生效为止。这时新的一天从夏令时
// 生效的那一刻开始，也就是该时刻所在时段的结束处（ZoneBounds 的 end）。
// 偏移为正的时区（Africa/Cairo、Asia/Beirut）往前给新一天的 01:00（新偏移），本地日期已变，它本身就是新一天的第一个时刻，
// 走普通分支直接返回。判断只看本地日期有没有变，不看时分：按"不存在的零点会变成非零点的时刻"去判，会把偏移为正的
// 时区的日界定到几个月后夏令时结束的那次切换。
func nextDayStart(now time.Time, loc *time.Location) time.Time {
	y, m, d := now.In(loc).Date()
	next := time.Date(y, m, d+1, 0, 0, 0, 0, loc)
	if ny, nm, nd := next.Date(); ny == y && nm == m && nd == d {
		_, end := next.ZoneBounds()
		return end
	}
	return next
}

// nodeExpiry 是一个节点在本次扫描里的到期观测；valid 为假表示本轮不评估这个节点，已有状态原样保留：库里的到期日
// 读不懂，或者续期写回没有落定（写回出错，或条件更新发现快照之后这一行变了）。
type nodeExpiry struct {
	hasExpiry bool
	daysLeft  int
	valid     bool
}

// SweepExpiry 在 writeMu 下做一次日历类扫描（sweepExpiry）。评估时机共四处：hub 启动与每个日历日开始
// （RunExpirySweep）、节点计费字段变化时与 ingest 写入变化的证书观测时调用它；保存启用的日历类规则时，SaveRule
// 已持 writeMu，直接调 sweepExpiry。一次扫描先把开着自动续期且早于今天的到期日推后并落库，再按推后之后的日期评估
// 全部启用的到期规则、按 probe_cert 的最新观测评估全部启用的证书到期规则。续期与评估在同一次 writeMu 下完成，
// 续期带来的恢复与续期本身在同一次扫描里发生，不会先发一条"已过期"再发恢复。
func (e *Engine) SweepExpiry(ctx context.Context) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	return e.sweepExpiry(ctx)
}

// sweepExpiry 的调用方持 writeMu。节点在开头读一次：之后才提交的计费修改（UpdateNode 不经 writeMu 写库）不在本次
// 快照里，本次可能按旧值多发一对转换；那次 UpdateNode 提交后自己调用 SweepExpiry，排在本次之后，按新值收敛。
// 续期写回经 RenewExpiry 的条件更新，不会盖掉快照之后的修改。
func (e *Engine) sweepExpiry(ctx context.Context) error {
	cy := newCycle()
	defer e.flush(cy)
	today := Today(e.clk.Now(), e.cfg.Location)
	nodes, err := e.st.ListMonitoringNodes(ctx)
	if err != nil {
		return err
	}
	var errs []error
	// 续期写回没有落定的节点本轮不评估，见下面两处 skip 的说明。
	skip := map[int64]bool{}
	for i := range nodes {
		n := &nodes[i]
		to, ok := renewedExpiry(n.Billing, today)
		if !ok {
			continue
		}
		renewed, err := e.st.RenewExpiry(ctx, n.ID, n.Billing.Cycle, n.Billing.ExpiresOn, to)
		if err != nil {
			// 写回出错，本轮没有得到推后之后的日期。按未推后的快照评估，开着自动续期的节点会收到"已过期"，续期成功的
			// 下一轮又收到"到期日已更新"，这一对通知都是假的；所以跳过它，错误随扫描返回。日界循环（RunExpirySweep）里的
			// 扫描出错时按退避重扫、在那一次重试续期；UpdateNode 与保存规则触发的扫描出错只记日志，由下一次扫描重试。
			errs = append(errs, err)
			skip[n.ID] = true
			continue
		}
		if !renewed {
			// 条件更新没有写入，说明快照之后这一行变了：计费被改过，改它的 UpdateNode 提交后自己会再扫描一次、
			// 按新值收敛；或者节点已被删除，没有要评估的对象。两种情形都不该按过期的快照评估。
			skip[n.ID] = true
			continue
		}
		e.log.Info("node expiry renewed", "node_id", n.ID, "node", n.Name, "cycle", string(n.Billing.Cycle), "from", n.Billing.ExpiresOn, "to", to)
		n.Billing.ExpiresOn = to
	}
	observed := make(map[int64]nodeExpiry, len(nodes))
	for _, n := range nodes {
		o := nodeExpiry{valid: !skip[n.ID]}
		if o.valid && n.Billing.ExpiresOn != "" {
			d, err := ParseDate(n.Billing.ExpiresOn)
			if err != nil {
				// 写入口都校验过日期，只有手改的库会走到这里。跳过而不是按"没有到期日"处理：读不懂的值既不该触发，
				// 也不该把已触发的告警当作恢复发出去。
				e.log.Warn("node expires_on is not a YYYY-MM-DD date; expiry rules skip this node", "node_id", n.ID, "expires_on", n.Billing.ExpiresOn)
				o.valid = false
			} else {
				o.hasExpiry, o.daysLeft = true, daysBetween(today, d)
			}
		}
		observed[n.ID] = o
	}
	for _, r := range e.Rules() {
		if !r.Enabled || r.Kind != store.KindExpiry {
			continue
		}
		candidates := map[int64]bool{}
		for _, n := range nodes {
			if !inScope(r, n.ID) || n.Rented {
				// 出租期间不提醒节点到期；不进候选集，已有状态由 pruneCandidates 撤销。
				continue
			}
			candidates[n.ID] = true
			ob := observed[n.ID]
			if !ob.valid {
				continue
			}
			o := ExpiryObservation{HasExpiry: ob.hasExpiry, DaysLeft: ob.daysLeft, DaysBefore: r.DaysBefore}
			cur := e.entry(stateKey{r.ID, n.ID})
			next, tr := NextExpiry(cur.state, o)
			summary, value := expirySummary(n, r, o, tr, cur.firedExpiresOn)
			// 进入 firing 的观测一定有到期日（NextExpiry 对没有到期日的观测只给 ok），记下的日期非空。
			fired := ""
			if next == store.StateFiring {
				fired = n.Billing.ExpiresOn
			}
			if err := e.apply(ctx, cy, r, n.ID, next, false, false, fired, tr, summary, value); err != nil {
				errs = append(errs, err)
			}
		}
		if err := e.pruneCandidates(ctx, r.ID, candidates); err != nil {
			errs = append(errs, err)
		}
	}
	errs = e.sweepCertExpiry(ctx, cy, nodes, today, errs)
	return errors.Join(errs...)
}

// expirySummary 是到期告警的文案；value 是剩余天数，因清除到期日而恢复时为 0。一条规则对一个节点只提醒一次：
// 触发文案按进入窗口时的剩余天数写"将于"或"已于"。此后留在 firing，NextExpiry 不给转换，apply 也跳过状态不变的
// 写，两处各自都挡住第二条。
// 恢复文案按离开窗口的原因。firedOn 是恢复之前那个 firing 状态记下的触发时到期日：当前到期日与它相同，日期没变，
// 写"已不在提醒窗口内"（规则的提前天数调小了；今天往回挪——hub 换了 --timezone、墙钟被往回调——也会走到这里，
// 这句同样成立）；
// 不同就是到期日改过，手动修改与自动续期都算。引擎只经触发转换进入 firing 并总是记下日期，firedOn 为空的 firing
// 只可能来自绕过引擎写的库，这时当前日期非空、与它不等，按到期日改过写。
func expirySummary(n store.Node, r store.AlertRule, o ExpiryObservation, tr *store.Transition, firedOn string) (string, float64) {
	recovered := tr != nil && *tr == store.TransitionRecovered
	switch {
	case recovered && !o.HasExpiry:
		return fmt.Sprintf("节点 %s 已清除到期日（规则 %s）", n.Name, r.Name), 0
	case recovered && n.Billing.ExpiresOn == firedOn:
		return fmt.Sprintf("节点 %s 已不在提醒窗口内（规则 %s）", n.Name, r.Name), float64(o.DaysLeft)
	case recovered:
		return fmt.Sprintf("节点 %s 到期日已更新为 %s（规则 %s）", n.Name, n.Billing.ExpiresOn, r.Name), float64(o.DaysLeft)
	case o.DaysLeft < 0:
		return fmt.Sprintf("节点 %s 已于 %s 到期（已过期 %d 天，规则 %s）", n.Name, n.Billing.ExpiresOn, -o.DaysLeft, r.Name), float64(o.DaysLeft)
	}
	return fmt.Sprintf("节点 %s 将于 %s 到期（剩 %d 天，规则 %s）", n.Name, n.Billing.ExpiresOn, o.DaysLeft, r.Name), float64(o.DaysLeft)
}

// 到期扫描出错后的退避：第一次重扫在出错那一轮开始后 1 分钟，之后每次翻倍，上限 1 小时。
const (
	expiryRetryFirst = time.Minute
	expiryRetryMax   = time.Hour
)

// nextSweepAt 是下一次到期扫描的时刻。start 是这一轮扫描开始前读到的钟，failures 是到这一轮为止连续失败的次数。
// 没有失败时是 start 之后的第一个日界；有失败时是退避到期与日界中较早的那个：日界到了照常按新的一天扫描，退避只是
// 让出错的一天不必等到下一个日界。
func nextSweepAt(start time.Time, loc *time.Location, failures int) time.Time {
	boundary := nextDayStart(start, loc)
	if failures == 0 {
		return boundary
	}
	backoff := expiryRetryFirst
	for i := 1; i < failures && backoff < expiryRetryMax; i++ {
		backoff *= 2
	}
	if retry := start.Add(min(backoff, expiryRetryMax)); retry.Before(boundary) {
		return retry
	}
	return boundary
}

// RunExpirySweep 先扫描一次（hub 停机跨过的零点由这一次补上），此后在 hub 时区每个日历日开始时扫描一次；扫描出错时
// 按退避重扫（nextSweepAt），成功即回到日界节奏。
//
// 下一次触发时刻由本轮扫描之前读到的钟（sweepRound 记下的 start）算出，nextSweepAt 保证它不晚于 start 之后的第一个
// 日界：没有失败时恰是那个日界，有失败时是退避到期与它之中较早的那个。所以扫描期间跨过的日界至多引起一次立即的重扫，
// 不会被跳过：扫描在那个时刻之前结束，定时器在它到来时触发；扫描拖过了它，定时器时长为负、立即触发，重扫按新的一天
// 评估。扫描自己若已按新的一天评估过，重扫只是重复：续期已经写回，状态没变时 apply 直接返回、不写库。反过来，
// 扫描之后才读钟，拖过日界的那一轮会从新的一天算出再下一个日界，新一天的续期与提醒就晚一整天。
//
// 定时器按单调钟走，时长在扫描之后按当时的墙钟算：墙钟被调整只影响当轮，不累积；提前触发只是多扫一次（按的仍是
// 前一天），下一轮再按真正的日界定时。
func (e *Engine) RunExpirySweep(ctx context.Context) {
	failures := 0
	for {
		var wait time.Duration
		wait, failures = e.sweepRound(ctx, failures)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// sweepRound 是 RunExpirySweep 的一轮：扫描一次，返回距下一次扫描的时长与更新后的连续失败次数（出错加一，成功清零）。
// 下一次的时刻按扫描之前读到的钟算（nextSweepAt），时长按扫描之后读到的钟算，理由见 RunExpirySweep。
func (e *Engine) sweepRound(ctx context.Context, failures int) (time.Duration, int) {
	start := e.clk.Now()
	err := e.SweepExpiry(ctx)
	if err != nil {
		failures++
	} else {
		failures = 0
	}
	wait := nextSweepAt(start, e.cfg.Location, failures).Sub(e.clk.Now())
	if err != nil {
		e.log.Error("expiry sweep failed", "err", err, "retry_in", max(wait, 0))
	}
	return wait, failures
}
