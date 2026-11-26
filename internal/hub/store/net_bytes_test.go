package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/metric"
)

// trafficMinuteRows 给 [from, to) 的每一分钟一行，收 rx、发 tx 字节。
func trafficMinuteRows(nodeID int64, from, to int64, rx, tx float64) []metric.Row {
	var rows []metric.Row
	for ts := from; ts < to; ts += 60 {
		b := metric.NewBucket()
		b.AddSum(metric.RxBytes, rx)
		b.AddSum(metric.TxBytes, tx)
		rows = append(rows, metric.Row{NodeID: nodeID, TS: ts, CoverageStart: from, Bucket: b})
	}
	return rows
}

// 三小时的分钟行，上卷到 1h 水位 +2h、5m 水位 +2h15m 后，像 Prune 那样删掉各级已冻结的细行：首个小时只剩 1h 行，
// 第二个小时只剩 1h 行，第三个小时前 15 分钟只剩 5m 行、之后还有 1m 行。整小时的中段读小时级；端点所在的那一小时逐级
// 细分到仍完整的一级：第三个小时里 5m 完整、2:15 起 1m 也完整，端点 2:17 逐分钟分开；首个小时的细级都已清理，整块归到
// 它起始的一侧。
func TestNetBytesBetweenSumsAcrossLevels(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := context.Background()
	base := clk.Now().Truncate(time.Hour).Unix()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: trafficMinuteRows(id, base, base+3*3600, 10, 1)}); err != nil {
		t.Fatal(err)
	}
	clk.SetWall(time.Unix(base+2*3600+20*60, 0))
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	if got := watermark(t, s, "1h"); got != base+2*3600 {
		t.Fatalf("1h watermark = %d, want %d", got, base+2*3600)
	}
	frozen := base + 2*3600 + 15*60
	if got := watermark(t, s, "5m"); got != frozen {
		t.Fatalf("5m watermark = %d, want %d", got, frozen)
	}
	if err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM metric_1m WHERE ts < ?", frozen); err != nil {
			return err
		}
		_, err := tx.Exec("DELETE FROM metric_5m WHERE ts < ?", base+2*3600)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	at := func(sec int64) time.Time { return time.Unix(base+sec, 0) }
	for _, c := range []struct {
		name     string
		from, to int64
		rx, tx   int64
	}{
		// 首个小时细级已清理、起点 0:30 在它中间：整块归到它起始的一侧（区间之外）；中段一小时；末端 2:00–2:17 逐分钟。
		{"first hour pruned, last hour by minute", 30 * 60, 2*3600 + 17*60, 600 + 170, 60 + 17},
		{"last hour on a 5-minute boundary", 3600, 2*3600 + 15*60, 600 + 150, 60 + 15},
		{"rest of the last hour", 2*3600 + 17*60, 3 * 3600, 430, 43},
		// 首个小时只剩小时行：起点在它中间的区间不计入它，起点在整点上的区间计入整小时。
		{"inside a pruned hour", 20 * 60, 40 * 60, 0, 0},
		{"from the start of a pruned hour", 0, 40 * 60, 600, 60},
		{"empty", 3600, 3600, 0, 0},
		{"reversed", 3600, 60, 0, 0},
	} {
		rx, tx, err := s.NetBytesBetween(ctx, id, at(c.from), at(c.to))
		if err != nil {
			t.Fatal(err)
		}
		if rx != c.rx || tx != c.tx {
			t.Errorf("%s [%d, %d) = %d / %d, want %d / %d", c.name, c.from, c.to, rx, tx, c.rx, c.tx)
		}
	}
	// 相邻两段在同一个快照里互补：端点 2:17 两侧逐分钟分开，相加等于整段，不重不漏。
	left, _, _ := s.NetBytesBetween(ctx, id, at(3600), at(2*3600+17*60))
	right, _, _ := s.NetBytesBetween(ctx, id, at(2*3600+17*60), at(3*3600))
	whole, _, _ := s.NetBytesBetween(ctx, id, at(3600), at(3*3600))
	if left+right != whole {
		t.Errorf("split at 2:17: %d + %d != %d", left, right, whole)
	}
}

// minuteValue 是测试里第 ts 分钟的收字节数：逐分钟不同，端点错一分钟或整块归错一侧都会改变和。发字节恒为 1。
func minuteValue(ts int64) float64 { return float64(ts/60%997 + 1) }

func variedMinuteRows(nodeID int64, from, to int64) []metric.Row {
	var rows []metric.Row
	for ts := from; ts < to; ts += 60 {
		b := metric.NewBucket()
		b.AddSum(metric.RxBytes, minuteValue(ts))
		b.AddSum(metric.TxBytes, 1)
		rows = append(rows, metric.Row{NodeID: nodeID, TS: ts, CoverageStart: from, Bucket: b})
	}
	return rows
}

// minuteSum 是 [from, to) 里起点落在区间内的分钟按 minuteValue 求和：NetBytesBetween 的定义本身，不读库。
func minuteSum(from, to int64) (rx, tx int64) {
	for ts := from - from%60; ts < to; ts += 60 {
		if ts >= from {
			rx += int64(minuteValue(ts))
			tx++
		}
	}
	return rx, tx
}

// 周期边界不在整点上：Asia/Kolkata（+05:30）的零点是 UTC 的 HH:30，Asia/Kathmandu（+05:45）的是 HH:15，重置时刻 08:07
// 落在 5 分钟桶的中间。细级都还在时，两端逐分钟读，结果与逐分钟求和一致；1m 已清理、5m 还在时，5 分钟整点上的端点
// 仍一致，不在 5 分钟整点上的端点所在的那个 5 分钟桶整块归到它起始的一侧；细级都已清理时整小时归到它起始的一侧。
func TestNetBytesBetweenSplitsNonHourBoundariesByMinute(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := context.Background()
	base := clk.Now().Truncate(time.Hour).Unix()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: variedMinuteRows(id, base, base+6*3600)}); err != nil {
		t.Fatal(err)
	}
	clk.SetWall(time.Unix(base+6*3600+20*60, 0))
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	at := func(sec int64) time.Time { return time.Unix(base+sec, 0) }
	check := func(name string, from, to int64, wantRx, wantTx int64) {
		t.Helper()
		rx, tx, err := s.NetBytesBetween(ctx, id, at(from), at(to))
		if err != nil {
			t.Fatal(err)
		}
		if rx != wantRx || tx != wantTx {
			t.Errorf("%s [%d, %d) = %d / %d, want %d / %d", name, from, to, rx, tx, wantRx, wantTx)
		}
	}
	exact := func(name string, from, to int64) {
		t.Helper()
		rx, tx := minuteSum(base+from, base+to)
		check(name, from, to, rx, tx)
	}
	// 1m、5m、1h 都在。
	exact("+05:30 boundary to a later +05:30 boundary", 30*60, 4*3600+30*60)
	exact("+05:45 boundary to now", 15*60, 5*3600+42*60+17)
	exact("08:07 reset time", 3600+7*60, 3*3600+7*60)

	// 像 Prune 那样删掉前四小时的 1m 行（5m 已上卷消费过它们），后两小时的 1m 行留着。
	if err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM metric_1m WHERE ts < ?", base+4*3600)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	exact("5-minute boundaries with 1m pruned", 30*60, 3*3600+45*60)
	exact("1m still kept at the far end", 30*60, 4*3600+7*60)
	// 1:07 落在 1m 已清理的 5 分钟桶 [1:05, 1:10) 里：这个桶整块归到它起始的一侧（1:05 早于 1:07，不计入）。
	rx, tx := minuteSum(base+3600+10*60, base+2*3600)
	mid1, mid2 := minuteSum(base+2*3600, base+3*3600)
	check("5-minute bucket across a pruned boundary", 3600+7*60, 3*3600, rx+mid1, tx+mid2)

	// 再删掉前两小时的 5m 行（1h 已消费）：前两小时只剩小时行，起点在小时中间时那一小时整块不计入。
	if err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM metric_5m WHERE ts < ?", base+2*3600)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rx, tx = minuteSum(base+3600, base+3*3600)
	check("hour across a fully pruned boundary", 30*60, 3*3600, rx, tx)
	rx, tx = minuteSum(base, base+3600)
	check("pruned hour starting at the boundary", 0, 40*60, rx, tx)
}
