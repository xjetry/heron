package api

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/live"
	"google.golang.org/protobuf/proto"
)

func TestCoverageBothAPIsFromReception(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, token := h.createNode(t, "coverage")
	h.setPublic(t, id, "coverage", true)
	base := h.clk.Now().Truncate(time.Minute)
	h.clk.SetWall(base.Add(-time.Second))
	h.live.SetReceiving(true)
	if err := h.report(t, token, &heronv1.Metrics{BootId: testBootID}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		end := base.Add(time.Duration(i+1) * time.Minute)
		for h.clk.Now().Before(end) {
			h.clk.Advance(min(live.ObservationInterval, end.Sub(h.clk.Now())))
			h.live.ObservationHeartbeat()
		}
		h.ingest.Flush(t.Context(), false)
	}
	for _, points := range []uint32{1, 1000} {
		q := &heronv1.QueryMetricsRequest{NodeId: id, From: base.Unix(), To: base.Add(20 * time.Minute).Unix(), MaxPoints: points}
		a, err := h.admin.QueryMetrics(t.Context(), connect.NewRequest(q))
		if err != nil {
			t.Fatal(err)
		}
		p, err := h.publicClient().QueryMetrics(t.Context(), connect.NewRequest(q))
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(a.Msg, p.Msg) {
			t.Fatalf("public/admin differ: %v / %v", a.Msg, p.Msg)
		}
		c := a.Msg.CoverageSummary
		if c == nil || c.CoverageStart == nil || *c.CoverageStart != base.Add(-time.Minute).Unix() || c.EligibleMinutes != 10 || c.ObservedMinutes != 10 || c.ObservedReportedMinutes != 0 {
			t.Fatalf("summary=%v", c)
		}
		if len(a.Msg.Coverage) != len(a.Msg.Ts) || len(a.Msg.Ts) == 0 {
			t.Fatal("coverage not aligned", a.Msg)
		}
		for _, series := range a.Msg.Series {
			for _, sample := range series.Samples {
				if sample.N != 0 || sample.Mean != nil || sample.Max != nil || sample.Sum != nil {
					t.Fatalf("observation fabricated sample: %v", sample)
				}
			}
		}
	}
}
