package alert

import (
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

// rttMinute 是一个只有一次成功探测、往返 ms 毫秒的分钟桶。
func rttMinute(ms float64) metric.ProbeBucket {
	us := uint32(ms * 1000)
	return metric.ProbeBucket{Sent: 1, RttN: 1, RttSumUs: uint64(us), RttMinUs: us, RttMaxUs: us}
}

// rttMinutes 从 start 起写 n 个每分钟 ms 毫秒的分钟行。
func (f *fixture) rttMinutes(t *testing.T, task uint64, node int64, start int64, n int, ms float64) {
	t.Helper()
	buckets := make([]metric.ProbeBucket, n)
	for i := range buckets {
		buckets[i] = rttMinute(ms)
	}
	f.minutes(t, task, node, start, buckets...)
}

// rollupTo 把时钟推到 at 并上卷：基线只读 5 分钟桶，分钟行要先上卷才进得了基线。
func (f *fixture) rollupTo(t *testing.T, at time.Time) {
	t.Helper()
	f.clk.SetWall(at)
	must(t, f.st.Rollup(t.Context()))
}

func adaptiveRule(task uint64, forMinutes, minSamples int) store.AlertRule {
	return store.AlertRule{Name: "延迟基线", Kind: store.KindProbe, Enabled: true, AllNodes: true, TaskID: task, Metric: store.MetricRttMs, ForMinutes: forMinutes,
		RttMode: store.RttRelative, BaselineMode: store.BaselineAdaptive, BaselineWindowS: 3600, BaselineMinSamples: minSamples,
		UpperDeviationPct: 100, LowerDeviationPct: 50, CooldownS: 600}
}

func fixedRule(task uint64, forMinutes int, baselineMs float64) store.AlertRule {
	return store.AlertRule{Name: "延迟固定基线", Kind: store.KindProbe, Enabled: true, AllNodes: true, TaskID: task, Metric: store.MetricRttMs, ForMinutes: forMinutes,
		RttMode: store.RttRelative, BaselineMode: store.BaselineFixed, FixedBaselineMs: baselineMs,
		UpperDeviationPct: 100, LowerDeviationPct: 50, CooldownS: 600}
}

func (f *fixture) baseline(t *testing.T, ruleID, nodeID int64) (store.AlertBaseline, bool) {
	t.Helper()
	b, err := f.st.ReadAlertBaselines(t.Context())
	must(t, err)
	return b.Row(ruleID, nodeID)
}

// 基线不含判定窗口：判定窗口里的高时延桶已经上卷进 5 分钟级，若算进基线，中位数会被它们拉到 500 ms。
func TestBaselineExcludesJudgmentWindow(t *testing.T) {
	f := newFixture(t)
	node := f.ids[0]
	task := f.task(t, f.ids[:1])
	t0 := f.clk.Now()
	r := f.rule(t, adaptiveRule(task, 10, 1))
	// 判定窗口是 t0 之前的 10 分钟 [t0−10m, t0)；基线窗口在它之前。
	f.rttMinutes(t, task, node, t0.Unix()-1200, 5, 50)
	f.rttMinutes(t, task, node, t0.Unix()-600, 10, 500)
	f.rollupTo(t, t0.Add(10*time.Minute))
	must(t, f.e.RecomputeBaselines(t.Context(), t0))
	row, ok := f.baseline(t, r.ID, node)
	if !ok || row.Buckets != 1 || row.BaselineUs != 50_000 {
		t.Fatalf("baseline=%+v ok=%v; want one 50 ms bucket from before the judgment window", row, ok)
	}
}

// 越带（含两端）进入 firing，文案带实测均值、基线与偏差；回到带内恢复。固定基线 50 ms、上偏差 100%、下偏差 50%：带是 [25, 100]。
func TestRelativeRuleFiresOutsideBandAndRecovers(t *testing.T) {
	f := newFixture(t)
	node := f.ids[0]
	task := f.task(t, f.ids[:1])
	r := f.rule(t, fixedRule(task, 2, 50))
	ts := f.clk.Now().Unix() - 300
	f.minutes(t, task, node, ts, rttMinute(100), rttMinute(100), rttMinute(25), rttMinute(60))
	must(t, f.e.EvaluateProbes(t.Context(), ts))
	wantState(t, f.e, r.ID, node, store.StatePending)
	must(t, f.e.EvaluateProbes(t.Context(), ts+60))
	wantState(t, f.e, r.ID, node, store.StateFiring)
	ev := f.events(t)
	if len(ev) != 1 || ev[0].Value != 100 {
		t.Fatalf("events=%+v", ev)
	}
	for _, want := range []string{"rtt 均值 100.0 ms", "基线 50.0 ms", "偏差 +100.0%", "正常带 25.0–100.0 ms"} {
		if !strings.Contains(ev[0].Summary, want) {
			t.Errorf("firing summary %q lacks %q", ev[0].Summary, want)
		}
	}
	// 下界同样含等号：25 ms 仍在带外，不恢复。
	must(t, f.e.EvaluateProbes(t.Context(), ts+120))
	wantState(t, f.e, r.ID, node, store.StateFiring)
	must(t, f.e.EvaluateProbes(t.Context(), ts+180))
	wantState(t, f.e, r.ID, node, store.StateOK)
	if ev = f.events(t); len(ev) != 2 || ev[0].Transition != store.TransitionRecovered || !strings.Contains(ev[0].Summary, "偏差 +20.0%") {
		t.Fatalf("events after recovery=%+v", ev)
	}
}

// 桶数不足最小样本数时基线无效：越带的分钟不触发；已在 firing 的，回到带内的分钟也不恢复。
func TestAdaptiveBaselineBelowMinSamplesNeitherFiresNorRecovers(t *testing.T) {
	f := newFixture(t)
	node := f.ids[0]
	task := f.task(t, f.ids[:1])
	t0 := f.clk.Now()
	r := f.rule(t, adaptiveRule(task, 1, 3))
	f.rttMinutes(t, task, node, t0.Unix()-1800, 10, 50)
	f.rollupTo(t, t0)
	must(t, f.e.RecomputeBaselines(t.Context(), t0))
	if row, ok := f.baseline(t, r.ID, node); !ok || row.Buckets != 2 {
		t.Fatalf("baseline=%+v ok=%v; want two buckets, below the three required", row, ok)
	}
	f.minutes(t, task, node, t0.Unix(), rttMinute(500), rttMinute(50))
	must(t, f.e.EvaluateProbes(t.Context(), t0.Unix()))
	// 没有状态行即 ok（engine.entry）：基线无效时这一轮没有任何转换。
	wantState(t, f.e, r.ID, node, "")
	must(t, f.st.SetAlertState(t.Context(), r.ID, node, store.StateFiring, f.clk.Now(), time.Time{}))
	must(t, f.e.Load(t.Context()))
	must(t, f.e.EvaluateProbes(t.Context(), t0.Unix()+60))
	wantState(t, f.e, r.ID, node, store.StateFiring)
	if ev := f.events(t); len(ev) != 0 {
		t.Fatalf("events with an invalid baseline: %+v", ev)
	}
}

// 任务改目标：评估立刻不再用旧目标的基线（行的指纹与任务当前的指纹不同），重算把行清掉并把累积起点设为当下；
// 此后的重算只读起点之后的桶，旧目标留在同一个 task_id 下的历史不会被算回来。
func TestTaskTargetChangeResetsBaseline(t *testing.T) {
	f := newFixture(t)
	node := f.ids[0]
	task := f.task(t, f.ids[:1])
	t0 := f.clk.Now()
	r := f.rule(t, adaptiveRule(task, 1, 1))
	f.rttMinutes(t, task, node, t0.Unix()-1800, 10, 50)
	f.rollupTo(t, t0)
	must(t, f.e.RecomputeBaselines(t.Context(), t0))
	if row, ok := f.baseline(t, r.ID, node); !ok || row.Buckets != 2 {
		t.Fatalf("baseline before the change=%+v ok=%v", row, ok)
	}
	_, _, err := f.st.SaveProbeTask(t.Context(), &heronv1.ProbeTask{Id: task, Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "10.0.0.9", IntervalS: 5, TimeoutMs: 100}, store.NodeSelector{NodeIDs: f.ids[:1]})
	must(t, err)

	f.minutes(t, task, node, t0.Unix(), rttMinute(500))
	must(t, f.e.EvaluateProbes(t.Context(), t0.Unix()))
	wantState(t, f.e, r.ID, node, "")

	reset := t0.Add(2 * time.Minute)
	f.clk.SetWall(reset)
	must(t, f.e.RecomputeBaselines(t.Context(), reset))
	row, ok := f.baseline(t, r.ID, node)
	if !ok || row.Buckets != 0 || !row.AccumulateFrom.Equal(reset) {
		t.Fatalf("baseline after the target change=%+v ok=%v; want it cleared with accumulate_from=%v", row, ok, reset)
	}
	// 保存规则让下一轮立即重算这条规则：旧目标的 2 个桶仍在基线窗口里，但早于累积起点。
	f.rule(t, r)
	later := reset.Add(time.Minute)
	f.clk.SetWall(later)
	must(t, f.e.RecomputeBaselines(t.Context(), later))
	if row, ok = f.baseline(t, r.ID, node); !ok || row.Buckets != 0 || !row.AccumulateFrom.Equal(reset) {
		t.Fatalf("baseline recomputed after the reset=%+v ok=%v; want no buckets from before %v", row, ok, reset)
	}
}

// 冷却：上一次进入 firing 之后 cooldown_s 之内，新的触发停在 pending、不产生事件；恢复照常投递；冷却期满后照常触发。
// 上一次进入 firing 的时刻落库（alert_state.fired_at），hub 重启不清零。
func TestCooldownHoldsNewFiringAcrossRestart(t *testing.T) {
	f := newFixture(t)
	node := f.ids[0]
	task := f.task(t, f.ids[:1])
	r := f.rule(t, fixedRule(task, 1, 50))
	start := f.clk.Now()
	ts := start.Unix() - 60
	step := func(offset time.Duration, ms float64, want store.AlertState) {
		t.Helper()
		f.clk.SetWall(start.Add(offset))
		minute := ts + int64(offset/time.Minute)*60
		f.minutes(t, task, node, minute, rttMinute(ms))
		must(t, f.e.EvaluateProbes(t.Context(), minute))
		wantState(t, f.e, r.ID, node, want)
	}
	step(0, 200, store.StateFiring)
	step(time.Minute, 50, store.StateOK)
	step(2*time.Minute, 200, store.StatePending)
	f.restart(t)
	step(3*time.Minute, 200, store.StatePending)
	if ev := f.events(t); len(ev) != 2 || ev[0].Transition != store.TransitionRecovered || ev[1].Transition != store.TransitionFiring {
		t.Fatalf("events inside the cooldown=%+v; want one firing and its recovery", ev)
	}
	step(10*time.Minute, 200, store.StateFiring)
	if ev := f.events(t); len(ev) != 3 || ev[0].Transition != store.TransitionFiring {
		t.Fatalf("events after the cooldown=%+v; want a second firing", ev)
	}
}

// 重算不持 writeMu：评估（持 writeMu）进行中，重算照常完成，二者各占评估池的一个连接（store.evaluationPoolSize）。
func TestRecomputeBaselinesDoesNotHoldWriteMu(t *testing.T) {
	f := newFixture(t)
	node := f.ids[0]
	task := f.task(t, f.ids[:1])
	t0 := f.clk.Now()
	r := f.rule(t, adaptiveRule(task, 1, 1))
	f.rttMinutes(t, task, node, t0.Unix()-1800, 10, 50)
	f.rollupTo(t, t0)
	f.e.writeMu.Lock()
	done := make(chan error, 1)
	go func() { done <- f.e.RecomputeBaselines(t.Context(), t0) }()
	select {
	case err := <-done:
		f.e.writeMu.Unlock()
		must(t, err)
	case <-time.After(testwait.Bound):
		f.e.writeMu.Unlock()
		t.Fatal("RecomputeBaselines blocked on writeMu")
	}
	if _, ok := f.baseline(t, r.ID, node); !ok {
		t.Fatal("no baseline row after the recompute")
	}
}

// 已有行的 (规则, 节点) 只在自己的哈希分钟重算；作用域或任务分配收缩之后不再评估的对，行在重算轮次里删掉。
func TestRecomputeBaselinesSlotAndPrune(t *testing.T) {
	f := newFixture(t)
	node := f.ids[0]
	task := f.task(t, f.ids[:1])
	t0 := f.clk.Now()
	r := f.rule(t, adaptiveRule(task, 1, 1))
	f.rttMinutes(t, task, node, t0.Unix()-1800, 10, 50)
	f.rollupTo(t, t0)
	must(t, f.e.RecomputeBaselines(t.Context(), t0))
	first, _ := f.baseline(t, r.ID, node)
	slot := baselineSlot(r.ID, node)
	hour := t0.Truncate(time.Hour).Add(time.Hour)
	off := hour.Add(time.Duration((slot+1)%60) * time.Minute)
	must(t, f.e.RecomputeBaselines(t.Context(), off))
	if row, _ := f.baseline(t, r.ID, node); !row.ComputedAt.Equal(first.ComputedAt) {
		t.Fatalf("recomputed outside its slot: computed_at %v → %v", first.ComputedAt, row.ComputedAt)
	}
	on := hour.Add(time.Duration(slot) * time.Minute)
	must(t, f.e.RecomputeBaselines(t.Context(), on))
	if row, _ := f.baseline(t, r.ID, node); !row.ComputedAt.Equal(on) {
		t.Fatalf("not recomputed in its slot %d: computed_at=%v want %v", slot, row.ComputedAt, on)
	}
	_, _, err := f.st.SaveProbeTask(t.Context(), &heronv1.ProbeTask{Id: task, Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "127.0.0.1", IntervalS: 5, TimeoutMs: 100}, store.NodeSelector{NodeIDs: f.ids[1:]})
	must(t, err)
	must(t, f.e.RecomputeBaselines(t.Context(), on.Add(time.Minute)))
	if _, ok := f.baseline(t, r.ID, node); ok {
		t.Fatal("baseline row survived the node leaving the task")
	}
}
