package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/store"
	"google.golang.org/protobuf/proto"
)

var alertKinds = map[heronv1.AlertKind]store.AlertKind{
	heronv1.AlertKind_ALERT_KIND_OFFLINE:     store.KindOffline,
	heronv1.AlertKind_ALERT_KIND_PROBE:       store.KindProbe,
	heronv1.AlertKind_ALERT_KIND_EXPIRY:      store.KindExpiry,
	heronv1.AlertKind_ALERT_KIND_RESOURCE:    store.KindResource,
	heronv1.AlertKind_ALERT_KIND_CERT_EXPIRY: store.KindCertExpiry,
	heronv1.AlertKind_ALERT_KIND_TRAFFIC:     store.KindTraffic,
}
var resourceMetrics = map[heronv1.ResourceMetric]store.ResourceMetric{
	heronv1.ResourceMetric_RESOURCE_METRIC_MEMORY_USED_PCT: store.MetricMemoryUsedPct,
	heronv1.ResourceMetric_RESOURCE_METRIC_DISK_USED_PCT:   store.MetricDiskUsedPct,
	heronv1.ResourceMetric_RESOURCE_METRIC_CPU_PCT:         store.MetricCpuPct,
	heronv1.ResourceMetric_RESOURCE_METRIC_LOAD1_PER_CORE:  store.MetricLoad1PerCore,
	heronv1.ResourceMetric_RESOURCE_METRIC_NET_RX_BPS:      store.MetricNetRxBps,
	heronv1.ResourceMetric_RESOURCE_METRIC_NET_TX_BPS:      store.MetricNetTxBps,
}
var probeMetrics = map[heronv1.ProbeMetric]store.ProbeMetric{
	heronv1.ProbeMetric_PROBE_METRIC_LOSS_PCT: store.MetricLossPct,
	heronv1.ProbeMetric_PROBE_METRIC_RTT_MS:   store.MetricRttMs,
}
var rttModes = map[heronv1.RttMode]store.RttMode{
	heronv1.RttMode_RTT_MODE_THRESHOLD: store.RttThreshold,
	heronv1.RttMode_RTT_MODE_RELATIVE:  store.RttRelative,
}
var baselineModes = map[heronv1.BaselineMode]store.BaselineMode{
	heronv1.BaselineMode_BASELINE_MODE_ADAPTIVE: store.BaselineAdaptive,
	heronv1.BaselineMode_BASELINE_MODE_FIXED:    store.BaselineFixed,
}
var channelKinds = map[heronv1.ChannelKind]store.ChannelKind{
	heronv1.ChannelKind_CHANNEL_KIND_TELEGRAM: store.ChannelTelegram,
	heronv1.ChannelKind_CHANNEL_KIND_WEBHOOK:  store.ChannelWebhook,
}

func enumFor[K comparable, V comparable](values map[K]V, value V) K {
	for key, v := range values {
		if v == value {
			return key
		}
	}
	var zero K
	return zero
}

func ruleProto(r store.AlertRule) *heronv1.AlertRule {
	return &heronv1.AlertRule{Id: r.ID, Name: r.Name, Kind: enumFor(alertKinds, r.Kind), Enabled: r.Enabled, AllNodes: r.AllNodes, NodeIds: r.NodeIDs, ChannelIds: r.ChannelIDs, TaskId: r.TaskID, Metric: enumFor(probeMetrics, r.Metric), Threshold: r.Threshold, ForMinutes: uint32(r.ForMinutes), DaysBefore: uint32(r.DaysBefore), CreatedAt: r.CreatedAt.Unix(), ResourceMetric: enumFor(resourceMetrics, r.ResourceMetric), RecoveryThreshold: r.RecoveryThreshold, SelectorTags: r.SelectorTags,
		RttMode: enumFor(rttModes, r.RttMode), BaselineMode: enumFor(baselineModes, r.BaselineMode), BaselineWindowS: uint32(r.BaselineWindowS), BaselineMinSamples: uint32(r.BaselineMinSamples),
		UpperDeviationPct: r.UpperDeviationPct, LowerDeviationPct: r.LowerDeviationPct, CooldownS: uint32(r.CooldownS), FixedBaselineMs: r.FixedBaselineMs}
}

func (s *Service) ListAlertRules(ctx context.Context, _ *connect.Request[heronv1.ListAlertRulesRequest]) (*connect.Response[heronv1.ListAlertRulesResponse], error) {
	rules, err := s.store.ListVisibleAlertRules(ctx)
	if err != nil {
		return nil, internalError("list alert rules")
	}
	out := &heronv1.ListAlertRulesResponse{}
	visible := map[int64]bool{}
	for _, r := range rules {
		visible[r.ID] = true
		out.Rules = append(out.Rules, ruleProto(r))
	}
	for _, state := range s.alerts.States() {
		if !visible[state.RuleID] {
			continue
		}
		if p, ok := store.Principal(ctx); ok && !p.AllowsNode(state.NodeID) {
			continue
		}
		out.States = append(out.States, &heronv1.AlertStateEntry{RuleId: state.RuleID, NodeId: state.NodeID, State: string(state.State), SinceAt: state.SinceAt.Unix(), Flapping: state.Flapping, Silenced: state.FiredSilenced})
	}
	return connect.NewResponse(out), nil
}

func (s *Service) SaveAlertRule(ctx context.Context, req *connect.Request[heronv1.SaveAlertRuleRequest]) (*connect.Response[heronv1.SaveAlertRuleResponse], error) {
	r := req.Msg.GetRule()
	if err := checkTaskID(r.GetTaskId(), "rule.task_id"); err != nil {
		return nil, err
	}
	kind, err := parseEnum(alertKinds, r.GetKind(), "rule", "kind")
	if err != nil {
		return nil, err
	}
	var metric store.ProbeMetric
	// 只有探测规则使用指标。其余种类显式传入的指标也先按协议枚举解析，表外值在这里以协议词汇报错；表内值由
	// alert.CheckRule 以"非探测规则必须不指定"拒绝。
	if kind == store.KindProbe || r.GetMetric() != heronv1.ProbeMetric_PROBE_METRIC_UNSPECIFIED {
		metric, err = parseEnum(probeMetrics, r.GetMetric(), "rule", "metric")
		if err != nil {
			return nil, err
		}
	}
	var resourceMetric store.ResourceMetric
	if kind == store.KindResource || r.GetResourceMetric() != heronv1.ResourceMetric_RESOURCE_METRIC_UNSPECIFIED {
		resourceMetric, err = parseEnum(resourceMetrics, r.GetResourceMetric(), "rule", "resource_metric")
		if err != nil {
			return nil, err
		}
	}
	// rtt_mode 的零值是"没发这个字段"：rtt 规则按固定阈值收（不带它的旧客户端保存的就是固定阈值规则），存储层显式落值、
	// 回显 RTT_MODE_THRESHOLD。其余规则的非零值同样先按协议枚举解析，表外值以协议词汇报错，表内值由 store.CheckKindFields
	// 以"非 rtt 规则必须不指定"拒绝。
	var rttMode store.RttMode
	if r.GetRttMode() != heronv1.RttMode_RTT_MODE_UNSPECIFIED {
		rttMode, err = parseEnum(rttModes, r.GetRttMode(), "rule", "rtt_mode")
		if err != nil {
			return nil, err
		}
	} else if kind == store.KindProbe && metric == store.MetricRttMs {
		rttMode = store.RttThreshold
	}
	// baseline_mode 没有旧客户端：零值就是未指定，相对判定下由 store.CheckKindFields 拒绝。
	var baselineMode store.BaselineMode
	if r.GetBaselineMode() != heronv1.BaselineMode_BASELINE_MODE_UNSPECIFIED {
		baselineMode, err = parseEnum(baselineModes, r.GetBaselineMode(), "rule", "baseline_mode")
		if err != nil {
			return nil, err
		}
	}
	tags, err := cleanTags("rule.selector_tags", r.GetSelectorTags())
	if err != nil {
		return nil, err
	}
	saved, err := s.alerts.SaveRule(ctx, store.AlertRule{ID: r.GetId(), Name: r.GetName(), Kind: kind, Enabled: r.GetEnabled(), AllNodes: r.GetAllNodes(), NodeIDs: r.GetNodeIds(), ChannelIDs: r.GetChannelIds(), TaskID: r.GetTaskId(), Metric: metric, Threshold: r.GetThreshold(), ForMinutes: int(r.GetForMinutes()), DaysBefore: int(r.GetDaysBefore()), ResourceMetric: resourceMetric, RecoveryThreshold: r.GetRecoveryThreshold(), SelectorTags: tags,
		RttMode: rttMode, BaselineMode: baselineMode, BaselineWindowS: int(r.GetBaselineWindowS()), BaselineMinSamples: int(r.GetBaselineMinSamples()),
		UpperDeviationPct: r.GetUpperDeviationPct(), LowerDeviationPct: r.GetLowerDeviationPct(), CooldownS: int(r.GetCooldownS()), FixedBaselineMs: r.GetFixedBaselineMs()})
	if err != nil {
		return nil, s.operationError(err, "rule", "saving alert rule failed")
	}
	return connect.NewResponse(&heronv1.SaveAlertRuleResponse{Rule: ruleProto(saved)}), nil
}

func (s *Service) DeleteAlertRule(ctx context.Context, req *connect.Request[heronv1.DeleteAlertRuleRequest]) (*connect.Response[heronv1.DeleteAlertRuleResponse], error) {
	if err := s.alerts.DeleteRule(ctx, req.Msg.GetId()); err != nil {
		return nil, s.operationError(err, "id", "deleting alert rule failed")
	}
	return connect.NewResponse(&heronv1.DeleteAlertRuleResponse{}), nil
}

func channelProto(c store.NotifyChannel) (*heronv1.NotifyChannel, error) {
	rate := uint32(c.RatePerMinute)
	out := &heronv1.NotifyChannel{Id: c.ID, Name: c.Name, Kind: enumFor(channelKinds, c.Kind), CreatedAt: c.CreatedAt.Unix(), RatePerMinute: &rate}
	switch c.Kind {
	case store.ChannelTelegram:
		var cfg alert.TelegramConfig
		if err := json.Unmarshal([]byte(c.Config), &cfg); err != nil {
			return nil, err
		}
		// 明文只供 Engine 合并与 Queue 投递；所有 API 回显共用此出口以免泄漏。
		out.Telegram = &heronv1.TelegramConfig{HasBotToken: cfg.BotToken != "", ChatId: cfg.ChatID}
	case store.ChannelWebhook:
		var cfg alert.WebhookConfig
		if err := json.Unmarshal([]byte(c.Config), &cfg); err != nil {
			return nil, err
		}
		u, err := url.Parse(cfg.URL)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(cfg.Headers))
		for k := range cfg.Headers {
			names = append(names, k)
		}
		sort.Strings(names)
		out.Webhook = &heronv1.WebhookConfig{HasUrl: cfg.URL != "", UrlHost: u.Scheme + "://" + u.Host, HeaderNames: names, Method: cfg.Method, BodyTemplate: cfg.BodyTemplate}
	}
	return out, nil
}

func (s *Service) ListNotifyChannels(_ context.Context, _ *connect.Request[heronv1.ListNotifyChannelsRequest]) (*connect.Response[heronv1.ListNotifyChannelsResponse], error) {
	out := &heronv1.ListNotifyChannelsResponse{}
	for _, c := range s.alerts.Channels() {
		p, err := channelProto(c)
		if err != nil {
			return nil, s.operationError(err, "channel", "reading notify channel failed")
		}
		out.Channels = append(out.Channels, p)
	}
	return connect.NewResponse(out), nil
}

func (s *Service) SaveNotifyChannel(ctx context.Context, req *connect.Request[heronv1.SaveNotifyChannelRequest]) (*connect.Response[heronv1.SaveNotifyChannelResponse], error) {
	c := req.Msg.GetChannel()
	kind, err := parseEnum(channelKinds, c.GetKind(), "channel", "kind")
	if err != nil {
		return nil, err
	}
	var config any
	switch c.GetKind() {
	case heronv1.ChannelKind_CHANNEL_KIND_TELEGRAM:
		t := c.GetTelegram()
		config = alert.TelegramConfig{BotToken: t.GetBotToken(), ChatID: t.GetChatId()}
	case heronv1.ChannelKind_CHANNEL_KIND_WEBHOOK:
		w := c.GetWebhook()
		config = alert.WebhookConfig{URL: w.GetUrl(), Method: w.GetMethod(), Headers: w.GetHeaders(), BodyTemplate: w.GetBodyTemplate(), RemoveHeaders: w.GetRemoveHeaders()}
	}
	b, err := json.Marshal(config)
	if err != nil {
		return nil, s.operationError(err, "channel", "encoding notify channel failed")
	}
	rate := alert.DefaultRatePerMinute(kind)
	if c.RatePerMinute != nil {
		rate = int(c.GetRatePerMinute())
	}
	saved, err := s.alerts.SaveChannel(ctx, store.NotifyChannel{ID: c.GetId(), Name: c.GetName(), Kind: kind, Config: string(b), RatePerMinute: rate})
	if err != nil {
		return nil, s.operationError(err, "channel", "saving notify channel failed")
	}
	out, err := channelProto(saved)
	if err != nil {
		return nil, s.operationError(err, "channel", "reading saved notify channel failed")
	}
	return connect.NewResponse(&heronv1.SaveNotifyChannelResponse{Channel: out}), nil
}

func (s *Service) DeleteNotifyChannel(ctx context.Context, req *connect.Request[heronv1.DeleteNotifyChannelRequest]) (*connect.Response[heronv1.DeleteNotifyChannelResponse], error) {
	if err := s.alerts.DeleteChannel(ctx, req.Msg.GetId()); err != nil {
		return nil, s.operationError(err, "id", "deleting notify channel failed")
	}
	return connect.NewResponse(&heronv1.DeleteNotifyChannelResponse{}), nil
}

func (s *Service) TestNotifyChannel(ctx context.Context, req *connect.Request[heronv1.TestNotifyChannelRequest]) (*connect.Response[heronv1.TestNotifyChannelResponse], error) {
	for _, c := range s.alerts.Channels() {
		if c.ID == req.Msg.GetId() {
			if err := s.notifier.SendTest(ctx, c); err != nil {
				code := connect.CodeFailedPrecondition
				if _, retry := alert.Classify(err); retry {
					code = connect.CodeUnavailable
				}
				return nil, connect.NewError(code, err)
			}
			return connect.NewResponse(&heronv1.TestNotifyChannelResponse{}), nil
		}
	}
	return nil, s.operationError(store.NotFoundError{Kind: store.ObjectNotifyChannel, ID: req.Msg.GetId()}, "id", "reading notify channel failed")
}

func (s *Service) ListAlertEvents(ctx context.Context, req *connect.Request[heronv1.ListAlertEventsRequest]) (*connect.Response[heronv1.ListAlertEventsResponse], error) {
	limit := req.Msg.GetLimit()
	if limit == 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	events, err := s.store.ListAlertEvents(ctx, req.Msg.GetNodeId(), req.Msg.GetBeforeId(), int(limit))
	if err != nil {
		return nil, s.operationError(err, "", "listing alert events failed")
	}
	out := &heronv1.ListAlertEventsResponse{}
	for _, ev := range events {
		p := &heronv1.AlertEvent{Id: ev.ID, RuleId: ev.RuleID, NodeId: ev.NodeID, Transition: string(ev.Transition), At: ev.At.Unix(), Summary: ev.Summary, Value: ev.Value, Silenced: ev.Silenced}
		for _, d := range ev.Deliveries {
			// 只读口径：原文可能含接收方回显的密钥，只给类别与状态码（GetAlertDeliveryError 是原文的唯一出口）。
			failure, err := deliveryFailureProto(d.Failure)
			if err != nil {
				return nil, s.operationError(err, "", "listing alert events failed")
			}
			v := &heronv1.AlertDelivery{Id: d.ID, ChannelId: d.ChannelID, Attempts: uint32(d.Attempts), Ok: d.OK, Done: d.Done, Failure: failure}
			if d.Failure == store.FailureHTTPStatus {
				v.HttpStatus = proto.Uint32(uint32(d.HTTPStatus))
			}
			if !d.DeliveredAt.IsZero() {
				v.DeliveredAt = proto.Int64(d.DeliveredAt.Unix())
			}
			p.Deliveries = append(p.Deliveries, v)
		}
		out.Events = append(out.Events, p)
	}
	return connect.NewResponse(out), nil
}

// UNSPECIFIED 在协议上的含义是"没有失败"；库里出现表外的类别说明读写口径不一致，
// 返回 internal，不能让一次失败被读成没有失败。
func deliveryFailureProto(f store.DeliveryFailure) (heronv1.DeliveryFailure, error) {
	switch f {
	case store.FailureNone:
		return heronv1.DeliveryFailure_DELIVERY_FAILURE_UNSPECIFIED, nil
	case store.FailureHTTPStatus:
		return heronv1.DeliveryFailure_DELIVERY_FAILURE_HTTP_STATUS, nil
	case store.FailureTransport:
		return heronv1.DeliveryFailure_DELIVERY_FAILURE_TRANSPORT, nil
	case store.FailureRequest:
		return heronv1.DeliveryFailure_DELIVERY_FAILURE_REQUEST, nil
	case store.FailureChannelInvalid:
		return heronv1.DeliveryFailure_DELIVERY_FAILURE_CHANNEL_INVALID, nil
	case store.FailureChannelDeleted:
		return heronv1.DeliveryFailure_DELIVERY_FAILURE_CHANNEL_DELETED, nil
	case store.FailureResultUnrecorded:
		return heronv1.DeliveryFailure_DELIVERY_FAILURE_RESULT_UNRECORDED, nil
	case store.FailureUnclassified:
		return heronv1.DeliveryFailure_DELIVERY_FAILURE_UNCLASSIFIED, nil
	}
	return 0, fmt.Errorf("unknown delivery failure %q", f)
}

func (s *Service) GetAlertDeliveryError(ctx context.Context, req *connect.Request[heronv1.GetAlertDeliveryErrorRequest]) (*connect.Response[heronv1.GetAlertDeliveryErrorResponse], error) {
	text, err := s.store.GetDeliveryError(ctx, req.Msg.GetDeliveryId())
	if err != nil {
		return nil, s.operationError(err, "delivery_id", "reading delivery error failed")
	}
	return connect.NewResponse(&heronv1.GetAlertDeliveryErrorResponse{Error: text}), nil
}
