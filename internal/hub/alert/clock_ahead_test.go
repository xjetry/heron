package alert

import (
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/live"
	"github.com/xjetry/heron/internal/hub/store"
)

// 墙钟曾偏快时内存里的 notifiedAt 可能落在当前墙钟之后：冷却判定不把负耗时当成仍在冷却内，
// 同时正常过去时间仍在冷却期内的触发照常拦成 pending。
func TestCoolDownDoesNotSuppressWhenNotifiedAtAhead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	rule := store.AlertRule{CooldownS: 600}
	tr := store.TransitionFiring

	// 未来 notifiedAt：不得压住触发。
	state, gotTr := coolDown(rule, stateEntry{notifiedAt: now.Add(8 * time.Hour)}, now, store.StateFiring, &tr)
	if state != store.StateFiring || gotTr == nil || *gotTr != store.TransitionFiring {
		t.Fatalf("future notifiedAt: state=%v tr=%v, want firing", state, gotTr)
	}
	// 正常冷却期内：照常压成 pending。
	state, gotTr = coolDown(rule, stateEntry{notifiedAt: now.Add(-60 * time.Second)}, now, store.StateFiring, &tr)
	if state != store.StatePending || gotTr != nil {
		t.Fatalf("within cooldown: state=%v tr=%v, want pending/nil", state, gotTr)
	}
}

// 墙钟曾偏快并写下未来的 accumulate_from：重算基线把 accumulate_from 钳位回当前分钟并持久化，
// 校回后等满判定窗口即可恢复收集真实 5m 桶，不必等到快钟留下的未来时刻。
func TestRecomputeBaselineClampsFutureAccumulateFrom(t *testing.T) {
	f := newFixture(t)
	node := f.ids[0]
	task := f.task(t, f.ids[:1])
	t0 := f.clk.Now().Truncate(5 * time.Minute)
	r := f.rule(t, adaptiveRule(task, 5, 1))

	// 先重算一次拿到真实任务指纹，再写入指向 8 小时后的 accumulate_from。
	must(t, f.e.RecomputeBaselines(t.Context(), t0))
	seed, ok := f.baseline(t, r.ID, node)
	if !ok {
		t.Fatal("initial baseline row missing")
	}
	future := t0.Add(8 * time.Hour)
	seed.AccumulateFrom = future
	saved, err := f.st.SaveAlertBaseline(t.Context(), task, seed)
	must(t, err)
	if !saved {
		t.Fatal("SaveAlertBaseline rejected seed row")
	}

	// 在 t0 重算：accumulate_from 被钳位到 t0 并落库。
	must(t, f.e.recomputeBaseline(t.Context(), r, node, seed.TaskFingerprint, future, t0))
	row, ok := f.baseline(t, r.ID, node)
	if !ok {
		t.Fatal("baseline row missing")
	}
	if !row.AccumulateFrom.Equal(t0) {
		t.Fatalf("AccumulateFrom = %v, want clamped to %v", row.AccumulateFrom, t0)
	}

	// 写入 t0 起 5 分钟的真实探测并上卷，随后在 t0+10m 重算，基线应立即收集到该 5m 桶。
	f.rttMinutes(t, task, node, t0.Unix(), 5, 42)
	f.rollupTo(t, t0.Add(10*time.Minute))
	must(t, f.e.recomputeBaseline(t.Context(), r, node, row.TaskFingerprint, row.AccumulateFrom, t0.Add(10*time.Minute)))
	row, ok = f.baseline(t, r.ID, node)
	if !ok || row.Buckets != 1 || row.BaselineUs != 42_000 {
		t.Fatalf("after correction baseline = %+v (ok=%v), want 1 bucket at 42ms", row, ok)
	}
}

// 离线抖动窗口对未来墙钟时间戳的防御：
//   - node.LastSeenAt 落在未来时，offlineStart 钳位到当前墙钟，避免 SinceRecovery 被拉大而漏判抖动抑制。
//   - recoveredAt 落在未来时，SweepOffline 把它钳位到当前墙钟，避免抖动抑制窗口被拉长 Δ。
func TestSweepOfflineClampsFutureWallClockTimestamps(t *testing.T) {
	t.Run("future last_seen_at does not bypass flap suppression", func(t *testing.T) {
		f := newFixture(t)
		node := f.ids[0]
		now := f.clk.Now()
		got := f.e.offlineStart(live.Entry{}, false, store.Node{ID: node, LastSeenAt: now.Add(8 * time.Hour)}, now)
		if !got.Equal(now) {
			t.Fatalf("offlineStart = %v, want clamped to %v", got, now)
		}
	})

	t.Run("future recovered_at does not extend flap window by clock offset", func(t *testing.T) {
		f := newFixture(t)
		node := f.ids[0]
		r := f.rule(t, store.AlertRule{Name: "offline", Kind: store.KindOffline, Enabled: true, AllNodes: true})
		now := f.clk.Now()

		// 内存里的 recoveredAt 指向 8 小时后的未来时刻。
		f.e.mu.Lock()
		f.e.states[stateKey{r.ID, node}] = stateEntry{state: store.StateOK, recoveredAt: now.Add(8 * time.Hour)}
		f.e.mu.Unlock()

		// 第一次 SweepOffline 把未来的 recoveredAt 一次性钳位到当前墙钟 now。
		must(t, f.e.SweepOffline(t.Context()))

		// 推进超过 flapWindow（1h）但远小于快钟偏差 8h，节点上报一次后再离线 70s（超过普通宽限、
		// 小于抖动宽限 flapGrace）：因为 recoveredAt 已被钳位到 now，抖动窗口已过期，应直接进入 firing 而非再等 8 小时。
		f.clk.Advance(flapWindow + time.Minute)
		f.l.Observe(node, "src", &heronv1.Metrics{})
		f.clk.Advance(70 * time.Second)
		must(t, f.e.SweepOffline(t.Context()))
		wantState(t, f.e, r.ID, node, store.StateFiring)
	})
}
