package ingest

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/hub/store"
	"google.golang.org/protobuf/proto"
)

func ifaceList(counters ...uint64) []*heronv1.NetInterfaceCounter {
	var out []*heronv1.NetInterfaceCounter
	for i := 0; i+1 < len(counters); i += 2 {
		out = append(out, &heronv1.NetInterfaceCounter{Name: fmt.Sprintf("eth%d", i/2), RxBytes: counters[i], TxBytes: counters[i+1]})
	}
	return out
}

func listReport(tok string, m *heronv1.Metrics, list []*heronv1.NetInterfaceCounter) *connect.Request[heronv1.ReportRequest] {
	req := report(tok, m)
	req.Msg.NetInterfaces = list
	return req
}

// 列表形状不合法时整条拒收，live 与账本均不变。
func TestReportRejectsMalformedInterfaces(t *testing.T) {
	over := make([]*heronv1.NetInterfaceCounter, agentwire.MaxNetInterfaces+1)
	for i := range over {
		over[i] = &heronv1.NetInterfaceCounter{Name: fmt.Sprintf("veth%03d", i)}
	}
	for _, tc := range []struct {
		name string
		m    *heronv1.Metrics
		list []*heronv1.NetInterfaceCounter
		err  string
	}{
		{"too_many", &heronv1.Metrics{BootId: testBootID}, over, "at most 64 interfaces; got 65"},
		{"unsorted", &heronv1.Metrics{BootId: testBootID}, []*heronv1.NetInterfaceCounter{{Name: "eth1"}, {Name: "eth0"}}, "unique and sorted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub(t)
			id, tok := h.node(t)
			_, err := h.client.Report(t.Context(), listReport(tok, tc.m, tc.list))
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("error = %v, want InvalidArgument naming %q", err, tc.err)
			}
			if _, ok := h.live.Get(id); ok {
				t.Error("rejected report changed live state")
			}
			if _, ok := h.book.Get(id); ok {
				t.Error("rejected report reached the traffic book")
			}
		})
	}
}

// 只带列表的上报合法：进入 live 之前用推导值补上合计与摘要，live 里的 Metrics 与双报时逐字节相同；列表本身不进 live。
// 合计始终取列表推导值，账本按逐网卡入账。
func TestReportNormalizesTotalsFromInterfaces(t *testing.T) {
	list := ifaceList(^uint64(0), 7, 5, 9)
	sum := agentwire.NetInterfaceSummary(list)
	want := &heronv1.Metrics{BootId: testBootID, NetRxTotal: proto.Uint64(sum.Rx), NetTxTotal: proto.Uint64(sum.Tx), NetCounterEpoch: sum.Epoch}
	for _, tc := range []struct {
		name string
		m    *heronv1.Metrics
	}{
		{"list_only", &heronv1.Metrics{BootId: testBootID}},
		{"dual", proto.Clone(want).(*heronv1.Metrics)},
		{"rx_only", &heronv1.Metrics{BootId: testBootID, NetRxTotal: proto.Uint64(sum.Rx)}},
		{"epoch_only", &heronv1.Metrics{BootId: testBootID, NetCounterEpoch: sum.Epoch}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub(t)
			id, tok := h.node(t)
			if _, err := h.client.Report(t.Context(), listReport(tok, tc.m, list)); err != nil {
				t.Fatal(err)
			}
			e, ok := h.live.Get(id)
			if !ok || !proto.Equal(e.Metrics, want) {
				t.Fatalf("live metrics = %v, want %v", e.Metrics, want)
			}
			book, _ := h.book.Get(id)
			wantBase := []store.TrafficCounter{{Name: "eth0", Rx: 1<<63 - 1, Tx: 7}, {Name: "eth1", Rx: 5, Tx: 9}}
			if !reflect.DeepEqual(book.Baseline, wantBase) || book.NetCounterEpoch != "" {
				t.Fatalf("book baseline = %+v epoch %q, want per-interface %+v", book.Baseline, book.NetCounterEpoch, wantBase)
			}
		})
	}
	t.Run("network_failure_stays_missing", func(t *testing.T) {
		h := newHub(t)
		id, tok := h.node(t)
		h.mustReport(t, tok, &heronv1.Metrics{BootId: testBootID})
		if e, _ := h.live.Get(id); e.Metrics.NetRxTotal != nil || e.Metrics.NetTxTotal != nil || e.Metrics.NetCounterEpoch != "" {
			t.Fatalf("report without counters gained totals: %v", e.Metrics)
		}
	})
}

// 只带列表的上报与双报在分钟桶与累计值上结果相同：合计视图只用于跨形态，逐网卡上报的增量来自逐网卡差分。
func TestListOnlyReportsAccountIntoTotalsAndMinuteBucket(t *testing.T) {
	h := newHub(t)
	id, tok := h.node(t)
	if _, err := h.client.Report(t.Context(), listReport(tok, &heronv1.Metrics{BootId: testBootID}, ifaceList(1000, 1000, 50, 50))); err != nil {
		t.Fatal(err)
	}
	pace(h)
	if _, err := h.client.Report(t.Context(), listReport(tok, &heronv1.Metrics{BootId: testBootID}, ifaceList(1100, 1300, 60, 70))); err != nil {
		t.Fatal(err)
	}
	if e, _ := h.book.Get(id); e.TotalRx != 110 || e.TotalTx != 320 {
		t.Fatalf("book after two list-only reports: %+v", e)
	}
	if sum, n := rxAccounted(h.live.Drain().Rows); sum != 110 || n != 1 {
		t.Fatalf("minute bucket rx = %v/%d, want 110/1", sum, n)
	}
}
