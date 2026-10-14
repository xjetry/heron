package alert

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
)

// rttBand 是 rtt 相对判定在一对 (规则, 节点) 上的正常带（毫秒）：[基线×(1−下偏差), 基线×(1+上偏差)]。
type rttBand struct{ baselineMs, lo, hi float64 }

// relativeBand 给出相对判定此刻的正常带；ok 为假表示基线无效（不是相对判定、没有基线行、桶数不足、任务指纹已变），
// 调用方把整个判定窗口当作没有读数，规则既不触发也不恢复。固定基线恒有效。
func relativeBand(r store.AlertRule, b store.Baselines, nodeID int64) (rttBand, bool) {
	if r.Kind != store.KindProbe || r.Metric != store.MetricRttMs || r.RttMode != store.RttRelative {
		return rttBand{}, false
	}
	var base float64
	switch r.BaselineMode {
	case store.BaselineFixed:
		base = r.FixedBaselineMs
	case store.BaselineAdaptive:
		row, ok := b.Usable(r, nodeID)
		if !ok {
			return rttBand{}, false
		}
		base = float64(row.BaselineUs) / 1000
	default:
		return rttBand{}, false
	}
	return rttBand{baselineMs: base, lo: base * (1 - r.LowerDeviationPct/100), hi: base * (1 + r.UpperDeviationPct/100)}, true
}

// exceeds 报告分钟均值是否越出正常带。两端都含等号，沿用固定阈值"触发含阈值等号"的惯例。下偏差取 100%
// 时 lo 为 0：均值是 RttSumUs/RttN 且 RttN > 0，对公网目标恒为正，带下的那一侧因此不可达，规则只按上偏差判定。
func (b rttBand) exceeds(ms float64) bool { return ms >= b.hi || ms <= b.lo }

// summary 是相对判定的事件文案：实测均值、基线与偏差百分比，以及正常带。触发与恢复共用：恢复时同一组数说明它回到了带内。
func (b rttBand) summary(node, rule string, ms float64) string {
	return fmt.Sprintf("节点 %s 规则 %s：rtt 均值 %.1f ms，基线 %.1f ms，偏差 %+.1f%%（正常带 %.1f–%.1f ms）",
		node, rule, ms, b.baselineMs, (ms-b.baselineMs)/b.baselineMs*100, b.lo, b.hi)
}

// coolDown 实现冷却：上一次进入 firing 之后 cooldown_s 之内，状态机给出的新 firing 转换改成停在 pending，不产生转换；
// 冷却期满后仍越带的下一轮照常触发。只拦进入 firing 这一种转换：恢复与其余状态照常走，所以恢复投递不受冷却影响。
// 上一次进入 firing 的时刻来自 alert_state.fired_at（store.StateRow.FiredAt），hub 重启不清零。cooldown_s 只在相对
// 判定的规则上非零（store.CheckKindFields），其余规则原样通过。
func coolDown(r store.AlertRule, cur stateEntry, now time.Time, next store.AlertState, tr *store.Transition) (store.AlertState, *store.Transition) {
	if tr == nil || *tr != store.TransitionFiring || r.CooldownS == 0 || cur.firedAt.IsZero() {
		return next, tr
	}
	if now.Sub(cur.firedAt) < time.Duration(r.CooldownS)*time.Second {
		return store.StatePending, nil
	}
	return next, tr
}

// baselineSlot 是 (规则, 节点) 在每小时里重算的分钟（0–59）：按哈希打散，一小时内各对的历史读均匀摊开，
// 而不是在整点一起读。
func baselineSlot(ruleID, nodeID int64) int64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d/%d", ruleID, nodeID)
	return int64(h.Sum64() % 60)
}

// median 是桶均值的中位数，桶等权：被评估的量就是分钟（桶）均值，按样本数加权会让高频任务的桶主导。偶数个取中间两个的平均。
func median(values []float64) float64 {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// BaselineRecomputeOffset 是重算轮次在分钟边界之后的偏移：错开刷出（0.5s）、维护（2s）与探测评估（3s），
// 与它们不在同一时刻争写协程与读池。
const BaselineRecomputeOffset = 30 * time.Second

// RunBaselineRecompute 每分钟跑一轮 RecomputeBaselines，直到 ctx 结束。
func (e *Engine) RunBaselineRecompute(ctx context.Context) {
	for {
		now := e.clk.Now()
		timer := time.NewTimer(now.Truncate(time.Minute).Add(time.Minute + BaselineRecomputeOffset).Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if err := e.RecomputeBaselines(ctx, e.clk.Now()); err != nil {
			e.log.Error("baseline recompute failed", "err", err)
		}
	}
}

// RecomputeBaselines 是一轮基线重算。每对 (规则, 节点) 每小时在 baselineSlot 那一分钟重算一次；没有行的、
// 规则刚保存过的在本轮就算；行的任务指纹与任务当前的指纹不同的，本轮在同一次写里清掉基线并把累积起点设为 now，
// 此后只读起点之后的桶（重新累积）。
//
// 不持 writeMu：它只读历史（经 HistoryReader，评估池，与评估各占一个连接，见 store.evaluationPoolSize），写经 store 的
// 写协程。读与写之间规则、节点或任务可能变了，SaveAlertBaseline 在写事务里重新核对，失效的写被丢弃。一轮内逐对
// 顺序读，同一时刻至多一条重算读在飞；RunBaselineRecompute 是它唯一的生产调用方。
func (e *Engine) RecomputeBaselines(ctx context.Context, now time.Time) error {
	e.mu.Lock()
	dirty := e.baselineDirty
	e.baselineDirty = map[int64]bool{}
	e.mu.Unlock()
	var rules []store.AlertRule
	for _, r := range e.Rules() {
		if r.Enabled && r.AdaptiveBaseline() {
			rules = append(rules, r)
		}
	}
	if len(rules) == 0 {
		return nil
	}
	nodes, err := e.monitoringNodes(ctx)
	if err != nil {
		return err
	}
	snapshot, err := e.st.ReadAlertBaselines(ctx)
	if err != nil {
		return err
	}
	minute := now.Truncate(time.Minute)
	slot := minute.Unix() / 60 % 60
	var errs []error
	for _, r := range rules {
		ids, err := e.st.ProbeTaskNodeIDs(ctx, r.TaskID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		fingerprint, ok := snapshot.Fingerprint(r.TaskID)
		if !ok {
			continue
		}
		var keep []int64
		for _, node := range nodes {
			if !inScope(r, node.ID) || !slices.Contains(ids, node.ID) {
				continue
			}
			keep = append(keep, node.ID)
			row, has := snapshot.Row(r.ID, node.ID)
			if has && row.TaskFingerprint != fingerprint {
				reset := store.AlertBaseline{RuleID: r.ID, NodeID: node.ID, ComputedAt: minute, TaskFingerprint: fingerprint, AccumulateFrom: now}
				if _, err := e.st.SaveAlertBaseline(ctx, r.TaskID, reset); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			if has && !dirty[r.ID] && baselineSlot(r.ID, node.ID) != slot {
				continue
			}
			if err := e.recomputeBaseline(ctx, r, node.ID, fingerprint, row.AccumulateFrom, minute); err != nil {
				errs = append(errs, err)
			}
		}
		// 作用域或任务分配收缩之后不再评估的 (规则, 节点)，它们的行在这里删掉；读历史失败的对仍在 keep 里，行保留。
		if err := e.st.PruneAlertBaselines(ctx, r.ID, keep); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recomputeBaseline 按定义重算一对 (规则, 节点) 的基线并写回。窗口是 [max(判定窗口起点 − baseline_window_s, 累积起点),
// 判定窗口起点)：判定窗口是 minute 之前的 for_minutes 个已闭合分钟，基线不含它。此后一小时里判定窗口只会后移，
// 缓存的基线始终早于当时的判定窗口。
func (e *Engine) recomputeBaseline(ctx context.Context, r store.AlertRule, nodeID int64, fingerprint string, accumulateFrom time.Time, minute time.Time) error {
	judgeStart := minute.Unix() - int64(r.ForMinutes)*60
	from := judgeStart - int64(r.BaselineWindowS)
	if !accumulateFrom.IsZero() {
		from = max(from, accumulateFrom.Unix())
	}
	var means []float64
	if from < judgeStart {
		var err error
		means, err = e.history.ProbeBucketMeans(ctx, nodeID, r.TaskID, from, judgeStart)
		if err != nil {
			return err
		}
	}
	b := store.AlertBaseline{RuleID: r.ID, NodeID: nodeID, Buckets: len(means), ComputedAt: minute, TaskFingerprint: fingerprint, AccumulateFrom: accumulateFrom}
	if len(means) > 0 {
		b.BaselineUs = int64(math.Round(median(means)))
	}
	_, err := e.st.SaveAlertBaseline(ctx, r.TaskID, b)
	return err
}
