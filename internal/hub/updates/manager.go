// Package updates 管理已持久化的节点更新授权；上报路径只读写内存。
package updates

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/update"
	"google.golang.org/protobuf/proto"
)

type Manager struct {
	st  *store.Store
	clk clock.Clock
	log *slog.Logger
	op  sync.Mutex
	mu  sync.Mutex
	// states 是 hub 合并后的每节点状态：Task 归 hub（Start、Cancel 与 flush 的推进写它），其余字段是 agent 原报的
	// 最后已知值。它持久化，hub 重启后由 Load 读回，Start 与面板读的都是它。
	states map[int64]*heronv1.UpdateStatus
	// observed 是自上次 flush 以来确认的触达（每节点最后一份原报），flush 据它合并 states、推进任务。
	observed map[int64]*heronv1.UpdateStatus
	// reported 是每节点 agent 最近一次带全文的上报：全文与 agent 给的摘要，只在内存里。它与 states 分开存放——
	// states 的 Task 可被 Start、Cancel 改写，拿它充当原报会让摘要对上一份 agent 从没报过的状态。hub 重启即丢失，
	// 此后第一次只带摘要的上报得到 want_update。存进 reported 与 observed 的消息只读不改。
	reported map[int64]original
	wake     chan struct{}
	// boundAgent 是 hub 绑定的 agent 版本（规则见 Makefile 的 AGENT_VERSION 注释），节点在线更新唯一可用的目标；New 之后不变。
	// API 下发的 bound_agent_version 也读它（BoundAgent），绑定只有这一个持有者。
	boundAgent string
}

// original 是 agent 原报的全文与摘要；Observe 只在两者齐全时存入。
type original struct {
	status *heronv1.UpdateStatus
	hash   uint64
}

// New 装配 Manager。boundAgent 是 hub 绑定的 agent 版本：hub 的 release 不携带 agent 产物，
// 节点只能更新到构建时从仓库根 AGENT_VERSION 注入的这个版本。
func New(st *store.Store, clk clock.Clock, log *slog.Logger, boundAgent string) *Manager {
	return &Manager{st: st, clk: clk, log: log, states: make(map[int64]*heronv1.UpdateStatus), observed: make(map[int64]*heronv1.UpdateStatus), reported: make(map[int64]original), wake: make(chan struct{}, 1), boundAgent: boundAgent}
}

// BoundAgent 返回 hub 绑定的 agent 版本，空串表示没有绑定。
func (m *Manager) BoundAgent() string {
	return m.boundAgent
}
func (m *Manager) Load(ctx context.Context) error {
	states, err := m.st.NodeUpdates(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.states = states
	m.mu.Unlock()
	return nil
}
func clone(s *heronv1.UpdateStatus) *heronv1.UpdateStatus {
	if s == nil {
		return &heronv1.UpdateStatus{Reason: "agent has not reported online update support"}
	}
	return proto.Clone(s).(*heronv1.UpdateStatus)
}
func (m *Manager) Snapshot(id int64) *heronv1.UpdateStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return clone(m.states[id])
}

// ActiveTasks 返回仍在进行中的节点任务（ID → 版本）。中转缓存据此按引用释放：版本不再被任何进行中的任务
// 引用即可丢弃；任务的取用计数也只在任务进行中才有意义。
func (m *Manager) ActiveTasks() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string)
	for _, s := range m.states {
		if t := s.GetTask(); t != nil && update.ActiveState(t.State) {
			out[t.Id] = t.Version
		}
	}
	return out
}
func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Observe 只在 Report 鉴权后调用，是上报进入更新簿记的唯一入口。s 是请求的 UpdateStatus 全文，hash 是 update_hash，
// nil 即缺席。没有摘要时不采信全文，记为未报告在线更新支持并清除原报；
// 带摘要和全文时保存原报；只带相同摘要时复用原报确认触达；未知或不同摘要时索要全文，不更新状态。
//
// 返回的任务与上面四种情形无关：任务必须先以 dispatched 落盘，才能进入下行响应。
func (m *Manager) Observe(id int64, s *heronv1.UpdateStatus, hash *uint64) (task *heronv1.UpdateTask, want bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch prev, ok := m.reported[id]; {
	case hash != nil && s != nil:
		o := original{status: clone(s), hash: *hash}
		m.reported[id] = o
		m.observed[id] = o.status
		m.signal()
	case hash != nil && ok && prev.hash == *hash:
		m.observed[id] = prev.status
		m.signal()
	case hash != nil:
		want = true
	default:
		delete(m.reported, id)
		m.observed[id] = clone(nil)
		m.signal()
	}
	t := m.states[id].GetTask()
	if t != nil && t.State == "dispatched" && t.ExpiresAt > m.clk.Now().Unix() {
		return proto.Clone(t).(*heronv1.UpdateTask), want
	}
	return nil, want
}

func NewRequest(version string, now time.Time) update.Request {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return update.Request{ID: hex.EncodeToString(b[:]), Version: version, ExpiresAt: now.Add(24 * time.Hour).Unix()}
}

// Start 校验目标后把节点排队进入更新。绑定检查先于一切：hub 只发与自己一起构建的 agent 版本，
// 没有稳定绑定的 hub 拒绝所有节点在线更新。
func (m *Manager) Start(ctx context.Context, id int64, version string) (*heronv1.UpdateTask, error) {
	// 节点只能更新到 hub 绑定的 agent 版本：只发 hub 的 release 不带 agent 产物，别的版本号在官方
	// release 里不一定有 agent 包，也没有与这个 hub 一起跑过端到端。绑定为空（没有注入的构建）或不是正式版时，
	// 节点在线更新一律不可用——更新器只接受正式版（update.ValidVersion），空值在这里是收紧。
	if !update.ValidVersion(m.boundAgent) {
		return nil, errors.New("this hub has no stable bound agent version; node online updates are unavailable")
	}
	if version != m.boundAgent {
		return nil, fmt.Errorf("node updates must target this hub's bound agent version %s, got %s", m.boundAgent, version)
	}
	m.op.Lock()
	defer m.op.Unlock()
	s := m.Snapshot(id)
	if !s.Supported {
		return nil, errors.New("node does not support online updates: " + s.Reason)
	}
	if !update.Newer(version, s.Version) {
		return nil, errors.New("target must be a newer stable version")
	}
	if t := s.Task; t != nil && update.ActiveState(t.State) {
		return nil, errors.New("node already has an active update")
	}
	r := NewRequest(version, m.clk.Now())
	s.Task = update.TaskProto(&update.Job{Request: r, State: "queued", UpdatedAt: m.clk.Now().Unix()})
	if err := m.st.StartNodeUpdate(ctx, id, s); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.states[id] = s
	m.mu.Unlock()
	return proto.Clone(s.Task).(*heronv1.UpdateTask), nil
}

func (m *Manager) Cancel(ctx context.Context, id int64, taskID string) error {
	m.op.Lock()
	defer m.op.Unlock()
	s := m.Snapshot(id)
	if s.Task == nil || s.Task.Id != taskID || s.Task.State != "queued" {
		return errors.New("only the matching queued update can be cancelled")
	}
	s.Task.State = "cancelled"
	s.Task.UpdatedAt = m.clk.Now().Unix()
	if err := m.st.CancelNodeUpdate(ctx, id, s); err != nil {
		return err
	}
	m.mu.Lock()
	m.states[id] = s
	m.mu.Unlock()
	return nil
}

func (m *Manager) Forget(id int64) {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	delete(m.states, id)
	delete(m.observed, id)
	delete(m.reported, id)
	m.mu.Unlock()
}

func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-t.C:
		}
		m.flush(ctx)
	}
}

func (m *Manager) flush(ctx context.Context) {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	observed := m.observed
	m.observed = make(map[int64]*heronv1.UpdateStatus)
	states := make(map[int64]*heronv1.UpdateStatus, len(m.states))
	for id, s := range m.states {
		states[id] = clone(s)
	}
	m.mu.Unlock()
	for id := range observed {
		if states[id] == nil {
			states[id] = clone(nil)
		}
	}
	for id, s := range states {
		before := proto.Clone(s)
		o, seen := observed[id]
		if seen {
			// Task 之外的字段都是 agent 原报，整体取自本次触达：逐个登记字段会让后加进 UpdateStatus 的字段停在
			// 第一次落库的值或永远为空，面板与 GetUpdates 读到的就不是 agent 报的。Task 归 hub，原报里的任务只用来推进它。
			own := s.Task
			proto.Reset(s)
			proto.Merge(s, o)
			s.Task = own
			if task, reported := s.Task, o.Task; task != nil && reported != nil && task.Id == reported.Id && task.Version == reported.Version && (update.ActiveState(task.State) || task.State == "unconfirmed") && task.State != "queued" {
				// 旧进程或不相干任务不能凭结果字符串宣告新程序已上报。
				if reported.State != "succeeded" || o.Version == task.Version {
					if advances(task.State, reported.State) && (task.State != reported.State || task.Error != reported.Error) {
						task.State = reported.State
						task.Error = reported.Error
						task.UpdatedAt = m.clk.Now().Unix()
					}
				}
			}
		}
		if task := s.Task; seen && Superseded(task, s.Version, update.ActiveState(task.GetState()) && o.Task.GetId() == task.GetId()) {
			// 本次上报的运行版本已达到任务目标，而这条任务不是成功记录、本机更新器也没在执行它：它不再描述节点的
			// 更新状态。清除而不是改判——改成成功是冒称，留着失败会让已在目标版本的节点一直挂着旧错误，
			// 留着进行中会永久占用更新入口。只在收到上报的这一轮判定，运行版本取自同一次上报。
			s.Task = nil
		}
		if task := s.Task; task != nil {
			if update.ActiveState(task.State) && task.ExpiresAt <= m.clk.Now().Unix() {
				// 下发后失联不能推断执行结果；本机更新器仍负责互斥与恢复，后台只结束等待。
				if task.State == "queued" {
					task.State = "expired"
				} else {
					task.State = "unconfirmed"
				}
				task.UpdatedAt = m.clk.Now().Unix()
			} else if task.State == "queued" && seen && s.Supported {
				task.State = "dispatched"
				task.UpdatedAt = m.clk.Now().Unix()
			}
		}
		if proto.Equal(before, s) {
			continue
		}
		if err := m.st.SaveNodeUpdate(ctx, id, s); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// 节点已删除。ingest.Service.Forget 在等在途上报退出之前就调了 Forget，迟到的上报会在那之后留下原报；
				// 写库在事务里裁决节点不存在，这里是能看到它的地方，丢掉它，免得它随已删节点的 ID 一直留在内存里。
				m.mu.Lock()
				delete(m.reported, id)
				m.mu.Unlock()
				continue
			}
			m.log.Error("persist node update", "node", id, "err", err)
			if seen {
				m.mu.Lock()
				if _, newer := m.observed[id]; !newer {
					m.observed[id] = o
				}
				m.mu.Unlock()
			}
			continue
		}
		m.mu.Lock()
		m.states[id] = s
		m.mu.Unlock()
	}
}

// 上报可跨断线重试；同一任务的旧进度不能把已确认的后续阶段回退。
func advances(current, next string) bool {
	switch next {
	case "succeeded", "failed", "rolled_back", "expired":
		return true
	}
	if current == "unconfirmed" {
		return false
	}
	stages := map[string]int{"dispatched": 1, "downloading": 2, "stopping": 3, "installing": 4, "verifying": 5, "rolling_back": 6}
	return stages[next] != 0 && stages[next] >= stages[current]
}

// Superseded 判定一条任务记录是否已不再描述目标机器的更新状态（节点状态校正与 hub 自身状态的投影共用）：运行版本已达到（或超过）任务目标，
// 而这条任务既不是成功记录、也不在本机更新器的执行中。失败、回滚、过期、取消、结果未确认的任务，
// 以及本机更新器没有记录的进行中任务，都可能在节点经重装或手动升级到达目标后仍挂着旧错误；
// 留着它们只会让面板对已在目标版本的机器显示失败。成功记录保留，它说明目标是怎样到达的；
// executing 为真表示本机更新器正在执行这条任务（新进程已运行目标版本但仍在验证阶段），不能提前清除。
// 它是 hub 侧对任务记录的展示与占用策略，不属于 agent 与 hub 之间的协议，所以放在只有 hub 构建的包里：
// 放进 agent 共用的 internal/update 会改变 agent 的构建输入，使只改 hub 的修正被迫随 agent 一起发布。
// 两边版本都必须是可比较的正式版：Newer 对非法版本恒返回 false，取反会把开发版或空版本误判为已达到目标。
func Superseded(task *heronv1.UpdateTask, running string, executing bool) bool {
	if task == nil || task.State == "succeeded" || executing {
		return false
	}
	return update.ValidVersion(task.Version) && update.ValidVersion(running) && !update.Newer(task.Version, running)
}
