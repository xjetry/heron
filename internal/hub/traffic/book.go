package traffic

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
)

// FlushPeriod 是脏条目落盘的周期；退出时另刷一次。正常刷出下崩溃最多丢一个周期的内存增量；
// 只要 agent 未换启动周期、计数器未倒退（合计形态另要求网卡集合不变），重启后首次上报相对已落盘基线的差分会把这段
// 补回，基线形态不同时经合计视图衔接（Account）。
// 刷出持续失败时丢的不止一个周期；Flush 保留脏状态，Run 记录日志并重试。
const FlushPeriod = 10 * time.Second

// DefaultResetDay 与 node.traffic_reset_day 的列默认值一致。
const DefaultResetDay = 1

type Storage interface {
	LoadTraffic(ctx context.Context) ([]store.TrafficRecord, error)
	WriteTraffic(ctx context.Context, recs []store.TrafficRecord) ([]int64, error)
	TrafficResetDays(ctx context.Context) (map[int64]int, error)
}

var _ Storage = (*store.Store)(nil)

// State 是落盘的累计状态，与 traffic 表一行及其 traffic_baseline 行对应。计数器与累计值用 int64：与 SQLite
// INTEGER 同宽；计数器在入账时逐项截到 MaxInt64，管理接口拒绝越界的校正用量。
//
// Baseline 与 NetCounterEpoch 的三种形态（没有基线、合计、逐网卡）见 store.CheckTrafficBaseline。Baseline 是不可变的
// 有序值切片：Account 每次构造新切片整体替换，从不原地改写元素，所以 Flush 抓到的快照、committed、Get / View /
// Committed 交出的副本，以及 Adjust 与周期滚动用的副本，都不会被之后的入账改写；按值复制 State 即得到一致的快照。
type State struct {
	BootID             string
	NetCounterEpoch    string
	Baseline           []store.TrafficCounter
	TotalRx, TotalTx   int64
	PeriodRx, PeriodTx int64
	PeriodStart        time.Time
	// ObservedAt 是 Account 最近处理带计数上报的 hub 时刻，取整到秒；零值表示未知。
	ObservedAt time.Time
}

func (s State) HasBaseline() bool { return len(s.Baseline) > 0 }

// perInterface 报告基线是不是逐网卡形态：合计形态的唯一一项名称为空，网卡名非空。
func (s State) perInterface() bool { return len(s.Baseline) > 0 && s.Baseline[0].Name != "" }

// totalsView 是一份计数的合计视图：集合摘要与截到 MaxInt64 的合计。跨形态衔接只比较合计视图。
type totalsView struct {
	epoch  string
	rx, tx int64
}

// totalsView 给出有基线时的合计视图。合计形态就是它本身；逐网卡形态经 agentwire.SummarizeNetInterfaces 推导——与 agent
// 填 net_rx_total、net_counter_epoch 的是同一个函数，所以同一组网卡在两种形态下得到相同的摘要；合计按同一种 uint64
// 算术从（已截断的）逐项基线相加后再截到 MaxInt64，与旧的合计分支对上报合计的截断同一口径。
func (s State) totalsView() totalsView {
	if !s.perInterface() {
		return totalsView{epoch: s.NetCounterEpoch, rx: s.Baseline[0].Rx, tx: s.Baseline[0].Tx}
	}
	sum := agentwire.SummarizeNetInterfaces(s.Baseline, func(c store.TrafficCounter) (string, uint64, uint64) {
		return c.Name, uint64(c.Rx), uint64(c.Tx)
	})
	return totalsView{epoch: sum.Epoch, rx: clamp(sum.Rx), tx: clamp(sum.Tx)}
}

// Entry 是读侧视图：状态加上由重置日推出的边界，后两项不落盘。
type Entry struct {
	State
	ResetDay  int
	NextReset time.Time
}

// Delta 是一次上报入账的字节增量。
type Delta struct{ Rx, Tx int64 }

type entry struct {
	State
	committed *State
	dirty     bool
}

// Book 对同一启动周期内不回退的计数做差分；新周期满足时间与合计下降条件时整笔补记，否则只换基线。
// Account 以 boot_id 和接口集合决定基线形态，Flush 按 writeMu → mu 快照后提交；提交失败保留 dirty 状态等待重试。
type Book struct {
	st  Storage
	clk clock.Clock
	tz  *time.Location
	log *slog.Logger

	// 同一节点写库的顺序与内存状态更新的顺序一致，否则 Flush 的旧快照会盖掉已成功的校正。
	// Flush、Adjust 与 Commit 统一按 writeMu → mu 取锁；Flush 等待写库时释放 mu，入账不等待数据库。
	writeMu sync.Mutex

	// mu 保护 entries 与 resetDay。Adjust 与 Commit 在 mu 下同步写库：持锁期间所有入账等待，
	// 换来"返回成功即已持久化、返回失败则内存与库都未变"；校正是罕见的管理操作。
	mu       sync.Mutex
	entries  map[int64]*entry
	resetDay map[int64]int
}

func New(st Storage, clk clock.Clock, tz *time.Location, log *slog.Logger) *Book {
	return &Book{st: st, clk: clk, tz: tz, log: log, entries: map[int64]*entry{}, resetDay: map[int64]int{}}
}

func (b *Book) Zone() *time.Location { return b.tz }

// Load 从库恢复条目与重置日。只要 agent 未换启动周期、网卡集合且计数器未倒退，恢复的基线
// 就让重启后的首次上报通过差分补回最后一次成功刷出之后丢失的内存增量。
func (b *Book) Load(ctx context.Context) error {
	recs, err := b.st.LoadTraffic(ctx)
	if err != nil {
		return err
	}
	days, err := b.st.TrafficResetDays(ctx)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = map[int64]*entry{}
	// 库值是 int64，类型已限定上界；手工改库可能留下负累计值，恢复时统一收敛到非负范围。
	for _, r := range recs {
		// 基线形态已由 LoadTraffic 按 store.CheckTrafficBaseline 判过，这里原样接过。
		b.entries[r.NodeID] = &entry{State: State{BootID: r.BootID, NetCounterEpoch: r.NetCounterEpoch, Baseline: r.Baseline, TotalRx: max(r.TotalRx, 0), TotalTx: max(r.TotalTx, 0),
			PeriodRx: max(r.PeriodRx, 0), PeriodTx: max(r.PeriodTx, 0), PeriodStart: r.PeriodStart, ObservedAt: r.ObservedAt}}
		s := b.entries[r.NodeID].State
		b.entries[r.NodeID].committed = &s
	}
	b.resetDay = days
	return nil
}

func clamp(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// satAdd 是非负增量的饱和加法：总量到界后停在 MaxInt64，不回绕成负数。
// 调用方保证 d >= 0（差分时计数器不小于基线，校正值经 clamp 非负）。逐网卡的增量也用它逐项累计，
// 不先形成可能回绕的总差。
func satAdd(a, d int64) int64 {
	if a > math.MaxInt64-d {
		return math.MaxInt64
	}
	return a + d
}

func (b *Book) day(nodeID int64) int {
	if d, ok := b.resetDay[nodeID]; ok && d >= 1 && d <= 28 {
		return d
	}
	return DefaultResetDay
}

// rolled 只计算状态副本，校正写库失败时不能把周期滚动留在内存里。
// 周期起点按旧时区落盘；换时区后若该起点之后的首个新边界已过，下一次读取、入账或刷出会清零周期量。
// 连续错过多个周期时直接跳到 now 所在的周期——中间周期的用量没有载体，本来就丢了。
func (b *Book) rolled(s State, day int, now time.Time) (State, bool) {
	if now.Before(NextResetAfter(s.PeriodStart, day, b.tz)) {
		return s, false
	}
	s.PeriodStart = PeriodStart(now, day, b.tz)
	s.PeriodRx, s.PeriodTx = 0, 0
	return s, true
}

func (b *Book) roll(nodeID int64, e *entry, now time.Time) {
	if next, changed := b.rolled(e.State, b.day(nodeID), now); changed {
		e.State, e.dirty = next, true
	}
}

func (b *Book) view(nodeID int64, s State) Entry {
	day := b.day(nodeID)
	return Entry{State: s, ResetDay: day, NextReset: NextResetAfter(s.PeriodStart, day, b.tz)}
}

func (b *Book) fresh(nodeID int64, now time.Time) State {
	return State{PeriodStart: PeriodStart(now, b.day(nodeID), b.tz)}
}

// Account 入账一次上报：返回可供分钟桶使用的增量；false 表示本次只换基线或缺读数。
// 增量是否真的进桶由调用方按上报间隔判定。
//
// ifaces 非空时按逐网卡计数入账，调用方保证它已通过 agentwire.ValidateNetInterfaces，且与 m 的合计、摘要（若有）一致
// （ingest 的准入）；账本只从 ifaces 推合计视图，不读 m 的合计。ifaces 为空时按 m 的合计入账。
// boot_id 改变时由 increment 判定是否整笔补记；同一启动周期内：
//   - 逐网卡上报、逐网卡基线：两次都在且收发都不倒退的网卡计入；新出现的只建基线，消失的丢掉，倒退的只重建它自己。
//   - 形态不同（逐网卡上报、合计基线，或合计上报、逐网卡基线）：比较两边的合计视图，摘要相同且合计都不倒退时按合计差
//     入账，否则只重建。上报是否同时带合计与此无关，所以内存已换成逐网卡、落盘之前崩溃之后只带列表的上报，仍能相对
//     落盘的合计基线接上；上报在逐网卡与合计之间切换、集合摘要相同且合计不倒退时也不丢区间。
//   - 合计上报、合计基线：摘要相同且合计不倒退时入账，与引入逐网卡之前相同。
//
// 无论哪条规则，之后基线都换成本次上报的形态与读数。
func (b *Book) Account(nodeID int64, m *heronv1.Metrics, ifaces []*heronv1.NetInterfaceCounter) (Delta, bool) {
	var next []store.TrafficCounter
	var epoch string
	var view totalsView
	switch {
	case len(ifaces) > 0:
		next = make([]store.TrafficCounter, len(ifaces))
		for i, c := range ifaces {
			next[i] = store.TrafficCounter{Name: c.GetName(), Rx: clamp(c.GetRxBytes()), Tx: clamp(c.GetTxBytes())}
		}
		sum := agentwire.NetInterfaceSummary(ifaces)
		view = totalsView{epoch: sum.Epoch, rx: clamp(sum.Rx), tx: clamp(sum.Tx)}
	case m.NetRxTotal != nil && m.NetTxTotal != nil:
		rx, tx := clamp(m.GetNetRxTotal()), clamp(m.GetNetTxTotal())
		next = []store.TrafficCounter{{Rx: rx, Tx: tx}}
		epoch = m.GetNetCounterEpoch()
		view = totalsView{epoch: epoch, rx: rx, tx: tx}
	default:
		// 无读数不是 0：基线不动，下一次仍相对旧基线做差分。
		return Delta{}, false
	}
	now := b.clk.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[nodeID]
	if e == nil {
		e = &entry{State: b.fresh(nodeID, now)}
		b.entries[nodeID] = e
	} else {
		b.roll(nodeID, e, now)
	}
	d, ok := increment(e.State, m, next, view, now)
	e.BootID, e.NetCounterEpoch, e.Baseline, e.dirty = m.GetBootId(), epoch, next, true
	e.ObservedAt = time.Unix(now.Unix(), 0).UTC()
	if !ok {
		return Delta{}, false
	}
	e.TotalRx, e.TotalTx = satAdd(e.TotalRx, d.Rx), satAdd(e.TotalTx, d.Tx)
	e.PeriodRx, e.PeriodTx = satAdd(e.PeriodRx, d.Rx), satAdd(e.PeriodTx, d.Tx)
	return d, true
}

// increment 按 Account 的规则求本次上报相对基线 s 的增量。next 是本次上报的基线形态，view 是它的合计视图。
func increment(s State, m *heronv1.Metrics, next []store.TrafficCounter, view totalsView, now time.Time) (Delta, bool) {
	if !s.HasBaseline() {
		return Delta{}, false
	}
	if s.BootID != m.GetBootId() {
		// uptime 与旧观测时刻用于区分本机重启和另一台共用 token、早已开机的机器。
		// 秒数先校验再相减，不转换为 time.Duration，避免 uint64 的截断或纳秒乘法溢出。
		if m.UptimeS == nil || s.ObservedAt.IsZero() || now.Unix() < 0 || m.GetUptimeS() > uint64(now.Unix()) ||
			now.Unix()-int64(m.GetUptimeS()) <= s.ObservedAt.Unix() {
			return Delta{}, false
		}
		// 合计下降只是启发式，不能证明每个方向、每块网卡的计数都始于新周期。
		// 它会漏记：例如旧 1 GB/1 GB、新 2 GB/2 GB，两向都不下降就不计。
		// 它也会误计，例如网卡集合缩小，或旧 eth0=(1000,1000)、新 eth0=(1100,10)：
		// tx 下降使整笔 rx=1100 也入账，虽然 rx 可能延续旧计数；这些例子不是封闭清单。
		// Flush 在 mu 下经 record 从同一 State 复制 BootID、ObservedAt、基线和累计量，
		// WriteTraffic 的同一写事务将它们原子提交，writeMu 串行化 Flush、Adjust 与 Commit。
		// 因此落盘 BootID 仍在旧周期时，新周期计数尚未落盘，整笔补记不会重复；
		// 崩溃丢失未刷出的观测只会使 ObservedAt 偏早、放宽时间条件，不会重复已落盘计数。
		base := s.totalsView()
		if view.rx < base.rx || view.tx < base.tx {
			return Delta{Rx: view.rx, Tx: view.tx}, true
		}
		return Delta{}, false
	}
	if s.perInterface() && next[0].Name != "" {
		return interfaceDelta(s.Baseline, next)
	}
	base := s.totalsView()
	if view.epoch != base.epoch || view.rx < base.rx || view.tx < base.tx {
		return Delta{}, false
	}
	return Delta{Rx: view.rx - base.rx, Tx: view.tx - base.tx}, true
}

// interfaceDelta 逐网卡求差：两边都按名称字节序（store.CheckTrafficBaseline 与 agentwire.ValidateNetInterfaces 的形状），
// 归并一遍。只计两边都在、收发都不倒退的网卡；没有任何网卡满足时 false。
func interfaceDelta(prev, cur []store.TrafficCounter) (Delta, bool) {
	var d Delta
	ok := false
	i := 0
	for _, c := range cur {
		for i < len(prev) && prev[i].Name < c.Name {
			i++
		}
		if i == len(prev) || prev[i].Name != c.Name || c.Rx < prev[i].Rx || c.Tx < prev[i].Tx {
			continue
		}
		d.Rx, d.Tx = satAdd(d.Rx, c.Rx-prev[i].Rx), satAdd(d.Tx, c.Tx-prev[i].Tx)
		ok = true
	}
	return d, ok
}

// Get 返回有条目的节点的视图；读也会推进周期，所以周期量在边界之后读到的一定是清零的。
func (b *Book) Get(nodeID int64) (Entry, bool) {
	now := b.clk.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[nodeID]
	if !ok {
		return Entry{}, false
	}
	b.roll(nodeID, e, now)
	return b.view(nodeID, e.State), true
}

// View 是每个节点都有的视图：没有条目的节点得到零用量与按重置日算出的当前周期边界，
// 不创建条目。hub 对从未上报的节点确实累计了 0 字节，这与"无读数"不同。
func (b *Book) View(nodeID int64) Entry {
	if e, ok := b.Get(nodeID); ok {
		return e
	}
	now := b.clk.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.view(nodeID, b.fresh(nodeID, now))
}

// Committed 原样复制已提交观测，不按当前重置日滚动。Load、成功的 Flush、Adjust、Commit
// 是唯一发布点；告警引擎另检查周期有效性，未提交的清零不能造成恢复事件。
func (b *Book) Committed() map[int64]State {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[int64]State, len(b.entries))
	for id, e := range b.entries {
		if e.committed != nil {
			out[id] = *e.committed
		}
	}
	return out
}

// Commit 与 Adjust、Flush 共用写锁顺序，周期滚动与写库成功后才发布；锁内不调用告警引擎。
func (b *Book) Commit(ctx context.Context, nodeID int64) (Entry, error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[nodeID]
	if e == nil {
		e = &entry{State: b.fresh(nodeID, b.clk.Now())}
	}
	next, _ := b.rolled(e.State, b.day(nodeID), b.clk.Now())
	written, err := b.st.WriteTraffic(ctx, []store.TrafficRecord{record(nodeID, next)})
	if err != nil {
		return Entry{}, err
	}
	if len(written) == 0 {
		return Entry{}, store.ErrNotFound
	}
	e.State, e.committed, e.dirty = next, &next, false
	b.entries[nodeID] = e
	return b.view(nodeID, next), nil
}

// SetResetDay 只更新配置。边界按新日子在下一次入账、读取或刷出时判定：若 now 已越过
// 按新日子算出的下一个边界，周期立即开始新的一段、周期量清零。
func (b *Book) SetResetDay(nodeID int64, day int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resetDay[nodeID] = day
}

func record(nodeID int64, s State) store.TrafficRecord {
	return store.TrafficRecord{NodeID: nodeID, BootID: s.BootID, NetCounterEpoch: s.NetCounterEpoch, Baseline: s.Baseline, TotalRx: s.TotalRx, TotalTx: s.TotalTx,
		PeriodRx: s.PeriodRx, PeriodTx: s.PeriodTx, PeriodStart: s.PeriodStart, ObservedAt: s.ObservedAt}
}

// Adjust 把当前周期用量覆盖为给定值，总量按同一差值调整，基线不动。
// 总量始终不小于周期量（入账对两者加同一增量，校正按同一差值改两者），所以
// total - period 非负；max 只是对手工改库的防御。写穿：锁内先写库、成功才改内存。
// 没有条目的节点也可以校正：先建一条没有基线的条目，它见到的首个计数器只取基线。
// 节点不存在时写事务跳过该行，这里转成 ErrNotFound 且不留下条目。
func (b *Book) Adjust(ctx context.Context, nodeID int64, periodRx, periodTx uint64) (Entry, error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	// api 已拒绝越界的校正值，这里只对直接调用者兜底。
	rx, tx := clamp(periodRx), clamp(periodTx)
	now := b.clk.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[nodeID]
	if e == nil {
		e = &entry{State: b.fresh(nodeID, now)}
	}
	next, _ := b.rolled(e.State, b.day(nodeID), now)
	next.TotalRx = satAdd(max(next.TotalRx-next.PeriodRx, 0), rx)
	next.TotalTx = satAdd(max(next.TotalTx-next.PeriodTx, 0), tx)
	next.PeriodRx, next.PeriodTx = rx, tx
	written, err := b.st.WriteTraffic(ctx, []store.TrafficRecord{record(nodeID, next)})
	if err != nil {
		return Entry{}, err
	}
	if len(written) == 0 {
		return Entry{}, store.ErrNotFound
	}
	e.State, e.dirty = next, false
	e.committed = &next
	b.entries[nodeID] = e
	return b.view(nodeID, next), nil
}

// Forget 在节点删除后清理；删除节点的 traffic 行由 DeleteNode 的事务负责。
func (b *Book) Forget(nodeID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, nodeID)
	delete(b.resetDay, nodeID)
}

// Flush 判定周期滚动并把脏条目一次写库。写失败时把这些条目重新标脏，下一轮重试；
// 期间新入账的条目本来就是脏的，不会被误清。
func (b *Book) Flush(ctx context.Context) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	now := b.clk.Now()
	b.mu.Lock()
	var recs []store.TrafficRecord
	var ids []int64
	written := map[int64]State{}
	for id, e := range b.entries {
		b.roll(id, e, now)
		if e.dirty {
			recs = append(recs, record(id, e.State))
			ids = append(ids, id)
			written[id] = e.State
			e.dirty = false
		}
	}
	b.mu.Unlock()
	if len(recs) == 0 {
		return nil
	}
	committed, err := b.st.WriteTraffic(ctx, recs)
	if err != nil {
		b.mu.Lock()
		for _, id := range ids {
			if e := b.entries[id]; e != nil {
				e.dirty = true
			}
		}
		b.mu.Unlock()
		return err
	}
	if skipped := len(recs) - len(committed); skipped > 0 {
		b.log.Warn("traffic rows for deleted nodes dropped", "skipped", skipped)
	}
	b.mu.Lock()
	for _, id := range committed {
		state := written[id]
		if e := b.entries[id]; e != nil {
			e.committed = &state
		}
	}
	b.mu.Unlock()
	return nil
}

// Run 每 FlushPeriod 刷出一次，ctx 结束时再刷一次后返回。调用方在停止 HTTP 准入之后、
// 关库之前等待它返回，最后一次刷出才不会漏掉最后几次上报。
func (b *Book) Run(ctx context.Context) {
	t := time.NewTicker(FlushPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := b.Flush(context.Background()); err != nil {
				b.log.Error("traffic flush on shutdown failed", "err", err)
			}
			return
		case <-t.C:
			if err := b.Flush(ctx); err != nil {
				b.log.Error("traffic flush failed", "err", err)
			}
		}
	}
}
