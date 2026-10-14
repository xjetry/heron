package live

import (
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/metric"
	"google.golang.org/protobuf/proto"
)

func at(sec int) time.Time { return time.Unix(int64(sec), 0).UTC() }

func TestOnlineIsLastSeenWithinTTL(t *testing.T) {
	clk := clock.NewFake(at(600))
	l := New(clk, 30*time.Second)
	if l.Online(1) {
		t.Fatal("never-reported node must be offline")
	}
	l.Observe(1, "", &heronv1.Metrics{})
	if !l.Online(1) {
		t.Fatal("node must be online from its first report")
	}
	clk.Advance(29 * time.Second)
	if !l.Online(1) {
		t.Fatal("still inside TTL")
	}
	clk.Advance(time.Second)
	if l.Online(1) {
		t.Fatal("TTL elapsed, must be offline")
	}
}

func TestOnlineUsesMonotonicClock(t *testing.T) {
	clk := clock.NewFake(at(600))
	l := New(clk, 30*time.Second)
	l.Observe(1, "", &heronv1.Metrics{})
	clk.SetWall(at(600 + 3600)) // 墙钟前跳一小时，单调钟不动
	if !l.Online(1) {
		t.Fatal("wall clock jump must not affect online state")
	}
}

func TestFlushTakesOnlyClosedBuckets(t *testing.T) {
	clk := clock.NewFake(at(600)) // 分钟 600 的起点
	l := New(clk, 30*time.Second)
	l.Observe(1, "", &heronv1.Metrics{CpuPct: proto.Float64(10)})
	if rows := l.Flush().Rows; len(rows) != 0 {
		t.Fatalf("bucket for the current minute must stay open, got %d rows", len(rows))
	}
	clk.Advance(60 * time.Second)
	l.Observe(1, "", &heronv1.Metrics{CpuPct: proto.Float64(50)})
	rows := l.Flush().Rows
	if len(rows) != 1 || rows[0].TS != 600 || rows[0].NodeID != 1 {
		t.Fatalf("rows = %+v, want one row for ts 600", rows)
	}
	if mean, _ := rows[0].Bucket.Mean(0); mean != 10 {
		t.Fatalf("flushed bucket mean = %v, want 10 (the sample at 660 belongs to the open bucket)", mean)
	}
	if again := l.Flush().Rows; len(again) != 0 {
		t.Fatalf("flush must take the bucket away; second flush returned %d rows", len(again))
	}
}

func TestDrainTakesEverything(t *testing.T) {
	clk := clock.NewFake(at(600))
	l := New(clk, 30*time.Second)
	l.Observe(1, "", &heronv1.Metrics{CpuPct: proto.Float64(1)})
	l.Observe(2, "", &heronv1.Metrics{CpuPct: proto.Float64(2)})
	if rows := l.Drain().Rows; len(rows) != 2 {
		t.Fatalf("drain returned %d rows, want 2", len(rows))
	}
}

func TestWallClockSetBackLandsInEarlierMinute(t *testing.T) {
	clk := clock.NewFake(at(660))
	l := New(clk, 30*time.Second)
	l.Observe(1, "", &heronv1.Metrics{CpuPct: proto.Float64(1)})
	clk.SetWall(at(610)) // 回拨到上一分钟
	l.Observe(1, "", &heronv1.Metrics{CpuPct: proto.Float64(3)})
	clk.SetWall(at(720))
	rows := l.Flush().Rows
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want separate buckets for 600 and 660", len(rows))
	}
	seen := map[int64]float64{}
	for _, r := range rows {
		seen[r.TS] = r.Bucket.Sum[0]
	}
	if seen[600] != 3 || seen[660] != 1 {
		t.Fatalf("sums by ts = %v, want 600:3 660:1", seen)
	}
}

func TestGetReflectsLatestReport(t *testing.T) {
	clk := clock.NewFake(at(600))
	l := New(clk, 30*time.Second)
	l.Observe(7, "", &heronv1.Metrics{CpuPct: proto.Float64(42)})
	e, ok := l.Get(7)
	if !ok || e.Metrics.GetCpuPct() != 42 || !e.Online {
		t.Fatalf("entry = %+v ok=%v", e, ok)
	}
	l.Forget(7)
	if _, ok := l.Get(7); ok {
		t.Fatal("forgotten node must be gone")
	}
}

func TestObserveReportsFirstAndMonotonicGap(t *testing.T) {
	clk := clock.NewFake(at(600))
	l := New(clk, 30*time.Second)
	ts, gap, first := l.Observe(1, "", &heronv1.Metrics{})
	if ts != 600 || !first || gap != 0 {
		t.Fatalf("first observe: ts=%d gap=%v first=%v", ts, gap, first)
	}
	clk.Advance(45 * time.Second)
	ts, gap, first = l.Observe(1, "", &heronv1.Metrics{})
	if ts != 600 || first || gap != 45*time.Second {
		t.Fatalf("second observe: ts=%d gap=%v first=%v, want 600 45s false", ts, gap, first)
	}
	clk.Advance(10 * time.Second)
	clk.SetWall(at(100)) // 墙钟回拨：间隔仍由单调钟给出
	if _, gap, _ = l.Observe(1, "", &heronv1.Metrics{}); gap != 10*time.Second {
		t.Fatalf("gap after wall clock set back = %v, want 10s", gap)
	}
}

func TestAddBytesLandsInTheGivenBucketOnly(t *testing.T) {
	clk := clock.NewFake(at(600))
	l := New(clk, 30*time.Second)
	ts, _, _ := l.Observe(1, "", &heronv1.Metrics{})
	l.AddBytes(1, ts, 1500, 700)
	l.AddBytes(1, ts, 500, 300)
	l.AddBytes(2, ts, 9, 9) // 从未 Observe 的节点：丢弃，不凭空建条目
	clk.Advance(time.Minute)
	rows := l.Flush().Rows
	if len(rows) != 1 || rows[0].NodeID != 1 || rows[0].TS != 600 {
		t.Fatalf("rows = %+v", rows)
	}
	b := rows[0].Bucket
	if b.Sum[metric.RxBytes] != 2000 || b.N[metric.RxBytes] != 2 || b.Sum[metric.TxBytes] != 1000 || b.N[metric.TxBytes] != 2 {
		t.Fatalf("rx %v/%d tx %v/%d, want 2000/2 1000/2", b.Sum[metric.RxBytes], b.N[metric.RxBytes], b.Sum[metric.TxBytes], b.N[metric.TxBytes])
	}
	if _, ok := l.Get(2); ok {
		t.Fatal("AddBytes created a node entry")
	}
}

func TestAddProbeFoldsIntoMeasuredMinuteAndFlushesWithMetrics(t *testing.T) {
	clk := clock.NewFake(at(600))
	l := New(clk, 30*time.Second)
	l.Observe(1, "", &heronv1.Metrics{CpuPct: proto.Float64(10)})
	clk.Advance(time.Minute)
	l.AddProbe(1, at(670), 7, &heronv1.ProbeResult{Outcome: &heronv1.ProbeResult_RttUs{RttUs: 1200}})
	l.AddProbe(1, at(675), 7, &heronv1.ProbeResult{Outcome: &heronv1.ProbeResult_Timeout{Timeout: &heronv1.Timeout{}}})
	l.AddProbe(1, at(630), 9, &heronv1.ProbeResult{Outcome: &heronv1.ProbeResult_RttUs{RttUs: 800}})
	batch := l.Flush()
	if len(batch.Rows) != 1 || batch.Rows[0].TS != 600 || batch.Rows[0].NodeID != 1 || batch.Rows[0].Bucket.Sum[metric.Index("cpu")] != 10 {
		t.Fatalf("closed metrics=%+v", batch.Rows)
	}
	if len(batch.Probes) != 1 || batch.Probes[0].NodeID != 1 || batch.Probes[0].TS != 600 || batch.Probes[0].TaskID != 9 || batch.Probes[0].Bucket.Sent != 1 || batch.Probes[0].Bucket.RttMinUs != 800 {
		t.Fatalf("closed probes=%+v", batch.Probes)
	}
	if !l.Flush().Empty() {
		t.Fatal("second Flush returned already taken rows")
	}
	// 已刷出的分钟可再次接到迟到结果，但新内存桶不能再携带上次已交出的计数。
	l.AddProbe(1, at(630), 9, &heronv1.ProbeResult{Outcome: &heronv1.ProbeResult_RttUs{RttUs: 400}})
	late := l.Flush()
	if len(late.Probes) != 1 || late.Probes[0].Bucket.Sent != 1 || late.Probes[0].Bucket.RttSumUs != 400 {
		t.Fatalf("late probes=%+v", late.Probes)
	}
	batch = l.Drain()
	if len(batch.Rows) != 0 || len(batch.Probes) != 1 {
		t.Fatalf("drain=%+v", batch)
	}
	p := batch.Probes[0]
	if p.NodeID != 1 || p.TS != 660 || p.TaskID != 7 || *p.Bucket != (metric.ProbeBucket{Sent: 2, Lost: 1, RttN: 1, RttSumUs: 1200, RttMinUs: 1200, RttMaxUs: 1200}) {
		t.Fatalf("open probe=%+v bucket=%+v", p, *p.Bucket)
	}
	if !l.Drain().Empty() {
		t.Fatal("second Drain returned already taken rows")
	}
	l.Forget(1)
	l.AddProbe(1, at(670), 7, &heronv1.ProbeResult{Outcome: &heronv1.ProbeResult_RttUs{RttUs: 1}})
	if _, ok := l.Get(1); ok {
		t.Error("AddProbe revived forgotten node")
	}
	if !l.Drain().Empty() {
		t.Error("AddProbe created rows after Forget")
	}
}
