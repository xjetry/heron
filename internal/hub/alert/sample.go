package alert

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
)

// SampleChannelLimit 是一份样例至多发往的渠道数。试投递在一次请求里同步发往每个渠道，每个至多 NotifyTimeout，
// 这个上界与并发发送一起给出请求时长的上界；预览不出网，同一上界只为两种入口口径一致。
const SampleChannelLimit = 16

// Sample 是一条规则（可以是未保存的草稿）对一个节点在某一阶段会发出的告警消息，以及它要发往的已保存渠道。
type Sample struct {
	Message  Message
	Channels []store.NotifyChannel
}

// CheckSamplePhase 是样例阶段的唯一判定：只有 firing 与 recovered 两个阶段有消息可发。错误是路径为 transition 的
// FieldError；协议层在解析规则与查节点之前调用它，Sample 自己也调用，不另写一份比较。
func CheckSamplePhase(phase store.Transition) error {
	if phase != store.TransitionFiring && phase != store.TransitionRecovered {
		return oneOf("transition", string(phase), string(store.TransitionFiring), string(store.TransitionRecovered))
	}
	return nil
}

// Sample 给出规则 r 对节点 n 在 phase（firing 或 recovered，见 CheckSamplePhase）阶段会发出的消息。
//
// 消息与真实投递同形：摘要由各种类评估用的同一个文案函数写出，观测由规则自己的阈值、提前天数与节点的配置推出（见
// sampleEvent），消息由 eventMessage 组装——与 Queue.message 给单事件批次的同一个函数。r 先过 CheckRule，与保存同一套
// 字段校验；作用域与引用的任务是否存在不影响消息，留给保存时裁决。
//
// 渠道只能是库里已保存的：r.ChannelIDs 按引擎的渠道快照逐个解析，不认识的 id 即 NotFound。样例因此不会发往请求里
// 临时给出的地址——出站目标只来自保存时经过 CheckChannel 的配置。重复的 id 合并（与 store 保存规则时同一口径），
// 合并后须有 1 到 SampleChannelLimit 个。
func (e *Engine) Sample(ctx context.Context, r store.AlertRule, n store.Node, phase store.Transition) (Sample, error) {
	if err := CheckSamplePhase(phase); err != nil {
		return Sample{}, err
	}
	if err := CheckRule(r); err != nil {
		return Sample{}, err
	}
	channels, err := e.sampleChannels(r.ChannelIDs)
	if err != nil {
		return Sample{}, err
	}
	ev, err := e.sampleEvent(ctx, r, n, phase)
	if err != nil {
		return Sample{}, err
	}
	return Sample{Message: eventMessage(r.Name, string(r.Kind), n.Name, ev), Channels: channels}, nil
}

func (e *Engine) sampleChannels(ids []int64) ([]store.NotifyChannel, error) {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(ids) == 0 || len(ids) > SampleChannelLimit {
		return nil, invalid("channel_ids", "must name between 1 and %d saved notify channels", SampleChannelLimit)
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]store.NotifyChannel, 0, len(ids))
	for _, id := range ids {
		c, ok := e.channels[id]
		if !ok {
			return nil, store.NotFoundError{Kind: store.ObjectNotifyChannel, ID: id}
		}
		out = append(out, c)
	}
	return out, nil
}

// sampleFiringDuration 是样例恢复文案里的告警持续时长。真实恢复写的是这一次 firing 的实际时长，样例没有这一次，
// 取一个固定值，只为让恢复文案带上与真实相同的尾巴（withFiringDuration）。
const sampleFiringDuration = 30 * time.Minute

// sampleEvent 是样例的事件：触发取刚好越过阈值的那一点，恢复取回到正常范围内的一个值（各种类分支写明取哪个），文案由
// 该种类评估用的同一个函数写出，恢复文案追加 apply 同样追加的告警持续时长。At 是此刻。离线样例按"本次启动后上报过"的
// 形态写已离线时长；hub 重启后从未上报的节点真实文案写最后上报时刻，样例不覆盖那一支。
func (e *Engine) sampleEvent(ctx context.Context, r store.AlertRule, n store.Node, phase store.Transition) (store.AlertEvent, error) {
	tr := &phase
	ev := store.AlertEvent{Transition: phase, At: e.clk.Now()}
	var rd reading
	switch r.Kind {
	case store.KindOffline:
		// 刚好触发的已离线时长：至少 TTL，且不短于节点宽限（NextOffline）。恢复时节点刚刚上报，时长为零。
		unseen := max(e.cfg.TTL, Observation{Grace: time.Duration(n.OfflineGraceS) * time.Second}.grace())
		if phase == store.TransitionRecovered {
			unseen = 0
		}
		ev.Summary, rd = offlineSummary(n, r, true, unseen, tr), observedValue(unseen.Seconds())
	case store.KindProbe:
		// 固定阈值：触发取阈值本身（触发含等号），恢复取阈值的一半。相对判定：触发取带的上沿（含等号），恢复取基线。
		var band rttBand
		value := r.Threshold
		if r.RttMode == store.RttRelative {
			var err error
			if band, err = e.sampleBand(ctx, r, n.ID); err != nil {
				return store.AlertEvent{}, err
			}
			value = band.hi
			if phase == store.TransitionRecovered {
				value = band.baselineMs
			}
		} else if phase == store.TransitionRecovered {
			value = r.Threshold / 2
		}
		ev.Summary, rd = probeSummary(n, r, band, value), observedValue(value)
	case store.KindResource:
		ev.Summary, rd = resourceSummary(n, r, tr, e.fullestMount(r, n.ID)), observedValue(r.Threshold)
		if phase == store.TransitionRecovered {
			rd = observedValue(r.RecoveryThreshold)
		}
	case store.KindTraffic:
		// 节点没设配额时真实评估不会触发（NextTraffic），样例代之以 sampleQuotaBytes，只为让文案带上配额的形状。
		quota, mode := n.TrafficQuotaBytes, n.TrafficQuotaMode
		if quota == 0 {
			quota, mode = sampleQuotaBytes, "sum"
		}
		n.TrafficQuotaMode = mode
		pct := r.Threshold
		if phase == store.TransitionRecovered {
			pct = r.Threshold / 2
		}
		o := TrafficObservation{Current: true, QuotaBytes: quota, UsedBytes: uint64(float64(quota) * pct / 100), Percent: pct, Threshold: r.Threshold}
		ev.Summary, rd = trafficSummary(n, r, o, tr)
	case store.KindExpiry, store.KindCertExpiry:
		// 到期日取刚进入提醒窗口的那一天（今天 + 提前天数，按 hub 时区的日历）。恢复取"已不在提醒窗口内"那一支：
		// 触发时记下的到期日与当前相同。
		expiresOn := Today(ev.At, e.cfg.Location).AddDate(0, 0, r.DaysBefore).Format(time.DateOnly)
		o := ExpiryObservation{HasExpiry: true, DaysLeft: r.DaysBefore, DaysBefore: r.DaysBefore}
		if r.Kind == store.KindExpiry {
			n.Billing.ExpiresOn = expiresOn
			ev.Summary, rd = expirySummary(n, r, o, tr, expiresOn)
		} else {
			ev.Summary, rd = certSummary(n, r, expiresOn, o, tr, expiresOn)
		}
	default:
		// Sample 先过 CheckRule，表外种类到不了这里；新增种类而漏了这里的分支时，样例以内部错误失败而不是给出错的文案。
		return store.AlertEvent{}, fmt.Errorf("no sample for rule kind %q", r.Kind)
	}
	// 值与单位经真实事件同一处（eventValue）成形，样例消息的 Value、ValueUnit 与真实投递同口径。
	var err error
	if ev.Value, ev.ValueUnit, err = eventValue(r, rd); err != nil {
		return store.AlertEvent{}, err
	}
	if phase == store.TransitionRecovered {
		ev.Summary = withFiringDuration(ev.Summary, sampleFiringDuration, atRecovery)
	}
	return ev, nil
}

// sampleQuotaBytes 是节点没设流量配额时样例代用的配额（1 TiB）。
const sampleQuotaBytes = 1 << 40

// sampleBaselineMs 是相对判定的样例在没有可用基线时代用的基线：自适应基线还没算出（草稿未保存、桶数不足、任务指纹
// 已变）时真实评估既不触发也不恢复，样例代之以它，只为让文案带上正常带的形状。
const sampleBaselineMs = 100

// sampleBand 是相对判定样例的正常带：有可用的基线（固定基线恒可用；自适应基线按规则 id 读库里的行，见 relativeBand）
// 就用它，否则以 sampleBaselineMs 为基线。
func (e *Engine) sampleBand(ctx context.Context, r store.AlertRule, nodeID int64) (rttBand, error) {
	var baselines store.Baselines
	if r.AdaptiveBaseline() {
		var err error
		if baselines, err = e.st.ReadAlertBaselines(ctx); err != nil {
			return rttBand{}, err
		}
	}
	if band, ok := relativeBand(r, baselines, nodeID); ok {
		return band, nil
	}
	return bandAround(r, sampleBaselineMs), nil
}
