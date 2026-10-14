package store

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"google.golang.org/protobuf/proto"
)

// minuteRows 造 [from, to) 每分钟一行：cpu = 该分钟序号，mem_used 恒 100，swap 无读数。
func minuteRows(nodeID int64, from, to int64) []metric.Row {
	var rows []metric.Row
	for i, ts := 0, from; ts < to; i, ts = i+1, ts+60 {
		b := metric.NewBucket()
		b.Add(&heronv1.Metrics{CpuPct: proto.Float64(float64(i)), MemUsed: proto.Uint64(100)})
		rows = append(rows, metric.Row{NodeID: nodeID, TS: ts, CoverageStart: from, Bucket: b})
	}
	return rows
}

func readLevel(t *testing.T, s *Store, lv Level, nodeID int64) []metric.Row {
	t.Helper()
	rows, err := s.r.QueryContext(context.Background(), metricSelect(metricFamily.tables[levelIndex(lv)]), nodeID, 0, int64(1)<<40)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out, err := scanBucketRows(rows, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func watermark(t *testing.T, s *Store, level string) int64 {
	t.Helper()
	var v int64
	if err := s.r.QueryRow("SELECT upto_ts FROM rollup_state WHERE level = ?", level).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRollupIsExactAndIdempotent(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := context.Background()
	base := clk.Now().Truncate(time.Hour).Unix() // 整点
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: minuteRows(id, base, base+15*60)}); err != nil {
		t.Fatal(err)
	}
	clk.SetWall(time.Unix(base+20*60, 0)) // now = 整点+20m → ceiling = +15m
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	first := readLevel(t, s, levels[1], id)
	if len(first) != 3 {
		t.Fatalf("5m rows = %d, want 3", len(first))
	}
	// 第二个 5m 桶覆盖分钟 5..9：cpu 和 = 35、n = 5、max = 9；mem 和 = 500、max = 100。
	b := first[1].Bucket
	if first[1].TS != base+300 || b.Sum[0] != 35 || b.N[0] != 5 || b.Max[0] != 9 || b.Sum[1] != 500 || b.N[1] != 5 || b.Max[1] != 100 {
		t.Fatalf("second 5m bucket = ts %d sum %v n %v max %v", first[1].TS, b.Sum, b.N, b.Max)
	}
	if b.N[2] != 0 {
		t.Fatalf("swap had no readings, n = %d", b.N[2])
	}
	if got := watermark(t, s, "5m"); got != base+15*60 {
		t.Fatalf("5m watermark = %d, want %d", got, base+15*60)
	}
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	second := readLevel(t, s, levels[1], id)
	if len(second) != len(first) {
		t.Fatalf("second rollup changed row count: %d vs %d", len(second), len(first))
	}
	for i := range first {
		if first[i].TS != second[i].TS || first[i].Bucket.Sum[0] != second[i].Bucket.Sum[0] || first[i].Bucket.N[0] != second[i].Bucket.N[0] || first[i].Bucket.Max[0] != second[i].Bucket.Max[0] {
			t.Fatalf("rollup is not idempotent at %d: %+v vs %+v", first[i].TS, first[i].Bucket, second[i].Bucket)
		}
	}
}

func TestRollupStopsAtLag(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := context.Background()
	base := clk.Now().Truncate(time.Hour).Unix()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: minuteRows(id, base, base+15*60)}); err != nil {
		t.Fatal(err)
	}
	clk.SetWall(time.Unix(base+14*60+30, 0)) // ceiling = +9m30s → 对齐到 +5m
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readLevel(t, s, levels[1], id); len(got) != 1 || got[0].TS != base {
		t.Fatalf("rows within the lag were rolled up: %+v", got)
	}
	if got := watermark(t, s, "5m"); got != base+300 {
		t.Fatalf("5m watermark = %d, want %d", got, base+300)
	}
}

func TestHourRollupOnlyUsesFrozenFiveMinuteRows(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := context.Background()
	base := clk.Now().Truncate(time.Hour).Unix()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	// 两个整小时的分钟行；now 让 5m 水位停在第二小时中间。
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: minuteRows(id, base, base+120*60)}); err != nil {
		t.Fatal(err)
	}
	clk.SetWall(time.Unix(base+95*60, 0)) // ceiling = +90m
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	if got := watermark(t, s, "5m"); got != base+90*60 {
		t.Fatalf("5m watermark = %d", got)
	}
	if got := watermark(t, s, "1h"); got != base+3600 {
		t.Fatalf("1h watermark = %d, want %d (only the first hour is frozen at the 5m level)", got, base+3600)
	}
	hours := readLevel(t, s, levels[2], id)
	if len(hours) != 1 || hours[0].TS != base || hours[0].Bucket.N[0] != 60 || hours[0].Bucket.Sum[0] != 1770 {
		t.Fatalf("1h rows = %+v, want one bucket with n=60 sum=0+…+59=1770", hours)
	}
}

func TestRollupRollsBackRowsWhenWatermarkUpdateFails(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := context.Background()
	base := clk.Now().Truncate(time.Hour).Unix()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: minuteRows(id, base, base+15*60)}); err != nil {
		t.Fatal(err)
	}
	// 让"推进水位"这一步在插入之后失败：触发器在 UPDATE 时中止事务。
	if _, err := s.w.ExecContext(ctx, "CREATE TRIGGER block_watermark BEFORE UPDATE ON rollup_state BEGIN SELECT RAISE(ABORT, 'watermark update blocked'); END"); err != nil {
		t.Fatal(err)
	}
	clk.SetWall(time.Unix(base+20*60, 0))
	if err := s.Rollup(ctx); err == nil {
		t.Fatal("rollup succeeded although the watermark update failed")
	}
	if got := readLevel(t, s, levels[1], id); len(got) != 0 {
		t.Fatalf("5m rows survived a failed watermark update: %+v", got)
	}
	if got := watermark(t, s, "5m"); got != 0 {
		t.Fatalf("5m watermark = %d, want 0", got)
	}
}

func TestChooseLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		span      int64
		maxPoints int
		level     string
		step      int64
	}{
		{3600, 720, "1m", 60},
		{6 * 3600, 720, "1m", 60},
		{6 * 3600, 100, "1m", 240},
		{6*3600 + 1, 720, "5m", 300},
		{7 * 86400, 720, "5m", 900},
		{7*86400 + 1, 720, "1h", 3600},
		{30 * 86400, 720, "1h", 7200},
		{400 * 86400, 720, "1h", 50400},
		{3600, 0, "1m", 60},
	}
	for _, c := range cases {
		lv, step := ChooseLevel(1_000_000, 1_000_000+c.span, c.maxPoints)
		if lv.Name != c.level || step != c.step {
			t.Fatalf("span %d max %d: got %s/%d, want %s/%d", c.span, c.maxPoints, lv.Name, step, c.level, c.step)
		}
		if step%lv.Bucket != 0 {
			t.Fatalf("step %d is not a multiple of bucket %d", step, lv.Bucket)
		}
		if c.maxPoints > 0 && (c.span+step-1)/step > int64(c.maxPoints) {
			t.Fatalf("span %d / step %d exceeds %d points", c.span, step, c.maxPoints)
		}
	}
}

func TestQueryMetricsRebucketsAndKeepsNoData(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := context.Background()
	base := clk.Now().Truncate(time.Hour).Unix()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: minuteRows(id, base, base+10*60)}); err != nil {
		t.Fatal(err)
	}
	lv, step := ChooseLevel(base+30, base+10*60, 4) // 570s / 4 → 需要 143s → 对齐 180s
	if lv.Name != "1m" || step != 180 {
		t.Fatalf("level %s step %d", lv.Name, step)
	}
	rows, err := s.QueryMetrics(ctx, id, base+30, base+10*60, lv, step)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || rows[0].TS != base || rows[3].TS != base+540 {
		t.Fatalf("rows = %+v, want 4 buckets from %d", rows, base)
	}
	// 第一个 180s 桶覆盖分钟 0、1、2：cpu 和 3、n 3、max 2；swap 始终无读数。
	if b := rows[0].Bucket; b.Sum[0] != 3 || b.N[0] != 3 || b.Max[0] != 2 || b.N[2] != 0 {
		t.Fatalf("first bucket = sum %v n %v max %v", b.Sum, b.N, b.Max)
	}
	if _, ok := rows[0].Bucket.Mean(2); ok {
		t.Fatal("swap had no readings but Mean reported a value")
	}
	if _, err := s.QueryMetrics(ctx, id, base, base+600, lv, 90); err == nil {
		t.Fatal("step that is not a multiple of the bucket must be rejected")
	}
}

func TestPruneDeletesBeyondRetentionInChunks(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	seedProbeTasks(t, s, 1)
	ctx := context.Background()
	now := clk.Now().Truncate(time.Hour)
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	orphan := int64(999) // 不在 node 表里的孤儿行也要被清
	var rows []metric.Row
	for d := int64(0); d < 10; d++ {
		for _, node := range []int64{id, orphan} {
			b := metric.NewBucket()
			b.Add(&heronv1.Metrics{CpuPct: proto.Float64(1)})
			rows = append(rows, metric.Row{NodeID: node, TS: now.Unix() - d*86400, Bucket: b})
		}
	}
	// 绕过正式写入口构造历史遗留孤儿行；正式写入会拒绝不存在的节点。
	if err := s.write(ctx, func(tx *sql.Tx) error {
		for _, row := range rows {
			args := append([]any{row.NodeID, row.TS}, bucketArgs(row.Bucket)...)
			if _, err := tx.Exec(insertMetricFixture("metric_1m"), args...); err != nil {
				return err
			}
			if _, err := tx.Exec(upsertProbeMinute, probeArgs(probeRow(row.NodeID, row.TS, 1, []uint32{10}, 0, 0))...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := Retention{M1: 7 * 24 * time.Hour, M5: 30 * 24 * time.Hour, H1: 365 * 24 * time.Hour}
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	trace := traceWrites(t, s)
	if err := s.setRollupWatermark(t.Context(), "probe_5m", now.Unix()-8*86400); err != nil {
		t.Fatal(err)
	}
	n, err := s.Prune(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	// 保留 ts >= now − 7d：d = 8、9 两天在保留期之外，d = 7 恰在 cutoff 上属保留侧；两个节点各 2 行。
	// 探测族只消费到 d = 8，故本轮仅删除 d = 9 的两行；d = 8 要等自己的水位推进。
	if n != 6 {
		t.Fatalf("pruned %d rows, want 6", n)
	}
	deletes := 0
	for _, statements := range trace.snapshot() {
		for _, stmt := range statements {
			if strings.HasPrefix(stmt.query, "DELETE FROM metric_1m ") {
				deletes++
				if span := stmt.args[2].Value.(int64) - stmt.args[1].Value.(int64); span > 86400 {
					t.Fatalf("prune slice span=%d exceeds one day", span)
				}
			}
		}
	}
	if deletes != 4 {
		t.Fatalf("prune used %d delete transactions, want 4", deletes)
	}
	for _, node := range []int64{id, orphan} {
		left := readLevel(t, s, levels[0], node)
		if len(left) != 8 {
			t.Fatalf("node %d has %d rows left, want 8", node, len(left))
		}
		for _, row := range left {
			if row.TS < now.Unix()-7*86400 {
				t.Fatalf("row at %d survived the cutoff", row.TS)
			}
		}
	}
	if n, err := s.Prune(ctx, r); err != nil || n != 0 {
		t.Fatalf("second prune = %d %v, want 0 nil", n, err)
	}
	// 孤儿节点的行不出现在任何查询结果里（family.live），剩余行按表直接数。
	for _, node := range []int64{id, orphan} {
		left := probeMinuteTS(t, s, node)
		if len(left) != 9 || left[0] != now.Unix()-8*86400 {
			t.Fatalf("probe consumption boundary: node=%d ts=%v, want 9 from %d", node, left, now.Unix()-8*86400)
		}
	}
	if err := s.setRollupWatermark(t.Context(), "probe_5m", now.Unix()); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Prune(ctx, r); err != nil || n != 2 {
		t.Fatalf("consumed probe prune=%d err=%v, want 2", n, err)
	}
	for _, node := range []int64{id, orphan} {
		left := probeMinuteTS(t, s, node)
		if len(left) != 8 || left[0] != now.Unix()-7*86400 {
			t.Fatalf("probe retention boundary: node=%d ts=%v, want 8 from %d", node, left, now.Unix()-7*86400)
		}
	}
}

// probeMinuteTS 按升序列出节点在 probe_1m 里的全部 ts，含孤儿行；查询接口按配置层过滤，看不到孤儿行。
func probeMinuteTS(t *testing.T, s *Store, node int64) []int64 {
	t.Helper()
	ts, err := scanIDs(s.r.QueryContext(t.Context(), "SELECT ts FROM probe_1m WHERE node_id = ? ORDER BY ts", node))
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestRetentionValidate(t *testing.T) {
	t.Parallel()
	if err := DefaultRetention.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Retention{M1: 6 * time.Hour, M5: 168 * time.Hour, H1: 168 * time.Hour, AlertEvents: 90 * 24 * time.Hour}).Validate(); err != nil {
		t.Fatalf("retention minima rejected: %v", err)
	}
	bad := []Retention{
		{M1: 6*time.Hour - time.Second, M5: 30 * 24 * time.Hour, H1: 365 * 24 * time.Hour},
		{M1: 6 * time.Hour, M5: 168*time.Hour - time.Second, H1: 365 * 24 * time.Hour},
		{M1: 6 * time.Hour, M5: 168 * time.Hour, H1: 168*time.Hour - time.Second},
		{M1: 40 * 24 * time.Hour, M5: 30 * 24 * time.Hour, H1: 365 * 24 * time.Hour},
	}
	for _, r := range bad {
		r.AlertEvents = DefaultRetention.AlertEvents
		if err := r.Validate(); err == nil {
			t.Fatalf("%+v accepted", r)
		}
	}
}

func TestRetentionMinimaMatchChooseLevelWindows(t *testing.T) {
	t.Parallel()
	for _, target := range levels[:2] {
		t.Run(target.Name, func(t *testing.T) {
			// 从真实选级函数找切换点，不复制其阈值；粗级尚未被选中的最大跨度即服务窗口。
			lo, hi := int64(1), int64(400*86400)
			for lo < hi {
				mid := lo + (hi-lo+1)/2
				chosen, _ := ChooseLevel(0, mid, 0)
				if chosen.Bucket <= target.Bucket {
					lo = mid
				} else {
					hi = mid - 1
				}
			}
			window := time.Duration(lo) * time.Second
			r := DefaultRetention
			r.M1 = MinRetentionM1
			value := &r.M1
			if target.Name == "5m" {
				value = &r.M5
			}
			*value = window
			if err := r.Validate(); err != nil {
				t.Fatalf("%s longest ChooseLevel window %v rejected: %v", target.Name, window, err)
			}
			*value -= time.Second
			if err := r.Validate(); err == nil {
				t.Fatalf("%s retention %v accepted below longest ChooseLevel window %v", target.Name, *value, window)
			}
		})
	}
}

func TestNextMaintenanceAtLandsTwoSecondsPastTheMinute(t *testing.T) {
	t.Parallel()
	for _, wall := range []time.Time{
		time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 1, 12, 0, 1, 999_000_000, time.UTC),
		time.Date(2026, 1, 1, 12, 0, 2, 0, time.UTC),
		time.Date(2026, 1, 1, 12, 0, 59, 0, time.UTC),
	} {
		next := nextMaintenanceAt(wall)
		want := time.Date(2026, 1, 1, 12, 1, 2, 0, time.UTC)
		if !next.Equal(want) || !next.After(wall) {
			t.Fatalf("nextMaintenanceAt(%v) = %v, want %v", wall, next, want)
		}
	}
}

func TestChooseLevelCapsAlignedWindows(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		from, to  int64
		maxPoints int
	}{
		{"aligned", 0, 600, 10},
		{"both_edges", 30, 630, 10},
		{"exact_end", 30, 600, 10},
		{"one_point", 59, 61, 1},
		{"one_point_across_hour", 3599, 3601, 1},
		{"rebucket", 30, 600, 4},
		{"six_hours", 30, 6*3600 + 30, 360},
		{"thirty_days", 1_000_000, 1_000_000 + 30*86400, 720},
		{"long_single_point", 1_000_000, 1_000_000 + 400*86400, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			lv, step := ChooseLevel(c.from, c.to, c.maxPoints)
			if step < lv.Bucket || step%lv.Bucket != 0 {
				t.Fatalf("step %d is not a positive multiple of bucket %d", step, lv.Bucket)
			}
			// [from, to) 的首尾可能属于不同的对齐桶，不能只用跨度除以步长。
			points := (c.to-1)/step - c.from/step + 1
			if points > int64(c.maxPoints) {
				t.Fatalf("aligned window [%d, %d) at step %d covers %d buckets, limit %d", c.from, c.to, step, points, c.maxPoints)
			}
		})
	}
}

func TestQueryMetricsHonorsAlignedPointLimitWithoutDroppingSamples(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := context.Background()
	base := clk.Now().Truncate(time.Hour).Unix()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: minuteRows(id, base, base+11*60)}); err != nil {
		t.Fatal(err)
	}
	lv, step := ChooseLevel(base+30, base+630, 10)
	rows, err := s.QueryMetrics(ctx, id, base+30, base+630, lv, step)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 || len(rows) > 10 {
		t.Fatalf("aligned query returned %d points with maxPoints=10, step=%d", len(rows), step)
	}
	var count uint32
	var sum float64
	for _, row := range rows {
		count += row.Bucket.N[0]
		sum += row.Bucket.Sum[0]
	}
	if count != 11 || sum != 55 {
		t.Fatalf("point limiting lost samples: n=%d sum=%v, want 11 and 55", count, sum)
	}
}

func TestChooseLevelKeepsFinestStepWithinAlignedBudget(t *testing.T) {
	t.Parallel()
	for _, from := range []int64{0, 1, 30, 59, 3599, 1_000_000, 1_767_225_630} {
		for _, span := range []int64{1, 60, 61, 570, 600, 6 * 3600, 6*3600 + 1, 7 * 86400, 7*86400 + 1, 30 * 86400, 400 * 86400} {
			for _, budget := range []int{1, 2, 4, 10, 100, 720} {
				to := from + span
				lv, step := ChooseLevel(from, to, budget)
				// 逐个枚举桶长倍数作为独立基准，不复用被测的跳跃计算。
				want := lv.Bucket
				for (to-1)/want-from/want+1 > int64(budget) {
					want += lv.Bucket
				}
				if step != want {
					t.Fatalf("window [%d, %d) budget %d: step %d, finest valid step %d", from, to, budget, step, want)
				}
			}
		}
	}
}

// Sum 列复用 sum/n 存储：写入、加法合并、上卷都必须把它当可加量对待。
func TestSumColumnsRoundTripAndRollUp(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := context.Background()
	base := clk.Now().Truncate(time.Hour).Unix()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	var rows []metric.Row
	for i := int64(0); i < 5; i++ {
		b := metric.NewBucket()
		b.AddSum(metric.RxBytes, 100)
		b.AddSum(metric.RxBytes, 50)
		rows = append(rows, metric.Row{NodeID: id, TS: base + i*60, CoverageStart: base, Bucket: b})
	}
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: rows}); err != nil {
		t.Fatal(err)
	}
	// 同一分钟再写一次：加法合并，不覆盖。
	again := metric.NewBucket()
	again.AddSum(metric.RxBytes, 1)
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{{NodeID: id, TS: base, CoverageStart: base, Bucket: again}}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadMinuteRows(ctx, id, base, base+300)
	if err != nil || len(got) != 5 {
		t.Fatalf("read: %v %v", got, err)
	}
	if b := got[0].Bucket; b.Sum[metric.RxBytes] != 151 || b.N[metric.RxBytes] != 3 || b.N[metric.TxBytes] != 0 {
		t.Fatalf("minute 0: rx %v/%d tx n=%d, want 151/3 and no tx", b.Sum[metric.RxBytes], b.N[metric.RxBytes], b.N[metric.TxBytes])
	}
	clk.SetWall(time.Unix(base+20*60, 0))
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	five, _ := LevelByName("5m")
	agg := readLevel(t, s, five, id)
	if len(agg) != 1 || agg[0].Bucket.Sum[metric.RxBytes] != 751 || agg[0].Bucket.N[metric.RxBytes] != 11 {
		t.Fatalf("5m rows = %+v, want one row with rx 751/11", agg)
	}
}

func TestProbeRollupIsExactIdempotentAndIndependentOfMetrics(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	seedProbeTasks(t, s, 7, 9)
	ctx := t.Context()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	base := clk.Now().Truncate(time.Hour).Unix()
	batch := metric.Batch{Rows: minuteRows(id, base, base+900)}
	for i := int64(0); i < 15; i++ {
		batch.Probes = append(batch.Probes,
			probeRow(id, base+i*60, 7, []uint32{uint32(100 + i)}, 1, 1),
			probeRow(id, base+i*60, 9, nil, 2, 0))
	}
	if _, err := s.WriteMinuteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	clk.SetWall(time.Unix(base+1200, 0))
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"5m", "probe_5m"} {
		if got := watermark(t, s, state); got != base+900 {
			t.Fatalf("independent watermark %s=%d, want %d", state, got, base+900)
		}
	}
	first, err := s.QueryProbes(ctx, id, base, base+900, levels[1], 300, 0)
	if err != nil || len(first) != 6 {
		t.Fatalf("probe_5m rows=%d err=%v, want 6", len(first), err)
	}
	for i, row := range first {
		j := i % 3
		want := metric.ProbeBucket{Sent: 10, Lost: 10}
		task := uint64(9)
		if i < 3 {
			task = 7
			want = metric.ProbeBucket{Sent: 15, Lost: 5, Errors: 5, RttN: 5, RttSumUs: uint64(510 + j*25), RttMinUs: uint32(100 + j*5), RttMaxUs: uint32(104 + j*5)}
		}
		if row.NodeID != id || row.TaskID != task || row.TS != base+int64(j)*300 || *row.Bucket != want {
			t.Fatalf("rollup row=%+v bucket=%+v, want task=%d ts=%d bucket=%+v", row, *row.Bucket, task, base+int64(j)*300, want)
		}
	}
	var nulls int
	if err := s.r.QueryRow("SELECT COUNT(*) FROM probe_5m WHERE task_id=9 AND rtt_min_us IS NULL AND rtt_max_us IS NULL").Scan(&nulls); err != nil || nulls != 3 {
		t.Fatalf("all-loss NULL buckets=%d err=%v, want 3", nulls, err)
	}
	// 测试库回退水位以真正重算同一批桶，避免第二次调用只走空操作。
	if err := s.setRollupWatermark(t.Context(), "probe_5m", base); err != nil {
		t.Fatal(err)
	}
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := s.QueryProbes(ctx, id, base, base+900, levels[1], 300, 0)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated rollup changed rows: first=%s second=%s err=%v", formatProbeRows(first), formatProbeRows(second), err)
	}
	clk.SetWall(time.Unix(base+3900, 0))
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	hour, err := s.QueryProbes(ctx, id, base, base+3600, levels[2], 3600, 0)
	want := []metric.ProbeRow{
		{NodeID: id, TS: base, TaskID: 7, Bucket: &metric.ProbeBucket{Sent: 45, Lost: 15, Errors: 15, RttN: 15, RttSumUs: 1605, RttMinUs: 100, RttMaxUs: 114}},
		{NodeID: id, TS: base, TaskID: 9, Bucket: &metric.ProbeBucket{Sent: 30, Lost: 30}},
	}
	if err != nil || !reflect.DeepEqual(hour, want) {
		t.Fatalf("hour rollup=%s err=%v, want %s", formatProbeRows(hour), err, formatProbeRows(want))
	}
	if got := watermark(t, s, "probe_1h"); got != base+3600 {
		t.Fatalf("probe_1h watermark=%d, want %d", got, base+3600)
	}
}

func TestQueriesReadSelectedFamilyLevel(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	seedProbeTasks(t, s, 1)
	ctx := t.Context()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	// 各级故意写不同的值，避免读错表后又经二次聚合得到相同结果而掩盖路由错误。
	if err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE rollup_state SET upto_ts = 3600"); err != nil {
			return err
		}
		for i := range levels {
			args := append([]any{id, int64(0)}, bucketArgs(bucket(float64(i+1)))...)
			if _, err := tx.Exec(insertMetricFixture(metricFamily.tables[i]), args...); err != nil {
				return err
			}
			query := strings.Replace(upsertProbeMinute, "INSERT INTO probe_1m", "INSERT INTO "+probeFamily.tables[i], 1)
			if _, err := tx.Exec(query, probeArgs(probeRow(id, 0, 1, []uint32{uint32(i + 1)}, 0, 0))...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i, lv := range levels {
		t.Run(lv.Name, func(t *testing.T) {
			rows, err := s.QueryMetrics(ctx, id, 0, lv.Bucket, lv, lv.Bucket)
			var sums []float64
			for _, row := range rows {
				sums = append(sums, row.Bucket.Sum[0])
			}
			if err != nil || len(sums) != 1 || sums[0] != float64(i+1) {
				t.Fatalf("metric query read wrong level %s: sums=%v err=%v, want [%d]", lv.Name, sums, err, i+1)
			}
			probes, err := s.QueryProbes(ctx, id, 0, lv.Bucket, lv, lv.Bucket, 0)
			if err != nil || len(probes) != 1 || probes[0].Bucket.RttSumUs != uint64(i+1) {
				t.Fatalf("probe query read wrong level %s: rows=%s err=%v, want sum %d", lv.Name, formatProbeRows(probes), err, i+1)
			}
		})
	}
}

// 上卷的源行按 family.live 过滤：已删任务的细级行在清理作业完成之前仍在表里，不会被聚合成粗级行；同一节点上仍存在的
// 任务照常上卷。节点那一侧的过滤由 node_id IN (SELECT id FROM node) 承载，这里钉的是任务那一侧。
func TestRollupSkipsRowsOfDeletedTask(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	node, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	seedProbeTasks(t, s, 6, 7)
	base := clk.Now().Truncate(time.Hour).Unix()
	setWatermark(t, s, "probe_5m", base)
	setWatermark(t, s, "probe_1h", base)
	if err := s.write(ctx, func(tx *sql.Tx) error {
		for m := int64(0); m < 10; m++ {
			for _, task := range []int{6, 7} {
				if _, err := tx.Exec("INSERT INTO probe_1m (node_id, ts, task_id, sent) VALUES (?, ?, ?, 1)", node, base+m*60, task); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteProbeTask(ctx, 6); err != nil {
		t.Fatal(err)
	}
	clk.SetWall(time.Unix(base+3600+int64(RollupLag/time.Second), 0))
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	count := func(task int) int64 {
		var n int64
		if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM probe_5m WHERE task_id = ?", task).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count(7); got != 2 {
		t.Fatalf("live task rolled into %d five-minute buckets, want 2", got)
	}
	if got := count(6); got != 0 {
		t.Fatalf("deleted task rolled into %d five-minute buckets, want 0: rollup must filter source rows by family.live", got)
	}
}
