package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
)

// MaxComparisonNodes 是一次对比分块至多携带的节点数，也是对比查询的额度权重
// （R = quotaRowsPerSeries × MaxComparisonNodes）。它是服务端唯一的常量：入口校验、
// ListProbeComparisonNodes.max_nodes_per_query 下发与额度共用。
//
// 初值 32；成本验收的门槛是：分块每窗口 p99 不超过同窗口单节点查询，且饱和组里
// 受保护读者（期内空闲同类基线）的 p99 不超 2 倍。32 节点的分块已可观察到比单节点
// 查询重（IN 清单逐节点定位、按节点拆序列排序），16 以下分块数据量明显小于单节点
// 查询（16 × 1 任务 vs 64 任务）；暂取 16，最终值以受控环境的验收实测为准。
const MaxComparisonNodes = 16

// comparisonKeyWhere 生成对比查询的等值键约束：task_id 等值 + 节点清单。占位符与
// comparisonKeyArgs 同序，前导两列匹配 (task_id, node_id, ts) 索引，ts 范围由 queryFamily 追加。
func comparisonKeyWhere(nodeCount int) string {
	return "task_id = ? AND node_id IN (" + strings.TrimSuffix(strings.Repeat("?, ", nodeCount), ", ") + ")"
}

func comparisonKeyArgs(taskID uint64, nodeIDs []int64) []any {
	args := make([]any, 0, len(nodeIDs)+1)
	args = append(args, int64(taskID))
	for _, id := range nodeIDs {
		args = append(args, id)
	}
	return args
}

// QueryProbeComparison 返回一个任务在 nodeIDs 各节点上 [from, to) 内按 step 聚合的样本，
// 按 (node_id, ts) 升序。来源以 (task_id, node_id) 等值加 ts 范围读取，走 (task_id, node_id, ts)
// 索引：每个节点要读的行与其他节点、其他任务、已删除任务的行无关。窗口内没有样本的节点
// 不出现在返回值里，由调用方按请求的节点清单补空序列。
func (s *Store) QueryProbeComparison(ctx context.Context, taskID uint64, nodeIDs []int64, from, to int64, lv Level, step int64) ([]metric.ProbeRow, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}
	shape := queryShape{
		keyWhere:    comparisonKeyWhere(len(nodeIDs)),
		keyArgs:     comparisonKeyArgs(taskID, nodeIDs),
		groupKey:    "node_id",
		seriesLimit: int64(MaxComparisonNodes),
		// 对比形状的源行由 (task_id, node_id) 等值定位，每节点每桶至多一行：节点数就是精确的序列数。
		seriesEstimate: int64(len(nodeIDs)),
		byTaskIndex:    true,
	}
	return queryFamily(ctx, s, requestRead, probeFamily, shape, from, to, lv, step,
		func(rows *sql.Rows) ([]metric.ProbeRow, error) { return scanComparisonRows(rows, taskID) }, nil)
}

// scanComparisonRows 扫描 (ts, node_id, 六个值列)。TaskID 不在输出列里：它由查询的等值键
// 给定，所有行同值。值列的解释与 scanProbeRows 相同。
func scanComparisonRows(rows *sql.Rows, taskID uint64) ([]metric.ProbeRow, error) {
	var out []metric.ProbeRow
	for rows.Next() {
		var ts, node, sent, lost, errs, sum int64
		var mn, mx sql.NullInt64
		if err := rows.Scan(&ts, &node, &sent, &lost, &errs, &sum, &mn, &mx); err != nil {
			return nil, err
		}
		out = append(out, metric.ProbeRow{NodeID: node, TS: ts, TaskID: taskID, Bucket: probeBucket(sent, lost, errs, sum, mn, mx)})
	}
	return out, rows.Err()
}

// PublicNodeWhere 是公开节点谓词的 SQL 片段，约束别名为 n 的节点表。公开服务的节点可见范围
// 全部经它（以及读 public 列的 NodeIsPublic）承载：ListPublicNodes、对比候选 ListComparisonNodes。
const PublicNodeWhere = "n.public = 1"

// ComparisonVisibility 选择对比候选的节点可见性口径：管理端按调用方凭据的节点范围
// （nodeScopeSQL，从 ctx 取 principal，会话与全站 token 不限），公开端只看公开节点。
type ComparisonVisibility int

const (
	ComparisonScoped ComparisonVisibility = iota
	ComparisonPublic
)

func (v ComparisonVisibility) where(ctx context.Context) string {
	if v == ComparisonPublic {
		return PublicNodeWhere
	}
	return nodeScopeSQL(ctx, "n.id")
}

// ListComparisonNodes 在同一个只读事务里读出任务的当前配置、选择器与调用方可见的候选节点。
// 任务不存在时 ok 为 false；候选为空与任务不存在在返回值上同形（nodeIDs 为空），调用方按同一
// NotFound 语义处理，不在这里区分。候选按节点全序 (sort_order, id) 升序。
func (s *Store) ListComparisonNodes(ctx context.Context, taskID uint64, visibility ComparisonVisibility) (rec ProbeTaskRecord, ok bool, nodeIDs []int64, err error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ProbeTaskRecord{}, false, nil, err
	}
	defer tx.Rollback()
	t := &heronv1.ProbeTask{}
	var id, kind, interval, timeout, order int64
	var all bool
	q := "SELECT id, kind, target, interval_s, timeout_ms, all_nodes, sort_order, dns_server FROM probe_task WHERE id = ?"
	switch err := tx.QueryRowContext(ctx, q, int64(taskID)).Scan(&id, &kind, &t.Target, &interval, &timeout, &all, &order, &t.DnsServer); {
	case errors.Is(err, sql.ErrNoRows):
		return ProbeTaskRecord{}, false, nil, nil
	case err != nil:
		return ProbeTaskRecord{}, false, nil, err
	}
	t.Id, t.Kind, t.IntervalS, t.TimeoutMs = taskID, heronv1.ProbeKind(kind), uint32(interval), uint32(timeout)
	rec = ProbeTaskRecord{Task: t, AllNodes: all, SortOrder: order}
	if rec.SelectorTags, err = selectorTags(tx, "probe_task", "task_id", int64(taskID)); err != nil {
		return ProbeTaskRecord{}, false, nil, err
	}
	if rec.NodeIDs, err = coveredNodesTx(tx, taskID); err != nil {
		return ProbeTaskRecord{}, false, nil, err
	}
	// 候选 = 任务的覆盖集合 ∩ 调用方可见节点。覆盖经 probeCoverage（与注册表、告警同一口径），
	// 任务不存在时这里自然为空，与上面的不存在分支落在同一返回形态。
	rows, err := tx.QueryContext(ctx,
		"SELECT n.id FROM node n WHERE "+visibility.where(ctx)+" AND n.id IN (SELECT node_id FROM ("+probeCoverage+") WHERE task_id = ?) ORDER BY n.sort_order, n.id",
		int64(taskID))
	if err != nil {
		return ProbeTaskRecord{}, false, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return ProbeTaskRecord{}, false, nil, err
		}
		nodeIDs = append(nodeIDs, id)
	}
	if err := rows.Err(); err != nil {
		return ProbeTaskRecord{}, false, nil, err
	}
	return rec, true, nodeIDs, nil
}
