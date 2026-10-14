package api

import (
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

// tcpLive 是 Metrics 与 PublicMetrics 共有的四个读取器：实时快照两端各取各的消息类型。
type tcpLive interface {
	GetTcpActiveOpensPs() float64
	GetTcpPassiveOpensPs() float64
	GetTcpOutSegsPs() float64
	GetTcpRetransSegsPs() float64
}

// 四个 TCP 速率进管理端与公开端的实时快照与历史：单位 count/s，建连带峰值、段只有均值，重传率可由两列的 mean × n
// 求出。
func TestTCPRatesOnAdminAndPublicRealtimeAndHistory(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, tok := h.createNode(t, "tcp")
	h.setPublic(t, id, "tcp", true)
	if err := h.report(t, tok, &heronv1.Metrics{TcpActiveOpensPs: proto.Float64(3), TcpPassiveOpensPs: proto.Float64(2),
		TcpOutSegsPs: proto.Float64(200), TcpRetransSegsPs: proto.Float64(2)}); err != nil {
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
	live := map[string]tcpLive{}
	for _, n := range adminSnap.Msg.Nodes {
		if n.Id == id && n.Metrics != nil {
			live["admin"] = n.Metrics
		}
	}
	for _, n := range publicSnap.Msg.Nodes {
		if n.Id == id && n.Metrics != nil {
			live["public"] = n.Metrics
		}
	}
	for _, name := range []string{"admin", "public"} {
		m := live[name]
		if m == nil || m.GetTcpActiveOpensPs() != 3 || m.GetTcpPassiveOpensPs() != 2 || m.GetTcpOutSegsPs() != 200 || m.GetTcpRetransSegsPs() != 2 {
			t.Fatalf("%s realtime tcp = %v, want 3 2 200 2", name, m)
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
		sample := func(series string) *heronv1.MetricSample {
			t.Helper()
			for _, s := range resp.Series {
				if s.Name != series {
					continue
				}
				if s.Unit != "count/s" {
					t.Fatalf("%s %s unit = %q, want count/s", name, series, s.Unit)
				}
				for _, p := range s.Samples {
					if p.N > 0 {
						return p
					}
				}
			}
			t.Fatalf("%s history has no sample for %s", name, series)
			return nil
		}
		if a := sample("tcp_active_opens_ps"); a.GetMean() != 3 || a.Max == nil || a.GetMax() != 3 {
			t.Fatalf("%s active = %v", name, a)
		}
		if p := sample("tcp_passive_opens_ps"); p.GetMean() != 2 || p.Max == nil || p.GetMax() != 2 {
			t.Fatalf("%s passive = %v", name, p)
		}
		out, retrans := sample("tcp_out_segs_ps"), sample("tcp_retrans_segs_ps")
		if out.Max != nil || retrans.Max != nil {
			t.Fatalf("%s segment series must carry no max", name)
		}
		if ratio := retrans.GetMean() * float64(retrans.N) / (out.GetMean() * float64(out.N)) * 100; ratio != 1 {
			t.Fatalf("%s retrans ratio = %v, want 1", name, ratio)
		}
	}
}
