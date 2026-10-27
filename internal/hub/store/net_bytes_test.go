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

// 三小时的分钟行，上卷到 1h 水位 +2h、5m 水位 +2h15m 后，像 Prune 那样删掉各级已冻结的细行：和只有同时读 1h、5m 与剩下的 1m 行才对得上。
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
	// from 在首个小时中段、to 在第三个小时中段：两端都按小时桶取整，三个小时整段计入。
	rx, tx, err := s.NetBytesBetween(ctx, id, at(30*60), at(2*3600+17*60))
	if err != nil {
		t.Fatal(err)
	}
	if rx != 1800 || tx != 180 {
		t.Fatalf("sum over three hours = %d / %d, want 1800 / 180", rx, tx)
	}
	rx, tx, err = s.NetBytesBetween(ctx, id, at(3600), at(2*3600+15*60))
	if err != nil {
		t.Fatal(err)
	}
	if rx != 1200 || tx != 120 {
		t.Fatalf("sum from the second hour = %d / %d, want 1200 / 120", rx, tx)
	}
	for _, span := range [][2]int64{{3600, 3600}, {3600, 60}} {
		rx, tx, err = s.NetBytesBetween(ctx, id, at(span[0]), at(span[1]))
		if err != nil || rx != 0 || tx != 0 {
			t.Fatalf("empty span %v = %d / %d (%v), want 0 / 0", span, rx, tx, err)
		}
	}
}
