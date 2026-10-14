package client

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/agent/collect"
	"github.com/xjetry/heron/internal/agent/prober"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/probelimit"
)

// Runner 的上报重试遵守退避上限；服务端间隔经 agentwire.ClampReportInterval 校验，
// 避免失守配置把恢复请求拖过在线 TTL。Run 检测休眠后清空采集速率基线和探测结果队列，
// 因为跨休眠的时长不能归入一次有效采样或一次仍及时的探测结果。
type Runner struct {
	Collector *collect.Collector
	Client    heronv1connect.AgentServiceClient
	Token     string
	Clock     clock.Clock
	Sleep     func(context.Context, time.Duration) error
	Rand      func() float64
	Log       *slog.Logger
	// Prober 必填，持有与 hub 对账的完整清单及摘要。
	Prober *prober.Scheduler
	// Results 必填，与 Prober 共用同一个结果队列。
	Results *prober.Queue
	// Interval 是收到第一个响应之前使用的间隔；之后用 hub 下发的，经 agentwire.ClampReportInterval 限定。
	Interval time.Duration
	// Network 是后台的出口探测：每轮读它的结果进 Facts，每次成功应答后把 hub 给的逐族开关（detection）交给它。
	// 为空时不做出口探测，保持采集循环无额外网络依赖。
	Network interface {
		Snapshot() *heronv1.NetworkInfo
		Skip(*heronv1.NetworkDetection)
	}
	Updates interface {
		// Ready 关闭即本机更新状态的第一轮查询完成，此前 Snapshot 为 nil；构造第一份上报前必须等它（见 Run）。
		Ready() <-chan struct{}
		Snapshot() *heronv1.UpdateStatus
		Reported(*heronv1.UpdateTask)
	}
}

func sleepReal(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// 休眠信号的阈值必须小于结果的迟到预算：跨越休眠 S 的结果真龄比单调钟年龄大 S，
// S ≥ MaxResultAge 时它们必然超龄应弃；阈值更小保证这类休眠必然被判出、在下一轮
// 循环开头（先于取队列上报）整体作废，不会先被当作合法结果上报。Runner 同时引用
// clock.Drift 与 probelimit.MaxResultAge，断言放这里读者才看得见前提由谁保证；
// 一轮长度上限内两钟正常偏差的推导见 clock.MaxClockJump 的注释（间隔上限
// agentwire.MaxTTL/agentwire.ReportsPerTTL，退避封顶 agentwire.ReportsPerTTL 倍）。
// 差值以纳秒计（约 1e11），转 uint64 而不是 uint：32 位架构（armv7、386）的 uint 放不下，会把成立的不等式也判成溢出。
const _ = uint64(probelimit.MaxResultAge - clock.MaxClockJump - 1)

// omitDerivedNetTotals 去掉能由列表推导的合计与摘要。hub 用同一列表推导，重复携带没有意义。
// 空列表与缺席编码相同；列表缺席时无法推导，合计必须由 metrics 自己携带。
func omitDerivedNetTotals(m *heronv1.Metrics, list []*heronv1.NetInterfaceCounter) {
	if len(list) == 0 {
		return
	}
	m.NetRxTotal, m.NetTxTotal, m.NetCounterEpoch = nil, nil, ""
}

// Run 每次携带 facts 与任务清单摘要供 hub 对账，间隔以响应为准。
// 失败退避、成功即回到下发间隔；实时指标不缓存，因为过期的实时数据没有意义。
// 探测结果在迟到预算内重试，InvalidArgument 例外：本批丢弃而不回队。
func (r *Runner) Run(ctx context.Context) error {
	if err := r.Collector.Validate(); err != nil {
		return err
	}
	if r.Prober == nil {
		return errors.New("Runner.Prober: required")
	}
	if r.Results == nil {
		return errors.New("Runner.Results: required")
	}
	if r.Sleep == nil {
		r.Sleep = sleepReal
	}
	if r.Rand == nil {
		r.Rand = rand.Float64
	}
	interval := r.Interval
	// 首报必须携带刷新后的真实更新状态而不是缺席或占位：hub 按摘要对账，首报缺席会让下一次上报
	// 多走一轮 want_update。等待的上限由 update 包客户端的单次往返超时承载（protocol.go 里 http.Client 的
	// Timeout，3 s）：没有本机更新器时连接立即失败、立即返回，首报不会被无限推迟。Updates 为 nil 时不等。
	if r.Updates != nil {
		select {
		case <-r.Updates.Ready():
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// sendFacts、sendUpdate 是上一次成功应答的索要（进程启动后的首次上报两者都带）；失败的上报不改它们，重试保持
	// 上一轮的决定。
	sendFacts, sendUpdate := true, true
	attempt := 0
	var dropped uint64
	var clamped bool
	var clampedMs uint32
	var drift clock.Drift
	for {
		// 休眠信号先于 Metrics 判定：触发时本轮采样就是新基线的首样本，
		// 跨越休眠的积压结果在取队列上报之前整体作废（规则见 Runner）。
		wall, mono := r.Clock.Now(), r.Clock.Mono()
		if wallDelta, monoDelta, jumped := drift.Observe(wall, mono); jumped {
			r.Results.Clear()
			r.Collector.ResetRates()
			r.Log.Warn("clock jump: discarded queued probe results and reset rate baselines", "wall_delta", wallDelta, "mono_delta", monoDelta)
		}
		// 识别一次，交给 Metrics 与 Facts：同一周期的范围与容量一致。
		snap := r.Collector.Identify()
		m, err := r.Collector.Metrics(snap)
		if err != nil {
			r.Log.Warn("partial collection", "err", err)
		}
		list := r.Collector.NetInterfaces()
		omitDerivedNetTotals(m, list)
		req := connect.NewRequest(&heronv1.ReportRequest{Metrics: m, NetInterfaces: list})
		if r.Updates != nil {
			if u := r.Updates.Snapshot(); u != nil {
				h := UpdateHash(u)
				req.Msg.UpdateHash = &h
				if sendUpdate {
					req.Msg.Update = u
				}
			}
		}
		// 超龄过滤与 age_ms 必须取同一时刻，否则刚通过过滤的结果可能以大于 MaxResultAge 的年龄发出并被 hub 丢弃；
		// 循环开头已读出的 mono 就是这个时刻，重复读取会让两次取值落在不同瞬间。
		now := mono
		taken := r.Results.Take(now, probelimit.MaxResultAge, probelimit.MaxResultsPerReport)
		req.Msg.ProbeResults = prober.ToProto(taken, now)
		// 摘要在尚未收到任何清单时为 nil（缺席），与空清单的空串 SHA-256 严格区分。
		req.Msg.TasksDigest = r.Prober.TasksDigest()
		req.Header().Set("Authorization", "Bearer "+r.Token)
		// Facts 只读几个小文件；每轮重算才能让 hub 从摘要变化发现运行期间的变更。
		f := r.Collector.Facts(snap)
		if f.Diagnostics != nil {
			f.Diagnostics.ReportIntervalMs = uint32(interval / time.Millisecond)
		}
		if r.Network != nil {
			f.Network = r.Network.Snapshot()
		}
		hash := FactsHash(f)
		if sendFacts {
			req.Msg.Facts = f
		}
		req.Msg.FactsHash = hash

		resp, err := r.Client.Report(ctx, req)
		if err != nil {
			// InvalidArgument 可来自 hub 对结构非法探测结果的拒绝、validateMetrics/validateFacts 对非法主机数据的拒绝，
			// 或 Connect 客户端解码/解压响应失败。前两种对同一内容的拒绝是确定性的：
			// 坏结果重发仍失败，非法主机数据持续时本批也只会在迟到预算内反复被拒。
			// hub 正常完成 Report 后的响应若解码/解压失败，本批已折叠，回队会重复入账。
			// 其他失败保留本批结果，下次 Take 丢弃超过迟到预算的部分。
			if connect.CodeOf(err) == connect.CodeInvalidArgument {
				if len(taken) > 0 {
					r.Log.Warn("discarding rejected probe results", "count", len(taken), "err", err)
				}
			} else {
				r.Results.Requeue(taken)
			}
		}
		if err == nil && resp.Msg.Tasks != nil {
			r.Prober.Apply(resp.Msg.Tasks)
		}
		if err == nil && r.Updates != nil {
			r.Updates.Reported(resp.Msg.Update)
		}
		// 每次成功应答都交出去，缺失也交（nil 即两族照常探测）：hub 清空手填后的应答不再要求停用，agent 据此恢复。
		if err == nil && r.Network != nil {
			r.Network.Skip(resp.Msg.GetDetection())
		}
		if total := r.Results.Dropped(); total > dropped {
			r.Log.Warn("probe results dropped", "dropped", total-dropped)
			dropped = total
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			attempt++
			d := Backoff(attempt, interval, r.Rand)
			r.Log.Warn("report failed", "err", err, "attempt", attempt, "retry_in", d)
			if err := r.Sleep(ctx, d); err != nil {
				return err
			}
			continue
		}
		attempt = 0
		sendFacts = resp.Msg.WantFacts
		sendUpdate = resp.Msg.WantUpdate
		// 间隔来自 hub，而 hub 可能失守：越界值取边界，同一个越界值只告警一次，不随每次上报刷屏。
		ms := resp.Msg.ReportIntervalMs
		d, ok := agentwire.ClampReportInterval(ms)
		if !ok && (!clamped || ms != clampedMs) {
			r.Log.Warn("hub assigned a report interval outside the protocol bounds; using the nearest bound", "assigned_ms", ms, "using", d)
		}
		clamped, clampedMs = !ok, ms
		interval = d
		if err := r.Sleep(ctx, interval); err != nil {
			return err
		}
	}
}
