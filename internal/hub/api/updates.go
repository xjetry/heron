package api

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/updates"
	"github.com/xjetry/heron/internal/update"
)

type localUpdateClient interface {
	Status(context.Context) update.Status
	Submit(context.Context, update.Request) (update.Job, error)
}
type releaseSource interface {
	Latest(context.Context) (string, error)
	BoundAgentVersion(ctx context.Context, tag string) (string, error)
}

func (s *Service) GetUpdates(ctx context.Context, req *connect.Request[heronv1.GetUpdatesRequest]) (*connect.Response[heronv1.GetUpdatesResponse], error) {
	out := &heronv1.GetUpdatesResponse{BoundAgentVersion: s.boundAgent()}
	if p, ok := store.Principal(ctx); !ok || p.AllNodes {
		local := update.StatusProto(s.updateLocal.Status(ctx), s.cfg.HubVersion)
		// hub 的任务记录属于本机更新器，hub 不改写它，只在投影时与节点用同一判定：运行版本已达到目标的
		// 未完成记录不再显示。本机更新器就是执行者，记录处于进行中即视为仍在执行。
		if updates.Superseded(local.Task, local.Version, update.ActiveState(local.Task.GetState())) {
			local.Task = nil
		}
		out.Targets = append(out.Targets, &heronv1.UpdateTarget{NodeId: 0, Status: local})
	}
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return nil, internalError("list update targets")
	}
	for _, n := range nodes {
		if n.Rented {
			// 已出租的节点没有 agent，不是更新目标；拒绝写在 store 的写事务里，这里不列出它。
			continue
		}
		status := &heronv1.UpdateStatus{Reason: "agent has not reported online update support"}
		if s.cfg.Updates != nil {
			status = s.cfg.Updates.Snapshot(n.ID)
		}
		out.Targets = append(out.Targets, &heronv1.UpdateTarget{NodeId: n.ID, Status: status})
	}
	if req.Msg.CheckLatest {
		check, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		out.LatestVersion, err = s.updateSource.Latest(check)
		if err != nil {
			out.CheckError = err.Error()
		} else {
			out.LatestBoundAgentVersion = s.latestBoundAgent(check, out.LatestVersion)
		}
	}
	return connect.NewResponse(out), nil
}

// latestBoundAgent 取最新正式版绑定的 agent 版本，取不到时返回空串表示未知。它不写 check_error：最新版本本身已经
// 取到，面板据此决定能否发起 hub 更新；绑定值只决定确认框怎么说明风险，未知由面板按"会变化"处理。三类失败的
// 处置相同，日志分开：资产缺失是该资产出现之前的 release 的常态，不合契约与读取失败才需要有人去看。
func (s *Service) latestBoundAgent(ctx context.Context, tag string) string {
	bound, err := s.updateSource.BoundAgentVersion(ctx, tag)
	switch {
	case err == nil:
		return bound
	case errors.Is(err, update.ErrBoundAgentMissing):
		s.log.Info("latest release publishes no AGENT_VERSION asset; its bound agent version is unknown", "version", tag)
	case errors.Is(err, update.ErrBoundAgentInvalid):
		s.log.Warn("latest release AGENT_VERSION asset is malformed; its bound agent version is unknown", "version", tag, "err", err)
	default:
		s.log.Warn("could not read the latest release AGENT_VERSION asset; its bound agent version is unknown", "version", tag, "err", err)
	}
	return ""
}

func (s *Service) StartUpdate(ctx context.Context, req *connect.Request[heronv1.StartUpdateRequest]) (*connect.Response[heronv1.StartUpdateResponse], error) {
	if req.Msg.NodeId < 0 || !update.ValidVersion(req.Msg.Version) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("positive node ID or zero for hub and canonical stable version required"))
	}
	var task *heronv1.UpdateTask
	if req.Msg.NodeId == 0 {
		if !update.Newer(req.Msg.Version, s.cfg.HubVersion) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("target must be newer than the running stable hub version"))
		}
		job, err := s.updateLocal.Submit(ctx, updates.NewRequest(req.Msg.Version, s.clk.Now()))
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		task = update.TaskProto(&job)
	} else {
		if s.cfg.Updates == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("node updates unavailable"))
		}
		var err error
		task, err = s.cfg.Updates.Start(ctx, req.Msg.NodeId, req.Msg.Version)
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
	}
	return connect.NewResponse(&heronv1.StartUpdateResponse{Task: task}), nil
}

func (s *Service) CancelUpdate(ctx context.Context, req *connect.Request[heronv1.CancelUpdateRequest]) (*connect.Response[heronv1.CancelUpdateResponse], error) {
	if req.Msg.NodeId <= 0 || len(req.Msg.Id) > 64 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("node ID and task ID required; hub updates cannot be cancelled"))
	}
	if s.cfg.Updates == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("node updates unavailable"))
	}
	if err := s.cfg.Updates.Cancel(ctx, req.Msg.NodeId, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&heronv1.CancelUpdateResponse{}), nil
}
