package api

import (
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

// 按核负载同时出现在管理端与公开端的实时快照和历史里。只从其中一条路径拿掉，对应的断言会红，另一条仍绿。
func TestLoad1PerCoreOnAdminAndPublicRealtimeAndHistory(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, tok := h.createNode(t, "load")
	h.setPublic(t, id, "load", true)
	perCore := 1.25
	if err := h.report(t, tok, &heronv1.Metrics{
		Load1: proto.Float64(2.5), Load5: proto.Float64(2), Load15: proto.Float64(1),
		Load1PerCore: proto.Float64(perCore),
	}); err != nil {
		t.Fatal(err)
	}
	adminSnap, err := h.admin.GetSnapshot(t.Context(), connect.NewRequest(&heronv1.GetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var adminLive *float64
	for _, n := range adminSnap.Msg.Nodes {
		if n.Id == id && n.Metrics != nil {
			adminLive = n.Metrics.Load1PerCore
		}
	}
	if adminLive == nil || *adminLive != perCore {
		t.Fatalf("admin realtime load1_per_core=%v, want %v", adminLive, perCore)
	}
	publicSnap, err := h.publicClient().GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var publicLive *float64
	for _, n := range publicSnap.Msg.Nodes {
		if n.Id == id {
			publicLive = n.GetMetrics().Load1PerCore
		}
	}
	if publicLive == nil || *publicLive != perCore {
		t.Fatalf("public realtime load1_per_core=%v, want %v", publicLive, perCore)
	}
	h.ingest.Flush(t.Context(), true)
	now := h.clk.Now().Unix()
	from := now - now%60
	q := &heronv1.QueryMetricsRequest{NodeId: id, From: from, To: from + 120, MaxPoints: 100}
	adminHist, err := h.admin.QueryMetrics(t.Context(), connect.NewRequest(q))
	if err != nil {
		t.Fatal(err)
	}
	publicHist, err := h.publicClient().QueryMetrics(t.Context(), connect.NewRequest(q))
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, resp *heronv1.QueryMetricsResponse) {
		t.Helper()
		for _, series := range resp.Series {
			if series.Name != "load1_per_core" {
				continue
			}
			for _, sample := range series.Samples {
				if sample.N > 0 && sample.GetMean() == perCore {
					return
				}
			}
			t.Fatalf("%s history has load1_per_core but no sample %v", name, perCore)
		}
		t.Fatalf("%s history omitted load1_per_core", name)
	}
	check("admin", adminHist.Msg)
	check("public", publicHist.Msg)
}
