package alert

import (
	"context"
	"errors"
	"fmt"

	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
)

type TrafficObservation struct {
	Current               bool
	QuotaBytes, UsedBytes uint64
	Percent, Threshold    float64
}

// NextTraffic 不引入 pending。Current 由提交周期的双边界检查提供，过期或墙钟回拨时
// 保留原状态，直到成功的账本写入发布当前周期观测，不能把未提交的滚动当成恢复。
func NextTraffic(cur store.AlertState, o TrafficObservation) (store.AlertState, *store.Transition) {
	if !o.Current {
		return cur, nil
	}
	if o.QuotaBytes == 0 || o.Percent < o.Threshold {
		return recovered(cur)
	}
	if cur == store.StateFiring {
		return cur, nil
	}
	tr := store.TransitionFiring
	return store.StateFiring, &tr
}

func quotaBytesText(n uint64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	v, i := float64(n), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.3g %s", v, units[i])
}

func trafficSummary(n store.Node, r store.AlertRule, o TrafficObservation, tr *store.Transition) string {
	if tr != nil && *tr == store.TransitionRecovered {
		if o.QuotaBytes == 0 {
			return fmt.Sprintf("节点 %s 已清除流量配额（规则 %s）", n.Name, r.Name)
		}
		return fmt.Sprintf("节点 %s 本周期流量用量 %.2f%% 已低于阈值 %.2f%%（规则 %s）", n.Name, o.Percent, r.Threshold, r.Name)
	}
	return fmt.Sprintf("节点 %s 本周期流量已用 %.2f%%（%s / %s，%s，规则 %s）", n.Name, o.Percent, quotaBytesText(o.UsedBytes), quotaBytesText(o.QuotaBytes), quotaModeText[n.TrafficQuotaMode], r.Name)
}

// quotaModeText 是配额口径在通知文案里的写法，流量告警与流量报告共用。
var quotaModeText = map[string]string{"sum": "收+发", "rx": "只收", "tx": "只发", "max": "收发取大者"}

func (e *Engine) SweepTraffic(ctx context.Context) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	return e.sweepTraffic(ctx, 0, 0)
}

// EvaluateTrafficNode 只观察命中节点，不能用局部候选集裁剪其它节点的持久状态。
func (e *Engine) EvaluateTrafficNode(ctx context.Context, nodeID int64) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	return e.sweepTraffic(ctx, 0, nodeID)
}

func (e *Engine) sweepTraffic(ctx context.Context, ruleID, nodeID int64) error {
	if e.traffic == nil {
		return nil
	}
	nodes, err := e.monitoringNodes(ctx)
	if err != nil {
		return err
	}
	cy := newCycle()
	defer e.flush(cy)
	return e.evaluateTraffic(ctx, cy, nodes, ruleID, nodeID)
}

// 全量巡检的 nodes 由 SweepOffline 同轮提供，不重复查询也不逐节点读库。
func (e *Engine) evaluateTraffic(ctx context.Context, cy *cycle, nodes []store.Node, ruleID, nodeID int64) error {
	if e.traffic == nil {
		return nil
	}
	committed := e.traffic.Committed()
	now := e.clk.Now()
	var errs []error
	for _, r := range e.Rules() {
		if !r.Enabled || r.Kind != store.KindTraffic || ruleID != 0 && ruleID != r.ID {
			continue
		}
		keep := map[int64]bool{}
		for _, n := range nodes {
			if !inScope(r, n.ID) || nodeID != 0 && nodeID != n.ID {
				continue
			}
			keep[n.ID] = true
			s, exists := committed[n.ID]
			current := !exists || !now.Before(s.PeriodStart) && now.Before(traffic.NextResetAfter(s.PeriodStart, n.TrafficResetDay, e.cfg.Location))
			used, pct, _ := traffic.Quota(traffic.Entry{State: s}, n.TrafficQuotaBytes, n.TrafficQuotaMode)
			o := TrafficObservation{Current: current, QuotaBytes: n.TrafficQuotaBytes, UsedBytes: used, Percent: pct, Threshold: r.Threshold}
			next, tr := NextTraffic(e.current(stateKey{r.ID, n.ID}), o)
			if !current {
				continue
			}
			if err := e.apply(ctx, cy, r, n.ID, next, false, n.Maintenance, "", tr, trafficSummary(n, r, o, tr), pct); err != nil {
				errs = append(errs, err)
			}
		}
		if nodeID == 0 {
			if err := e.pruneCandidates(ctx, r.ID, keep); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
