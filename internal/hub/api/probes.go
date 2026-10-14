package api

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
)

func detailProto(d probe.Detail) *heronv1.ProbeTaskDetail {
	return &heronv1.ProbeTaskDetail{Task: d.Task, AllNodes: d.AllNodes, NodeIds: d.NodeIDs, SelectorTags: d.SelectorTags}
}

func (s *Service) visibleProbeTasks(ctx context.Context) (uint64, []probe.Detail) {
	version, details := s.probes.List()
	visible := make([]probe.Detail, 0, len(details))
	for _, d := range details {
		if p, ok := store.Principal(ctx); ok && !p.AllowsSelector(d.AllNodes, d.SelectorTags, d.NodeIDs) {
			continue
		}
		visible = append(visible, d)
	}
	return version, visible
}

func (s *Service) ListProbeTasks(ctx context.Context, _ *connect.Request[heronv1.ListProbeTasksRequest]) (*connect.Response[heronv1.ListProbeTasksResponse], error) {
	version, details := s.visibleProbeTasks(ctx)
	resp := &heronv1.ListProbeTasksResponse{Version: version}
	for _, d := range details {
		resp.Tasks = append(resp.Tasks, detailProto(d))
	}
	return connect.NewResponse(resp), nil
}

// 字段校验在注册表经 probelimit.CheckTask 完成；节点存在与每节点上限由 store 保存事务裁决，
// 因为只有事务内计数与并发保存互斥。这里只把哨兵翻译成响应码，并补充请求字段名。
func (s *Service) SaveProbeTask(ctx context.Context, req *connect.Request[heronv1.SaveProbeTaskRequest]) (*connect.Response[heronv1.SaveProbeTaskResponse], error) {
	if err := checkTaskID(req.Msg.GetTask().GetId(), "task.id"); err != nil {
		return nil, err
	}
	tags, err := cleanTags("selector_tags", req.Msg.GetSelectorTags())
	if err != nil {
		return nil, err
	}
	if req.Msg.GetTask() == nil {
		return nil, invalid("task: required")
	}
	opts, err := probeSaveOptions(req.Msg)
	if err != nil {
		return nil, err
	}
	src := req.Msg.GetTask()
	task := proto.Clone(src).(*heronv1.ProbeTask)
	// 只输出：输入里的身份与 pin 不参与保存。pin 只来自 cert_pin，身份由存储按内容生成。
	task.ConfigId = nil
	task.CertSpkiSha256 = nil
	d, version, err := s.probes.Save(ctx, task, store.NodeSelector{AllNodes: req.Msg.GetAllNodes(), NodeIDs: req.Msg.GetNodeIds(), Tags: tags}, opts...)
	// 全部节点模式禁止携带显式分配，超限来自 all_nodes 开关本身。
	if req.Msg.GetAllNodes() && errors.Is(err, store.ErrNodeLimit) {
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("all_nodes: %w", err))
	}
	if err != nil {
		return nil, s.operationError(err, "task", "saving probe task failed")
	}
	return connect.NewResponse(&heronv1.SaveProbeTaskResponse{Task: detailProto(d), Version: version}), nil
}

// saveProbeTaskClass 把 SaveProbeTaskRequest 的字段分成三类。task 本身要下钻，不是可编辑路径。
// task.id 是资源身份。未出现在这张表里的新字段会让守卫测试失败。
var saveProbeTaskClass = map[string]string{
	"task": "descend", "node_ids": "editable", "all_nodes": "editable", "selector_tags": "editable",
	"cert_pin": "action", "expected_config_id": "action",
	"task.id": "identity", "task.kind": "editable", "task.target": "editable", "task.interval_s": "editable",
	"task.timeout_ms": "editable", "task.dns_server": "editable",
	"task.cert_spki_sha256": "output", "task.config_id": "output",
}

func probeMaskForbidden() []string {
	out := []string{"task"}
	for path, class := range saveProbeTaskClass {
		if class != "editable" && class != "descend" {
			out = append(out, path)
		}
	}
	return out
}

func probeSaveOptions(req *heronv1.SaveProbeTaskRequest) ([]store.ProbeSaveOption, error) {
	var opts []store.ProbeSaveOption
	if pin := req.GetCertPin(); pin != nil {
		switch action := pin.Action.(type) {
		case *heronv1.CertPinChange_SetSpkiSha256:
			if len(action.SetSpkiSha256) == 0 {
				return nil, invalid("cert_pin.set_spki_sha256: must not be empty; clearing the pin requires clear")
			}
			opts = append(opts, store.WithCertPin(action.SetSpkiSha256))
		case *heronv1.CertPinChange_Clear:
			opts = append(opts, store.ClearCertPin())
		default:
			return nil, invalid("cert_pin: set_spki_sha256 or clear is required")
		}
	}
	if len(req.GetExpectedConfigId()) > 0 {
		opts = append(opts, store.WithExpectedConfigID(req.GetExpectedConfigId()))
	}
	return opts, nil
}

func (s *Service) ListProbeCertificates(ctx context.Context, req *connect.Request[heronv1.ListProbeCertificatesRequest]) (*connect.Response[heronv1.ListProbeCertificatesResponse], error) {
	if err := checkComparisonTask(req.Msg.GetTaskId()); err != nil {
		return nil, err
	}
	view, err := s.store.ListProbeCertificates(ctx, req.Msg.GetTaskId())
	if err != nil {
		s.log.Error("listing probe certificates failed", "err", err)
		return nil, internalError("listing probe certificates failed")
	}
	if !view.Found {
		return nil, noComparisonTask()
	}
	if p, scoped := store.Principal(ctx); scoped && !p.AllowsSelector(view.Task.AllNodes, view.Task.SelectorTags, view.Task.NodeIDs) {
		return nil, noComparisonTask()
	}
	resp := &heronv1.ListProbeCertificatesResponse{ConfigId: view.Task.Task.GetConfigId(), CertSpkiSha256: append([]byte(nil), view.Task.Task.GetCertSpkiSha256()...)}
	for _, n := range view.Visible {
		node := &heronv1.NodeProbeCertificate{NodeId: n.NodeID}
		if n.Current != nil {
			node.Current = &heronv1.ProbeCertificateObservation{NotAfterS: n.Current.NotAfter, ObservedAt: n.Current.ObservedAt}
		}
		if n.Candidate != nil {
			node.Candidate = &heronv1.ProbeCertificateCandidate{SpkiSha256: n.Candidate.SPKI, NotAfterS: n.Candidate.NotAfter, Reason: heronv1.PresentedReason(n.Candidate.Reason), ObservedAt: n.Candidate.ObservedAt}
		}
		resp.Nodes = append(resp.Nodes, node)
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) DeleteProbeTask(ctx context.Context, req *connect.Request[heronv1.DeleteProbeTaskRequest]) (*connect.Response[heronv1.DeleteProbeTaskResponse], error) {
	if err := checkTaskID(req.Msg.GetId(), "id"); err != nil {
		return nil, err
	}
	version, err := s.probes.Delete(ctx, req.Msg.GetId())
	if err != nil {
		return nil, s.operationError(err, "id", "deleting probe task failed")
	}
	return connect.NewResponse(&heronv1.DeleteProbeTaskResponse{Version: version}), nil
}

func (s *Service) QueryProbes(ctx context.Context, req *connect.Request[heronv1.QueryProbesRequest]) (*connect.Response[heronv1.QueryProbesResponse], error) {
	m := req.Msg
	maxPoints, err := checkWindow(m.GetFrom(), m.GetTo(), m.GetMaxPoints())
	if err != nil {
		return nil, err
	}
	_, details := s.visibleProbeTasks(ctx)
	tasks := make(map[uint64]*heronv1.ProbeTask, len(details))
	order := make([]uint64, 0, len(details))
	for _, d := range details {
		tasks[d.Task.Id] = d.Task
		order = append(order, d.Task.Id)
	}
	// 历史样本仍可读，但图例只引用当前可见的配置，任务迁出范围后不能泄露其新目标。
	resp, err := s.history.probeSeries(ctx, m, maxPoints, func(id uint64) (heronv1.ProbeKind, string, bool) {
		t, ok := tasks[id]
		return t.GetKind(), t.GetTarget(), ok
	}, order, func(ctx context.Context) error { return s.requireNode(ctx, m.GetNodeId()) })
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) ReorderProbeTasks(ctx context.Context, req *connect.Request[heronv1.ReorderProbeTasksRequest]) (*connect.Response[heronv1.ReorderProbeTasksResponse], error) {
	for _, id := range req.Msg.GetIds() {
		if err := checkTaskID(id, "ids"); err != nil {
			return nil, err
		}
	}
	if err := s.probes.Reorder(ctx, req.Msg.GetIds()); err != nil {
		if errors.Is(err, store.ErrBadOrder) {
			return nil, invalid("ids must list every probe task exactly once")
		}
		return nil, s.operationError(err, "ids", "reordering probe tasks failed")
	}
	return connect.NewResponse(&heronv1.ReorderProbeTasksResponse{}), nil
}
