package api

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

// 库层的三种出口地址来源与协议枚举的全部取值一一对应。
func TestAddressSourcesMapEveryValue(t *testing.T) {
	t.Parallel()
	values := heronv1.AddressSource(0).Descriptor().Values()
	var got []heronv1.AddressSource
	for _, s := range []store.AddressSource{store.AddressNone, store.AddressDetected, store.AddressManual, store.AddressHubSeen} {
		v, ok := addressSources[s]
		if !ok {
			t.Fatalf("store source %d has no protocol value", s)
		}
		got = append(got, v)
	}
	var want []heronv1.AddressSource
	for i := 0; i < values.Len(); i++ {
		want = append(want, heronv1.AddressSource(values.Get(i).Number()))
	}
	if len(addressSources) != values.Len() || !slices.Equal(got, want) {
		t.Fatalf("protocol values %v, want %v", got, want)
	}
}

func updateAddressPins(t *testing.T, h *harness, id int64, v4, v6 string) (*heronv1.Node, error) {
	t.Helper()
	req := &heronv1.UpdateNodeRequest{Id: id, Name: "n", Public: true, TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0), Ipv4Pin: v4, Ipv6Pin: v6}
	resp, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetNode(), nil
}

func publicNetwork(t *testing.T, h *harness) *heronv1.PublicNetworkInfo {
	t.Helper()
	h.clk.Advance(snapshotTTL)
	snap := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil)
	var out heronv1.PublicSnapshot
	if err := protojson.Unmarshal(snap.body, &out); err != nil || len(out.GetNodes()) != 1 {
		t.Fatalf("public snapshot %s: %v", snap.body, err)
	}
	return out.GetNodes()[0].GetFacts().GetNetwork()
}

// 管理端：手填回显规范形，显示值手填优先（MANUAL、AVAILABLE），另一族照旧取探测；facts.network 始终是 agent 原报。
// 公开端：手填的族 state 为 AVAILABLE（agent 报的是 FAILED），地址不出现；清空手填即回到 agent 原报的 FAILED。
func TestNodeAddressPinDisplayAndPublicState(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	h.setPublic(t, id, "n", true)
	reported := &heronv1.NetworkInfo{
		Ipv4: &heronv1.AddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_FAILED, CheckedAt: 100},
		Ipv6: &heronv1.AddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE, Address: "2606:4700::1111", CheckedAt: 100},
	}
	if err := h.store.UpsertFacts(t.Context(), id, 1, &heronv1.Facts{Hostname: "h", Network: reported}); err != nil {
		t.Fatal(err)
	}
	available := heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE
	failed := heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_FAILED
	detectedV6 := &heronv1.NodeAddress{Address: "2606:4700::1111", Source: heronv1.AddressSource_ADDRESS_SOURCE_DETECTED, State: available}
	check := func(stage string, n *heronv1.Node, pin string, v4 *heronv1.NodeAddress, public heronv1.AddressDetectionState) {
		t.Helper()
		if n.GetIpv4Pin() != pin || n.GetIpv6Pin() != "" {
			t.Fatalf("%s: pins = %q/%q, want %q/\"\"", stage, n.GetIpv4Pin(), n.GetIpv6Pin(), pin)
		}
		if !proto.Equal(n.GetNetwork().GetIpv4(), v4) || !proto.Equal(n.GetNetwork().GetIpv6(), detectedV6) {
			t.Fatalf("%s: network = %v, want ipv4 %v and ipv6 %v", stage, n.GetNetwork(), v4, detectedV6)
		}
		if !proto.Equal(n.GetFacts().GetNetwork(), reported) {
			t.Fatalf("%s: facts.network = %v, want the agent's report %v unchanged", stage, n.GetFacts().GetNetwork(), reported)
		}
		got := publicNetwork(t, h)
		if got.GetIpv4().GetState() != public || got.GetIpv6().GetState() != available {
			t.Fatalf("%s: public network = %v, want ipv4 %s and ipv6 AVAILABLE", stage, got, public)
		}
	}
	check("detected", listedNode(t, h), "", &heronv1.NodeAddress{Source: heronv1.AddressSource_ADDRESS_SOURCE_DETECTED, State: failed}, failed)
	n, err := updateAddressPins(t, h, id, "8.8.8.8", "")
	if err != nil {
		t.Fatal(err)
	}
	manual := &heronv1.NodeAddress{Address: "8.8.8.8", Source: heronv1.AddressSource_ADDRESS_SOURCE_MANUAL, State: available}
	check("pinned (response)", n, "8.8.8.8", manual, available)
	check("pinned", listedNode(t, h), "8.8.8.8", manual, available)
	h.clk.Advance(snapshotTTL)
	if snap := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil); strings.Contains(string(snap.body), "8.8.8.8") || strings.Contains(string(snap.body), "2606:4700") {
		t.Fatalf("public snapshot leaks an address: %s", snap.body)
	}
	if n, err = updateAddressPins(t, h, id, "", ""); err != nil {
		t.Fatal(err)
	}
	check("cleared", n, "", &heronv1.NodeAddress{Source: heronv1.AddressSource_ADDRESS_SOURCE_DETECTED, State: failed}, failed)
}

// 被拒的手填返回 InvalidArgument，点名字段与期望取值；什么都不写。判定在 store，这里钉的是协议层的字段路径与文案。
func TestUpdateNodeValidatesAddressPins(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	if _, err := updateAddressPins(t, h, id, "8.8.8.8", "2606:4700::1111"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ v4, v6, want string }{
		{"2606:4700::1111", "", `ipv4_pin: must be empty or a public IPv4 address, e.g. 8.8.8.8; got "2606:4700::1111"`},
		{"192.168.1.1", "", `ipv4_pin: must be empty or a public IPv4 address, e.g. 8.8.8.8; got "192.168.1.1"`},
		{"", "fe80::1%eth0", `ipv6_pin: must be empty or a public IPv6 address without a zone, e.g. 2606:4700::1111; got "fe80::1%eth0"`},
		{"", "2606:4700::1111%eth0", `ipv6_pin: must be empty or a public IPv6 address without a zone, e.g. 2606:4700::1111; got "2606:4700::1111%eth0"`},
		{"", "8.8.8.8", `ipv6_pin: must be empty or a public IPv6 address without a zone, e.g. 2606:4700::1111; got "8.8.8.8"`},
	} {
		_, err := updateAddressPins(t, h, id, tc.v4, tc.v6)
		if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("pins %q/%q: err = %v, want InvalidArgument containing %q", tc.v4, tc.v6, err, tc.want)
		}
		if n := listedNode(t, h); n.GetIpv4Pin() != "8.8.8.8" || n.GetIpv6Pin() != "2606:4700::1111" {
			t.Fatalf("rejected pins %q/%q changed the node to %q/%q", tc.v4, tc.v6, n.GetIpv4Pin(), n.GetIpv6Pin())
		}
	}
}

// 从未上报 facts 的节点：没有手填时公开快照不带 facts（与其余主机信息一致）；手填之后公开那一族的状态为 AVAILABLE，
// PublicFacts 只有 network，地址不出现；管理端显示值为 MANUAL。标记跟着显示值走，不跟着 agent 有没有上报过。
func TestNeverReportedNodePublishesPinnedFamily(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	h.setPublic(t, id, "n", true)
	publicNode := func(stage string) *heronv1.PublicNode {
		t.Helper()
		h.clk.Advance(snapshotTTL)
		snap := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil)
		var out heronv1.PublicSnapshot
		if err := protojson.Unmarshal(snap.body, &out); err != nil || len(out.GetNodes()) != 1 {
			t.Fatalf("%s: public snapshot %s: %v", stage, snap.body, err)
		}
		if strings.Contains(string(snap.body), "8.8.8.8") {
			t.Fatalf("%s: public snapshot leaks the pinned address: %s", stage, snap.body)
		}
		return out.GetNodes()[0]
	}
	if pn := publicNode("unpinned"); pn.Facts != nil {
		t.Fatalf("unpinned never-reported node has public facts %v, want none", pn.Facts)
	}
	n, err := updateAddressPins(t, h, id, "8.8.8.8", "")
	if err != nil {
		t.Fatal(err)
	}
	available := heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE
	if got := n.GetNetwork().GetIpv4(); got.GetSource() != heronv1.AddressSource_ADDRESS_SOURCE_MANUAL || got.GetState() != available || got.GetAddress() != "8.8.8.8" {
		t.Fatalf("admin network.ipv4 = %v, want MANUAL AVAILABLE 8.8.8.8", got)
	}
	pn := publicNode("pinned")
	if pn.Facts == nil || pn.GetFacts().GetNetwork().GetIpv4().GetState() != available || pn.GetFacts().GetNetwork().GetIpv6() != nil {
		t.Fatalf("pinned never-reported node public facts = %v, want network.ipv4 AVAILABLE only", pn.Facts)
	}
	if f := pn.GetFacts(); f.GetOs() != "" || f.GetArch() != "" || f.GetCpuModel() != "" || f.GetCpuCores() != 0 {
		t.Fatalf("public facts of a never-reported node carry host fields: %v", f)
	}
	if _, err := updateAddressPins(t, h, id, "", ""); err != nil {
		t.Fatal(err)
	}
	if pn := publicNode("cleared"); pn.Facts != nil {
		t.Fatalf("cleared never-reported node has public facts %v, want none", pn.Facts)
	}
}
