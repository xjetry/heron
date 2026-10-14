package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/agent/client"
	"github.com/xjetry/heron/internal/agent/collect"
	"github.com/xjetry/heron/internal/agent/collect/collecttest"
	"github.com/xjetry/heron/internal/agent/prober"
	"github.com/xjetry/heron/internal/agentwire"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// 只带逐网卡列表的上报，管理快照与公开快照里照样有合计（ingest 用推导值补齐），形状与双报相同；网卡名只进账本，
// 两个快照里都没有。
func TestListOnlyReportHasTotalsInSnapshots(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, tok := h.createNode(t, "per-interface")
	h.setPublic(t, id, "per-interface", true)
	list := []*heronv1.NetInterfaceCounter{{Name: "uplink-secret-a", RxBytes: 1000, TxBytes: 2000}, {Name: "uplink-secret-b", RxBytes: 30, TxBytes: 40}}
	sum := agentwire.NetInterfaceSummary(list)
	req := connect.NewRequest(&heronv1.ReportRequest{Metrics: &heronv1.Metrics{BootId: testBootID}, NetInterfaces: list})
	req.Header().Set("Authorization", "Bearer "+tok)
	if _, err := h.agent.Report(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	admin, err := h.admin.GetSnapshot(t.Context(), connect.NewRequest(&heronv1.GetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(admin.Msg.Nodes) != 1 {
		t.Fatalf("admin snapshot nodes = %v", admin.Msg.Nodes)
	}
	if m := admin.Msg.Nodes[0].GetMetrics(); m.GetNetRxTotal() != sum.Rx || m.GetNetTxTotal() != sum.Tx || m.GetNetCounterEpoch() != sum.Epoch {
		t.Fatalf("admin snapshot metrics = %v, want totals %d/%d and epoch %s", m, sum.Rx, sum.Tx, sum.Epoch)
	}
	public, err := h.publicClient().GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(public.Msg.Nodes) != 1 || public.Msg.Nodes[0].GetMetrics().GetNetRxTotal() != sum.Rx || public.Msg.Nodes[0].GetMetrics().GetNetTxTotal() != sum.Tx {
		t.Fatalf("public snapshot = %v, want totals %d/%d", public.Msg, sum.Rx, sum.Tx)
	}
	adminJSON, err := protojson.Marshal(admin.Msg)
	if err != nil {
		t.Fatal(err)
	}
	raw := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil)
	if !bytes.Contains(raw.body, []byte(`"name":"per-interface"`)) || !bytes.Contains(adminJSON, []byte("per-interface")) {
		t.Fatalf("positive control missing: public %s admin %s", raw.body, adminJSON)
	}
	for what, body := range map[string][]byte{"admin": adminJSON, "public": raw.body} {
		if strings.Contains(string(body), "uplink-secret") {
			t.Fatalf("%s snapshot carries interface names: %s", what, body)
		}
	}
}

// recordingAgent 记下 Runner 发出的每个请求再原样转给 hub：断言的是 agent 真的只带了列表，而不是双报碰巧也对。
type recordingAgent struct {
	heronv1connect.AgentServiceClient
	mu   sync.Mutex
	sent []*heronv1.ReportRequest
}

func (a *recordingAgent) Report(ctx context.Context, req *connect.Request[heronv1.ReportRequest]) (*connect.Response[heronv1.ReportResponse], error) {
	a.mu.Lock()
	a.sent = append(a.sent, proto.Clone(req.Msg).(*heronv1.ReportRequest))
	a.mu.Unlock()
	return a.AgentServiceClient.Report(ctx, req)
}

// 真实的 Runner 对真实的 hub：agent 从首次上报起只带列表，管理与公开快照里照样有与列表一致的合计，
// 流量照常入账——经 ingest 的准入、补齐与账本，不只测账本。
func TestAgentSendsListOnlyAndHubStillShowsAndAccountsTotals(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, tok := h.createNode(t, "per-interface")
	h.setPublic(t, id, "per-interface", true)
	fsys := fstest.MapFS{"proc/sys/kernel/random/boot_id": {Data: []byte(testBootID + "\n")}}
	// 每轮 eth0 收 100、发 10 字节，eth1 收 1、发 2 字节。
	put := func(round uint64) {
		for name, c := range map[string][2]uint64{"eth0": {5000 + 100*round, 7000 + 10*round}, "eth1": {300 + round, 400 + 2*round}} {
			fsys["sys/class/net/"+name+"/statistics/rx_bytes"] = &fstest.MapFile{Data: fmt.Appendf(nil, "%d\n", c[0])}
			fsys["sys/class/net/"+name+"/statistics/tx_bytes"] = &fstest.MapFile{Data: fmt.Appendf(nil, "%d\n", c[1])}
		}
		// 结构规则对名字规则放行的每张网卡读 sysfs 的 type 与 uevent：网卡还在而属性读不出按网络采集失败处理，
		// 整个网络读数缺席，不是当作网卡已删除，所以只写计数会让每轮上报都不带列表。补齐普通以太网的缺省属性。
		collecttest.EthernetDefaults(fsys)
	}
	put(0)
	const rounds = 4
	rec := &recordingAgent{AgentServiceClient: h.agent}
	results := prober.NewQueue(prober.QueueCap)
	sched := prober.NewScheduler(prober.Multi{}, results, h.clk, slog.Default())
	t.Cleanup(sched.Stop)
	slept := 0
	r := &client.Runner{
		Collector: &collect.Collector{Host: hostFS(fsys), Clock: h.clk, Version: "t"},
		Client:    rec, Token: tok, Clock: h.clk, Rand: func() float64 { return 1 }, Log: slog.Default(),
		Prober: sched, Results: results, Interval: 10 * time.Second,
		// 每次上报之后时钟走一个下发间隔，再改计数；第 rounds 次上报之后停。
		Sleep: func(context.Context, time.Duration) error {
			slept++
			if slept == rounds {
				return context.Canceled
			}
			h.clk.Advance(10 * time.Second)
			put(uint64(slept))
			return nil
		},
	}
	if err := r.Run(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(rec.sent) != rounds {
		t.Fatalf("sent %d reports, want %d (a failed report retries and shifts the rounds)", len(rec.sent), rounds)
	}
	for i, req := range rec.sent {
		m, listed := req.GetMetrics(), len(req.NetInterfaces) == 2
		withTotals := m.NetRxTotal != nil || m.NetTxTotal != nil || m.NetCounterEpoch != ""
		if !listed || withTotals {
			t.Fatalf("report %d: list %v, totals given %v (%d/%d %q)", i, req.NetInterfaces, withTotals, m.GetNetRxTotal(), m.GetNetTxTotal(), m.NetCounterEpoch)
		}
	}
	sum := agentwire.NetInterfaceSummary(rec.sent[rounds-1].NetInterfaces)
	const wantRx, wantTx = (rounds - 1) * 101, (rounds - 1) * 12
	admin, err := h.admin.GetSnapshot(t.Context(), connect.NewRequest(&heronv1.GetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(admin.Msg.Nodes) != 1 {
		t.Fatalf("admin snapshot nodes = %v", admin.Msg.Nodes)
	}
	n := admin.Msg.Nodes[0]
	if m := n.GetMetrics(); m.GetNetRxTotal() != sum.Rx || m.GetNetTxTotal() != sum.Tx || m.GetNetCounterEpoch() != sum.Epoch {
		t.Fatalf("admin snapshot metrics = %v, want totals %d/%d and epoch %s", m, sum.Rx, sum.Tx, sum.Epoch)
	}
	if tr := n.GetTraffic(); tr.GetTotalRx() != wantRx || tr.GetTotalTx() != wantTx || tr.GetPeriodRx() != wantRx || tr.GetPeriodTx() != wantTx {
		t.Fatalf("admin snapshot traffic = %v, want %d/%d", tr, wantRx, wantTx)
	}
	public, err := h.publicClient().GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(public.Msg.Nodes) != 1 {
		t.Fatalf("public snapshot nodes = %v", public.Msg.Nodes)
	}
	p := public.Msg.Nodes[0]
	if p.GetMetrics().GetNetRxTotal() != sum.Rx || p.GetMetrics().GetNetTxTotal() != sum.Tx || p.GetTraffic().GetTotalRx() != wantRx || p.GetTraffic().GetTotalTx() != wantTx {
		t.Fatalf("public snapshot node = %v, want totals %d/%d and traffic %d/%d", p, sum.Rx, sum.Tx, wantRx, wantTx)
	}
}
