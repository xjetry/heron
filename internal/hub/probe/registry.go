// Package probe 是探测任务、分配与版本号的管理写入口和内存缓存。
//
// mu 使本包每次发布的版本、任务与分配一起对读侧可见；它不把库侧删除节点与 Forget 合成一个事务。
// writeMu 串行化存储访问到内存发布，防止保存、删除、建节点与重载反序发布；提交与发布之间读侧仍可看到旧数据。
// 管理写入由 store 在变更事务内递增版本；DeleteNode 对分配表只删除该节点的行，不递增，其他节点的清单未变。
// 建节点也改变清单（新节点继承全部 all_nodes 任务），所以两个建节点入口（CreateNode、RegisterNode）经本包落库，
// 由 store 在同一事务内读出新节点的覆盖并推进版本，本包在 writeMu 下按这份覆盖把新节点加进内存索引；
// auth 经 auth.NodeCreator 调用它们。
// 离线 CLI 建节点同样推进库里的版本、删节点同样删分配行，但不经本进程的写入口；运行中的 hub 由重载循环
// （nodeops.Reloader）发现库外写入后 Load，在那之前缓存落后于库（auth 的 token 映射同样如此）。
// 进程内删除由 nodeops.Service.Delete 编排：auth.DeleteNode 撤销 token，再经 ingest.Forget 等待在途上报退出；库外删除
// 由重载循环在重建 token 映射之后走同一条 Forget。
// Forget 与 Save 互斥且 Save 从提交到发布全程持 writeMu；DeleteNode 提交后调用 Forget，才能清掉较早保存发布的分配。
package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/probelimit"
	"google.golang.org/protobuf/proto"
)

var ErrInvalid = errors.New("invalid probe task")

// Detail 的 NodeIDs 是任务当前覆盖的节点，升序；AllNodes 为真时它是展开结果，随建删节点变化。
type Detail struct {
	Task         *heronv1.ProbeTask
	AllNodes     bool
	NodeIDs      []int64
	SelectorTags []string
}

type Registry struct {
	store *store.Store
	log   *slog.Logger

	writeMu sync.Mutex // 包内 writeMu → mu；跨包锁序见 internal/hub 的包注释（doc.go）
	mu      sync.RWMutex
	version uint64
	tasks   map[uint64]*heronv1.ProbeTask
	// allNodes 是 all_nodes 任务的集合，只供 List 回显开关，不参与覆盖的推导。byNode 与 nodesOf 总是 store 按
	// probeCoverage 读出的覆盖，读侧（TasksFor、Assigned、TargetFor、List）不区分选择器模式。
	allNodes     map[uint64]struct{}
	byNode       map[int64]map[uint64]struct{}
	nodesOf      map[uint64][]int64
	selectorTags map[uint64][]string
	sortOrder    map[uint64]int64
}

func New(st *store.Store, log *slog.Logger) *Registry {
	r := &Registry{store: st, log: log}
	r.reset()
	return r
}

func (r *Registry) reset() {
	r.tasks, r.allNodes, r.byNode, r.nodesOf = map[uint64]*heronv1.ProbeTask{}, map[uint64]struct{}{}, map[int64]map[uint64]struct{}{}, map[uint64][]int64{}
	r.selectorTags = map[uint64][]string{}
	r.sortOrder = map[uint64]int64{}
}

// Load 的读取与发布和其他写入口互斥，避免旧的重载快照覆盖刚发布的保存结果。
func (r *Registry) Load(ctx context.Context) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	version, recs, err := r.store.LoadProbeTasks(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.version = version
	r.reset()
	for _, rec := range recs {
		r.put(rec)
	}
	return nil
}

// 缓存拥有独立对象；保存结果和查询结果都不能让调用方绕过版本事务修改缓存。
// rec.NodeIDs 是 store 读出的覆盖（all_nodes 任务已展开），直接成为索引。
func (r *Registry) put(rec store.ProbeTaskRecord) {
	id := rec.Task.Id
	r.tasks[id] = proto.Clone(rec.Task).(*heronv1.ProbeTask)
	if rec.AllNodes {
		r.allNodes[id] = struct{}{}
	}
	r.nodesOf[id] = slices.Clone(rec.NodeIDs)
	r.selectorTags[id] = slices.Clone(rec.SelectorTags)
	r.sortOrder[id] = rec.SortOrder
	for _, node := range rec.NodeIDs {
		r.assign(node, id)
	}
}

func (r *Registry) assign(node int64, task uint64) {
	if r.byNode[node] == nil {
		r.byNode[node] = map[uint64]struct{}{}
	}
	r.byNode[node][task] = struct{}{}
}

func (r *Registry) remove(id uint64) {
	for _, node := range r.nodesOf[id] {
		delete(r.byNode[node], id)
		if len(r.byNode[node]) == 0 {
			delete(r.byNode, node)
		}
	}
	delete(r.tasks, id)
	delete(r.allNodes, id)
	delete(r.nodesOf, id)
	delete(r.selectorTags, id)
	delete(r.sortOrder, id)
}

func (r *Registry) Version() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.version
}

// TasksFor 返回按 id 升序的任务与版本；空清单也必须下发，agent 才能停止已撤销的任务。
func (r *Registry) TasksFor(nodeID int64) *heronv1.ProbeTasks {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := &heronv1.ProbeTasks{Version: r.version}
	for id := range r.byNode[nodeID] {
		task := proto.Clone(r.tasks[id]).(*heronv1.ProbeTask)
		out.Tasks = append(out.Tasks, task)
	}
	sort.Slice(out.Tasks, func(i, j int) bool { return out.Tasks[i].Id < out.Tasks[j].Id })
	return out
}

// CertPolicy 是附带证书观测在当前配置下能不能采信的事实。任务不在清单里时 ok 为 false。
func (r *Registry) CertPolicy(id uint64) (https, pinned bool, configID []byte, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	task, ok := r.tasks[id]
	if !ok {
		return false, false, nil, false
	}
	https = probelimit.IsHTTPSTarget(task.GetKind(), task.GetTarget())
	pinned = len(task.GetCertSpkiSha256()) > 0
	configID = append([]byte(nil), task.GetConfigId()...)
	return https, pinned, configID, true
}

// TaskCount 是节点当前覆盖的全部任务数，不论选择器是直接指定、按标签还是 all_nodes（byNode 是 store 读出的覆盖）。
func (r *Registry) TaskCount(nodeID int64) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byNode[nodeID])
}

func (r *Registry) Assigned(nodeID int64, taskID uint64) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.byNode[nodeID][taskID]
	return ok
}

// List 包含未分配的任务；版本与任务在同一个读锁下取得，不能混合两次内存发布的状态。
func (r *Registry) List() (uint64, []Detail) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Detail
	for _, id := range r.orderedIDsLocked() {
		task := r.tasks[id]
		_, all := r.allNodes[id]
		out = append(out, Detail{Task: proto.Clone(task).(*heronv1.ProbeTask), AllNodes: all, NodeIDs: slices.Clone(r.nodesOf[id]), SelectorTags: slices.Clone(r.selectorTags[id])})
	}
	return r.version, out
}

func (r *Registry) orderedIDsLocked() []uint64 {
	ids := make([]uint64, 0, len(r.tasks))
	for id := range r.tasks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := ids[i], ids[j]
		if r.sortOrder[a] != r.sortOrder[b] {
			return r.sortOrder[a] < r.sortOrder[b]
		}
		return a < b
	})
	return ids
}

// OrderedIDs 是展示顺序快照；TasksFor 保持按编号下发，不受展示重排影响。
func (r *Registry) OrderedIDs() []uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.orderedIDsLocked()
}

func (r *Registry) Reorder(ctx context.Context, ids []uint64) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if err := r.store.ReorderProbeTasks(ctx, ids); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, id := range ids {
		r.sortOrder[id] = int64(i)
	}
	return nil
}

// Target 返回任务当前的种类与目标，是管理端标注历史序列的口径。任务不在清单里时 ok 为 false。
// 历史行只带 task_id，查不到即已删除，这靠两个前提：ingest 的 foldResults 只收 Assigned 为真的结果，
// 所以历史里的每个 task_id 都曾是已发布的任务；任务表的 AUTOINCREMENT 保证编号不复用，所以不会标成别的任务。
func (r *Registry) Target(id uint64) (kind heronv1.ProbeKind, target string, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tasks[id]
	if !ok {
		return heronv1.ProbeKind_PROBE_KIND_UNSPECIFIED, "", false
	}
	return t.Kind, t.Target, true
}

// TargetFor 是公开端标注历史序列的口径：只对当前分配给 nodeID 的任务给出种类与目标，其余 ok 为 false。
// 节点公开即公开它正在探测的目标；历史里出现、但现在不分配给该节点的任务，当前目标可能从未被该节点探测过
// （撤下后改成了内网地址、只分配给私有节点），不在公开范围内。分配与目标在同一个读锁下读出：分开两次加锁，
// 中间的 Save 可能让"已分配"与"新目标"拼在一起。
func (r *Registry) TargetFor(nodeID int64, id uint64) (kind heronv1.ProbeKind, target string, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, known := r.tasks[id]
	if _, assigned := r.byNode[nodeID][id]; !known || !assigned {
		return heronv1.ProbeKind_PROBE_KIND_UNSPECIFIED, "", false
	}
	return t.Kind, t.Target, true
}

func dedupSorted(ids []int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// Save 的内存发布只发生在事务成功后，拒绝的保存不能改变任务、分配或版本。
// 全部节点、标签交集和显式分配互斥，与告警规则使用同一校验。显式空集合法且不覆盖任何节点，不能静默放宽。
func (r *Registry) Save(ctx context.Context, t *heronv1.ProbeTask, selector store.NodeSelector, opts ...store.ProbeSaveOption) (Detail, uint64, error) {
	// 有效任务（忽略输入里的 pin 与 config_id、套上指纹动作之后）由 store 用 CheckTask 裁决。
	// 在这里先查输入会把只输出字段上的非法值当成错误，而那两个字段输入时必须忽略。
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if err := selector.Check(); err != nil {
		return Detail{}, 0, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	selector.NodeIDs = dedupSorted(selector.NodeIDs)
	rec, version, err := r.store.SaveProbeTask(ctx, t, selector, opts...)
	var invalid store.InvalidTaskError
	if errors.As(err, &invalid) {
		return Detail{}, 0, fmt.Errorf("%w: %s", ErrInvalid, invalid.Error())
	}
	if err != nil {
		return Detail{}, 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.remove(rec.Task.Id)
	r.put(rec)
	r.version = version
	return Detail{Task: proto.Clone(rec.Task).(*heronv1.ProbeTask), AllNodes: rec.AllNodes, NodeIDs: slices.Clone(rec.NodeIDs), SelectorTags: slices.Clone(rec.SelectorTags)}, version, nil
}

// UpdateNode 与任务保存共用 writeMu，标签变更提交后按事务回读的覆盖发布，不能由较旧的任务快照覆盖。
func (r *Registry) UpdateNode(ctx context.Context, id int64, edit store.NodeEdit) (store.NodeUpdateResult, error) {
	return r.updateNodeScopes(func() (store.NodeUpdateResult, error) { return r.store.UpdateNodeTasks(ctx, id, edit) })
}

func (r *Registry) BatchUpdateNodeTags(ctx context.Context, ids []int64, add, remove []string) (store.NodeUpdateResult, error) {
	return r.updateNodeScopes(func() (store.NodeUpdateResult, error) { return r.store.BatchUpdateNodeTags(ctx, ids, add, remove) })
}

func (r *Registry) updateNodeScopes(mutate func() (store.NodeUpdateResult, error)) (store.NodeUpdateResult, error) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	result, err := mutate()
	if err != nil {
		return store.NodeUpdateResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	changedTasks := map[uint64][]int64{}
	for id, tasks := range result.Tasks {
		for task := range r.byNode[id] {
			if _, ok := changedTasks[task]; !ok {
				changedTasks[task] = nil
			}
		}
		delete(r.byNode, id)
		for _, task := range tasks {
			changedTasks[task] = append(changedTasks[task], id)
			r.assign(id, task)
		}
	}
	for task, nodes := range changedTasks {
		r.nodesOf[task] = slices.DeleteFunc(r.nodesOf[task], func(node int64) bool { _, changed := result.Tasks[node]; return changed })
		r.nodesOf[task] = append(r.nodesOf[task], nodes...)
		slices.Sort(r.nodesOf[task])
	}
	r.version = result.Version
	return result, nil
}

// CreateNode 与 RegisterNode 是两个建节点入口，实现 auth.NodeCreator。store 在建节点事务里读出新节点的覆盖、
// 检查上限并推进版本；这里在 writeMu 下发布，不与 Save、Delete、Load 反序。
func (r *Registry) CreateNode(ctx context.Context, spec store.NewNode, tokenHash []byte) (int64, error) {
	return r.addNode(func() (int64, store.NewNodeTasks, error) { return r.store.CreateNode(ctx, spec, tokenHash) })
}

func (r *Registry) RegisterNode(ctx context.Context, keyHash []byte, name string, tokenHash []byte) (int64, error) {
	return r.addNode(func() (int64, store.NewNodeTasks, error) { return r.store.RegisterNode(ctx, keyHash, name, tokenHash) })
}

// addNode 发布建节点事务的结果：新节点覆盖哪些任务只取 store 在该事务里按 probeCoverage 读出的 TaskIDs，版本取
// 该事务推进后的值。覆盖的读法只有 probeCoverage 一处，内存增量也由它决定；若在这里按 allNodes 集合另推一遍，
// 覆盖口径一改（all_nodes 的条件变了、多出按节点属性选中的一支），库侧读者与上限计数随之改变而内存索引不变，
// 新节点的清单、上报准入、公开标签与 List 的节点列表就与库不符，直到相关任务再被保存或 hub 重启重载。
//
// 增量只动新节点，依赖两条前提：
//   - 对 Load 过的注册表，TaskIDs 里的任务都在 tasks 里：probe_task 只经本包的 Save 与 Delete 写入，二者与
//     addNode 都在 writeMu 下完成落库与发布，Load 在 writeMu 下整体读入，所以持 writeMu 时库里的任务集合与内存
//     一致。离线 CLI 的注册表不 Load，这条不成立，但它的发布随进程丢弃、没有读者（见 cmd/hub 的 openOffline）；
//   - 其余节点的覆盖不因建节点而变：probeCoverage 的全部节点、分配行、标签交集分支都逐节点判断，插入一个
//     节点后只可能多出以新节点为一端的覆盖对。覆盖口径若加入依赖节点集合整体的条件，这条前提要重新核对。
func (r *Registry) addNode(insert func() (int64, store.NewNodeTasks, error)) (int64, error) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	id, created, err := insert()
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, task := range created.TaskIDs {
		r.nodesOf[task] = append(r.nodesOf[task], id)
		slices.Sort(r.nodesOf[task])
		r.assign(id, task)
	}
	r.version = created.Version
	return id, nil
}

func (r *Registry) Delete(ctx context.Context, id uint64) (uint64, error) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	version, err := r.store.DeleteProbeTask(ctx, id)
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.remove(id)
	r.version = version
	return version, nil
}

// Forget 只能在 store.DeleteNode 提交成功后调用，删除调用链必须先持久化删除再清内存分配。
// 它与 Save 互斥且 Save 从提交到发布全程持 writeMu；已持锁的 Save/Delete 完成存储往返与发布前，此调用会阻塞。
// 调用方不得持有写协程回调会获取的锁：回调经 Authenticate 取 auth.mu 读锁，调用方持写锁调 Forget 即阻塞回调形成环；ingest.mu 同理。
// 若在 ingest.stateMu 下调用且正在等待存储，全体上报也会排队等待。
// 只摘掉被删节点的前提与 addNode 的第二条相同：删节点只少掉以该节点为一端的覆盖对。覆盖口径若加入依赖节点集合
// 整体的一支，这里要与 addNode 一起重新核对，否则删节点后内存索引与库分叉，要到重启或相关任务被保存才收敛。
func (r *Registry) Forget(nodeID int64) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.byNode[nodeID] {
		r.nodesOf[id] = slices.DeleteFunc(r.nodesOf[id], func(node int64) bool { return node == nodeID })
	}
	delete(r.byNode, nodeID)
}
