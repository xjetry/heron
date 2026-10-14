package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

// hubSeenReport 经真实上报链路写来源（可带 facts）：带 X-Forwarded-For 的 Report 由可信代理（newHarness 的
// 第二参）解析出对端，h.ingest.Flush 把分钟行连同 last_source 落盘。facts 的异步写在 Report 里先行入队，
// Flush 的同步写在它之后入队，store 单写协程按 FIFO 消化，Flush 返回时两者都已提交。
func hubSeenReport(t *testing.T, h *harness, tok, xff string, facts *heronv1.Facts) {
	t.Helper()
	req := connect.NewRequest(&heronv1.ReportRequest{Metrics: &heronv1.Metrics{}, Facts: facts})
	req.Header().Set("Authorization", "Bearer "+tok)
	req.Header().Set("X-Forwarded-For", xff)
	if _, err := h.agent.Report(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	h.ingest.Flush(context.Background(), true)
}

// hubSeenNode 按 name 取 ListNodes 里那一个节点；两个节点的用例不能再用只认单节点的 listedNode。
func hubSeenNode(t *testing.T, h *harness, name string) *heronv1.Node {
	t.Helper()
	resp, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range resp.Msg.GetNodes() {
		if n.GetName() == name {
			return n
		}
	}
	t.Fatalf("ListNodes has no node %q: %v", name, resp.Msg.GetNodes())
	return nil
}

// hubSeenPublicNode 取推进 snapshotTTL 后的公开快照里 name 那个节点。
func hubSeenPublicNode(t *testing.T, h *harness, name string) *heronv1.PublicNode {
	t.Helper()
	h.clk.Advance(snapshotTTL)
	snap := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil)
	var out heronv1.PublicSnapshot
	if err := protojson.Unmarshal(snap.body, &out); err != nil {
		t.Fatalf("public snapshot %s: %v", snap.body, err)
	}
	for _, n := range out.GetNodes() {
		if n.GetName() == name {
			return n
		}
	}
	t.Fatalf("public snapshot has no node %q: %s", name, snap.body)
	return nil
}

var hubSeenLeakAddresses = []string{"8.8.8.8", "2606:4700"}

func hubSeenAssertNoLeak(t *testing.T, h *harness, stage string) {
	t.Helper()
	h.clk.Advance(snapshotTTL)
	snap := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil)
	for _, addr := range hubSeenLeakAddresses {
		if strings.Contains(string(snap.body), addr) {
			t.Fatalf("%s: public snapshot leaks the address %s: %s", stage, addr, snap.body)
		}
	}
}

// 管理端：agent 报 FAILED 的族显示 hub 看到的公网来源（HUB_SEEN、AVAILABLE、地址等于 last_source），agent 可用的族
// 照旧 DETECTED，facts.network 始终是 agent 原报。公开端：HUB_SEEN 的族 state 为 AVAILABLE，地址不出现——第一道在
// publicNetworkInput 的输出（投影输入就不带地址），第二道在公开响应的原始字节。
func TestNodeAddressHubSeenDisplayAndPublicState(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "127.0.0.1/32")
	h.login(t)
	id, tok := h.createNode(t, "n")
	h.setPublic(t, id, "n", true)
	available := heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE
	failed := heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_FAILED
	reported := &heronv1.NetworkInfo{
		Ipv4: &heronv1.AddressDetection{State: failed, CheckedAt: 100},
		Ipv6: &heronv1.AddressDetection{State: available, Address: "2606:4700::1111", CheckedAt: 100},
	}
	hubSeenReport(t, h, tok, "8.8.8.8", &heronv1.Facts{Hostname: "h", Network: reported})

	n := listedNode(t, h)
	if n.GetLastSource() != "8.8.8.8" {
		t.Fatalf("last_source = %q, want 8.8.8.8", n.GetLastSource())
	}
	want := &heronv1.NodeAddress{Address: "8.8.8.8", Source: heronv1.AddressSource_ADDRESS_SOURCE_HUB_SEEN, State: available}
	if !proto.Equal(n.GetNetwork().GetIpv4(), want) {
		t.Fatalf("network.ipv4 = %v, want HUB_SEEN AVAILABLE 8.8.8.8", n.GetNetwork().GetIpv4())
	}
	detectedV6 := &heronv1.NodeAddress{Address: "2606:4700::1111", Source: heronv1.AddressSource_ADDRESS_SOURCE_DETECTED, State: available}
	if !proto.Equal(n.GetNetwork().GetIpv6(), detectedV6) {
		t.Fatalf("network.ipv6 = %v, want the agent's detection %v", n.GetNetwork().GetIpv6(), detectedV6)
	}
	if !proto.Equal(n.GetFacts().GetNetwork(), reported) {
		t.Fatalf("facts.network = %v, want the agent's report %v unchanged", n.GetFacts().GetNetwork(), reported)
	}

	// 第一道：投影的输入就不带地址与探测时间，地址不公开不依赖投影丢弃这一道。
	input := publicNetworkInput([2]store.DisplayAddress{
		{Address: "8.8.8.8", Source: store.AddressHubSeen, State: available},
		{Address: "2606:4700::1111", Source: store.AddressHubSeen, State: available},
	})
	for _, family := range []*heronv1.AddressDetection{input.GetIpv4(), input.GetIpv6()} {
		if family.GetAddress() != "" || family.GetCheckedAt() != 0 {
			t.Fatalf("publicNetworkInput output carries %#v: address and checked_at must stay empty", family)
		}
		if family.GetState() != available {
			t.Fatalf("publicNetworkInput state = %s, want AVAILABLE for HUB_SEEN", family.GetState())
		}
	}

	pn := hubSeenPublicNode(t, h, "n")
	if got := pn.GetFacts().GetNetwork().GetIpv4(); got.GetState() != available {
		t.Fatalf("public network.ipv4 = %v, want AVAILABLE from HUB_SEEN", got)
	}
	if got := pn.GetFacts().GetNetwork().GetIpv6(); got.GetState() != available {
		t.Fatalf("public network.ipv6 = %v, want the agent's AVAILABLE kept", got)
	}
	hubSeenAssertNoLeak(t, h, "hub seen")
}

// 真实链路：经可信代理 XFF 上报的来源驱动显示值与公开状态。v4 来源让 agent 报 FAILED 的 v4 变 HUB_SEEN；换 v6 来源
// 后 v6 HUB_SEEN、v4 回到 agent 原报的 FAILED；私网来源两族都不回退。从未上报 facts 的节点（Report 不带
// facts）只带公网来源时，管理端显示 HUB_SEEN、公开端 PublicFacts 只有 network。两个节点分开构造，连续上报之间推进
// 时钟，分钟行不合并、facts 不残留。
func TestHubSeenSourceChainFromReports(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "127.0.0.1/32")
	h.login(t)
	everID, everTok := h.createNode(t, "ever")
	h.setPublic(t, everID, "ever", true)
	neverID, neverTok := h.createNode(t, "never")
	h.setPublic(t, neverID, "never", true)
	available := heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE
	failed := heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_FAILED
	hubSeen := &heronv1.NodeAddress{Address: "8.8.8.8", Source: heronv1.AddressSource_ADDRESS_SOURCE_HUB_SEEN, State: available}
	factsFailed := &heronv1.Facts{Hostname: "ever", Network: &heronv1.NetworkInfo{
		Ipv4: &heronv1.AddressDetection{State: failed, CheckedAt: 100},
	}}

	// 公网 v4 来源：v4 从 FAILED 变 HUB_SEEN。
	hubSeenReport(t, h, everTok, "8.8.8.8", factsFailed)
	ever := hubSeenNode(t, h, "ever")
	v4 := ever.GetNetwork().GetIpv4()
	if v4.GetSource() != heronv1.AddressSource_ADDRESS_SOURCE_HUB_SEEN || v4.GetAddress() != "8.8.8.8" || v4.GetState() != available {
		t.Fatalf("after v4 source: network.ipv4 = %v, want HUB_SEEN AVAILABLE 8.8.8.8", v4)
	}
	if pn := hubSeenPublicNode(t, h, "ever"); pn.GetFacts().GetNetwork().GetIpv4().GetState() != available {
		t.Fatalf("after v4 source: public network.ipv4 = %v, want AVAILABLE", pn.GetFacts().GetNetwork().GetIpv4())
	}

	// 换公网 v6 来源：来源只有一个族，v6 HUB_SEEN、v4 回到 agent 原报的 FAILED。
	h.clk.Advance(time.Minute)
	hubSeenReport(t, h, everTok, "2606:4700::1111", factsFailed)
	ever = hubSeenNode(t, h, "ever")
	if got := ever.GetNetwork().GetIpv4(); got.GetSource() != heronv1.AddressSource_ADDRESS_SOURCE_DETECTED || got.GetState() != failed {
		t.Fatalf("after v6 source: network.ipv4 = %v, want DETECTED FAILED", got)
	}
	v6 := ever.GetNetwork().GetIpv6()
	if v6.GetSource() != heronv1.AddressSource_ADDRESS_SOURCE_HUB_SEEN || v6.GetAddress() != "2606:4700::1111" || v6.GetState() != available {
		t.Fatalf("after v6 source: network.ipv6 = %v, want HUB_SEEN AVAILABLE 2606:4700::1111", v6)
	}
	pn := hubSeenPublicNode(t, h, "ever")
	if got := pn.GetFacts().GetNetwork().GetIpv4(); got.GetState() != failed {
		t.Fatalf("after v6 source: public network.ipv4 = %v, want FAILED", got)
	}
	if got := pn.GetFacts().GetNetwork().GetIpv6(); got.GetState() != available {
		t.Fatalf("after v6 source: public network.ipv6 = %v, want AVAILABLE", got)
	}

	// 私网来源：不是该族公网单播地址，两族都不回退。
	h.clk.Advance(time.Minute)
	hubSeenReport(t, h, everTok, "10.0.0.5", factsFailed)
	ever = hubSeenNode(t, h, "ever")
	if got := ever.GetNetwork().GetIpv4(); got.GetSource() != heronv1.AddressSource_ADDRESS_SOURCE_DETECTED || got.GetState() != failed {
		t.Fatalf("after private source: network.ipv4 = %v, want DETECTED FAILED", got)
	}
	if got := ever.GetNetwork().GetIpv6().GetSource(); got != heronv1.AddressSource_ADDRESS_SOURCE_UNSPECIFIED {
		t.Fatalf("after private source: network.ipv6 source = %s, want UNSPECIFIED", got)
	}
	pn = hubSeenPublicNode(t, h, "ever")
	if got := pn.GetFacts().GetNetwork().GetIpv4(); got.GetState() != failed {
		t.Fatalf("after private source: public network.ipv4 = %v, want FAILED", got)
	}
	if pn.GetFacts().GetNetwork().GetIpv6() != nil {
		t.Fatalf("after private source: public network.ipv6 = %v, want absent", pn.GetFacts().GetNetwork().GetIpv6())
	}
	hubSeenAssertNoLeak(t, h, "private source")

	// 从未上报 facts：Report 不带 facts，只有公网来源——管理端显示 HUB_SEEN，公开端 PublicFacts 只有 network。
	h.clk.Advance(time.Minute)
	hubSeenReport(t, h, neverTok, "8.8.8.8", nil)
	never := hubSeenNode(t, h, "never")
	if !proto.Equal(never.GetNetwork().GetIpv4(), hubSeen) {
		t.Fatalf("never-reported node network.ipv4 = %v, want HUB_SEEN AVAILABLE 8.8.8.8", never.GetNetwork().GetIpv4())
	}
	if never.GetFacts().GetNetwork() != nil {
		t.Fatalf("never-reported node has facts.network %v, want none", never.GetFacts().GetNetwork())
	}
	pn = hubSeenPublicNode(t, h, "never")
	f := pn.GetFacts()
	if f.GetNetwork().GetIpv4().GetState() != available || f.GetNetwork().GetIpv6() != nil {
		t.Fatalf("never-reported node public facts.network = %v, want ipv4 AVAILABLE only", f.GetNetwork())
	}
	if f.GetOs() != "" || f.GetArch() != "" || f.GetCpuModel() != "" || f.GetCpuCores() != 0 {
		t.Fatalf("public facts of a never-reported node carry host fields: %v", f)
	}
	hubSeenAssertNoLeak(t, h, "never reported")
}
