package api

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/live"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
)

const (
	defaultMaxPoints = 720
	maxMaxPoints     = 2000
	maxQuerySpan     = 400 * 24 * time.Hour
)

// history 是两族历史查询与跨节点对比的唯一实现，管理端与公开端共用。两端在两处不同，都由调用方
// 传入：节点准入（管理端 requireNode / store.NodeExists，公开端 requirePublic / store.NodeIsPublic），
// 以及探测序列怎样标注（taskLabel）。准入要读库，由这里在拿到本来源的在飞空位之后执行：入口与本层
// 为一个历史请求做的库读（节点准入与查询本身）都发生在持位期间，入口在进入之前只做不碰库的请求
// 校验（checkWindow、checkComparisonTask、checkComparisonNodes）。所有接口共用的鉴权（凭据查库）
// 在拦截器里、早于入口，不在这条约束内。准入若在拿空位之前，同一来源的突发请求会在排队前各占一个
// 读连接，按来源的在飞上限就封不住它的读并发。
type history struct {
	store *store.Store
	// probes 只用来取节点当前的任务数，给单节点探测查询选首次读池（见 probeSeries）。
	probes *probe.Registry
	log    *slog.Logger
	gate   *historyGate
}

// taskLabel 给出任务的种类与目标；ok 为 false 时两项留空，客户端退回编号。两端不同：管理端按
// 调用方可见的任务当前配置标注（probes.go 的 QueryProbes 传入可见任务闭包，任务迁出 token 范围后
// 不泄露其新目标），公开端用 probe.Registry.TargetFor（只标当前分配给被查节点的任务，历史里出现、
// 现已撤下的任务留空）。对比的 ListProbeComparisonNodes 不经它：候选与标注在那里的同一个读事务里读出。
type taskLabel func(taskID uint64) (kind heronv1.ProbeKind, target string, ok bool)

// checkWindow 为两族查询、两个服务维持同一套窗口与点数约束；只看请求本身，不查库。
func checkWindow(from, to int64, requested uint32) (int, error) {
	if from < 0 {
		return 0, invalid("from must be a nonnegative Unix timestamp; got %d", from)
	}
	if from >= to {
		return 0, invalid("from (%d) must be earlier than to (%d)", from, to)
	}
	// 两族水位从 Unix epoch 开始；非负秒差直接比较，避免转换纳秒时溢出。
	if span := to - from; span > int64(maxQuerySpan/time.Second) {
		return 0, invalid("window spans %d seconds; the maximum is %d (400 days)", to-from, int64(maxQuerySpan/time.Second))
	}
	maxPoints := int(requested)
	if maxPoints == 0 {
		maxPoints = defaultMaxPoints
	}
	if maxPoints > maxMaxPoints {
		return 0, invalid("max_points must be at most %d; got %d", maxMaxPoints, maxPoints)
	}
	return maxPoints, nil
}

// metrics 在拿到本来源空位后先执行 admit（该端的节点准入，要读库，见 history），再读指标桶。
func (h history) metrics(ctx context.Context, m *heronv1.QueryMetricsRequest, maxPoints int, admit func(context.Context) error) (*heronv1.QueryMetricsResponse, error) {
	// 按来源限在飞：等待不占读连接；defer 覆盖错误与 panic 路径。
	release, err := h.gate.acquire(ctx, historySource(ctx))
	if err != nil {
		return nil, err
	}
	defer release()
	if err := admit(ctx); err != nil {
		return nil, err
	}
	lv, step := store.ChooseLevel(m.GetFrom(), m.GetTo(), maxPoints)
	rows, summary, err := h.store.QueryMetricsCoverage(ctx, m.GetNodeId(), m.GetFrom(), m.GetTo(), lv, step)
	if err != nil {
		h.log.Error("metric query failed", "err", err)
		return nil, h.queryError(err, "metric query failed")
	}
	resp := &heronv1.QueryMetricsResponse{Level: lv.Name, StepS: uint32(step)}
	resp.CoverageSummary = &heronv1.CoverageSummary{EligibleMinutes: summary.EligibleMinutes,
		ObservedMinutes: summary.ObservedMinutes, ObservedReportedMinutes: summary.ObservedReportedMinutes, CoverageStart: summary.CoverageStart}
	series := make([]*heronv1.MetricSeries, len(metric.Columns))
	for i, c := range metric.Columns {
		series[i] = &heronv1.MetricSeries{Name: c.Name, Unit: c.Unit, Samples: make([]*heronv1.MetricSample, 0, len(rows))}
	}
	for _, r := range rows {
		resp.Ts = append(resp.Ts, r.TS)
		resp.Coverage = append(resp.Coverage, &heronv1.PointCoverage{Minutes: r.Coverage.Minutes, Observed: r.Coverage.Observed, ObservedReported: r.Coverage.ObservedReported})
		for i, c := range metric.Columns {
			sample := &heronv1.MetricSample{N: r.Bucket.N[i]}
			switch {
			case c.Kind == metric.Sum:
				// 可加量下发和，不下发均值：一分钟内的字节数除以入账次数没有意义。
				if r.Bucket.N[i] > 0 {
					sample.Sum = proto.Float64(r.Bucket.Sum[i])
				}
			default:
				if mean, ok := r.Bucket.Mean(i); ok {
					sample.Mean = proto.Float64(mean)
					if c.Kind == metric.MeanMax {
						sample.Max = proto.Float64(r.Bucket.Max[i])
					}
				}
			}
			series[i].Samples = append(series[i].Samples, sample)
		}
	}
	resp.Series = series
	return resp, nil
}

func (h history) probeSeries(ctx context.Context, m *heronv1.QueryProbesRequest, maxPoints int, label taskLabel, order []uint64, admit func(context.Context) error) (*heronv1.QueryProbesResponse, error) {
	// 按来源限在飞：等待不占读连接；defer 覆盖错误与 panic 路径。admit 在持位后执行，同 metrics。
	release, err := h.gate.acquire(ctx, historySource(ctx))
	if err != nil {
		return nil, err
	}
	defer release()
	if err := admit(ctx); err != nil {
		return nil, err
	}
	lv, step := store.ChooseLevel(m.GetFrom(), m.GetTo(), maxPoints)
	// 序列数取节点的全部任务数，不取调用方可见的任务数：读的是这个节点全部任务的行，受限 token 只见其中一部分，
	// 按可见数估会把同一条扫描低估进轻池，选池随调用方而变；管理与公开两端、各种 token 对同一节点同一窗口估计相同。
	rows, err := h.store.QueryProbes(ctx, m.GetNodeId(), m.GetFrom(), m.GetTo(), lv, step, h.probes.TaskCount(m.GetNodeId()))
	if err != nil {
		h.log.Error("probe query failed", "err", err)
		return nil, h.queryError(err, "probe query failed")
	}
	resp := &heronv1.QueryProbesResponse{Level: lv.Name, StepS: uint32(step)}
	var cur *heronv1.ProbeSeries
	for _, r := range rows { // store 已按 TaskID、TS 排序
		if r.Bucket.Sent == 0 {
			continue
		}
		if cur == nil || cur.TaskId != r.TaskID {
			cur = &heronv1.ProbeSeries{TaskId: r.TaskID}
			cur.Kind, cur.Target, _ = label(r.TaskID)
			resp.Series = append(resp.Series, cur)
		}
		sample := &heronv1.ProbeSample{Ts: r.TS, Sent: r.Bucket.Sent, Lost: r.Bucket.Lost, Errors: r.Bucket.Errors}
		if mean, ok := r.Bucket.RttMean(); ok {
			sample.RttMeanUs, sample.RttMinUs, sample.RttMaxUs = proto.Uint32(mean), proto.Uint32(r.Bucket.RttMinUs), proto.Uint32(r.Bucket.RttMaxUs)
		}
		cur.Samples = append(cur.Samples, sample)
	}
	rank := make(map[uint64]int, len(order))
	for i, id := range order {
		rank[id] = i
	}
	sort.Slice(resp.Series, func(i, j int) bool {
		a, b := resp.Series[i].TaskId, resp.Series[j].TaskId
		ra, knownA := rank[a]
		rb, knownB := rank[b]
		if knownA != knownB {
			return knownA
		}
		if knownA {
			return ra < rb
		}
		return a < b
	})
	return resp, nil
}

// queryError 把存储层的历史查询错误映射到 Connect 码：读量超额由请求窗口与库内实际行数共同
// 决定、可由调用方消除（缩小窗口、增大 max_points、等待数据整理），按原文返回 FailedPrecondition；
// 其余按内部错误处理，不把细节带给调用方。两个服务的历史入口共用这一映射。
func (h history) queryError(err error, operation string) error {
	var quota store.ReadQuotaError
	if errors.As(err, &quota) {
		return connect.NewError(connect.CodeFailedPrecondition, quota)
	}
	h.log.Error(operation, "err", err)
	return internalError(operation)
}

// comparisonSplits 按与单节点查询相同的节点准入逐节点判定分块：可见节点进 visible，不存在或不在
// 视野的进 unavailable，不加区分（与单节点查询对二者同一个 NotFound 一致），两者都保持请求顺序。
// nodeVisible 每个节点读一次库，只能在持有本来源空位时调用；唯一调用方是 history.comparison。
// 分块不核对分配：某节点上某任务的历史，单节点 QueryProbes 已按同一授权返回（含撤下与已删除
// 任务的行），分块不放宽任何可见范围，也就不需要分配或配置版本来授权。
func comparisonSplits(ctx context.Context, log *slog.Logger, ids []int64, nodeVisible func(context.Context, int64) (bool, error)) (visible, unavailable []int64, err error) {
	for _, id := range ids {
		ok, err := nodeVisible(ctx, id)
		if err != nil {
			log.Error("looking up node failed", "err", err)
			return nil, nil, internalError("looking up node failed")
		}
		if ok {
			visible = append(visible, id)
		} else {
			unavailable = append(unavailable, id)
		}
	}
	return visible, unavailable, nil
}

// comparison 组装对比分块的响应。nodeIDs 是请求里的节点清单（形状已由 checkComparisonNodes 校验），
// nodeVisible 是该端的逐节点准入（管理端 store.NodeExists 按调用方作用域，公开端 store.NodeIsPublic），
// 在拿到空位之后经 comparisonSplits 执行。窗口内没有样本的可见节点也出现（samples 为空），缺数与零值
// 在协议层可区分。
func (h history) comparison(ctx context.Context, taskID uint64, nodeIDs []int64, from, to int64, maxPoints int, nodeVisible func(context.Context, int64) (bool, error)) (*heronv1.QueryProbeComparisonResponse, error) {
	// 按来源限在飞：等待不占读连接；defer 覆盖错误与 panic 路径。
	release, err := h.gate.acquire(ctx, historySource(ctx))
	if err != nil {
		return nil, err
	}
	defer release()
	visible, unavailable, err := comparisonSplits(ctx, h.log, nodeIDs, nodeVisible)
	if err != nil {
		return nil, err
	}
	lv, step := store.ChooseLevel(from, to, maxPoints)
	rows, err := h.store.QueryProbeComparison(ctx, taskID, visible, from, to, lv, step)
	if err != nil {
		return nil, h.queryError(err, "probe comparison query failed")
	}
	resp := &heronv1.QueryProbeComparisonResponse{Level: lv.Name, StepS: uint32(step), UnavailableNodeIds: unavailable}
	series := make([]*heronv1.NodeProbeSamples, len(visible))
	index := make(map[int64]int, len(visible))
	for i, id := range visible {
		series[i] = &heronv1.NodeProbeSamples{NodeId: id}
		index[id] = i
	}
	for _, r := range rows { // store 已按 (node_id, ts) 排序
		if r.Bucket.Sent == 0 {
			continue
		}
		sample := &heronv1.ProbeSample{Ts: r.TS, Sent: r.Bucket.Sent, Lost: r.Bucket.Lost, Errors: r.Bucket.Errors}
		if mean, ok := r.Bucket.RttMean(); ok {
			sample.RttMeanUs, sample.RttMinUs, sample.RttMaxUs = proto.Uint32(mean), proto.Uint32(r.Bucket.RttMinUs), proto.Uint32(r.Bucket.RttMaxUs)
		}
		series[index[r.NodeID]].Samples = append(series[index[r.NodeID]].Samples, sample)
	}
	resp.Series = series
	return resp, nil
}

// liveState 是两端快照共用的在线判定：在线只来自 live；库里的 last_seen_at 只在 live 没有该节点
// （hub 重启后尚未再上报）时用来展示"上次见到"，不参与在线判定。
func liveState(l *live.Live, n store.Node) (online bool, lastSeen *int64, m *heronv1.Metrics) {
	if e, ok := l.Get(n.ID); ok {
		return e.Online, proto.Int64(e.LastSeenWall.Unix()), e.Metrics
	}
	if !n.LastSeenAt.IsZero() {
		return false, proto.Int64(n.LastSeenAt.Unix()), nil
	}
	return false, nil, nil
}
