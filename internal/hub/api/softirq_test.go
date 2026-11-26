package api

import (
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

// softirq 占比从一次上报进管理端与公开端的实时快照，入账写库后经两端的 QueryMetrics 读成 percent 序列，带均值与峰值。
// 与 steal、iowait 一样在两端都可见。
func TestSoftirqOnAdminAndPublicRealtimeAndHistory(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, tok := h.createNode(t, "softirq")
	h.setPublic(t, id, "softirq", true)
	if err := h.report(t, tok, &heronv1.Metrics{CpuPct: proto.Float64(40), CpuSoftirqPct: proto.Float64(12.5)}); err != nil {
		t.Fatal(err)
	}
	adminSnap, err := h.admin.GetSnapshot(t.Context(), connect.NewRequest(&heronv1.GetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	publicSnap, err := h.publicClient().GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]*float64{}
	for _, n := range adminSnap.Msg.Nodes {
		if n.Id == id && n.Metrics != nil {
			live["admin"] = n.Metrics.CpuSoftirqPct
		}
	}
	for _, n := range publicSnap.Msg.Nodes {
		if n.Id == id && n.Metrics != nil {
			live["public"] = n.Metrics.CpuSoftirqPct
		}
	}
	for _, name := range []string{"admin", "public"} {
		if v := live[name]; v == nil || *v != 12.5 {
			t.Fatalf("%s realtime softirq = %v, want 12.5", name, v)
		}
	}
	h.ingest.Flush(t.Context(), true)
	now := h.clk.Now().Unix()
	from := now - now%60
	q := &heronv1.QueryMetricsRequest{NodeId: id, From: from, To: from + 120, MaxPoints: 100}
	admin, err := h.admin.QueryMetrics(t.Context(), connect.NewRequest(q))
	if err != nil {
		t.Fatal(err)
	}
	public, err := h.publicClient().QueryMetrics(t.Context(), connect.NewRequest(q))
	if err != nil {
		t.Fatal(err)
	}
	for name, resp := range map[string]*heronv1.QueryMetricsResponse{"admin": admin.Msg, "public": public.Msg} {
		var got *heronv1.MetricSample
		for _, s := range resp.Series {
			if s.Name != "cpu_softirq_pct" {
				continue
			}
			if s.Unit != "percent" {
				t.Fatalf("%s cpu_softirq_pct unit = %q, want percent", name, s.Unit)
			}
			for _, p := range s.Samples {
				if p.N > 0 {
					got = p
				}
			}
		}
		if got == nil || got.N != 1 || got.GetMean() != 12.5 || got.Max == nil || got.GetMax() != 12.5 {
			t.Fatalf("%s history cpu_softirq_pct = %v, want one sample of 12.5 with its peak", name, got)
		}
	}
}
