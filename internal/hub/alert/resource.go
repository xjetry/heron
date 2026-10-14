package alert

import (
	"context"
	"errors"
	"fmt"

	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
)

// NextResource 的触发与恢复各需完整的连续窗口；滞回区间保持 firing，缺失读数不能证明健康。
func NextResource(current store.AlertState, samples []MinuteSample, recovery float64) (store.AlertState, *store.Transition) {
	if len(samples) == 0 {
		return current, nil
	}
	bad, healthy := true, true
	for _, sample := range samples {
		bad = bad && sample.Present && sample.Exceeds
		healthy = healthy && sample.Present && sample.Value <= recovery
	}
	if current == store.StateFiring {
		if healthy {
			t := store.TransitionRecovered
			return store.StateOK, &t
		}
		return current, nil
	}
	if bad {
		t := store.TransitionFiring
		return store.StateFiring, &t
	}
	last := samples[len(samples)-1]
	if last.Present && last.Exceeds {
		return store.StatePending, nil
	}
	return store.StateOK, nil
}

// resourceIndex 把资源指标映射到取值列；常量值与列名相同的指标按名命中。
// cpu_pct 是语义名，列名是 cpu。load1_per_core 就是列名：值已是按核负载，这里不再换算。
func resourceIndex(m store.ResourceMetric) int {
	switch m {
	case store.MetricCpuPct:
		return metric.Index("cpu")
	default:
		return metric.Index(string(m))
	}
}

func resourceLabel(m store.ResourceMetric) string {
	switch m {
	case store.MetricMemoryUsedPct:
		return "内存"
	case store.MetricDiskUsedPct:
		return "磁盘"
	case store.MetricCpuPct:
		return "CPU"
	case store.MetricLoad1PerCore:
		return "每核负载"
	case store.MetricNetRxBps:
		return "下行速率"
	case store.MetricNetTxBps:
		return "上行速率"
	default:
		// 未知值由 CheckRule 挡在保存与载入之外；到不了这里，露出原值而不是误标成某个已知指标。
		return string(m)
	}
}

// 通知数值的单位随指标：百分比与每核负载按原值，字节速率换算成 Mbps（与面板输入同一口径，1 Mbps = 125000 bytes/s）。
func resourceValueText(m store.ResourceMetric, v float64) string {
	switch m {
	case store.MetricLoad1PerCore:
		return fmt.Sprintf("%.2f", v)
	case store.MetricNetRxBps, store.MetricNetTxBps:
		return fmt.Sprintf("%.1f Mbps", v*8/1e6)
	default:
		return fmt.Sprintf("%.1f%%", v)
	}
}

func (e *Engine) EvaluateResources(ctx context.Context, minuteTS int64) error {
	if minuteTS%60 != 0 {
		return invalid("minuteTS", "must be aligned to 60 seconds")
	}
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	cy := newCycle()
	defer e.flush(cy)
	nodes, err := e.st.ListMonitoringNodes(ctx)
	if err != nil {
		return err
	}
	lv, _ := store.LevelByName("1m")
	var errs []error
	for _, rule := range e.Rules() {
		if !rule.Enabled || rule.Kind != store.KindResource {
			continue
		}
		keep := map[int64]bool{}
		for _, node := range nodes {
			if !inScope(rule, node.ID) {
				continue
			}
			keep[node.ID] = true
			from := minuteTS - int64(rule.ForMinutes-1)*60
			rows, err := e.history.QueryMetrics(ctx, node.ID, from, minuteTS+60, lv, 60)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			// 按核负载读 load1_per_core 列的分钟均值。agent 没报这一列时 n=0，这一分钟无读数，
			// 既不触发也不恢复，也不退回 load1。分母变化不会改写已经入库的分钟：当时的商就在这一列里。
			samples := make([]MinuteSample, rule.ForMinutes)
			index := resourceIndex(rule.ResourceMetric)
			for _, row := range rows {
				value, present := row.Bucket.Mean(index)
				samples[(row.TS-from)/60] = MinuteSample{Present: present, Value: value, Exceeds: value >= rule.Threshold}
			}
			next, transition := NextResource(e.current(stateKey{rule.ID, node.ID}), samples, rule.RecoveryThreshold)
			value := samples[len(samples)-1].Value
			label := resourceLabel(rule.ResourceMetric)
			summary := fmt.Sprintf("节点 %s %s持续 %d 分钟达到 %s（规则 %s）", node.Name, label, rule.ForMinutes, resourceValueText(rule.ResourceMetric, rule.Threshold), rule.Name)
			if transition != nil && *transition == store.TransitionRecovered {
				summary = fmt.Sprintf("节点 %s %s持续 %d 分钟不高于 %s，已恢复（规则 %s）", node.Name, label, rule.ForMinutes, resourceValueText(rule.ResourceMetric, rule.RecoveryThreshold), rule.Name)
			}
			if err := e.apply(ctx, cy, rule, node.ID, next, false, node.Maintenance, "", transition, summary, value); err != nil {
				errs = append(errs, err)
			}
		}
		if err := e.pruneCandidates(ctx, rule.ID, keep); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
