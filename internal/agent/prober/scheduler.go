package prober

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/probelimit"
	"google.golang.org/protobuf/proto"
)

type Scheduler struct {
	engine Engine
	queue  *Queue
	clk    clock.Clock
	log    *slog.Logger
	Sleep  func(context.Context, time.Duration) error
	Rand   func() float64
	mu     sync.Mutex
	// digest 是最近一次收到的清单的对账摘要（agentwire.TasksDigest），hasDigest 区分
	// "尚未收到任何清单"与"收到空清单"：前者上报时不带摘要，后者带空串的 SHA-256。
	// digestErr 记录上一次摘要计算失败，用于状态翻转时记日志而不是每次 Apply 都记。
	hasDigest bool
	digest    []byte
	digestErr bool
	running   map[uint64]*runningTask
	wg        sync.WaitGroup
}

type runningTask struct {
	task   *heronv1.ProbeTask
	cancel context.CancelFunc
}

func NewScheduler(engine Engine, queue *Queue, clk clock.Clock, log *slog.Logger) *Scheduler {
	return &Scheduler{engine: engine, queue: queue, clk: clk, log: log, Rand: rand.Float64, Sleep: sleep, running: map[uint64]*runningTask{}}
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// TasksDigest 返回当前持有清单的对账摘要（agentwire.TasksDigest）；尚未收到任何清单时返回 nil，
// 上报据此省略该字段——空清单的摘要是空串的 SHA-256，与缺席严格区分。
func (s *Scheduler) TasksDigest() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasDigest {
		return nil
	}
	return slices.Clone(s.digest)
}

// Apply 整份替换任务集；未变的任务不重启计时。字段规则由 CheckTask 保证，数量由本入口限制，
// hub 保存时用同一份 probelimit 校验字段，store 在事务内守住每节点数量上限；同版本正常部署不会下发被拒清单。
// 每个被拒任务留一条 error 结果（结果队列有容量上限，面板靠它显示原因），日志每次 Apply 至多一行汇总：清单来自 hub，
// 失守的 hub 可以在一个 64 KiB 响应里塞进数万个空任务并每次上报都重发，逐条告警会把受限的响应放大成无界的日志。
func (s *Scheduler) Apply(tasks *heronv1.ProbeTasks) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 摘要代表收到并持有的整份清单：被 CheckTask 拒绝的任务也在其中，hub 发出的就是这份，二者才可比。
	// 编码失败时摘要缺席（hub 会重发完整清单），失败是异常路径，只在状态翻转时记一行日志。
	d, err := agentwire.TasksDigest(tasks.GetTasks())
	if err != nil {
		if !s.digestErr {
			s.log.Warn("tasks digest unavailable; reports omit it", "err", err)
		}
		s.digestErr = true
		s.digest, s.hasDigest = nil, false
	} else {
		if s.digestErr {
			s.log.Info("tasks digest recovered")
		}
		s.digestErr = false
		s.digest, s.hasDigest = d, true
	}
	want := map[uint64]*heronv1.ProbeTask{}
	sorted := slices.SortedFunc(slices.Values(tasks.GetTasks()), func(a, b *heronv1.ProbeTask) int { return cmp.Compare(a.GetId(), b.GetId()) })
	rejected := 0
	var firstID uint64
	var firstWhy string
	reject := func(t *heronv1.ProbeTask, why string) {
		s.queue.Push(Result{TaskID: t.GetId(), Outcome: Outcome{Err: why}, At: s.clk.Mono(), ConfigID: t.GetConfigId()})
		if rejected == 0 {
			firstID, firstWhy = t.GetId(), why
		}
		rejected++
	}
	for i, t := range sorted {
		if i >= probelimit.MaxTasksPerNode {
			reject(t, fmt.Sprintf("more than %d tasks assigned; task dropped", probelimit.MaxTasksPerNode))
			continue
		}
		if err := probelimit.CheckTask(t); err != nil {
			reject(t, err.Error())
			continue
		}
		// 调度器持有独立快照，调用方复用消息不能绕过 Apply 改变正在执行的任务。
		want[t.GetId()] = proto.Clone(t).(*heronv1.ProbeTask)
	}
	if rejected > 0 {
		s.log.Warn("probe tasks rejected", "count", rejected, "first_task", firstID, "first_reason", firstWhy)
	}
	for id, r := range s.running {
		if t, ok := want[id]; !ok || !proto.Equal(t, r.task) {
			r.cancel()
			delete(s.running, id)
		}
	}
	// 登记先于启动：引擎侧的登记簿（如 HTTP 的证书限频身份）在第一个探测 goroutine
	// 起跑之前对齐当前清单，于是调度器管理下的每次探测都对应一份已登记的身份——
	// 登记簿查不到的探测一定来自刚被取消的旧任务，引擎据此拒绝为它携带观测或重建键。
	// 被拒任务不在 want 里，其记录一并清掉；换了身份的旧记录同样删除，任务消失再出现时按首次探测处理。
	if p, ok := s.engine.(taskPruner); ok {
		alive := make(map[uint64]string, len(want))
		for id, t := range want {
			alive[id] = string(t.GetConfigId())
		}
		p.pruneTasks(alive)
	}
	for id, t := range want {
		if _, ok := s.running[id]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.running[id] = &runningTask{task: t, cancel: cancel}
		s.wg.Add(1)
		go s.run(ctx, t)
	}
}

// 首次偏移避免齐发；后续周期从触发时刻计算，不把探测耗时累加到周期。
// 超时预算不超过间隔由 probelimit 的范围与 MinIntervalS*1000 ≥ MaxTimeoutMs 的编译期断言保证。
// 单任务循环串行调用引擎，停止后不发布半途结果。
func (s *Scheduler) run(ctx context.Context, t *heronv1.ProbeTask) {
	defer s.wg.Done()
	interval := time.Duration(t.GetIntervalS()) * time.Second
	if err := s.Sleep(ctx, time.Duration(s.Rand()*float64(interval))); err != nil {
		return
	}
	for {
		next := s.clk.Mono() + interval
		out := s.engine.Probe(ctx, t)
		if ctx.Err() != nil {
			return
		}
		// 结果原样回显产生它时的任务身份：任务之后改了内容也不改写这条结果的身份。
		s.queue.Push(Result{TaskID: t.GetId(), Outcome: out, At: s.clk.Mono(), ConfigID: t.GetConfigId()})
		if err := s.Sleep(ctx, max(next-s.clk.Mono(), 0)); err != nil {
			return
		}
	}
}

// Stop 等待全部任务退出；调用方在此之后不得再 Apply。
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.running {
		r.cancel()
	}
	clear(s.running)
	s.wg.Wait()
}
