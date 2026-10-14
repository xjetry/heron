package api

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/xjetry/heron/internal/hub/ratelimit"
	"github.com/xjetry/heron/internal/hub/store"
)

// 历史查询按来源限并发：同一来源（公开端一个客户端来源、管理端一个调用方凭据）在飞的历史
// 查询至多 historyInFlightPerSource() 个，超出的最多等 historyAcquireWait 再执行，仍无空位返回
// ResourceExhausted。限流只限准入速率，封不住无界在飞积压：重查询零点几秒一个、来源满速
// 开环发送时，在飞数随"准入速率 × 单请求时长"无限增长。这里约束的是历史请求的数量：同一来源
// 至多这么多个历史请求同时在读库（节点准入也在持位后执行，见 history），一个来源挤不掉别的来源；
// 来源数本身不设上限。读连接的上界不在这里，由 store 里请求读用的两个读池各自的上限承载：预计扫描量超过
// 分界的大扫描走大扫描池，池的上限（GOMAXPROCS，至少 2）是大扫描连接数有界的主语，超出的在池里
// 排队；一个来源至多占它的四分之一（不足 4 核时至多一半）。见 store 的 readPoolFor 与 historyPoolSize。
const (
	// historyAcquireWait：空位腾出的时间尺度按 365d 重查询的量级取；等待期间不占读连接、
	// 不开读事务，只挂在通知通道上。
	historyAcquireWait = 5 * time.Second
)

// historyInFlightPerSource 是每来源同时在飞的历史查询上限：hub 可用 CPU 数的四分之一，至少 1。
// CPU 数取 GOMAXPROCS（Linux 上 Go 1.25 起默认按 cgroup 的 CPU 配额取值）。历史查询是 CPU 密集的
// 扫描，一个来源占用的 CPU 比例决定其他读者被拖慢多少。受控环境（GitHub 托管 runner，AMD EPYC 7763，
// 4 核，GOMAXPROCS=4）上用开环饱和来源测受保护读者的 p99（压着时比空闲时），门槛 2 倍：
//   - 上限 4：两轮六组都不通过，2.4–7.4 倍；
//   - 上限 2：三组都不通过，1.5–2.5 倍；
//   - 上限 1：三组都通过，1.1–1.6 倍。
//
// 同机闭环（365d 16 节点分块）k=1 时读者约 1.3 倍，k=2 时指标读者已到 2.2 倍。取 CPU 的四分之一，
// 4 核机器上就是测过并通过的 1。面板一页同时在飞的历史查询至多 2 个：对比页受 ProbeComparison 的
// MAX_IN_FLIGHT_CHUNKS 限制，节点详情两族各一个。上限为 1 的小机器上，这两个请求依次执行，后一个
// 至多等 historyAcquireWait。
func historyInFlightPerSource() int {
	return max(1, runtime.GOMAXPROCS(0)/4)
}

// historySource 给出这次历史查询按谁计数的来源键，口径与限流器一致：公开端是
// ratelimit.BySource 放行时记下的客户端来源（auth.SourceKey 归一化：IPv4 按地址、
// IPv6 按 /64）；管理端不经限流，来源是调用方凭据（API token 按编号、面板会话按
// 会话令牌）。都没有时退回单个键——两个服务的入口都在各自中间件之内，只是兜底。
func historySource(ctx context.Context) string {
	if src, ok := ratelimit.SourceOf(ctx); ok {
		return src.String()
	}
	if p, ok := store.Principal(ctx); ok {
		return fmt.Sprintf("api-token/%d", p.ID)
	}
	if tok, ok := ctx.Value(sessionKey{}).(string); ok && tok != "" {
		return "session/" + tok
	}
	return "unknown"
}

// historyGate 是历史查询的按来源信号量。状态有界：一个来源既无在飞也无等待时即从表中
// 删除，不随来源数增长。管理端与公开端各持一个实例：两端的来源键空间本就不同
// （凭据 vs 客户端地址），互不挤占。
type historyGate struct {
	mu      sync.Mutex
	sources map[string]*sourceInFlight
	// wait 是等待空位的上限、limit 是每来源在飞上限，newHistoryGate 取 historyAcquireWait 与
	// historyInFlightPerSource()；测试可以改（受控环境比较不同档）。
	wait  time.Duration
	limit int
}

// wait 只计此刻挂在 notify 上的等待者：每个等待者醒来（不论因为通知、超时还是取消）都先把自己
// 减掉，再决定拿空位还是返回。回收与唤醒都读它——醒来拿到空位的等待者若不减，wait 永久多一，
// 这个来源就再也回收不掉，状态随出现过争用的来源数增长。
type sourceInFlight struct {
	inUse  int
	wait   int
	notify chan struct{} // 关闭即广播“有空位”，等待者醒来重新排队
}

func newHistoryGate() *historyGate {
	return &historyGate{sources: make(map[string]*sourceInFlight), wait: historyAcquireWait, limit: historyInFlightPerSource()}
}

// acquire 占一个空位；返回的 release 幂等，调用方 defer 它即可覆盖错误与 panic 路径。
// 等待期间不占任何读连接。超时返回的 ResourceExhausted 文案与限流的
// “rate limit exceeded …”区分开：这是并发在飞过多，稍后重试即可。
func (g *historyGate) acquire(ctx context.Context, source string) (release func(), err error) {
	timer := time.NewTimer(g.wait)
	defer timer.Stop()
	g.mu.Lock()
	e := g.sources[source]
	if e == nil {
		e = &sourceInFlight{notify: make(chan struct{})}
		g.sources[source] = e
	}
	for {
		if e.inUse < g.limit {
			e.inUse++
			g.mu.Unlock()
			return g.releaseOf(source, e), nil
		}
		e.wait++
		ch := e.notify
		g.mu.Unlock()
		gaveUp := false
		select {
		case <-ch:
		case <-ctx.Done():
			gaveUp = true
		case <-timer.C:
			gaveUp = true
		}
		g.mu.Lock()
		e.wait--
		if gaveUp {
			g.dropIfIdle(source, e)
			g.mu.Unlock()
			return nil, historyBusy(g.limit)
		}
	}
}

func historyBusy(limit int) error {
	return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf(
		"too many concurrent history queries from this source: at most %d may be in flight; retry shortly",
		limit))
}

func (g *historyGate) releaseOf(source string, e *sourceInFlight) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			e.inUse--
			if e.inUse == 0 && e.wait == 0 {
				// 无在飞也无等待：删掉，状态不随来源数增长。
				if cur, ok := g.sources[source]; ok && cur == e {
					delete(g.sources, source)
				}
				return
			}
			if e.wait > 0 {
				// 唤醒等待者：换一条新通知通道再关旧的，醒来的重新排队拿空位。
				ch := e.notify
				e.notify = make(chan struct{})
				close(ch)
			}
		})
	}
}

func (g *historyGate) dropIfIdle(source string, e *sourceInFlight) {
	if e.inUse == 0 && e.wait == 0 {
		if cur, ok := g.sources[source]; ok && cur == e {
			delete(g.sources, source)
		}
	}
}
