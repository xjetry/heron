package ingest

import (
	"math"
	"strings"
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

func tcpRates(a, p, o, r *float64) *heronv1.Metrics {
	return &heronv1.Metrics{TcpActiveOpensPs: a, TcpPassiveOpensPs: p, TcpOutSegsPs: o, TcpRetransSegsPs: r}
}

// 四项来自同一次读取，重传率要求分子分母同源：缺任一项或值非法都整条拒收，live 不变。
func TestTCPRatesAdmission(t *testing.T) {
	h := newHub(t)
	id, tok := h.node(t)
	reject := func(name string, m *heronv1.Metrics, snippet string) {
		t.Helper()
		pace(h)
		_, err := h.client.Report(t.Context(), report(tok, m))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), snippet) {
			t.Fatalf("%s: error=%v, want InvalidArgument containing %q", name, err, snippet)
		}
		if _, ok := h.live.Get(id); ok {
			t.Fatalf("%s changed live state", name)
		}
	}
	one, nan, inf := proto.Float64(1), math.NaN(), math.Inf(1)
	reject("缺一项", tcpRates(one, one, one, nil), "must be given together")
	reject("只有一项", tcpRates(nil, nil, nil, one), "must be given together")
	reject("非有限", tcpRates(one, one, &nan, one), "tcp_out_segs_ps")
	reject("无穷", tcpRates(&inf, one, one, one), "tcp_active_opens_ps")
	reject("负数", tcpRates(one, proto.Float64(-1), one, one), "tcp_passive_opens_ps")
	pace(h)
	zero := proto.Float64(0)
	if _, err := h.client.Report(t.Context(), report(tok, tcpRates(zero, zero, zero, proto.Float64(2)))); err != nil {
		t.Fatalf("measured zeros with retrans above out must be accepted: %v", err)
	}
	st, ok := h.live.Get(id)
	if !ok || st.Metrics.GetTcpRetransSegsPs() != 2 || st.Metrics.TcpOutSegsPs == nil {
		t.Fatalf("live = %+v", st)
	}
}
