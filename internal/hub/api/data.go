package api

import (
	"context"
	"time"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// GetSnapshot 的在线判定见 liveState。
func (s *Service) GetSnapshot(ctx context.Context, _ *connect.Request[heronv1.GetSnapshotRequest]) (*connect.Response[heronv1.GetSnapshotResponse], error) {
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		s.log.Error("listing nodes failed", "err", err)
		return nil, internalError("listing nodes failed")
	}
	out := &heronv1.GetSnapshotResponse{Now: s.clk.Now().Unix(), ReportIntervalMs: uint32(s.cfg.ReportInterval / time.Millisecond), HubVersion: s.cfg.HubVersion, BoundAgentVersion: s.boundAgent()}
	for _, n := range nodes {
		st := &heronv1.NodeStatus{Id: n.ID, Name: n.Name, Traffic: trafficProto(s.traffic.View(n.ID), n)}
		st.Online, st.LastSeenAt, st.Metrics = liveState(s.live, n)
		out.Nodes = append(out.Nodes, st)
	}
	return connect.NewResponse(out), nil
}

func (s *Service) QueryMetrics(ctx context.Context, req *connect.Request[heronv1.QueryMetricsRequest]) (*connect.Response[heronv1.QueryMetricsResponse], error) {
	m := req.Msg
	maxPoints, err := checkWindow(m.GetFrom(), m.GetTo(), m.GetMaxPoints())
	if err != nil {
		return nil, err
	}
	resp, err := s.history.metrics(ctx, m, maxPoints, func(ctx context.Context) error { return s.requireNode(ctx, m.GetNodeId()) })
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// requireNode 是管理端历史查询的节点准入：不存在即 NotFound，错误写出 id。
func (s *Service) requireNode(ctx context.Context, id int64) error {
	exists, err := s.store.NodeExists(ctx, id)
	if err != nil {
		s.log.Error("looking up node failed", "err", err)
		return internalError("looking up node failed")
	}
	if !exists {
		return notFound(id)
	}
	return nil
}
