// Package nodeops 编排节点生命周期：建、改、删、轮换 token、批量改标签，以及删除之后清理进程内状态。
//
// 一个节点的状态分在几处：库（节点行、标签、计费、token hash），auth 的 token 映射，探测任务注册表的覆盖索引，
// 告警引擎的规则作用域与状态，流量账本的重置日与累计，ingest 名下的在线快照、待刷出分钟桶、更新状态与 facts 摘要。
// 每个协作者各自保证"先提交库、成功后才改自己的内存"；本包负责的是跨协作者的那部分：哪些写要串行、哪些
// 内存更新必须跟在哪次提交之后、哪些慢操作不能挡住别的节点。
//
// mu（节点编辑锁）的不变式：流量账本里的重置日与库里的 traffic_reset_day 一致，ingest 的逐族探测开关与库里的手填
// 出口地址一致，节点删除并 Forget 之后不再为它建内存状态。Update 在 mu 下完成"库提交 → SetResetDay、SetAddressPins
// → 流量 Commit"，Delete 在 mu 下提交库删除；于是
//   - 两个 Update 的"提交、改内存"不交错，内存里的重置日是最后提交的那一次；
//   - Delete 提交之后才开始的 Update 在库层得到 ErrNotFound，不会调 SetResetDay，锁外的 Forget 不会被它重建；
//   - Delete 提交之前已提交的 Update 在放锁前已改完内存，Forget 在 Delete 放锁之后才运行，排在它后面。
//
// mu 之下会去取的别包的锁与不成环的依据见 internal/hub 的包注释（doc.go）。
//
// Create 与 RotateToken 不取 mu：二者都不写流量账本，与 mu 守护的不变式无关；token 映射与库的一致由 auth.mutMu 保证。
//
// 慢操作都在 mu 之外：到期扫描与流量评估要等 alert.writeMu（离线巡检、探测评估、日界扫描都可能正持有）再做一整轮
// 写回；Forget 要等在途上报与评估退出。持 mu 等它们只会挡住其它节点的编辑与删除。
package nodeops

import (
	"context"
	"log/slog"
	"sync"

	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
)

// Credentials 是节点 token 映射的持有者，实现是 auth.Auth：它在 mutMu 下先写库、成功后改映射。
type Credentials interface {
	CreateNode(ctx context.Context, spec store.NewNode) (int64, string, error)
	RotateToken(ctx context.Context, id int64) (string, error)
	DeleteNode(ctx context.Context, id int64) error
}

// NodeWriter 是节点可编辑字段与标签的写入口，实现是 probe.Registry：标签变化改变任务覆盖，提交与覆盖发布由它串行。
type NodeWriter interface {
	UpdateNode(ctx context.Context, id int64, edit store.NodeEdit) (store.NodeUpdateResult, error)
	BatchUpdateNodeTags(ctx context.Context, ids []int64, add, remove []string) (store.NodeUpdateResult, error)
}

// Alerts 是告警引擎，实现是 alert.Engine。UpdateScope 在 writeMu 下执行 mutate 并按其结果刷新规则与静默的作用域，
// 返回计费字段是否变化；SweepExpiry、EvaluateTrafficNode、Forget 都取 writeMu。
type Alerts interface {
	UpdateScope(mutate func() (store.NodeUpdateResult, error)) (bool, error)
	SweepExpiry(ctx context.Context) error
	EvaluateTrafficNode(ctx context.Context, nodeID int64) error
	Forget(nodeID int64)
}

// Traffic 是流量账本，实现是 traffic.Book。
type Traffic interface {
	SetResetDay(nodeID int64, day int)
	Commit(ctx context.Context, nodeID int64) (traffic.Entry, error)
}

// NodeState 是上报侧的进程内状态持有者，实现是 ingest.Service。它的 Forget 是上报侧节点状态的唯一清单：
// 任务注册表的覆盖索引、更新状态、在线快照、流量账本、限流桶、facts 与任务摘要、逐族探测开关、待刷出的分钟桶。
// SetAddressPins 发布已提交的手填出口地址，Update 在 mu 下、库提交成功之后调用，与 SetResetDay 同一口径。
type NodeState interface {
	SetAddressPins(nodeID int64, ipv4Pin, ipv6Pin string)
	Forget(nodeID int64)
}

// Deps 是 Service 的协作者，全部必需：New 逐字段核对非 nil。字段都是接口，只能识别"没有给出"：装进接口的 nil
// 指针（typed nil）不等于 nil，会通过核对、到第一次调用才空指针，装配方不得这样传。
type Deps struct {
	Credentials Credentials
	Nodes       NodeWriter
	Alerts      Alerts
	Traffic     Traffic
	State       NodeState
	Log         *slog.Logger
}

// Service 的不变式与锁序见包注释。
type Service struct {
	mu sync.Mutex

	creds   Credentials
	nodes   NodeWriter
	alerts  Alerts
	traffic Traffic
	state   NodeState
	log     *slog.Logger
}

// New 对缺失的依赖 panic，口径与 api.New 相同：装配只在 serve 与测试夹具里，缺依赖只能是装配代码写错了。
func New(deps Deps) *Service {
	if deps.Credentials == nil {
		panic("nodeops.Deps.Credentials must be set")
	}
	if deps.Nodes == nil {
		panic("nodeops.Deps.Nodes must be set")
	}
	if deps.Alerts == nil {
		panic("nodeops.Deps.Alerts must be set")
	}
	if deps.Traffic == nil {
		panic("nodeops.Deps.Traffic must be set")
	}
	if deps.State == nil {
		panic("nodeops.Deps.State must be set")
	}
	if deps.Log == nil {
		panic("nodeops.Deps.Log must be set")
	}
	return &Service{creds: deps.Credentials, nodes: deps.Nodes, alerts: deps.Alerts, traffic: deps.Traffic, state: deps.State, log: deps.Log}
}

// Create 建节点并返回 id 与安装 token。spec.Billing 由调用方按 store.Billing 的规则校验过。错误原样来自 Credentials（容量满是
// store.ErrNodeLimit）；返回错误时节点不存在。
//
// 带着计费建节点与 Update 改计费同理由立刻扫描一次：新建即过期或在提醒窗口内的节点不等到零点才触发。
// 节点已提交，扫描失败只记日志，日界扫描会补上。新节点此前没有状态，也不存在被裁剪的作用域，不经 UpdateScope。
func (s *Service) Create(ctx context.Context, spec store.NewNode) (int64, string, error) {
	id, tok, err := s.creds.CreateNode(ctx, spec)
	if err != nil {
		return 0, "", err
	}
	if spec.Billing != (store.Billing{}) {
		if err := s.alerts.SweepExpiry(context.WithoutCancel(ctx)); err != nil {
			s.log.Error("expiry sweep after node create failed", "node", id, "err", err)
		}
	}
	return id, tok, nil
}

// Update 整体替换节点的可编辑字段（edit 已由调用方校验与清洗）。错误原样来自 NodeWriter 或 UpdateScope：节点不存在
// 是 store.ErrNotFound，返回错误时库与各处内存都未改。返回 nil 时计费、已出租与流量引起的扫描都已做完，调用方此后
// 回读到的节点已是扫描之后的值（自动续期推后的到期日）；放锁之后节点可能已被并发的 Delete 删掉，回读得到 ErrNotFound。
func (s *Service) Update(ctx context.Context, id int64, edit store.NodeEdit) error {
	s.mu.Lock()
	trafficChanged := false
	rentedChanged := false
	billingChanged, err := s.alerts.UpdateScope(func() (store.NodeUpdateResult, error) {
		result, err := s.nodes.UpdateNode(ctx, id, edit)
		trafficChanged = result.TrafficChanged
		rentedChanged = result.RentedChanged
		return result, err
	})
	if err == nil {
		// 只有库提交成功才改内存；mu 跨越库写入与内存更新并与删除共用，失败或并发请求都不能使两者分叉。
		s.traffic.SetResetDay(id, edit.TrafficResetDay)
		// 库里存的是规范形，开关只看空与非空，规范化不改变它，所以按 edit 发布与按库发布相同。
		s.state.SetAddressPins(id, edit.IPv4Pin, edit.IPv6Pin)
		if trafficChanged {
			if _, commitErr := s.traffic.Commit(context.WithoutCancel(ctx), id); commitErr != nil {
				s.log.Error("traffic commit after node update failed", "node", id, "err", commitErr)
				trafficChanged = false
			}
		}
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	// 计费字段变了就立刻按新值扫描一次：续费之后不等到零点才恢复。已出租开关翻转同样立刻扫：
	// 到期候选集随开关变，标上后它已有的 firing 要立刻撤销，摘掉后回到候选集要立刻评估。修改已提交，扫描失败
	// 只记日志，下一次扫描会再评估。扫描放在 mu 之外，理由见包注释。
	if billingChanged || rentedChanged {
		if err := s.alerts.SweepExpiry(context.WithoutCancel(ctx)); err != nil {
			s.log.Error("expiry sweep after node update failed", "node", id, "err", err)
		}
	}
	if trafficChanged {
		if err := s.alerts.EvaluateTrafficNode(context.WithoutCancel(ctx), id); err != nil {
			s.log.Error("traffic evaluation after node update failed", "node", id, "err", err)
		}
	}
	return nil
}

// BatchUpdateTags 给 ids 里每个节点加上 add、去掉 remove（调用方已校验、去重、排序），一次事务。错误原样来自
// NodeWriter 或 UpdateScope（超出每节点标签上限是 store.ErrTagLimit），返回错误时库与内存都未改。持 mu 与 Update、
// Delete 串行：标签变化与 Update 的整体替换写同一组行。
func (s *Service) BatchUpdateTags(ctx context.Context, ids []int64, add, remove []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.alerts.UpdateScope(func() (store.NodeUpdateResult, error) {
		return s.nodes.BatchUpdateNodeTags(ctx, ids, add, remove)
	})
	return err
}

// RotateToken 换发节点 token 并返回新的明文。旧 token 在返回前失效（见 auth.Auth.RotateToken）。节点不存在是
// store.ErrNotFound。节点身份不变，进程内状态照旧，不 Forget。
func (s *Service) RotateToken(ctx context.Context, id int64) (string, error) {
	return s.creds.RotateToken(ctx, id)
}

// Delete 删除节点：先由 Credentials 提交库删除并撤销 token，再清掉进程内状态（Forget）。返回 nil 同时意味着持久化
// 删除完成与进程内状态清除；返回错误时什么都没清。节点不存在是 store.ErrNotFound。
func (s *Service) Delete(ctx context.Context, id int64) error {
	s.mu.Lock()
	err := s.creds.DeleteNode(ctx, id)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	// 删除已提交，Update 在库层得到不存在后不会再调 SetResetDay，锁外 Forget 不会与编辑交错重建流量状态。
	// 持 mu 等待在途上报与评估只会阻塞其它节点的编辑，因此清理放在锁外。
	s.Forget(id)
	return nil
}

// Forget 清掉一个已不在库里的节点的全部进程内状态，是这份清理的唯一实现。
//
// 前提：节点的库删除已提交、token 已从 auth 的映射里撤销（之后不再有该节点的上报能通过鉴权），且库删除不与任何
// Update 的"库提交 → 改内存"交错。Delete 在 mu 下提交删除，三条都由它保证；库外删除（离线子命令）由 Reloader
// 经 forgetRemoved 调用，后两条由那里保证，否则一次在库删除之前提交的 Update 可能在 Forget 之后才调 SetResetDay，
// 重建已删节点的重置日。
//
// 调用方不得持 mu 或任何协作者的锁：上报侧的清理要等在途上报与写协程回调退出，告警侧要等 writeMu（见各自的 Forget）。
// 先清上报侧、后清告警：上报侧返回后，该节点不再有观测进入在线快照与待刷出队列（见 ingest.Service.Forget），
// 告警侧清掉的是到那时为止已发布的作用域与状态。
func (s *Service) Forget(id int64) {
	s.state.Forget(id)
	// 同步清掉告警缓存，列表不能残留已删除节点的作用域与状态。
	s.alerts.Forget(id)
}

// forgetRemoved 清掉库外删除的节点的进程内状态，ids 来自 Reloader：token 映射已按库重建、这些节点已不在映射里，
// Forget 的前两条前提成立。第三条——库删除不与 Update 的"库提交 → 改内存"交错——在这里补上：库外删除不经 mu，
// 一次在库删除之前提交的 Update 此刻可能仍持着 mu、尚未调 SetResetDay。先取一次 mu 再放：库删除在调用之前已提交
// （Reloader 已读到它），所以屏障之前持过 mu 的 Update 已改完内存，屏障之后才取得 mu 的 Update 的库写在库删除之后，
// 在库层得到 ErrNotFound、不改内存；随后的 Forget 不会被任何一个 Update 重建。
//
// 屏障取一次而不是让 Forget 每次自取：Delete 调 Forget 时屏障已由它自己的持锁区间给出，Forget 自取只是多一次争用。
// Forget 不在 mu 之下做，理由见 Forget。
func (s *Service) forgetRemoved(ids []int64) {
	if len(ids) == 0 {
		return
	}
	s.mu.Lock()
	//lint:ignore SA2001 空临界区就是屏障本身：只为等持 mu 的 Update 放锁，理由见函数注释。
	s.mu.Unlock()
	for _, id := range ids {
		s.Forget(id)
	}
}
