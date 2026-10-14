package api

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

func TestNetworkPeaksFromReportToBothHistoryAPIs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, token := h.createNode(t, "peaks")
	h.setPublic(t, id, "peaks", true)
	base := h.clk.Now().Truncate(time.Hour).Unix()
	for i, rate := range []uint64{800, 20, 100} {
		if err := h.report(t, token, &heronv1.Metrics{BootId: testBootID, NetRxTotal: proto.Uint64(uint64(i) * 300), NetTxTotal: proto.Uint64(0), NetRxBps: proto.Uint64(rate), NetTxBps: proto.Uint64(0)}); err != nil {
			t.Fatal(err)
		}
		h.clk.Advance(10 * time.Second)
	}
	h.clk.Advance(time.Minute)
	if err := h.report(t, token, &heronv1.Metrics{BootId: testBootID}); err != nil {
		t.Fatal(err)
	}
	h.ingest.Flush(t.Context(), true)
	for _, window := range []int64{3600, 86400, 10 * 86400} {
		h.clk.Advance(2 * time.Hour)
		if err := h.store.Rollup(t.Context()); err != nil {
			t.Fatal(err)
		}
		q := &heronv1.QueryMetricsRequest{NodeId: id, From: base, To: base + window, MaxPoints: 2000}
		admin, err := h.admin.QueryMetrics(t.Context(), connect.NewRequest(q))
		if err != nil {
			t.Fatal(err)
		}
		public, err := h.publicClient().QueryMetrics(t.Context(), connect.NewRequest(q))
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(admin.Msg, public.Msg) {
			t.Fatalf("history projections differ: %v / %v", admin.Msg, public.Msg)
		}
		series := map[string]*heronv1.MetricSeries{}
		for _, s := range admin.Msg.Series {
			series[s.Name] = s
		}
		rx, tx, bytes := series["net_rx_bps"], series["net_tx_bps"], series["rx_bytes"]
		if rx == nil || tx == nil || bytes == nil || len(rx.Samples) == 0 {
			t.Fatalf("missing network series: %v", admin.Msg)
		}
		if rx.Unit != "bytes/s" || rx.Samples[0].GetMax() != 800 || rx.Samples[0].N != 3 || rx.Samples[0].GetMean() != 920.0/3 {
			t.Fatalf("sampled peak lost: %v", rx)
		}
		if tx.Samples[0].Max == nil || tx.Samples[0].GetMax() != 0 || tx.Samples[0].N != 3 {
			t.Fatalf("valid zero peak lost: %v", tx)
		}
		if bytes.Samples[0].GetSum() != 600 || bytes.Samples[0].Max != nil {
			t.Fatalf("traffic semantics changed: %v", bytes)
		}
		if window == 3600 {
			if len(rx.Samples) != 2 || rx.Samples[1].N != 0 || rx.Samples[1].Max != nil || rx.Samples[1].Mean != nil {
				t.Fatalf("missing rate fabricated a sample: %v", rx)
			}
		}
	}
}
