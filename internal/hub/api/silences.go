package api

import (
	"context"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/store"
)

var silenceKinds = map[heronv1.SilenceKind]store.SilenceKind{
	heronv1.SilenceKind_SILENCE_KIND_DAILY: store.SilenceDaily,
	heronv1.SilenceKind_SILENCE_KIND_ONCE:  store.SilenceOnce,
}

func silenceProto(s store.Silence) *heronv1.Silence {
	return &heronv1.Silence{Id: s.ID, Name: s.Name, Enabled: s.Enabled, AllNodes: s.AllNodes, AllRules: s.AllRules, RuleIds: s.RuleIDs, NodeIds: s.NodeIDs, SelectorTags: s.SelectorTags, Kind: enumFor(silenceKinds, s.Kind), StartHhmm: s.StartHHMM, EndHhmm: s.EndHHMM, FromAt: s.FromAt, UntilAt: s.UntilAt, Reason: s.Reason, CreatedAt: s.CreatedAt.Unix()}
}

// ListSilences 读库而不是引擎的内存快照：库是唯一事实来源。维护任务（PruneAlertEvents）不经引擎删掉
// 到期的一次性静默，内存快照在那之后会短暂留着它们——读库让列表、编辑与删除始终对得上。
func (s *Service) ListSilences(ctx context.Context, _ *connect.Request[heronv1.ListSilencesRequest]) (*connect.Response[heronv1.ListSilencesResponse], error) {
	silences, err := s.store.ListSilences(ctx)
	if err != nil {
		return nil, internalError("list silences")
	}
	now := s.clk.Now()
	out := &heronv1.ListSilencesResponse{Now: now.Unix()}
	for _, si := range silences {
		active := si.Enabled && (si.AllRules || len(si.RuleIDs) > 0) && alert.SilenceActive(si, now, s.cfg.Location)
		out.Silences = append(out.Silences, &heronv1.SilenceEntry{Silence: silenceProto(si), Active: active})
	}
	return connect.NewResponse(out), nil
}

func (s *Service) SaveSilence(ctx context.Context, req *connect.Request[heronv1.SaveSilenceRequest]) (*connect.Response[heronv1.SaveSilenceResponse], error) {
	p := req.Msg.GetSilence()
	kind, err := parseEnum(silenceKinds, p.GetKind(), "silence", "kind")
	if err != nil {
		return nil, err
	}
	tags, err := cleanTags("silence.selector_tags", p.GetSelectorTags())
	if err != nil {
		return nil, err
	}
	saved, err := s.alerts.SaveSilence(ctx, store.Silence{ID: p.GetId(), Name: p.GetName(), Enabled: p.GetEnabled(), AllNodes: p.GetAllNodes(), AllRules: p.GetAllRules(), RuleIDs: p.GetRuleIds(), NodeIDs: p.GetNodeIds(), SelectorTags: tags, Kind: kind, StartHHMM: p.GetStartHhmm(), EndHHMM: p.GetEndHhmm(), FromAt: p.GetFromAt(), UntilAt: p.GetUntilAt(), Reason: p.GetReason()})
	if err != nil {
		return nil, s.operationError(err, "silence", "saving silence failed")
	}
	return connect.NewResponse(&heronv1.SaveSilenceResponse{Silence: silenceProto(saved)}), nil
}

func (s *Service) DeleteSilence(ctx context.Context, req *connect.Request[heronv1.DeleteSilenceRequest]) (*connect.Response[heronv1.DeleteSilenceResponse], error) {
	if err := s.alerts.DeleteSilence(ctx, req.Msg.GetId()); err != nil {
		return nil, s.operationError(err, "id", "deleting silence failed")
	}
	return connect.NewResponse(&heronv1.DeleteSilenceResponse{}), nil
}
