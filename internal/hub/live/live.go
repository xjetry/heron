// Package live 持有每个节点的实时状态：最新指标、last_seen 与未落盘的分钟桶。
//
// 不变式：在线 ⇔ now − last_seen < TTL，且这是在线的唯一来源——不存在第二张
// 在线表，也没有连接状态可以与它分叉。last_seen 用单调钟，墙钟回拨不影响。
package live

import (
	"sync"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/metric"
)

// Live 的 Observe 只在成功上报后更新 lastSeen；Online 用单调时钟与 ttl 比较，
// 库里的节点行或连接仍存在都不能单独使节点在线。心跳计数与管理快照共用这个判定。
type Live struct {
	mu          sync.Mutex
	clk         clock.Clock
	ttl         time.Duration
	nodes       map[int64]*entry
	coverage    map[int64]int64
	observation observation
}

type entry struct {
	metrics      *heronv1.Metrics
	lastSeen     time.Duration
	lastSeenWall time.Time
	// source 是最近一次上报的来源地址，与 lastSeenWall 在同一次 Observe 里更新，随刷出的行一起落盘。
	source string
	// buckets 按桶起始（墙钟 Unix 秒，60 对齐）索引。同一节点可以同时有多个
	// 未刷出的桶：墙钟回拨时新样本会落进更早的分钟，它们各自独立、刷出后
	// 由写库时的加法合并并入已有的行。
	buckets map[int64]*metric.Bucket
	probes  map[probeKey]*metric.ProbeBucket
}

type probeKey struct {
	ts   int64
	task uint64
}

type Entry struct {
	Metrics      *heronv1.Metrics
	LastSeen     time.Duration
	LastSeenWall time.Time
	Source       string
	Online       bool
}

func New(clk clock.Clock, ttl time.Duration) *Live {
	return &Live{clk: clk, ttl: ttl, nodes: map[int64]*entry{}, coverage: map[int64]int64{}}
}

func minuteOf(t time.Time) int64 {
	s := t.Unix()
	return s - s%60
}

// Observe 记录一次已通过校验的上报。source 是 hub 看到的来源地址（auth.SourceText），只留最后一次：与 last_seen 同为
// "最近一次上报"的事实，v4 与 v6 交替上报时面板看到的就是最近那一次。返回样本所属分钟桶的起始、距该节点上一次上报的
// 单调间隔，以及这是否是本进程里该节点的首次上报（first 为 true 时 gap 无意义）。调用方保证 m 不再被修改。
func (l *Live) Observe(nodeID int64, source string, m *heronv1.Metrics) (ts int64, gap time.Duration, first bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now, wall := l.clk.Mono(), l.clk.Now()
	ts = minuteOf(wall)
	if _, ok := l.coverage[nodeID]; !ok {
		l.coverage[nodeID] = ts
	}
	e := l.nodes[nodeID]
	if e == nil {
		e = &entry{buckets: map[int64]*metric.Bucket{}, probes: map[probeKey]*metric.ProbeBucket{}}
		l.nodes[nodeID] = e
		first = true
	} else {
		gap = now - e.lastSeen
	}
	e.metrics, e.lastSeen, e.lastSeenWall, e.source = m, now, wall, source
	l.bucket(e, ts).Add(m)
	return ts, gap, first
}

func (l *Live) bucket(e *entry, ts int64) *metric.Bucket {
	b := e.buckets[ts]
	if b == nil {
		b = metric.NewBucket()
		e.buckets[ts] = b
	}
	return b
}

// AddBytes 把一次上报算出的字节增量记进 ts 所在的分钟桶。ts 来自同一次上报的 Observe：
// 增量与该次上报的其余指标同桶，桶按上报到达的墙钟分钟归属。节点已被 Forget 时丢弃——
// Forget 之后不得再为该节点建任何内存状态。
func (l *Live) AddBytes(nodeID int64, ts int64, rx, tx int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.nodes[nodeID]
	if e == nil {
		return
	}
	b := l.bucket(e, ts)
	b.AddSum(metric.RxBytes, float64(rx))
	b.AddSum(metric.TxBytes, float64(tx))
}

// AddProbe 把一条结果折叠进测量时刻所在分钟的 (任务) 桶。at 由调用方按 收到时刻 − age_ms 算出，
// 所以同一次上报里的结果可以落进不同分钟；迟到结果所属的分钟若已刷出，会在这里开一个同键的
// 新桶，刷出后由写库的加法合并并入已有的行。节点已被 Forget 时丢弃——Forget 之后不得再建内存状态。
func (l *Live) AddProbe(nodeID int64, at time.Time, taskID uint64, r *heronv1.ProbeResult) {
	ts := minuteOf(at)
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.nodes[nodeID]
	if e == nil {
		return
	}
	k := probeKey{ts: ts, task: taskID}
	b := e.probes[k]
	if b == nil {
		b = &metric.ProbeBucket{}
		e.probes[k] = b
	}
	b.Add(r)
}

func (l *Live) online(e *entry, now time.Duration) bool {
	return now-e.lastSeen < l.ttl
}

func (l *Live) Online(nodeID int64) bool {
	now := l.clk.Mono()
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.nodes[nodeID]
	return ok && l.online(e, now)
}

func (l *Live) Get(nodeID int64) (Entry, bool) {
	now := l.clk.Mono()
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.nodes[nodeID]
	if !ok {
		return Entry{}, false
	}
	return Entry{Metrics: e.metrics, LastSeen: e.lastSeen, LastSeenWall: e.lastSeenWall, Source: e.source, Online: l.online(e, now)}, true
}

// Flush 取走所有已闭合的桶：起始早于当前分钟的。取走即从 live 删除——每个
// 桶至多被交给写协程一次，写库的加法合并才不会重复计入。
func (l *Live) Flush() metric.Batch {
	return l.take(false)
}

// Drain 取走全部桶，包括当前分钟仍开着的；退出时用。
func (l *Live) Drain() metric.Batch {
	return l.take(true)
}

func (l *Live) take(all bool) metric.Batch {
	l.mu.Lock()
	defer l.mu.Unlock()
	seal := l.readClock()
	before := minuteOf(seal.wall)
	observed := l.sealObservations(seal)
	if all {
		before = 1<<62 - 1
	}
	var batch metric.Batch
	for id, start := range l.coverage {
		for ts := range observed {
			if ts < start {
				continue
			}
			if e := l.nodes[id]; e != nil && e.buckets[ts] != nil {
				continue
			}
			batch.Rows = append(batch.Rows, metric.Row{NodeID: id, TS: ts, Bucket: metric.NewBucket(), ObservationOnly: true, Observed: true, CoverageStart: start})
		}
	}
	for id, e := range l.nodes {
		for ts, b := range e.buckets {
			if ts >= before {
				continue
			}
			batch.Rows = append(batch.Rows, metric.Row{NodeID: id, TS: ts, Bucket: b, LastSeen: e.lastSeenWall, Source: e.source,
				Observed: observed[ts] && l.coverage[id] <= ts, CoverageStart: l.coverage[id]})
			delete(e.buckets, ts)
		}
		for k, b := range e.probes {
			if k.ts >= before {
				continue
			}
			batch.Probes = append(batch.Probes, metric.ProbeRow{NodeID: id, TS: k.ts, TaskID: k.task, Bucket: b})
			delete(e.probes, k)
		}
	}
	return batch
}

// Forget 删除节点时调用；未刷出的桶随之丢弃，被删节点的历史无处可挂。
func (l *Live) Forget(nodeID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.nodes, nodeID)
	delete(l.coverage, nodeID)
}
