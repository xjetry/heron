package alert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/live"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
)

// flush 在一次评估调用结束时、仍持 writeMu 时逐个事件调用 Enqueue：不得阻塞，也不得回调 Engine 的写方法，否则会
// 阻塞全部写入或自锁。读方法只取 mu，flush 调用时不持 mu，因此可以读取 Channels 等快照。
type Sender interface{ Enqueue(ev store.AlertEvent) }

// cycle 是一次评估调用（一次 SweepOffline、一次 EvaluateProbes、一次 sweepExpiry）：同一次调用产生的事件属于同一个
// 评估周期，按调用而不是时间窗划分，合并窗口因此有上界——至多一次离线巡检或一次探测评估，不为等更多事件推迟发送。
//
// 合并只发生在投递层，事件层不变：alert_event 仍每个规则×节点一行，事件是状态机的事实记录，合并是发送策略。
// 合并的键是（渠道，评估周期，规则，转换方向）；firing 与 recovered 不混，一条消息里既有掉线又有恢复读不出结论。
// batches 记下本周期里每个键已开的批次，后续同键的转换请求加入它（store.DeliveryTarget.Batch）。events 在调用结束时
// 才交给 Sender：经这条路径入队的批次，worker 第一次读到它时已经收齐。worker 从库里补货（窗口曾满、启动积压或存储
// 故障之后）不经过这里，可能在调用中途读到尚未收齐的批次；那时一次发送覆盖哪些行由 store.BeginBatchAttempt 保证与
// 拼消息时读到的相同，此后加入的行要么让它拒绝、worker 重读后一并发出，要么因批次已开始尝试而新开一批。
type cycle struct {
	batches map[batchKey]int64
	events  []store.AlertEvent
}

type batchKey struct {
	channel, rule int64
	transition    store.Transition
}

func newCycle() *cycle { return &cycle{batches: map[batchKey]int64{}} }

// flush 把本周期落库的事件交给 Sender。调用方持 writeMu，在评估调用返回前调用（含出错返回），已提交的转换都会入队；
// 未装配 Sender 时转换仍完整落库，投递行可供后续续投。
func (e *Engine) flush(cy *cycle) {
	e.mu.RLock()
	sender := e.sender
	e.mu.RUnlock()
	if sender == nil {
		return
	}
	for _, ev := range cy.events {
		// 被维护静默抑制的事件没有投递行（recordAlertEvent 已保证），入队只是空转，在这里就跳过。
		if ev.Silenced {
			continue
		}
		sender.Enqueue(ev)
	}
}

type Config struct {
	TTL time.Duration
	// Location 是 hub 的 --timezone：到期日按它的日历日计，New 要求非 nil。
	Location *time.Location
}
type stateKey struct{ rule, node int64 }
type stateEntry struct {
	state   store.AlertState
	sinceAt time.Time
	// firedExpiresOn 与库里的 alert_state.fired_expires_on 同值（含义见 store.StateRow.FiredExpiresOn）：Load 从库读入，
	// apply 在写库成功后随状态一起发布。
	firedExpiresOn string
	// recoveredAt 与库里的 alert_state.recovered_at 同值（含义见 store.StateRow.RecoveredAt），Load 读入，apply 写库成功后发布。
	recoveredAt time.Time
	// firedAt 与库里的 alert_state.fired_at 同值（含义见 store.StateRow.FiredAt）：Load 读入；apply 写库成功后按 setAlertState
	// 的同一规则发布（从非 firing 进入 firing 取这次的 since，其余沿用），冷却判定读它。
	firedAt time.Time
	// firedSilenced 与库里的 alert_state.fired_silenced 同值（含义见 store.StateRow.FiredSilenced）：恢复是否投递只看它，
	// 与恢复时刻的静默状态无关，所以必须在内存里与状态同行记到 Load 之后的下一轮评估。
	firedSilenced bool
	// flapping 是离线巡检最近一次对这对规则与节点的判定：pending 且只因抖动抑制而未进入 firing（FlapDeferred）。
	// 它是由当前观测派生的展示量，不落库：重启后第一轮巡检即重新算出。apply 把它与 state 在同一个 mu 临界区里写入。
	flapping bool
}

// StateView 是对外的一条状态：落库的状态行，加上引擎在离线巡检里算出、不落库的 Flapping。
type StateView struct {
	store.StateRow
	Flapping bool
}

// Storage 是引擎对 store 的全部非历史读与写，*store.Store 满足它。历史读（QueryMetrics、QueryProbes）刻意不在其上：
// 引擎只能经 HistoryReader 读历史，评估读走评估池而不与请求驱动的读排队（store.readPoolFor），这一点由类型承载——
// 往这里加历史读方法，引擎就又能绕过评估池。
type Storage interface {
	ListAlertRules(ctx context.Context) ([]store.AlertRule, error)
	SaveAlertRule(ctx context.Context, r store.AlertRule) (store.AlertRule, error)
	DeleteAlertRule(ctx context.Context, id int64) error
	ListNotifyChannels(ctx context.Context) ([]store.NotifyChannel, error)
	SaveNotifyChannel(ctx context.Context, c store.NotifyChannel) (store.NotifyChannel, error)
	DeleteNotifyChannel(ctx context.Context, id int64) error
	ListAlertStates(ctx context.Context) ([]store.StateRow, error)
	SetAlertState(ctx context.Context, ruleID, nodeID int64, state store.AlertState, since, recoveredAt time.Time) error
	DeleteAlertState(ctx context.Context, ruleID, nodeID int64) error
	RecordTransition(ctx context.Context, ruleID, nodeID int64, state store.AlertState, firedExpiresOn string, recoveredAt time.Time, ev store.AlertEvent, targets []store.DeliveryTarget) (store.AlertEvent, error)
	ListSilences(ctx context.Context) ([]store.Silence, error)
	SaveSilence(ctx context.Context, si store.Silence) (store.Silence, error)
	DeleteSilence(ctx context.Context, id int64) error
	ListMonitoringNodes(ctx context.Context) ([]store.Node, error)
	RenewExpiry(ctx context.Context, id int64, cycle store.BillingCycle, from, to string) (bool, error)
	ProbeTaskNodeIDs(ctx context.Context, taskID uint64) ([]int64, error)
	ProbeCertsByTask(ctx context.Context, taskID uint64) (map[int64]int64, error)
	// 基线行是配置层的缓存，不是历史读：读写都在这里，算基线所需的历史只经 HistoryReader.ProbeBucketMeans。
	ReadAlertBaselines(ctx context.Context) (store.Baselines, error)
	SaveAlertBaseline(ctx context.Context, taskID uint64, b store.AlertBaseline) (bool, error)
	PruneAlertBaselines(ctx context.Context, ruleID int64, keep []int64) error
}

// HistoryReader 是引擎读历史的唯一途径；生产装配传 (*store.Store).Evaluation()。
type HistoryReader interface {
	QueryMetrics(ctx context.Context, nodeID int64, from, to int64, lv store.Level, step int64) ([]metric.Row, error)
	QueryProbes(ctx context.Context, nodeID int64, from, to int64, lv store.Level, step int64) ([]metric.ProbeRow, error)
	ProbeBucketMeans(ctx context.Context, nodeID int64, taskID uint64, from, to int64) ([]float64, error)
}

// writeMu 串行化读库、写库到内存发布；mu 只保护内存快照，不跨存储往返持有。持 writeMu 时会去取的别包的锁与跨包锁序
// 见 internal/hub 的包注释（doc.go）。
// 只有存储成功才在 mu 下发布，失败写入不会改变缓存。
type Engine struct {
	cfg      Config
	st       Storage
	history  HistoryReader
	live     *live.Live
	clk      clock.Clock
	log      *slog.Logger
	writeMu  sync.Mutex
	mu       sync.RWMutex
	rules    map[int64]store.AlertRule
	channels map[int64]store.NotifyChannel
	// silences 是维护静默的内存快照：Load 整表读入，SaveSilence/DeleteSilence/UpdateScope 在持久化成功后
	// 发布；apply 只读它做投递抑制，抑制不改写库里的任何状态。快照允许落后于库（prune 删掉的到期一次性静默
	// 会留到下一次发布，但它们 SilenceActive 恒为 false，见 publishSilences 的不变式注释）。
	silences map[int64]store.Silence
	states   map[stateKey]stateEntry
	// rentedUntil 由 SweepOffline 在 writeMu 下记录最近一次看到节点已出租的单调时刻；关闭出租后，lastSeen 不早于它。
	// 多条离线规则在同轮写入同一个 now，不会重复延长宽限。只存内存：新引擎 Load 后已按启动时刻重新计时。
	rentedUntil map[int64]time.Duration
	// started 与 startedWall 是本次 Load 的时刻，分别按单调钟与墙钟记：前者是本次启动后没有上报的节点量已离线时长的起点，
	// 后者只在库里也没有最后上报时充当离线开始（见 offlineStart）。
	started         time.Duration
	startedWall     time.Time
	sender          Sender
	traffic         *traffic.Book
	monitoringNodes func(context.Context) ([]store.Node, error)
	// baselineDirty 是保存后要在下一轮基线重算里立即重算的规则（基线窗口或最小样本数可能变了），受 mu 保护；
	// RecomputeBaselines 每轮整体取走。
	baselineDirty map[int64]bool
}

// New 对缺时区的 Config panic：到期扫描对 nil 时区调用 time.Time.In 会在运行中 panic，装配错误应当在启动时暴露。
func New(cfg Config, st Storage, history HistoryReader, l *live.Live, clk clock.Clock, log *slog.Logger) *Engine {
	if cfg.Location == nil {
		panic("alert.Config.Location must be set")
	}
	return &Engine{cfg: cfg, st: st, history: history, live: l, clk: clk, log: log, monitoringNodes: st.ListMonitoringNodes, baselineDirty: map[int64]bool{}, rules: map[int64]store.AlertRule{}, channels: map[int64]store.NotifyChannel{}, silences: map[int64]store.Silence{}, states: map[stateKey]stateEntry{}, rentedUntil: map[int64]time.Duration{}}
}
func (e *Engine) SetSender(s Sender) { e.mu.Lock(); defer e.mu.Unlock(); e.sender = s }

// SetTraffic 在启动扫描之前装配；writeMu 与所有评估入口共用，替换时不会混用两份账本。
func (e *Engine) SetTraffic(b *traffic.Book) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	e.traffic = b
}

func (e *Engine) Load(ctx context.Context) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	rules, err := e.st.ListAlertRules(ctx)
	if err != nil {
		return err
	}
	channels, err := e.st.ListNotifyChannels(ctx)
	if err != nil {
		return err
	}
	states, err := e.st.ListAlertStates(ctx)
	if err != nil {
		return err
	}
	silences, err := e.st.ListSilences(ctx)
	if err != nil {
		return err
	}
	validRules := map[int64]store.AlertRule{}
	for _, r := range rules {
		if err := CheckRule(r); err != nil {
			e.log.Warn("invalid alert rule skipped", "rule_id", r.ID, "err", err)
			continue
		}
		validRules[r.ID] = cloneRule(r)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.started, e.startedWall = e.clk.Mono(), e.clk.Now()
	e.rules = validRules
	e.channels = map[int64]store.NotifyChannel{}
	e.silences = map[int64]store.Silence{}
	for _, si := range silences {
		e.silences[si.ID] = cloneSilence(si)
	}
	e.states = map[stateKey]stateEntry{}
	for _, c := range channels {
		e.channels[c.ID] = c
	}
	for _, s := range states {
		if _, ok := validRules[s.RuleID]; ok {
			e.states[stateKey{s.RuleID, s.NodeID}] = stateEntry{state: s.State, sinceAt: s.SinceAt, firedExpiresOn: s.FiredExpiresOn, recoveredAt: s.RecoveredAt, firedSilenced: s.FiredSilenced, firedAt: s.FiredAt}
		}
	}
	return nil
}
func cloneRule(r store.AlertRule) store.AlertRule {
	r.NodeIDs = slices.Clone(r.NodeIDs)
	r.ChannelIDs = slices.Clone(r.ChannelIDs)
	r.SelectorTags = slices.Clone(r.SelectorTags)
	return r
}
func (e *Engine) Rules() []store.AlertRule {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []store.AlertRule
	for _, r := range e.rules {
		out = append(out, cloneRule(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (e *Engine) Channels() []store.NotifyChannel {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []store.NotifyChannel
	for _, c := range e.channels {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (e *Engine) States() []StateView {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []StateView
	for k, s := range e.states {
		out = append(out, StateView{store.StateRow{RuleID: k.rule, NodeID: k.node, State: s.state, SinceAt: s.sinceAt, FiredExpiresOn: s.firedExpiresOn, RecoveredAt: s.recoveredAt, FiredSilenced: s.firedSilenced, FiredAt: s.firedAt}, s.flapping})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RuleID != out[j].RuleID {
			return out[i].RuleID < out[j].RuleID
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out
}

// AllNodes 是唯一的放宽开关；SweepOffline 每轮从 store 枚举节点，因此新建节点自动纳入。
// 显式空集可能由删除节点产生，不能解释为全部节点。
func inScope(r store.AlertRule, id int64) bool { return store.ScopeContains(r.AllNodes, r.NodeIDs, id) }

func (e *Engine) SaveRule(ctx context.Context, r store.AlertRule) (store.AlertRule, error) {
	// 保存请求必须明确指定非空作用域；这不限制 DeleteNode 留下的持久化空集。
	if err := (store.NodeSelector{AllNodes: r.AllNodes, NodeIDs: r.NodeIDs, Tags: r.SelectorTags}).Check(); err != nil {
		var field store.KindFieldError
		if errors.As(err, &field) {
			return store.AlertRule{}, FieldError{Path: field.Field, Constraint: field.Constraint}
		}
		return store.AlertRule{}, err
	}
	if !r.AllNodes && len(r.NodeIDs) == 0 && len(r.SelectorTags) == 0 {
		return store.AlertRule{}, invalid("node_ids", "must not be empty unless all_nodes or selector_tags is set")
	}
	if err := CheckRule(r); err != nil {
		return store.AlertRule{}, err
	}
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	saved, err := e.st.SaveAlertRule(ctx, r)
	if err != nil {
		return store.AlertRule{}, err
	}
	e.publishRule(saved)
	if saved.AdaptiveBaseline() {
		e.mu.Lock()
		e.baselineDirty[saved.ID] = true
		e.mu.Unlock()
	}
	if saved.Enabled && saved.Kind == store.KindTraffic {
		if err := e.sweepTraffic(context.WithoutCancel(ctx), saved.ID, 0); err != nil {
			e.log.Error("traffic sweep after saving rule failed", "rule_id", saved.ID, "err", err)
		}
	}
	// 除此之外，日历类规则只在启动、日界与相关变化（计费、证书观测更新）时评估：若不在这里评估一次，新建、启用或
	// 改了提前天数的规则要等到下一个日界才有状态。规则已提交，扫描失败只记日志：保存本身成功了，下一次扫描会再评估。
	if saved.Enabled && calendarRule(saved.Kind) {
		if err := e.sweepExpiry(context.WithoutCancel(ctx)); err != nil {
			e.log.Error("expiry sweep after saving rule failed", "rule_id", saved.ID, "err", err)
		}
	}
	return saved, nil
}

// publishRule 在 SaveAlertRule 提交之后把规则与状态裁剪同步到内存；调用方持 writeMu。
func (e *Engine) publishRule(saved store.AlertRule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	previous := e.rules[saved.ID]
	// 种类、任务或指标变化后，旧观测不再描述当前规则，与 SaveAlertRule 的状态裁剪一致。
	// threshold、for_minutes 与 days_before 不改变身份，状态沿用，下一轮按新值判断是否恢复。
	identityChanged := previous.Kind != saved.Kind || previous.TaskID != saved.TaskID || previous.Metric != saved.Metric || previous.ResourceMetric != saved.ResourceMetric
	e.rules[saved.ID] = cloneRule(saved)
	// SaveAlertRule 已在同一事务裁剪状态，缓存只在提交后同步到相同集合。
	for k := range e.states {
		if k.rule == saved.ID && (identityChanged || !saved.Enabled || !inScope(saved, k.node)) {
			delete(e.states, k)
		}
	}
}

// UpdateScope 把节点标签修改与规则覆盖刷新串在评估写锁内，避免在途评估重新发布已退出作用域的状态。
// 刷新只替换规则并裁剪状态，不调用 Load；启动单调时钟属于本次进程，不能因编辑标签重新计时。
func (e *Engine) UpdateScope(mutate func() (store.NodeUpdateResult, error)) (bool, error) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	result, err := mutate()
	if err != nil {
		return false, err
	}
	for _, rule := range result.Rules {
		if err := CheckRule(rule); err != nil {
			continue
		}
		e.publishRule(rule)
	}
	// 标签变化对静默的 selector_tags 覆盖与规则覆盖在同一事务里重算（store.nodeScopesAfterUpdate），
	// 快照随这次更新整表替换，抑制判定不会用过期的覆盖。
	e.publishSilences(result.Silences)
	return result.BillingChanged, nil
}
func (e *Engine) DeleteRule(ctx context.Context, id int64) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	if err := e.st.DeleteAlertRule(ctx, id); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.rules, id)
	for k := range e.states {
		if k.rule == id {
			delete(e.states, k)
		}
	}
	return nil
}
func (e *Engine) SaveChannel(ctx context.Context, c store.NotifyChannel) (store.NotifyChannel, error) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	var previous store.NotifyChannel
	if c.ID != 0 {
		channels, err := e.st.ListNotifyChannels(ctx)
		if err != nil {
			return store.NotifyChannel{}, err
		}
		for _, old := range channels {
			if old.ID == c.ID {
				previous = old
				break
			}
		}
		if previous.ID == 0 {
			return store.NotifyChannel{}, store.NotFoundError{Kind: store.ObjectNotifyChannel, ID: c.ID}
		}
	}
	// api.channelProto 不回显凭据；writeMu 跨越读旧配置、合并、提交，两个保存不能互相覆盖所保留的值。
	switch c.Kind {
	case store.ChannelTelegram:
		cfg, err := decodeTelegram(c.Config)
		if err != nil {
			return store.NotifyChannel{}, err
		}
		if cfg.BotToken == "" && previous.Kind == store.ChannelTelegram {
			prev, err := decodeTelegram(previous.Config)
			if err != nil {
				return store.NotifyChannel{}, err
			}
			cfg.BotToken = prev.BotToken
		}
		b, err := json.Marshal(cfg)
		if err != nil {
			return store.NotifyChannel{}, err
		}
		c.Config = string(b)
	case store.ChannelWebhook:
		cfg, err := decodeWebhook(c.Config)
		if err != nil {
			return store.NotifyChannel{}, err
		}
		var prev WebhookConfig
		if previous.Kind == store.ChannelWebhook {
			prev, err = decodeWebhook(previous.Config)
			if err != nil {
				return store.NotifyChannel{}, err
			}
		}
		if cfg.URL == "" {
			cfg.URL = prev.URL
		}
		headers := map[string]string{}
		maps.Copy(headers, prev.Headers)
		for _, k := range cfg.RemoveHeaders {
			delete(headers, http.CanonicalHeaderKey(k))
		}
		maps.Copy(headers, cfg.Headers)
		cfg.Headers = headers
		if cfg.Method == "" {
			cfg.Method = "POST"
		}
		b, err := json.Marshal(cfg)
		if err != nil {
			return store.NotifyChannel{}, err
		}
		c.Config = string(b)
	}
	if err := CheckChannel(c); err != nil {
		return store.NotifyChannel{}, err
	}
	if c.Kind == store.ChannelWebhook {
		cfg, err := decodeWebhook(c.Config)
		if err != nil {
			return store.NotifyChannel{}, err
		}
		// remove_headers 只描述这次写入；持久化配置只保留合并结果，不混入已执行的指令。
		cfg.RemoveHeaders = nil
		b, err := json.Marshal(cfg)
		if err != nil {
			return store.NotifyChannel{}, err
		}
		c.Config = string(b)
	}
	saved, err := e.st.SaveNotifyChannel(ctx, c)
	if err != nil {
		return store.NotifyChannel{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.channels[saved.ID] = saved
	return saved, nil
}
func (e *Engine) DeleteChannel(ctx context.Context, id int64) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	if err := e.st.DeleteNotifyChannel(ctx, id); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.channels, id)
	return nil
}

// 调用方须在 store.DeleteNode 成功后且不持有存储回调需要的锁时调用。
// writeMu 等待在途评估发布完毕，防止其随后重建已删除节点的内存状态。
func (e *Engine) Forget(nodeID int64) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	for k := range e.states {
		if k.node == nodeID {
			delete(e.states, k)
		}
	}
	for id, r := range e.rules {
		r.NodeIDs = slices.DeleteFunc(r.NodeIDs, func(id int64) bool { return id == nodeID })
		e.rules[id] = r
	}
	for id, s := range e.silences {
		s.NodeIDs = slices.DeleteFunc(s.NodeIDs, func(id int64) bool { return id == nodeID })
		e.silences[id] = s
	}
}
func (e *Engine) current(k stateKey) store.AlertState { return e.entry(k).state }

// entry 是 k 的内存状态；没有状态的一对规则与节点按 ok 处理。
func (e *Engine) entry(k stateKey) stateEntry {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if s, ok := e.states[k]; ok {
		return s
	}
	return stateEntry{state: store.StateOK}
}

// 调用方持 writeMu；状态、事件与投递先由 store 原子提交，再发布内存，事件记入 cy，在调用结束时由 flush 交给 Sender。
// firedExpiresOn 只由到期扫描在进入 firing 时给出，其余调用传空。状态不变时 apply 不写库，所以一个 firing
// 状态记的始终是它进入 firing 那一刻的到期日。
// flapping 是同一轮观测算出的抖动标记：离线巡检传 FlapDeferred，其余种类传 false。它与状态在同一个 mu 临界区里写进
// states，状态不变、不写库时也更新；States 在 mu 下取快照，所以 ListAlertRules 读到的状态与标记来自同一轮观测。
// 维护静默只在事件生成这一处生效：maintenance 是节点当前的维护开关，抑制发生在 RecordTransition 的参数里——
// firing 转换被静默时事件照常落库（silenced=1）但没有投递行，状态行记 fired_silenced；恢复转换是否投递只看配对的
// firing 是否投递过（cur.firedSilenced），与恢复时刻是否静默无关，所以静默在 firing 中途结束不会补发通知。
// maintenance 由调用方从节点读；日历类规则（到期、证书到期）传 false——日历提醒永远不被静默，系统事件不经 apply。
func (e *Engine) apply(ctx context.Context, cy *cycle, r store.AlertRule, nodeID int64, next store.AlertState, flapping bool, maintenance bool, firedExpiresOn string, tr *store.Transition, summary string, value float64) error {
	k := stateKey{r.ID, nodeID}
	cur := e.entry(k)
	if cur.state == next {
		e.mu.Lock()
		defer e.mu.Unlock()
		// 没有状态项的一对由 entry 按 ok 处理，库里也没有它的行：状态不变说明这一轮的 next 也是 ok，而 FlapDeferred 只在
		// next 为 pending 时为真，标记必为假。不为它建内存项，否则内存会多出一行库里没有的状态。
		if s, ok := e.states[k]; ok {
			s.flapping = flapping
			e.states[k] = s
		}
		return nil
	}
	now := e.clk.Now()
	since := time.Unix(now.Unix(), 0).UTC()
	silenced := false
	if tr != nil {
		if *tr == store.TransitionRecovered {
			// 恢复事件记配对 firing 的 silenced 值：firing 被静默过的，恢复也不投递。
			silenced = cur.firedSilenced
		} else if !calendarRule(r.Kind) {
			// 日历类提醒（到期、证书到期）永远不被静默，其余种类的触发按此刻的维护开关与静默窗口抑制。
			e.mu.RLock()
			silenced = e.silencedNode(nodeID, maintenance, now)
			e.mu.RUnlock()
		}
	}
	// 状态行整行写入，上次恢复时刻必须显式带上：离线规则的恢复转换记下当下，其余写入沿用当前值（恢复之后的再次离线
	// 先写成 pending，那一次若不带上，窗口恰在要用时丢失）；其余种类不做抖动抑制，恒为零值。
	var recoveredAt time.Time
	if r.Kind == store.KindOffline {
		recoveredAt = cur.recoveredAt
		if tr != nil && *tr == store.TransitionRecovered {
			recoveredAt = since
		}
	}
	var ev store.AlertEvent
	var err error
	var merged map[int64]bool
	if tr != nil {
		targets := make([]store.DeliveryTarget, len(r.ChannelIDs))
		merged = e.mergedChannels(r.ChannelIDs)
		for i, id := range r.ChannelIDs {
			targets[i] = store.DeliveryTarget{ChannelID: id}
			if merged[id] {
				targets[i].Batch = cy.batches[batchKey{id, r.ID, *tr}]
			}
		}
		ev, err = e.st.RecordTransition(ctx, r.ID, nodeID, next, firedExpiresOn, recoveredAt, store.AlertEvent{Transition: *tr, At: now, Summary: summary, Value: value, Silenced: silenced}, targets)
	} else {
		// SetAlertState 不写触发日期，内存与库记同一个值。
		firedExpiresOn = ""
		err = e.st.SetAlertState(ctx, r.ID, nodeID, next, now, recoveredAt)
	}
	if err != nil {
		return err
	}
	// 与 setAlertState 推出 fired_at 的规则相同：状态在这里一定变了，进入 firing 即从非 firing 进入。
	firedAt := cur.firedAt
	if next == store.StateFiring {
		firedAt = since
	}
	e.mu.Lock()
	// 与库同事务写下的 fired_silenced 保持一致：只有 firing 携带触发时的静默标记，其余状态恒为假（见 RecordTransition）。
	e.states[k] = stateEntry{state: next, sinceAt: since, firedExpiresOn: firedExpiresOn, recoveredAt: recoveredAt, firedSilenced: next == store.StateFiring && silenced, flapping: flapping, firedAt: firedAt}
	e.mu.Unlock()
	if tr != nil {
		// 记下实际所在的批次而不是请求的：请求加入的批次已开始尝试时，store 新开了一批，后续同键的行加入新批。
		for _, d := range ev.Deliveries {
			if merged[d.ChannelID] {
				cy.batches[batchKey{d.ChannelID, r.ID, *tr}] = d.BatchID
			}
		}
		cy.events = append(cy.events, ev)
	}
	return nil
}

// mergedChannels 按当前渠道快照给出 ids 中合并发送的渠道（mergesBatches）。已删除的渠道不在快照里，按不合并处理，
// 随后 RecordTransition 的引用检查会拒绝它。
func (e *Engine) mergedChannels(ids []int64) map[int64]bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if c, ok := e.channels[id]; ok && mergesBatches(c.Kind) {
			out[id] = true
		}
	}
	return out
}

// 调用方持 writeMu 且已成功读取本规则候选集；读库失败时不能用空集代替候选集。
// 单行删除先持久化再发布，规则不再适用不是恢复观测，因此不经过 apply。
func (e *Engine) pruneCandidates(ctx context.Context, ruleID int64, keep map[int64]bool) error {
	var errs []error
	for _, s := range e.States() {
		if s.RuleID != ruleID || keep[s.NodeID] {
			continue
		}
		if err := e.st.DeleteAlertState(ctx, ruleID, s.NodeID); err != nil {
			errs = append(errs, err)
			continue
		}
		e.mu.Lock()
		delete(e.states, stateKey{ruleID, s.NodeID})
		e.mu.Unlock()
		e.log.Info("alert state no longer applicable", "rule_id", ruleID, "node_id", s.NodeID)
	}
	return errors.Join(errs...)
}

// offlineStart 是这次离线开始、即节点最后一次上报的墙钟，只用来与 recoveredAt 相减判定抖动窗口：recoveredAt 是落库的墙钟，
// 离线开始也必须是跨重启可比的墙钟。
//
//   - 本次启动后上报过：live 的 LastSeenWall。
//   - 否则取库里的 LastSeenAt。ingest.Service.RunFlusher 在每个分钟边界之后刷出已闭合的分钟桶、ctx 结束时刷出全部桶，
//     store.WriteMinuteBatch 随分钟行把 live 当时的 LastSeenWall 写进 node.last_seen_at。重启前已开始、重启时仍在 pending 的
//     离线因此保住原来的离线开始，重启前后按同一个宽限判定。
//   - 库里也没有（没有任何上报落过库）才退回本次启动的墙钟：这时离线开始不晚于启动时刻，启动时刻是能确定的最晚值。
//
// 落库值不晚于真实的最后上报：正常停机时全部刷出，二者相同；异常退出丢的是还没刷出的上报，存储正常时只有退出前约一个刷出周期
// 之内的那些。偏早只让 SinceRecovery 偏小、更偏向判进窗口，而窗口内的宽限 max(节点宽限, flapGrace) 不小于窗口外的节点宽限，
// 所以这个偏差只会推迟触发，不会让本该抑制的离线提前触发。
func (e *Engine) offlineStart(entry live.Entry, seen bool, node store.Node) time.Time {
	switch {
	case seen:
		return entry.LastSeenWall
	case !node.LastSeenAt.IsZero():
		return node.LastSeenAt
	default:
		return e.startedWall
	}
}

func (e *Engine) SweepOffline(ctx context.Context) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	cy := newCycle()
	defer e.flush(cy)
	nodes, err := e.monitoringNodes(ctx)
	if err != nil {
		return err
	}
	now := e.clk.Mono()
	var errs []error
	if err := e.evaluateTraffic(ctx, cy, nodes, 0, 0); err != nil {
		errs = append(errs, err)
	}
	for _, r := range e.Rules() {
		if !r.Enabled || r.Kind != store.KindOffline {
			continue
		}
		candidates := map[int64]bool{}
		for _, node := range nodes {
			if !inScope(r, node.ID) {
				continue
			}
			if node.Rented {
				// 出租节点即使仍有上报也不评估离线；不进候选集，已有状态由 pruneCandidates 撤销。
				e.rentedUntil[node.ID] = now
				continue
			}
			candidates[node.ID] = true
			k := stateKey{r.ID, node.ID}
			cur := e.entry(k)
			// 已离线时长按单调钟量，不受墙钟跳变影响：有本次启动后的上报从 live 的 LastSeen 起算，否则从本次 Load 的单调起点
			// 起算（重启不变式：重启前处于 pending 的从启动时刻重新计时）。落库的 LastSeenAt 不参与时长，用于下面的窗口判定与文案。
			lastSeen := e.started
			entry, seen := e.live.Get(node.ID)
			if seen {
				lastSeen = entry.LastSeen
			}
			if until, ok := e.rentedUntil[node.ID]; ok {
				if seen && entry.LastSeen >= until {
					delete(e.rentedUntil, node.ID)
				} else if lastSeen < until {
					lastSeen = until
				}
			}
			unseen := now - lastSeen
			o := Observation{Reported: seen, Unseen: unseen, Grace: time.Duration(node.OfflineGraceS) * time.Second, TTL: e.cfg.TTL}
			if !cur.recoveredAt.IsZero() {
				o.Recovered, o.SinceRecovery = true, e.offlineStart(entry, seen, node).Sub(cur.recoveredAt)
			}
			next, tr := NextOffline(cur.state, o)
			summary := fmt.Sprintf("节点 %s 离线 %s（规则 %s）", node.Name, unseen.Round(time.Second), r.Name)
			if !seen {
				last := "从未上报"
				if !node.LastSeenAt.IsZero() {
					last = "最后在线于 " + node.LastSeenAt.Format(time.RFC3339)
				}
				summary = fmt.Sprintf("节点 %s 离线，%s（规则 %s）", node.Name, last, r.Name)
			}
			if tr != nil && *tr == store.TransitionRecovered {
				summary = fmt.Sprintf("节点 %s 已恢复上报（规则 %s）", node.Name, r.Name)
			}
			if err := e.apply(ctx, cy, r, node.ID, next, FlapDeferred(cur.state, o), node.Maintenance, "", tr, summary, unseen.Seconds()); err != nil {
				errs = append(errs, err)
			}
		}
		if err := e.pruneCandidates(ctx, r.ID, candidates); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) EvaluateProbes(ctx context.Context, minuteTS int64) error {
	if minuteTS%60 != 0 {
		return invalid("minuteTS", "must be aligned to 60 seconds")
	}
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	cy := newCycle()
	defer e.flush(cy)
	nodes, err := e.st.ListMonitoringNodes(ctx)
	if err != nil {
		return err
	}
	lv, _ := store.LevelByName("1m")
	var errs []error
	// 基线行只在有启用的自适应基线规则时读一次，本轮各规则共用同一份快照。
	var baselines store.Baselines
	baselinesRead := false
	for _, r := range e.Rules() {
		if !r.Enabled || r.Kind != store.KindProbe {
			continue
		}
		if r.AdaptiveBaseline() && !baselinesRead {
			baselines, err = e.st.ReadAlertBaselines(ctx)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			baselinesRead = true
		}
		ids, err := e.st.ProbeTaskNodeIDs(ctx, r.TaskID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		candidates := map[int64]bool{}
		for _, node := range nodes {
			if !inScope(r, node.ID) || !slices.Contains(ids, node.ID) {
				continue
			}
			candidates[node.ID] = true
			from := minuteTS - int64(r.ForMinutes-1)*60
			rows, err := e.history.QueryProbes(ctx, node.ID, from, minuteTS+60, lv, 60)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			samples := make([]MinuteSample, r.ForMinutes)
			// 相对判定的基线无效时整窗没有读数：缺读数既不触发也不恢复（NextProbe），桶数不足、指纹已变与没有基线行都走这条路。
			band, bandOK := relativeBand(r, baselines, node.ID)
			for _, row := range rows {
				if row.TaskID != r.TaskID {
					continue
				}
				b := row.Bucket
				var value float64
				switch r.Metric {
				case store.MetricLossPct:
					if b.Sent == 0 {
						continue
					}
					value = float64(b.Lost) / float64(b.Sent) * 100
				case store.MetricRttMs:
					if b.RttN == 0 {
						continue
					}
					value = float64(b.RttSumUs) / float64(b.RttN) / 1000
				}
				if r.RttMode == store.RttRelative {
					if bandOK {
						samples[(row.TS-from)/60] = MinuteSample{Present: true, Exceeds: band.exceeds(value), Value: value}
					}
					continue
				}
				// 触发含阈值等号，恢复则必须低于阈值。
				samples[(row.TS-from)/60] = MinuteSample{Present: true, Exceeds: value >= r.Threshold, Value: value}
			}
			cur := e.entry(stateKey{r.ID, node.ID})
			next, tr := NextProbe(cur.state, samples, r.ForMinutes)
			next, tr = coolDown(r, cur, e.clk.Now(), next, tr)
			value := samples[len(samples)-1].Value
			summary := fmt.Sprintf("节点 %s 规则 %s：%s %.1f", node.Name, r.Name, r.Metric, value)
			if r.RttMode == store.RttRelative {
				summary = band.summary(node.Name, r.Name, value)
			}
			if err := e.apply(ctx, cy, r, node.ID, next, false, node.Maintenance, "", tr, summary, value); err != nil {
				errs = append(errs, err)
			}
		}
		if err := e.pruneCandidates(ctx, r.ID, candidates); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// OfflineSweepEvery 是离线巡检周期。RunOfflineSweep 用它做 ticker。
// 取消后必须在下一次 tick 前返回；节点过了离线宽限之后，投递还要再赶上这一周期。
const OfflineSweepEvery = 10 * time.Second

func (e *Engine) RunOfflineSweep(ctx context.Context) {
	ticker := time.NewTicker(OfflineSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.SweepOffline(ctx); err != nil {
				e.log.Error("offline sweep failed", "err", err)
			}
		}
	}
}

func nextProbeAt(now time.Time) time.Time {
	return now.Truncate(time.Minute).Add(time.Minute + 3*time.Second)
}

func probeMinuteAt(trigger time.Time) int64 {
	return trigger.Truncate(time.Minute).Add(-time.Minute).Unix()
}

// 分钟边界后 3s 晚于刷出调度的 0.5s 与维护调度的 2s，给通常的写入留出时间并错开竞争；
// 这些偏移不是提交屏障，存储阻塞时仍可能读到缺失分钟，NextProbe 不把缺失当作恢复。
func (e *Engine) RunProbeEvaluation(ctx context.Context) {
	for {
		now := e.clk.Now()
		timer := time.NewTimer(nextProbeAt(now).Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		minuteTS := probeMinuteAt(e.clk.Now())
		if err := e.EvaluateProbes(ctx, minuteTS); err != nil {
			e.log.Error("probe evaluation failed", "err", err)
		}
		if err := e.EvaluateResources(ctx, minuteTS); err != nil {
			e.log.Error("resource evaluation failed", "err", err)
		}
	}
}
