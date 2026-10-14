package api

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

// noComparisonTask 是对比入口对"没有可对比的任务"的唯一回答：任务不存在，或调用方一个可见的
// 分配节点都没有（公开端即任务只分配给了非公开节点）。二者不加区分，与单节点查询对"不存在"
// 与"不可见"返回同一 NotFound 同一口径：响应里区分二者会泄漏任务的存在。
func noComparisonTask() error {
	return connect.NewError(connect.CodeNotFound, errors.New("task_id: no probe task has this id"))
}

// checkComparisonTask 拒绝缺失的任务编号：编号从 1 起，0 只是客户端没设；不可表示的值仍交给
// checkTaskID。存在任务的空候选由 noComparisonTask 统一回答，与这里无关。
func checkComparisonTask(id uint64) error {
	if id == 0 {
		return invalid("task_id: required")
	}
	return checkTaskID(id, "task_id")
}

// checkComparisonNodes 校验分块的节点清单：1 到 max_nodes_per_query 个、不得重复、id 为正。
// 上限与服务端校验用同一个常量（store.MaxComparisonNodes），也是 ListProbeComparisonNodes
// 下发的值；客户端按它切块，不自己抄写。
func checkComparisonNodes(ids []int64) error {
	if len(ids) == 0 {
		return invalid("node_ids: at least one node is required")
	}
	if len(ids) > store.MaxComparisonNodes {
		return invalid("node_ids: at most %d nodes per query; got %d", store.MaxComparisonNodes, len(ids))
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return invalid("node_ids: node ids must be positive; got %d", id)
		}
		if seen[id] {
			return invalid("node_ids: must not repeat node %d", id)
		}
		seen[id] = true
	}
	return nil
}

// ListProbeComparisonNodes 是跨节点对比的第一步：任务标注与当前分配了该任务、调用方可见的节点
// 在同一个读快照里读出。标注规则同 QueryProbes：会话与全站 token 按任务当前配置，非全站 token
// 只在任务的整个作用域都可见时标注（与 ListProbeTasks 同一谓词）。
func (s *Service) ListProbeComparisonNodes(ctx context.Context, req *connect.Request[heronv1.ListProbeComparisonNodesRequest]) (*connect.Response[heronv1.ListProbeComparisonNodesResponse], error) {
	if err := checkComparisonTask(req.Msg.GetTaskId()); err != nil {
		return nil, err
	}
	rec, ok, nodes, err := s.store.ListComparisonNodes(ctx, req.Msg.GetTaskId(), store.ComparisonScoped)
	if err != nil {
		s.log.Error("listing comparison nodes failed", "err", err)
		return nil, internalError("listing comparison nodes failed")
	}
	if !ok || len(nodes) == 0 {
		return nil, noComparisonTask()
	}
	resp := &heronv1.ListProbeComparisonNodesResponse{NodeIds: nodes, MaxNodesPerQuery: uint32(store.MaxComparisonNodes)}
	if p, scoped := store.Principal(ctx); !scoped || p.AllowsSelector(rec.AllNodes, rec.SelectorTags, rec.NodeIDs) {
		resp.Kind, resp.Target = rec.Task.GetKind(), rec.Task.GetTarget()
	}
	return connect.NewResponse(resp), nil
}

// QueryProbeComparison 是跨节点对比的第二步：窗口校验、分块校验、逐节点授权与读量校验之后，
// 返回可见节点上该任务的样本。成员与标注是 List 时刻的；这里不核对分配、不返回标注——样本
// 本来就按任务 id 跨配置累积，与 QueryProbes 的历史一样。
func (s *Service) QueryProbeComparison(ctx context.Context, req *connect.Request[heronv1.QueryProbeComparisonRequest]) (*connect.Response[heronv1.QueryProbeComparisonResponse], error) {
	m := req.Msg
	maxPoints, err := checkWindow(m.GetFrom(), m.GetTo(), m.GetMaxPoints())
	if err != nil {
		return nil, err
	}
	if err := checkComparisonTask(m.GetTaskId()); err != nil {
		return nil, err
	}
	if err := checkComparisonNodes(m.GetNodeIds()); err != nil {
		return nil, err
	}
	resp, err := s.history.comparison(ctx, m.GetTaskId(), m.GetNodeIds(), m.GetFrom(), m.GetTo(), maxPoints, s.store.NodeExists)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// ListProbeComparisonNodes 的公开端：候选只含公开节点。任务只分配给非公开节点时与任务不存在
// 同一 NotFound，不暴露它的存在。候选非空即任务在该读事务里分配给了至少一个公开节点，
// 标注与 TargetFor 同一口径；GET 响应最多再被缓存使用 60 秒（proto 的 cache_max_age_s）。
func (p *Public) ListProbeComparisonNodes(ctx context.Context, req *connect.Request[heronv1.ListProbeComparisonNodesRequest]) (*connect.Response[heronv1.ListProbeComparisonNodesResponse], error) {
	if err := checkComparisonTask(req.Msg.GetTaskId()); err != nil {
		return nil, err
	}
	rec, ok, nodes, err := p.store.ListComparisonNodes(ctx, req.Msg.GetTaskId(), store.ComparisonPublic)
	if err != nil {
		p.log.Error("listing comparison nodes failed", "err", err)
		return nil, internalError("listing comparison nodes failed")
	}
	if !ok || len(nodes) == 0 {
		return nil, noComparisonTask()
	}
	resp := &heronv1.ListProbeComparisonNodesResponse{
		Kind: rec.Task.GetKind(), Target: rec.Task.GetTarget(), NodeIds: nodes, MaxNodesPerQuery: uint32(store.MaxComparisonNodes),
	}
	return connect.NewResponse(resp), nil
}

// QueryProbeComparison 的公开端：授权同单节点公开 QueryProbes（仅公开谓词），读量校验共用
// history.comparison。
func (p *Public) QueryProbeComparison(ctx context.Context, req *connect.Request[heronv1.QueryProbeComparisonRequest]) (*connect.Response[heronv1.QueryProbeComparisonResponse], error) {
	m := req.Msg
	maxPoints, err := checkWindow(m.GetFrom(), m.GetTo(), m.GetMaxPoints())
	if err != nil {
		return nil, err
	}
	if err := checkComparisonTask(m.GetTaskId()); err != nil {
		return nil, err
	}
	if err := checkComparisonNodes(m.GetNodeIds()); err != nil {
		return nil, err
	}
	resp, err := p.history.comparison(ctx, m.GetTaskId(), m.GetNodeIds(), m.GetFrom(), m.GetTo(), maxPoints, p.store.NodeIsPublic)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}
