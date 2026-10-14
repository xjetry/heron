package store

import (
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"google.golang.org/protobuf/proto"
)

func TestNetworkPeaksSurviveEveryRollupAndQueryBucket(t *testing.T) {
	t.Parallel()
	rx, tx := metric.Index("net_rx_bps"), metric.Index("net_tx_bps")
	if rx < 0 || tx < 0 {
		t.Fatal("network rate columns are missing")
	}
	s, clk := open(t)
	ctx := t.Context()
	base := clk.Now().Truncate(time.Hour).Unix()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "peaks"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	for i, rate := range []uint64{0, 900, 10} {
		b := metric.NewBucket()
		b.Add(&heronv1.Metrics{NetRxBps: proto.Uint64(rate), NetTxBps: proto.Uint64(0)})
		b.AddSum(metric.RxBytes, 60)
		if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{{NodeID: id, TS: base + int64(i)*60, CoverageStart: base, Bucket: b}}}); err != nil {
			t.Fatal(err)
		}
	}
	clk.SetWall(time.Unix(base+7200, 0))
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	for _, lv := range levels {
		rows, err := s.QueryMetrics(ctx, id, base, base+3600, lv, 3600)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s rows=%v err=%v", lv.Name, rows, err)
		}
		b := rows[0].Bucket
		if b.Max[rx] != 900 || b.N[rx] != 3 || b.Max[tx] != 0 || b.N[tx] != 3 || b.Sum[metric.RxBytes] != 180 {
			t.Fatalf("%s lost peaks or changed traffic: %+v", lv.Name, b)
		}
	}
}
