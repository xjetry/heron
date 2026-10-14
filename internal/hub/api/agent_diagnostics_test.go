package api

import (
	"bytes"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/agent/client"
	"github.com/xjetry/heron/internal/agent/collect"
	"github.com/xjetry/heron/internal/agent/collect/collecttest"
	"github.com/xjetry/heron/internal/hub/traffic"
	"github.com/xjetry/heron/internal/testwait"
	"google.golang.org/protobuf/proto"
)

// hostFS 是经真实采集器上报的测试共用的假主机：内容来自 fsys，磁盘总量 100、已用 50；识别查询不注入，
// 识别按读不出处理——这里的测试只看读数与诊断，不看执行环境。
func hostFS(fsys fs.FS) *collect.ProcFS {
	r := collecttest.New(fsys)
	r.UsageFunc = func() (uint64, uint64, error) { return 100, 50, nil }
	return &collect.ProcFS{Root: r, Reader: r}
}

func TestAgentScopeAndDiagnosticsThroughHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, token := h.createNode(t, "scope")
	h.setPublic(t, id, "scope", true)
	_, readerToken := createToken(t, h, "diagnostics-reader")
	fsys := fstest.MapFS{"proc/sys/kernel/random/boot_id": {Data: []byte(testBootID + "\n")}}
	put := func(name string, n uint64) {
		for direction, value := range map[string]uint64{"rx": n, "tx": n * 2} {
			fsys[fmt.Sprintf("sys/class/net/%s/statistics/%s_bytes", name, direction)] = &fstest.MapFile{Data: fmt.Appendf(nil, "%d", value)}
		}
	}
	col := &collect.Collector{Host: hostFS(fsys), Clock: h.clk, Version: "private-diagnostic-version", NetInclude: []string{"private-uplink-*"}}
	base := h.clk.Now().Truncate(time.Hour).Unix()
	var latest *heronv1.Facts
	var epoch string
	for i := range 5 {
		switch i {
		case 0:
			put("private-uplink-a", 100)
		case 1:
			put("private-uplink-a", 110)
		case 2:
			put("private-uplink-a", 120)
			put("private-uplink-b", 5000)
		case 3:
			put("private-uplink-a", 130)
			put("private-uplink-b", 5020)
		case 4:
			col.NetInclude = []string{"private-uplink-b"}
		}
		snap := col.Identify()
		m, _ := col.Metrics(snap)
		// 速率逐网卡求差：只有首样本缺失；加入 b、换掉过滤规则都只让新网卡建基线，留在集合里的网卡照常计。
		if (i == 0) != (m.NetRxBps == nil) || (i == 0) != (m.NetTxBps == nil) {
			t.Fatalf("sample %d rate presence: %v", i, m)
		}
		latest = col.Facts(snap)
		latest.Diagnostics.ReportIntervalMs = 10000
		req := connect.NewRequest(&heronv1.ReportRequest{Metrics: m, NetInterfaces: col.NetInterfaces(), Facts: latest, FactsHash: client.FactsHash(latest)})
		req.Header().Set("Authorization", "Bearer "+token)
		if _, err := h.agent.Report(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		epoch = m.NetCounterEpoch
		h.clk.Advance(10 * time.Second)
	}
	testwait.Until(t, time.Millisecond, func() bool {
		n, err := h.store.GetNode(t.Context(), id)
		return err == nil && proto.Equal(n.Facts, latest)
	}, "latest facts were not persisted")
	h.ingest.Flush(t.Context(), true)
	if err := h.book.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	reloaded := traffic.New(h.store, h.clk, h.book.Zone(), slog.Default())
	if err := reloaded.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	state, ok := reloaded.Get(id)
	// 逐网卡入账：a 的 10+10+10 与 b 加入之后的 20，b 加入与过滤换掉 a 都不丢留在集合里的网卡的增量。
	if !ok || state.TotalRx != 50 || state.TotalTx != 100 || state.NetCounterEpoch != "" || len(state.Baseline) != 1 || state.Baseline[0].Name != "private-uplink-b" {
		t.Fatalf("persisted scope baseline or totals: %+v", state)
	}
	// 重启后只带合计的上报（同一集合）经合计视图接上落盘的逐网卡基线。
	if delta, ok := reloaded.Account(id, &heronv1.Metrics{BootId: testBootID, NetCounterEpoch: epoch, NetRxTotal: proto.Uint64(5025), NetTxTotal: proto.Uint64(10050)}, nil); !ok || delta.Rx != 5 || delta.Tx != 10 {
		t.Fatalf("same-scope restart delta = %v %v", delta, ok)
	}
	reader := heronv1connect.NewAdminServiceClient(h.srv.Client(), h.srv.URL)
	req := connect.NewRequest(&heronv1.ListNodesRequest{})
	req.Header().Set("Authorization", "Bearer "+readerToken)
	nodes, err := reader.ListNodes(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	d := nodes.Msg.Nodes[0].GetFacts().GetDiagnostics()
	if !proto.Equal(d, latest.Diagnostics) || !slices.Equal(d.NetInterfaces, []string{"private-uplink-b"}) {
		t.Fatalf("management diagnostics missing: %v", d)
	}
	public := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil)
	if !bytes.Contains(public.body, []byte(`"name":"scope"`)) {
		t.Fatalf("public positive control missing: %s", public.body)
	}
	for _, secret := range []string{"private-uplink", "private-diagnostic-version", "diagnostics", "netCounterEpoch", epoch, token} {
		if bytes.Contains(public.body, []byte(secret)) {
			t.Fatalf("private diagnostic field leaked: %q in %s", secret, public.body)
		}
	}
	q := &heronv1.QueryMetricsRequest{NodeId: id, From: base, To: base + 3600, MaxPoints: 2000}
	adminHistory, err := h.admin.QueryMetrics(t.Context(), connect.NewRequest(q))
	if err != nil {
		t.Fatal(err)
	}
	publicHistory, err := h.publicClient().QueryMetrics(t.Context(), connect.NewRequest(q))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(adminHistory.Msg, publicHistory.Msg) {
		t.Fatal("management/public history disagree")
	}
	var byteSum float64
	var rateN uint32
	var peak float64
	for _, series := range adminHistory.Msg.Series {
		for _, sample := range series.Samples {
			if series.Name == "rx_bytes" {
				byteSum += sample.GetSum()
			}
			if series.Name == "net_rx_bps" {
				rateN += sample.N
				peak = max(peak, sample.GetMax())
			}
		}
	}
	if byteSum != 50 || rateN != 4 || peak != 3 {
		t.Fatalf("scope changes corrupted history: bytes=%v n=%v peak=%v", byteSum, rateN, peak)
	}
}
