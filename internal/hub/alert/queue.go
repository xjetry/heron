package alert

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
)

const QueueCap = 256

// NotifyTimeout 是一次渠道请求的总时限，serve 用它构造 outbound.NewClient。
// 等待重试与节奏空位不占用 worker；TestAlertRetentionCoversFullQueueDelivery 从此时限与等待上界推导保留期约束。
const NotifyTimeout = 10 * time.Second

// backoff[n-1] 是第 n 次尝试以可重试的渠道失败告终后，到下一次尝试的最短间隔。
var backoff = [...]time.Duration{time.Second, 4 * time.Second}

// MaxRetryAfter 是 429 应答的 Retry-After 纳入退避时的上限（规则见 Queue 类型注释）：固定的 1 s 与 4 s 短于 Telegram 常见的几十秒，
// 照它重试多数投递会以失败告终；不设上限则一个接收方就能让批次等上任意久。
const MaxRetryAfter = 5 * time.Minute

// rateWindow 是渠道节奏上限的计量窗口：rate_per_minute 限的是任意一段 rateWindow 内发出的请求数。
const rateWindow = time.Minute

// maxListedNodes 是一条合并消息逐行列出的节点数上限，其余写"另外 N 台"：IM 渠道对单条消息有长度上限，
// 全部节点同时掉线时消息必须仍能发出。
const maxListedNodes = 20

// maxBatchReads 是一次尝试里读批次、拼消息的次数上限。开始尝试时批次的行与读到的不同（ErrBatchChanged）就重读。
// 连续变化到上限说明批次还在长，这是正常去向而不是故障：行只在开批的那次评估调用里加入（Engine.apply 只把本次调用
// 开的批次号交给 RecordTransition，joinableBatch 拒绝已开始尝试的批次）。这时批次离开窗口、仍在库里待投递，由补货
// 或那次调用结束时 flush 的入队装回；worker 不退避，先发 ready 里的其它批次——上限就是让出 worker 的点。每次被拒
// 都对应其间至少一次行的加入或清理，所以来回的次数受这些行数约束。
const maxBatchReads = 3

// DeliveryRetryWait 是渠道失败可重试、应答不带 429 的 Retry-After、渠道节奏未满且存储正常时，retryWait 给一个批次
// 各次重试间隔的总和：第 n 次尝试得到可重试的渠道失败且未到 store.MaxDeliveryAttempts 时间隔是 backoff[n-1]，
// 所以依次是 backoff 的每一项。实际两次尝试之间可能更久，至少有下面这些情形（不是全部）：
//   - 429 的 Retry-After 把单次间隔拉长到至多 MaxRetryAfter；
//   - 渠道节奏已满时批次排队到下一个空位（rateSlot）；
//   - 间隔到期后批次回到 ready 队尾，唯一的 worker 要先发完排在它前面的批次；
//   - 下一次尝试的最早时刻随结果落库（not_before）时按秒向上取整，间隔到期后重读批次时按它再等，墙钟不在整秒时
//     比内存里算出的间隔多出不到 1 s；
//   - 满窗口时被挤出的等待批次（evictLocked）回到库里，要等 ready 取空且窗口有空位时的补货才装回，补货按批次号
//     先装更早的批次，所以可能在它的最早时刻之后很久；
//   - 存储失败时 Run 经 retryAfterFailure 退避（1s 起翻倍、上限 1 分钟），存储持续失败时没有总量上界。
//
// 启动行以 delivery_retry_wait 报出，scripts/e2e.sh 据此推出告警等待上限——e2e 的接收器是不限节奏的 Webhook
// 渠道、总回 200，同一时刻只有一个批次在投。TestQueueGivesUpAfterMaxAttempts 把它钉到只有一个批次、夹具墙钟停在
// 整秒时实际等过的总和上。
func DeliveryRetryWait() time.Duration {
	var total time.Duration
	for _, d := range backoff {
		total += d
	}
	return total
}

// waitKind 是批次在 waiting 里等什么，决定满窗口时能不能把它挤回库里：挤出的批次由补货装回，装回后第一件事是
// 重新判断能不能发（Queue.attempt），所以只有这个判断能复原原来的等待时，挤出才不会让它提前发送。
type waitKind int

const (
	// waitRate：等渠道节奏空位。空位由 sent 重算（rateSlot），挤出后装回照样等到那一刻。
	waitRate waitKind = iota
	// waitRetry：等重试间隔，下一次尝试的最早时刻已随这次的结果写进库（not_before），装回后照样等到那一刻。
	waitRetry
	// waitUnrecorded：等重试间隔，但这次的结果没写进库，那一刻只在内存里：挤出后装回会立即重试。
	waitUnrecorded
)

type waitEntry struct {
	at   time.Duration // 最早可尝试的单调时刻（clk.Mono）。
	kind waitKind
}

// Queue 的批次状态落在 alert_delivery；发送结果提交后才推进内存队列，重启可从库中恢复未完成批次。
// 429 的 Retry-After 只在 MaxRetryAfter 上限内延后下一次尝试，时间随批次持久化；固定退避不足以覆盖接收方限流时不丢批次。
// Queue 是投递的有界窗口与唯一的 worker。窗口里的单位是发送批次（store.DeliveryBatch）：批次在入库时已经定下，
// 队列只按批次号装填、发送与重试，不再合并或拆分——重启后按原批次重投，结果与崩溃前"发了什么"一致。
//
// 窗口分三部分：ready 是可以立即尝试的批次（先进先出），waiting 是等待重试间隔或渠道节奏空位的批次，外加 worker
// 正在尝试的那一批（current）。三者合计至多 limit 个（enqueue 检查；在途的批次回到 waiting 只是换个位置，不增加合计）。
// 等待不占用 worker：一个接收方要求的几分钟 Retry-After 或一个渠道的节奏上限不让 worker 停下，满队列的最坏投递时长
// 因此不超过告警事件的最短保留期（TestAlertRetentionCoversFullQueueDelivery）。等待也不把发往其它渠道的新批次挡在
// 窗口外，前提是窗口里有可挤出的批次：满窗口时先挤掉最旧的就绪批次，没有就绪批次时挤掉一个等待时刻能复原的批次
// （waitRate、waitRetry）；窗口里只剩 waitUnrecorded 与在途批次时，新批次留在库里，等窗口有空位时由补货装入。
type Queue struct {
	st           *store.Store
	channels     func() []store.NotifyChannel
	client       *http.Client
	telegramBase string
	clk          clock.Clock
	sleep        func(context.Context, time.Duration) error
	log          *slog.Logger
	// readBatch 是读批次的唯一入口（NewQueue 取 store.GetDeliveryBatch），每次尝试前回读。
	readBatch func(context.Context, int64) (store.DeliveryBatch, error)
	limit     int // ready、waiting 与在途合计至多这么多批次；NewQueue 取 QueueConfig.Limit，0 即 QueueCap。
	mu        sync.Mutex
	ready     []int64
	waiting   map[int64]waitEntry
	active    map[int64]int64 // ready ∪ waiting ∪ 在途的批次 → 它发往的渠道。
	current   int64           // 在途的批次（worker 正在尝试），0 表示没有。
	overflow  bool            // 库中可能仍有窗口外的待投递批次：满窗口没装下的、被挤出的、以 fateRefill 离开窗口的。
	// woken 与 cancelIdle 把入队告诉空闲的 worker，都在 mu 下读写：worker 进入空闲前在 mu 下看 woken，已有入队就
	// 不睡；否则登记本次空闲的 cancel，此后的入队调用它。next 在 mu 下清掉 woken，此前的入队都已反映在它看到的窗口里。
	woken      bool
	cancelIdle context.CancelFunc
	// sent 只由 worker 读写，不经 mu：渠道 → 最近 rateWindow 内各次请求的单调时刻，升序。进程重启后为空，
	// 所以一段 rateWindow 里重启 k 次时，这段时间内发出的请求至多是上限的 k+1 倍（每个进程各自至多一倍）。
	sent map[int64][]time.Duration
}

var _ Sender = (*Queue)(nil)

// QueueConfig 的两项都可省略，零值即生产行为。
type QueueConfig struct {
	// TelegramBase 是 Telegram Bot API 的根地址；空串取官方地址（NewTelegram），测试用它把请求发到本地接收器。
	TelegramBase string
	// Sleep 是 worker 等待重试间隔与渠道节奏空位的方式，ctx 取消时必须返回；nil 取真实的计时等待，测试用它推进假时钟。
	Sleep func(context.Context, time.Duration) error
	// Limit 是窗口容量（ready、waiting 与在途合计），0 取 QueueCap，负数拒绝。生产装配不设它；钉"满窗口溢出后补货"这类
	// 以窗口为界的性质的用例用小窗口，不必凑够 QueueCap+1 次真实投递——那在高负载下跑不进等待上界。
	Limit int
}

// QueueDeps 是 Queue 的协作者，全部必需：NewQueue 逐字段核对非 nil。
type QueueDeps struct {
	Store *store.Store
	// Channels 返回当前的渠道快照（alert.Engine.Channels），每次尝试投递时现取。
	Channels func() []store.NotifyChannel
	Client   *http.Client
	Clock    clock.Clock
	Log      *slog.Logger
}

// NewQueue 对依赖缺失 panic，口径与理由见 api.New。
func NewQueue(cfg QueueConfig, deps QueueDeps) *Queue {
	if cfg.Limit < 0 {
		panic("alert.QueueConfig.Limit must not be negative")
	}
	limit := cfg.Limit
	if limit == 0 {
		limit = QueueCap
	}
	if deps.Store == nil {
		panic("alert.QueueDeps.Store must be set")
	}
	if deps.Channels == nil {
		panic("alert.QueueDeps.Channels must be set")
	}
	if deps.Client == nil {
		panic("alert.QueueDeps.Client must be set")
	}
	if deps.Clock == nil {
		panic("alert.QueueDeps.Clock must be set")
	}
	if deps.Log == nil {
		panic("alert.QueueDeps.Log must be set")
	}
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	return &Queue{st: deps.Store, channels: deps.Channels, client: deps.Client, telegramBase: cfg.TelegramBase, clk: deps.Clock, sleep: sleep, log: deps.Log,
		readBatch: deps.Store.GetDeliveryBatch, limit: limit, waiting: make(map[int64]waitEntry), active: make(map[int64]int64), sent: make(map[int64][]time.Duration)}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// size 是窗口里的批次数：ready、waiting 与在途。调用方持 mu。
func (q *Queue) size() int {
	n := len(q.ready) + len(q.waiting)
	if q.current != 0 {
		n++
	}
	return n
}

// wakeLocked 把入队告诉 worker：它正在空闲就取消那次空闲，否则记下，让它下次进入空闲前看到。调用方持 mu。
func (q *Queue) wakeLocked() {
	if q.cancelIdle != nil {
		q.cancelIdle()
		q.cancelIdle = nil
		return
	}
	q.woken = true
}

// mu 同时保护窗口和 active；去重让有界窗口只装不同批次，同一批次的多行、重复的 Enqueue 与并发的 Requeue 都只占一格。
// worker 每次尝试前回读批次，保证已终态的旧项不再发送，包括并发 Requeue 读到的旧快照。
func (q *Queue) enqueue(batch, channel int64, evict bool) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.active[batch]; exists {
		return true
	}
	if q.size() >= q.limit {
		q.overflow = true
		if !evict {
			return false
		}
		if !q.evictLocked() {
			q.log.Warn("notification queue full of batches that cannot be put back; delivery batch left for refill", "batch_id", batch)
			return false
		}
	}
	q.ready = append(q.ready, batch)
	q.active[batch] = channel
	q.wakeLocked()
	return true
}

// evictLocked 给新批次腾出一格，挤出的批次仍在库里保持 done=0，由补货装回。先挤最旧的就绪批次；没有就绪批次时挤
// 一个等待时刻能复原的等待批次（见 waitKind）：节奏等待优先——它的时刻由内存里的 sent 重算，装回后判断时不用读
// 批次；已落库的重试等待装回后要读一次批次才看得到 not_before。同类里挤最晚到期的那个：它离下一次尝试最远，在它
// 到期之前补货把它装回的时间最多。在途与 waitUnrecorded 的批次不挤。
//
// 窗口满时每个新到达的批次挤出一个，被挤出的由补货装回。每一轮的代价是一次待投递索引的扫描（PendingBatches，与
// 库中待投递行数成正比），被挤出的是重试等待时再加一次批次读，与新到达自身的一次批次读加一次发送同量级。这一轮
// 由新到达驱动、不自激：挤出只发生在 Enqueue（引擎的新到达）里，补货的装填不挤出，窗口满时 next 也不补货，所以
// 没有新到达时不会来回挤出、装回。调用方持 mu。
func (q *Queue) evictLocked() bool {
	if len(q.ready) > 0 {
		old := q.ready[0]
		q.ready = q.ready[1:]
		delete(q.active, old)
		q.log.Warn("notification queue full; oldest delivery batch dropped", "batch_id", old)
		return true
	}
	var victim int64
	for b, w := range q.waiting {
		if w.kind == waitUnrecorded {
			continue
		}
		if cur, ok := q.waiting[victim]; !ok || w.kind < cur.kind || w.kind == cur.kind && (w.at > cur.at || w.at == cur.at && b > victim) {
			victim = b
		}
	}
	if victim == 0 {
		return false
	}
	delete(q.waiting, victim)
	delete(q.active, victim)
	q.log.Warn("notification queue full; waiting delivery batch put back for refill", "batch_id", victim)
	return true
}

// Engine 在一次评估调用结束时、仍持 writeMu 时调用 Enqueue；这里只操作有界内存窗口，不等数据库或网络。
// 同一批次的多行（多个事件）只入队一次。满窗口丢弃的项仍在库中保持 done=0；overflow 让 worker 在窗口取空后补货，
// 在本进程内续投。
func (q *Queue) Enqueue(ev store.AlertEvent) {
	for _, d := range ev.Deliveries {
		if !d.Done {
			q.enqueue(d.BatchID, d.ChannelID, true)
		}
	}
}

// 库中 done=0 的行是真源；按批次号升序装填窗口，未装下的批次由 worker 取空后继续补货。
func (q *Queue) Requeue(ctx context.Context) error {
	batches, err := q.st.PendingBatches(ctx)
	if err != nil {
		return err
	}
	for _, b := range batches {
		if !q.enqueue(b.ID, b.ChannelID, false) {
			break
		}
	}
	return nil
}

// fate 是一次尝试之后批次的去向，由 Run 落到窗口上。
type fate int

const (
	// fateRefill：批次离开窗口，仍在库里待投递，Run 留下补货信号（overflow），由补货装回。它是零值：出错返回时
	// 批次是否已终态无从确认，交给库里的真源判断——补货只装 done=0 的批次，多补一次只多一次索引扫描。读到重读上限
	// （maxBatchReads）也是这个去向。
	fateRefill fate = iota
	// fateWait：批次进入 waiting，到 at 再试，kind 说明它等的是什么。
	fateWait
	// fateSettled：批次已终态或已不存在，离开窗口。
	fateSettled
)

type outcome struct {
	fate fate
	at   time.Duration // 只对 fateWait 有意义。
	kind waitKind      // 只对 fateWait 有意义。
}

// 由装配方启动一个 worker；退出不清空库中待投递行，下一次 Requeue 接续未完成项。
// 每次发送前已持久化尝试计数，发送次数受 MaxDeliveryAttempts 约束。
// 结果未落盘可能来自崩溃、写失败，或关停取消时请求已到对端；后续重发都消耗尝试名额。
func (q *Queue) Run(ctx context.Context) {
	var retryDelay time.Duration
	for ctx.Err() == nil {
		batch, refill, until, timed := q.next()
		switch {
		case refill:
			// 只有 worker 自动补货，数据库往返不持 mu，也不占用 Engine 的写锁。
			if err := q.Requeue(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				q.log.Error("notification refill failed", "err", err)
				// next 在交出补货时已清掉信号，没读成的这一次要把它留回去。
				q.mu.Lock()
				q.overflow = true
				q.mu.Unlock()
				if err := q.retryAfterFailure(ctx, &retryDelay); err != nil {
					return
				}
			}
		case batch != 0:
			out, err := q.attempt(ctx, batch)
			q.mu.Lock()
			q.current = 0
			switch out.fate {
			case fateWait:
				q.waiting[batch] = waitEntry{out.at, out.kind}
			case fateSettled:
				delete(q.active, batch)
			case fateRefill:
				delete(q.active, batch)
				q.overflow = true
			}
			q.mu.Unlock()
			if err != nil && ctx.Err() == nil {
				q.log.Error("notification delivery failed", "batch_id", batch, "err", err)
				if err := q.retryAfterFailure(ctx, &retryDelay); err != nil {
					return
				}
			} else if err == nil {
				retryDelay = 0
			}
		default:
			q.idle(ctx, until, timed)
		}
	}
}

// next 取下一件事：到期的等待批次先按到期先后回到 ready 队尾；ready 取空且可能有库中余项时补货（窗口已满时不补——
// 装不进任何一项，只会空转读库）；否则取 ready 队首作为在途批次。都没有时给出最早的到期时刻供 idle 等待。
func (q *Queue) next() (batch int64, refill bool, until time.Duration, timed bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.woken = false
	now := q.clk.Mono()
	var due []int64
	for b, w := range q.waiting {
		if w.at <= now {
			due = append(due, b)
		}
	}
	slices.SortFunc(due, func(a, b int64) int { return cmp.Or(cmp.Compare(q.waiting[a].at, q.waiting[b].at), cmp.Compare(a, b)) })
	for _, b := range due {
		delete(q.waiting, b)
		q.ready = append(q.ready, b)
	}
	if len(q.ready) == 0 && q.overflow && q.size() < q.limit {
		q.overflow = false
		return 0, true, 0, false
	}
	if len(q.ready) > 0 {
		batch, q.ready = q.ready[0], q.ready[1:]
		q.current = batch
		return batch, false, 0, false
	}
	for _, w := range q.waiting {
		if !timed || w.at < until {
			until, timed = w.at, true
		}
	}
	return 0, false, until, timed
}

// idle 在没有可做的事时等待：有新入队即醒（enqueue 经 wakeLocked 取消这次等待）；有等待中的批次时最迟醒在最早的
// 到期时刻。等待经 q.sleep，测试据此推进时钟。
func (q *Queue) idle(ctx context.Context, until time.Duration, timed bool) {
	q.mu.Lock()
	if q.woken {
		q.mu.Unlock()
		return
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q.cancelIdle = cancel
	q.mu.Unlock()
	if timed {
		_ = q.sleep(wctx, until-q.clk.Mono())
	} else {
		<-wctx.Done()
	}
	q.mu.Lock()
	q.cancelIdle = nil
	q.mu.Unlock()
}

// retryAfterFailure 是存储故障后的 worker 级退避：避免故障期间空转读库和高频错误日志。续投信号不在这里留：
// 出错离开窗口的批次由它的去向（fateRefill）留下，补货没读成由 Run 的补货分支留回。单个批次的发送上限由
// BeginBatchAttempt 保证。同一个 worker 共用退避，attempt 返回 nil 时复位。
func (q *Queue) retryAfterFailure(ctx context.Context, delay *time.Duration) error {
	if *delay == 0 {
		*delay = time.Second
	} else {
		*delay = min(*delay*2, time.Minute)
	}
	return q.sleep(ctx, *delay)
}

// message 由批次的事件组成这次发送的消息：规则与节点用当前名称，事件文案用落库的原文。
//
// 单个事件的批次就是那个事件本身，Webhook 的模板字段逐事件含义不变。多个事件的批次合成一条：首行写规则、台数与转换
// 方向，其后逐行列出至多 maxListedNodes 个事件的原文，超出的写"另外 N 台"。批次只按（渠道，评估周期，规则，转换
// 方向）合并（Engine.apply），所以同批事件的规则与方向相同，首行取第一个事件的即可。合并只发生在 Telegram 渠道；
// 渠道在批次待发期间被改成 Webhook 时仍按合并文案发一次请求（Node 为逐个列出的节点名，Value 为 0）——"一个批次
// 一次发送"是尝试次数与结果能整批记账的前提，优先于模板字段的逐事件含义。
//
// 系统事件（登录、备份、流量报告）的标签与种类按 transition 给出（store.SystemEventKind），不按 0/0 推断。它们的批次
// 只有这一个事件：写侧每个渠道新开一批（store.systemTargets），Engine 只把告警转换并进它自己开的批次——所以登录通知与
// 流量报告发往同一个渠道也各发一条，种类与正文各是各的。正文就是落库的摘要：流量报告的摘要是整条报告
// （TrafficReportText），Telegram 原样发它，Webhook 的 summary 字段是它、kind 与 transition 都是 traffic_report。
func (q *Queue) message(ctx context.Context, b store.DeliveryBatch) (Message, error) {
	first := b.Events[0]
	if kind, ok := store.SystemEventKind(first.Transition); ok {
		return Message{Rule: "系统事件", Node: "Hub", Kind: kind, Transition: string(first.Transition), Summary: first.Summary, Value: first.Value, At: first.At}, nil
	}
	m := Message{Rule: fmt.Sprintf("规则 #%d", first.RuleID), Transition: string(first.Transition), At: first.At}
	rules, err := q.st.ListAlertRules(ctx)
	if err != nil {
		return m, err
	}
	for _, r := range rules {
		if r.ID == first.RuleID {
			m.Rule, m.Kind = r.Name, string(r.Kind)
			break
		}
	}
	listed := b.Events[:min(len(b.Events), maxListedNodes)]
	names := make([]string, 0, len(listed)+1)
	for _, ev := range listed {
		name := fmt.Sprintf("节点 #%d", ev.NodeID)
		node, err := q.st.GetNode(ctx, ev.NodeID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return m, err
		}
		if err == nil {
			name = node.Name
		}
		names = append(names, name)
	}
	if len(b.Events) == 1 {
		m.Node, m.Summary, m.Value = names[0], first.Summary, first.Value
		return m, nil
	}
	verb := "触发告警"
	if first.Transition == store.TransitionRecovered {
		verb = "已恢复"
	}
	lines := []string{fmt.Sprintf("规则 %s：%d 台节点%s", m.Rule, len(b.Events), verb)}
	for _, ev := range listed {
		lines = append(lines, ev.Summary)
	}
	if rest := len(b.Events) - len(listed); rest > 0 {
		more := fmt.Sprintf("另外 %d 台", rest)
		lines, names = append(lines, more), append(names, more)
	}
	m.Node, m.Summary = strings.Join(names, "、"), strings.Join(lines, "\n")
	return m, nil
}

// PruneAlertEvents 与投递并发，且在同一事务删除事件和投递；行消失等价于终态，
// 不是存储故障，不能通过失败退避拖住后续投递。仅归一事件和投递行的不存在错误。
func deliveryRemoved(err error) bool {
	var missing store.NotFoundError
	return errors.As(err, &missing) && (missing.Kind == store.ObjectAlertEvent || missing.Kind == store.ObjectAlertDelivery)
}

// attempt 对批次做至多一次尝试。批次在库里的行是真源，每次都回读：渠道删除会把行置为终态，已读到终态的旧队列项
// 不再发送。节奏在读批次之前判断：同一渠道的多个批次等同一个空位时，空位打开前谁也不读库。
//
// 重试按批次进行——同批各行共享尝试次数与结果（store.BeginBatchAttempt、store.UpdateBatch），一次尝试发送整批合成的
// 那一条消息。开始尝试只覆盖拼这条消息时读到的行：批次在读之后又有行加入（补货可以在一次巡检中途读到批次）时，
// BeginBatchAttempt 拒绝，这里重读、重拼；读到 maxBatchReads 次仍在变就让出 worker（fateRefill）。
//
// 每条返回路径都显式给出去向；出错返回用 outcome{}，即 fateRefill（见 fate）。
func (q *Queue) attempt(ctx context.Context, batch int64) (out outcome, err error) {
	defer func() {
		if deliveryRemoved(err) {
			out, err = outcome{fate: fateSettled}, nil
		}
	}()
	q.mu.Lock()
	id := q.active[batch]
	q.mu.Unlock()
	var c *store.NotifyChannel
	for _, row := range q.channels() {
		if row.ID == id {
			c = &row
			break
		}
	}
	if c == nil {
		q.releaseChannel(id)
		return q.settle(ctx, batch, store.DeliveryResult{Done: true, Failure: store.FailureChannelDeleted})
	}
	// 节奏已满时排队到下一个空位，不消耗尝试名额：超出上限的批次排队而不是丢弃。
	if at, full := q.rateSlot(*c, q.clk.Mono()); full {
		return outcome{fate: fateWait, at: at, kind: waitRate}, nil
	}
	var m Message
	var channel Channel
	var ds []store.Delivery
	for reads := 1; ; reads++ {
		b, err := q.readBatch(ctx, batch)
		if err != nil {
			return outcome{}, err
		}
		head := b.Deliveries[0]
		if head.Done {
			return outcome{fate: fateSettled}, nil
		}
		if head.Attempts >= store.MaxDeliveryAttempts {
			// 名额已耗尽而行仍未终态：最后一次尝试已发出，结果没落盘。接收方可能已经收到，
			// 所以不沿用更早一次的失败，也不留下它的原文与状态码。
			return q.settle(ctx, batch, store.DeliveryResult{Done: true, Failure: store.FailureResultUnrecorded})
		}
		// 上一次尝试写下的最早时刻（重试间隔、Retry-After）对补货与重启后装回的批次同样有效。
		if wait := head.NotBefore.Sub(q.clk.Now()); wait > 0 {
			return outcome{fate: fateWait, at: q.clk.Mono() + wait, kind: waitRetry}, nil
		}
		channel, err = ParseChannel(*c, q.client, q.telegramBase)
		if err != nil {
			r, _ := Classify(err)
			r.Done = true
			return q.settle(ctx, batch, r)
		}
		if m, err = q.message(ctx, b); err != nil {
			return outcome{}, err
		}
		rows := make([]int64, len(b.Deliveries))
		for i, d := range b.Deliveries {
			rows[i] = d.ID
		}
		ds, err = q.st.BeginBatchAttempt(ctx, batch, rows)
		switch {
		case errors.Is(err, store.ErrDeliveryDone):
			return outcome{fate: fateSettled}, nil
		case errors.Is(err, store.ErrBatchChanged) && reads < maxBatchReads:
			continue
		case errors.Is(err, store.ErrBatchChanged):
			q.log.Info("delivery batch still growing after repeated reads; left for refill", "batch_id", batch, "reads", reads)
			return outcome{fate: fateRefill}, nil
		case err != nil:
			return outcome{}, err
		}
		break
	}
	// 尝试已落盘即计入节奏：请求可能已到达接收方，无论结果如何都占用它的额度。
	q.sent[c.ID] = append(q.sent[c.ID], q.clk.Mono())
	sendErr := channel.Send(ctx, m)
	if ctx.Err() != nil {
		return outcome{}, ctx.Err()
	}
	r := store.DeliveryResult{OK: true, DeliveredAt: q.clk.Now()}
	retry := false
	if sendErr != nil {
		r, retry = Classify(sendErr)
	}
	attempts := ds[0].Attempts
	r.Done = r.OK || !retry || attempts >= store.MaxDeliveryAttempts
	if r.Done {
		return q.settle(ctx, batch, r)
	}
	wait := q.retryWait(sendErr, attempts)
	r.NotBefore = q.clk.Now().Add(wait)
	if err := q.st.UpdateBatch(ctx, batch, r); err != nil {
		// 请求已经发出，接收方要求的等待（Retry-After）与固定退避都已算出：结果没写进库不改变该等多久。批次按它
		// 留在 waiting（仍在 active，补货不会再装一份），存储退避照常。
		return outcome{fate: fateWait, at: q.clk.Mono() + wait, kind: waitUnrecorded}, err
	}
	return outcome{fate: fateWait, at: q.clk.Mono() + wait, kind: waitRetry}, nil
}

// settle 把终态结果写到批次上：写成了批次离开窗口（fateSettled）；没写成它仍在库里待投递（fateRefill），由补货装回。
func (q *Queue) settle(ctx context.Context, batch int64, r store.DeliveryResult) (outcome, error) {
	if err := q.st.UpdateBatch(ctx, batch, r); err != nil {
		return outcome{}, err
	}
	return outcome{fate: fateSettled}, nil
}

// releaseChannel 在读到渠道已删除时调用：它在 waiting 里的其余批次提前回到 ready，各自读到终态后离开窗口，
// 不再占着格子等到原定时刻；它的节奏记录一并丢掉。只由 worker 调用（sent 不经 mu）。
func (q *Queue) releaseChannel(channel int64) {
	delete(q.sent, channel)
	q.mu.Lock()
	defer q.mu.Unlock()
	var freed []int64
	for b := range q.waiting {
		if q.active[b] == channel {
			freed = append(freed, b)
		}
	}
	slices.Sort(freed)
	for _, b := range freed {
		delete(q.waiting, b)
		q.ready = append(q.ready, b)
	}
}

// rateSlot 在渠道最近 rateWindow 内的请求数已达 RatePerMinute 时给出下一个空位的单调时刻：第 RatePerMinute 近的
// 那次请求滑出窗口之时。顺带丢掉已滑出窗口的记录。RatePerMinute 为 0 表示不限。只由 worker 调用（见 Queue.sent）。
func (q *Queue) rateSlot(c store.NotifyChannel, now time.Duration) (time.Duration, bool) {
	log := q.sent[c.ID]
	i := 0
	for i < len(log) && log[i] <= now-rateWindow {
		i++
	}
	log = log[i:]
	if len(log) == 0 {
		delete(q.sent, c.ID)
	} else {
		q.sent[c.ID] = log
	}
	if c.RatePerMinute <= 0 || len(log) < c.RatePerMinute {
		return 0, false
	}
	return log[len(log)-c.RatePerMinute] + rateWindow, true
}

// retryWait 是第 attempts 次尝试以可重试失败告终后到下一次尝试的最短间隔（更久的情形见 DeliveryRetryWait）：固定退避
// backoff[attempts-1]；429 应答带 Retry-After 时取两者较大者，Retry-After 至多计 MaxRetryAfter。固定退避是下限：
// Retry-After 为 0 或已过去的日期不让重试比没有它时更密。
func (q *Queue) retryWait(err error, attempts int) time.Duration {
	wait := backoff[attempts-1]
	var f *sendFailure
	if errors.As(err, &f) && f.status == http.StatusTooManyRequests {
		if d, ok := parseRetryAfter(f.retryAfter, q.clk.Now()); ok {
			wait = max(wait, min(d, MaxRetryAfter))
		}
	}
	return wait
}

// parseRetryAfter 解析 Retry-After 的两种写法（RFC 9110 第 10.2.3 节）：非负整数秒，或相对 now 的 HTTP 日期。
// 超过 MaxRetryAfter 的秒数在换算前就截到上限，巨大的数值不会溢出成负的时长。读不懂的值返回 false，按固定退避。
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if strings.Trim(v, "0123456789") == "" {
		secs, err := strconv.ParseInt(v, 10, 64)
		if err != nil || secs > int64(MaxRetryAfter/time.Second) {
			return MaxRetryAfter, true
		}
		return time.Duration(secs) * time.Second, true
	}
	at, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	return at.Sub(now), true
}

// Classify 是渠道失败的唯一分类：队列据它落库并决定是否重试，TestNotifyChannel 据它选错误码。
// 真实渠道每条可达的失败路径都在 notify.go 带上类别，由 notify 的表驱动测试逐条钉住；
// 取不到类别说明某条路径漏了，记为 unclassified 让遗漏可见，而不是标成某个具体类别，且不重试。
func Classify(err error) (store.DeliveryResult, bool) {
	var f *sendFailure
	if !errors.As(err, &f) {
		return store.DeliveryResult{Failure: store.FailureUnclassified, Error: err.Error()}, false
	}
	return store.DeliveryResult{Failure: f.failure, HTTPStatus: f.status, Error: f.detail}, f.retryable()
}

func (q *Queue) SendTest(ctx context.Context, c store.NotifyChannel) error {
	channel, err := ParseChannel(c, q.client, q.telegramBase)
	if err != nil {
		return err
	}
	return channel.Send(ctx, Message{Rule: "测试规则", Node: "测试节点", Kind: "test", Transition: "test", Summary: "这是一条测试通知", At: q.clk.Now()})
}
