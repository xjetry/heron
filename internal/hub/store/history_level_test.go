package store

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/metric"
)

// unalignedNow 不落在任何桶边界上：截止点 alignDown(now − 保留期, 桶长) 与 now − 保留期因此不相等，
// 另算一份不对齐的截止点的选级会在边界上与 prune 分开。
var unalignedNow = time.Date(2026, 1, 1, 13, 37, 13, 0, time.UTC)

func openRetaining(t *testing.T, now time.Time, opts ...OpenOption) (*Store, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(now)
	s, err := Open(filepath.Join(t.TempDir(), "t.db"), clk, slog.Default(), MigrateSchema, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, clk
}

// cpuSamples 是窗口内 cpu 的样本总数：minuteRows 每分钟恰有一个 cpu 读数，聚合到任何级、任何步长都守恒。
func cpuSamples(rows []metric.Row) uint32 {
	i := metric.Index("cpu")
	var n uint32
	for _, r := range rows {
		n += r.Bucket.N[i]
	}
	return n
}

// 截止到现在（面板与公开页的窗口形状）：选级与只看跨度完全相同，保留期那条下限不起作用。
// 默认保留期与最短可接受保留期各验一遍：后者正是 Validate 的下限，面板窗口的分辨率不随保留期配置变化。
func TestChooseLevelKeepsWindowsEndingNowOnTheSpanLevel(t *testing.T) {
	t.Parallel()
	minima := Retention{M1: MinRetentionM1, M5: MinRetentionM5, H1: MinRetentionH1, AlertEvents: MinRetentionAlertEvents}
	for _, r := range []Retention{DefaultRetention, minima} {
		s, clk := openRetaining(t, unalignedNow, WithRetention(r))
		now := clk.Now().Unix()
		to := (now/60 + 1) * 60
		for _, span := range []int64{3600, 6 * 3600, 86400, 7 * 86400, 30 * 86400, 365 * 86400} {
			for _, maxPoints := range []int{0, 720, 1000} {
				gotLv, gotStep := s.ChooseLevel(to-span, to, maxPoints)
				wantLv, wantStep := chooseLevel(to-span, to, maxPoints, 0)
				if span > 7*86400 && wantLv.Name != "1h" || span <= 6*3600 && wantLv.Name != "1m" {
					t.Fatalf("span-only selection drifted: span %d → %s", span, wantLv.Name)
				}
				if gotLv != wantLv || gotStep != wantStep {
					t.Fatalf("retention %+v span %d max %d: got %s/%d, want %s/%d", r, span, maxPoints, gotLv.Name, gotStep, wantLv.Name, wantStep)
				}
			}
		}
	}
}

// 选级判定"某级仍保有窗口起点"与 prune 实际删到哪里是同一个截止点，两个方向都钉住：
// 选级仍选该级的最早起点处，那一行清理后还在；再早一个桶长（选级已改选更粗的级）的那一行被删。
func TestChooseLevelBoundaryIsThePruneCutoff(t *testing.T) {
	t.Parallel()
	for i, lv := range levels[:2] {
		t.Run(lv.Name, func(t *testing.T) {
			t.Parallel()
			s, clk := openRetaining(t, unalignedNow)
			ctx := t.Context()
			now := clk.Now().Unix()
			// 不经 cutoff，从选级本身找边界：保留期之外的起点选级会被推到更粗的级。
			chooses := func(from int64) bool {
				got, _ := s.ChooseLevel(from, from+lv.Bucket, 0)
				return got == lv
			}
			lo, hi := now-400*86400, now
			for lo < hi {
				mid := lo + (hi-lo)/2
				if chooses(mid) {
					hi = mid
				} else {
					lo = mid + 1
				}
			}
			edge := lo
			if edge%lv.Bucket != 0 || chooses(edge-1) || !chooses(edge) {
				t.Fatalf("edge %d is not a %s bucket boundary where selection switches", edge, lv.Name)
			}
			id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
			if err != nil {
				t.Fatal(err)
			}
			// 在 edge 与 edge − 桶长 各写一分钟；上卷后两者都落到本级（及更粗级）的桶里。
			batch := metric.Batch{Rows: append(minuteRows(id, edge-lv.Bucket, edge-lv.Bucket+60), minuteRows(id, edge, edge+60)...)}
			if _, err := s.WriteMinuteBatch(ctx, batch); err != nil {
				t.Fatal(err)
			}
			if err := s.Rollup(ctx); err != nil {
				t.Fatal(err)
			}
			before := readLevel(t, s, levels[i], id)
			if len(before) != 2 || before[0].TS != edge-lv.Bucket || before[1].TS != edge {
				t.Fatalf("%s rows before prune = %+v", lv.Name, before)
			}
			if _, err := s.Prune(ctx); err != nil {
				t.Fatal(err)
			}
			after := readLevel(t, s, levels[i], id)
			if len(after) != 1 || after[0].TS != edge {
				t.Fatalf("%s rows after prune = %+v, want only %d (selection keeps %s from there)", lv.Name, after, edge, lv.Name)
			}
		})
	}
}

// 过去的窗口在真实 Prune(DefaultRetention) 之后仍查得到：选级落到保留期仍保有窗口起点的级。
// 清理前用同一组 (级, 步长) 查一遍作对照：清理后结果与清理前逐行相同；只按跨度选的细级清理前有数、清理后为空。
func TestPastWindowsReadTheRetainedLevelAfterPrune(t *testing.T) {
	t.Parallel()
	s, clk := openRetaining(t, unalignedNow)
	ctx := t.Context()
	now := clk.Now().Unix()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	hour := int64(3600)
	cases := []struct {
		name     string
		from, to int64
		level    string
		// spanLevel 是只看跨度时会选的级；pruned 是它在清理后窗口内还剩的 cpu 样本数。
		spanLevel string
		pruned    uint32
	}{
		{"ten_days_ago_two_hours", alignDown(now-10*86400, hour), alignDown(now-10*86400, hour) + 2*hour, "5m", "1m", 0},
		{"forty_days_ago_one_day", alignDown(now-40*86400, 86400), alignDown(now-40*86400, 86400) + 86400, "1h", "5m", 0},
		// 起点在 1m 截止点之前约一小时、终点在其后约一小时：只按跨度选 1m 只剩截止点之后的那段，分钟数在下面按时钟算。
		{"across_the_1m_cutoff", alignDown(now-7*86400, hour) - hour, alignDown(now-7*86400, hour) + hour, "5m", "1m", 0},
	}
	cut := alignDown(now-int64(DefaultRetention.M1/time.Second), 60)
	cases[2].pruned = uint32((cases[2].to - cut) / 60)
	var rows []metric.Row
	for _, c := range cases {
		rows = append(rows, minuteRows(id, c.from, c.to)...)
	}
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: rows}); err != nil {
		t.Fatal(err)
	}
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	type result struct {
		rows   []metric.Row
		lv     Level
		step   int64
		before uint32
	}
	results := make([]result, len(cases))
	for i, c := range cases {
		lv, step := s.ChooseLevel(c.from, c.to, defaultTestMaxPoints)
		if lv.Name != c.level {
			t.Fatalf("%s: level %s, want %s", c.name, lv.Name, c.level)
		}
		got, err := s.QueryMetrics(ctx, id, c.from, c.to, lv, step)
		if err != nil {
			t.Fatal(err)
		}
		spanLv, spanStep := chooseLevel(c.from, c.to, defaultTestMaxPoints, 0)
		if spanLv.Name != c.spanLevel {
			t.Fatalf("%s: span-only level %s, want %s", c.name, spanLv.Name, c.spanLevel)
		}
		spanRows, err := s.QueryMetrics(ctx, id, c.from, c.to, spanLv, spanStep)
		if err != nil {
			t.Fatal(err)
		}
		results[i] = result{rows: got, lv: lv, step: step, before: cpuSamples(spanRows)}
		if want := uint32((c.to - c.from) / 60); cpuSamples(got) != want || results[i].before != want {
			t.Fatalf("%s before prune: chosen level has %d samples, span level %d, want %d", c.name, cpuSamples(got), results[i].before, want)
		}
	}
	if _, err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		lv, step := s.ChooseLevel(c.from, c.to, defaultTestMaxPoints)
		if lv != results[i].lv || step != results[i].step {
			t.Fatalf("%s: selection changed across prune: %s/%d vs %s/%d", c.name, lv.Name, step, results[i].lv.Name, results[i].step)
		}
		got, err := s.QueryMetrics(ctx, id, c.from, c.to, lv, step)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 || !sameRows(got, results[i].rows) {
			t.Fatalf("%s after prune: %d rows, %d samples; before %d rows", c.name, len(got), cpuSamples(got), len(results[i].rows))
		}
		spanLv, spanStep := chooseLevel(c.from, c.to, defaultTestMaxPoints, 0)
		spanRows, err := s.QueryMetrics(ctx, id, c.from, c.to, spanLv, spanStep)
		if err != nil {
			t.Fatal(err)
		}
		if n := cpuSamples(spanRows); n != c.pruned {
			t.Fatalf("%s: span-only level %s kept %d samples after prune, want %d", c.name, spanLv.Name, n, c.pruned)
		}
	}
}

// 窗口起点早于所有级的保留期：选最粗级，保留期之外那段没有行，其余照常返回。
func TestChooseLevelBeforeEveryRetentionPicksTheCoarsestLevel(t *testing.T) {
	t.Parallel()
	s, clk := openRetaining(t, unalignedNow)
	now := clk.Now().Unix()
	from := now - int64(DefaultRetention.H1/time.Second) - 86400
	lv, _ := s.ChooseLevel(from, from+3600, 0)
	if lv != levels[len(levels)-1] {
		t.Fatalf("window before every retention chose %s", lv.Name)
	}
}

const defaultTestMaxPoints = 720

func sameRows(a, b []metric.Row) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].TS != b[i].TS || cpuSamples(a[i:i+1]) != cpuSamples(b[i:i+1]) {
			return false
		}
		for j := range a[i].Bucket.Sum {
			if a[i].Bucket.Sum[j] != b[i].Bucket.Sum[j] || a[i].Bucket.N[j] != b[i].Bucket.N[j] {
				return false
			}
		}
	}
	return true
}
