package store

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNodeLimit 是每节点任务上限的分类哨兵：保存侧的 NodeLimitError 与建节点侧的 InheritedLimitError 都经 Is 归入它，
// 超限的是哪个节点、多少个任务由这两个类型的原文给出。哨兵文案只命名类别，不写成"某节点已满"：保存侧在写入之后
// 计数，拒绝的是会超出上限的写入，而不是已满的节点；建节点侧被拒的节点随事务回滚，并不存在。
var ErrNodeLimit = errors.New("probe task limit per node exceeded")

var ErrInUse = errors.New("in use")

// ErrPrecondition 是写事务里的前置条件失败：调用方据以发起写的取值已不是库里的当前值（保存探测任务时带来的
// expected_config_id 与任务当前身份不一致；手动续期所依据的周期与到期日已被改过）。与 ErrNotFound 分开，调用方才能
// 回答 FailedPrecondition 而不是把"已变"说成"不存在"。
var ErrPrecondition = errors.New("precondition failed")

// ErrNodeRented：目标节点已出租，没有 agent 可更新。由 StartNodeUpdate 在写事务里判定，错误文案点名节点。
var ErrNodeRented = errors.New("node is rented out")

// PreconditionError 携带哪一个前置条件没有满足。Detail 是给调用方的原文。
type PreconditionError struct{ Detail string }

func (e PreconditionError) Error() string        { return e.Detail }
func (e PreconditionError) Is(target error) bool { return target == ErrPrecondition }

// InvalidTaskError 是保存探测任务时请求本身不合法（指纹动作、前置条件的形状、CheckTask）。
// Detail 点名字段与约束；调用方把它译成 InvalidArgument。
type InvalidTaskError struct{ Detail string }

func (e InvalidTaskError) Error() string { return e.Detail }

type ObjectKind string

// 对象标识跨包消费：api 据此选择字段名，投递据此识别已消失的行。
// 统一引用常量，避免字符串字面量的拼写漂移绕过编译检查、令消费方静默失效。
const (
	ObjectNode          ObjectKind = "node"
	ObjectProbeTask     ObjectKind = "probe task"
	ObjectAlertRule     ObjectKind = "alert rule"
	ObjectNotifyChannel ObjectKind = "notify channel"
	ObjectAlertEvent    ObjectKind = "alert event"
	ObjectAlertDelivery ObjectKind = "alert delivery"
	ObjectSilence       ObjectKind = "silence"
)

type RuleReference struct {
	ID   int64
	Name string
}

type InUseError struct {
	Kind  ObjectKind
	ID    int64
	Rules []RuleReference
	// 隐藏引用仍阻止删除，但不能把范围外规则的名称与编号带进错误响应。
	HiddenRules bool
}

func (e InUseError) Error() string {
	names := make([]string, len(e.Rules))
	for i, r := range e.Rules {
		names[i] = fmt.Sprintf("%s (id %d)", r.Name, r.ID)
	}
	if e.HiddenRules {
		names = append(names, "additional rules outside the authorized scope")
	}
	return fmt.Sprintf("%s %d is referenced by alert rules: %s", e.Kind, e.ID, strings.Join(names, ", "))
}

func (e InUseError) Is(target error) bool { return target == ErrInUse }

// 错误由发现问题的存储层携带实体与约束；调用方不必解析文本即可保留哨兵分类。
type NotFoundError struct {
	Kind ObjectKind
	ID   int64
}

func (e NotFoundError) Error() string {
	return fmt.Sprintf("%s %d does not exist", e.Kind, e.ID)
}

func (e NotFoundError) Is(target error) bool {
	return target == ErrNotFound
}

// MoveRangeError 是 MoveNodes 的 position 越界：N（节点总数）、k（去重后要移动的节点数）与合法区间由写
// 事务里读到的全序推出，调用方据此写自解释的错误消息；越界不截断到区间端点——截断会让写错的位置静默
// 落到首尾。
type MoveRangeError struct {
	Total    int // N：同一事务里读到的节点总数
	Moving   int // k：去重后要移动的节点数
	Position uint32
}

func (e MoveRangeError) Error() string {
	return fmt.Sprintf("position must be between 1 and %d (N=%d nodes, k=%d moving); got %d", e.Total-e.Moving+1, e.Total, e.Moving, e.Position)
}

// NodeLimitError 是保存任务之后某个现有节点的任务数（显式分配加全部 all_nodes 任务）超过上限。
type NodeLimitError struct {
	NodeID int64
	Tasks  int
	Max    int
}

func (e NodeLimitError) Error() string {
	return fmt.Sprintf("node %d would have %d probe tasks (maximum %d)", e.NodeID, e.Tasks, e.Max)
}

func (e NodeLimitError) Is(target error) bool {
	return target == ErrNodeLimit
}

// InheritedLimitError 是建节点时新节点会继承的 all_nodes 任务数超过上限：新节点没有显式分配，它的任务数就是
// all_nodes 任务的个数。
type InheritedLimitError struct {
	Tasks int
	Max   int
}

func (e InheritedLimitError) Error() string {
	return fmt.Sprintf("a new node would inherit %d all-nodes probe tasks (maximum %d per node); assign some of them to explicit nodes or delete them first", e.Tasks, e.Max)
}

func (e InheritedLimitError) Is(target error) bool {
	return target == ErrNodeLimit
}

// LevelWatermark 是一次查询读到的某个粗级水位：Level 是 rollup_state 的键名，Upto 是水位时刻（Unix 秒）。
type LevelWatermark struct {
	Level string
	Upto  int64
}

// ReadQuotaError 是一次历史查询在对齐后的各级实际来源行数超过本次请求的额度（rollup.go 的
// quotaRowsPerSeries × 序列额度权重）。额度只约束实际要读的源行数：错误带出额度与参与查询的
// 各级水位所在时刻，供调用方缩短窗口或等待数据整理；不推断“维护落后”——水位只是事实，
// 触发与否只取决于窗口内实际有多少行。
type ReadQuotaError struct {
	Quota      int64
	Series     int64
	Watermarks []LevelWatermark
}

func (e ReadQuotaError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "this history query would read more than %d source rows (budget: %d rows per series x %d series); narrow the window, raise max_points, or wait for data maintenance to catch up", e.Quota, quotaRowsPerSeries, e.Series)
	for _, w := range e.Watermarks {
		fmt.Fprintf(&b, "; %s data is consolidated up to %s", w.Level, time.Unix(w.Upto, 0).UTC().Format(time.RFC3339))
	}
	return b.String()
}
