package agentwire

import (
	"fmt"
	"strings"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestDiagnosticsBoundedWhitelist(t *testing.T) {
	good := &heronv1.AgentDiagnostics{NetExclude: []string{"lo", "veth*"}, NetInterfaces: []string{"eth0"}, NetInterfacesTotal: 1, ReportIntervalMs: 10000}
	if err := ValidateDiagnostics(good); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDiagnostics(nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDiagnostics(&heronv1.AgentDiagnostics{FailedCollectors: []heronv1.CollectionComponent{heronv1.CollectionComponent_COLLECTION_COMPONENT_DISK_IO}}); err != nil {
		t.Fatalf("disk I/O component must be accepted: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*heronv1.AgentDiagnostics)
	}{
		{"unknown field", func(d *heronv1.AgentDiagnostics) { d.ProtoReflect().SetUnknown([]byte{0x3a, 0x01, 'x'}) }},
		{"both policies", func(d *heronv1.AgentDiagnostics) { d.NetInclude = []string{"eth*"} }},
		{"too many patterns", func(d *heronv1.AgentDiagnostics) {
			d.NetExclude = make([]string, MaxNetPatterns+1)
			for i := range d.NetExclude {
				d.NetExclude[i] = "lo"
			}
		}},
		{"long pattern", func(d *heronv1.AgentDiagnostics) { d.NetExclude = []string{strings.Repeat("x", MaxNetPatternBytes+1)} }},
		{"invalid glob", func(d *heronv1.AgentDiagnostics) { d.NetExclude = []string{"["} }},
		{"structure without rules", func(d *heronv1.AgentDiagnostics) {
			d.NetStructureExcluded = []*heronv1.NetStructureExclusion{{Name: "wg0", Reason: heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL}}
			d.NetStructureExcludedTotal = 1
		}},
		{"fallback without rules", func(d *heronv1.AgentDiagnostics) { d.NetStructureFallback = true }},
		{"rules with include", func(d *heronv1.AgentDiagnostics) {
			d.NetExclude, d.NetInclude, d.NetStructureRules = nil, []string{"eth*"}, true
		}},
		{"fallback with exclusions", func(d *heronv1.AgentDiagnostics) {
			d.NetStructureRules, d.NetStructureFallback = true, true
			d.NetStructureExcluded = []*heronv1.NetStructureExclusion{{Name: "wg0", Reason: heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL}}
			d.NetStructureExcludedTotal = 1
		}},
		{"unspecified reason", func(d *heronv1.AgentDiagnostics) {
			d.NetStructureRules = true
			d.NetStructureExcluded = []*heronv1.NetStructureExclusion{{Name: "wg0"}}
			d.NetStructureExcludedTotal = 1
		}},
		{"unknown reason", func(d *heronv1.AgentDiagnostics) {
			d.NetStructureRules = true
			d.NetStructureExcluded = []*heronv1.NetStructureExclusion{{Name: "wg0", Reason: 99}}
			d.NetStructureExcludedTotal = 1
		}},
		{"unsorted exclusions", func(d *heronv1.AgentDiagnostics) {
			d.NetStructureRules = true
			d.NetStructureExcluded = []*heronv1.NetStructureExclusion{
				{Name: "wg0", Reason: heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL},
				{Name: "br0", Reason: heronv1.NetStructureReason_NET_STRUCTURE_REASON_STACKED},
			}
			d.NetStructureExcludedTotal = 2
		}},
		{"exclusion count mismatch", func(d *heronv1.AgentDiagnostics) {
			d.NetStructureRules = true
			d.NetStructureExcluded = []*heronv1.NetStructureExclusion{{Name: "wg0", Reason: heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL}}
			d.NetStructureExcludedTotal = 2
		}},
		{"bad exclusion name", func(d *heronv1.AgentDiagnostics) {
			d.NetStructureRules = true
			d.NetStructureExcluded = []*heronv1.NetStructureExclusion{{Name: "wg\n0", Reason: heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL}}
			d.NetStructureExcludedTotal = 1
		}},
		{"failed net claims exclusions", func(d *heronv1.AgentDiagnostics) {
			d.NetInterfaces, d.NetInterfacesTotal = nil, 0
			d.FailedCollectors = []heronv1.CollectionComponent{heronv1.CollectionComponent_COLLECTION_COMPONENT_NET}
			d.NetStructureRules = true
			d.NetStructureExcluded = []*heronv1.NetStructureExclusion{{Name: "wg0", Reason: heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL}}
			d.NetStructureExcludedTotal = 1
		}},
		{"failed net claims fallback", func(d *heronv1.AgentDiagnostics) {
			d.NetInterfaces, d.NetInterfacesTotal = nil, 0
			d.FailedCollectors = []heronv1.CollectionComponent{heronv1.CollectionComponent_COLLECTION_COMPONENT_NET}
			d.NetStructureRules, d.NetStructureFallback = true, true
		}},
		{"unknown field in exclusion", func(d *heronv1.AgentDiagnostics) {
			d.NetStructureRules = true
			e := &heronv1.NetStructureExclusion{Name: "wg0", Reason: heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL}
			e.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 15, protowire.VarintType), 1))
			d.NetStructureExcluded, d.NetStructureExcludedTotal = []*heronv1.NetStructureExclusion{e}, 1
		}},
		{"control", func(d *heronv1.AgentDiagnostics) { d.NetExclude = []string{"lo\nsecret"} }},
		{"invalid utf8", func(d *heronv1.AgentDiagnostics) { d.NetExclude = []string{string([]byte{0xff})} }},
		{"total mismatch", func(d *heronv1.AgentDiagnostics) { d.NetInterfacesTotal = 2 }},
		{"long name", func(d *heronv1.AgentDiagnostics) {
			d.NetInterfaces = []string{strings.Repeat("x", MaxInterfaceNameBytes+1)}
		}},
		{"duplicate name", func(d *heronv1.AgentDiagnostics) {
			d.NetInterfaces = []string{"eth0", "eth0"}
			d.NetInterfacesTotal = 2
		}},
		{"unordered name", func(d *heronv1.AgentDiagnostics) {
			d.NetInterfaces = []string{"eth1", "eth0"}
			d.NetInterfacesTotal = 2
		}},
		{"unknown component", func(d *heronv1.AgentDiagnostics) { d.FailedCollectors = []heronv1.CollectionComponent{99} }},
		{"unspecified component", func(d *heronv1.AgentDiagnostics) { d.FailedCollectors = []heronv1.CollectionComponent{0} }},
		{"duplicate component", func(d *heronv1.AgentDiagnostics) { d.FailedCollectors = []heronv1.CollectionComponent{1, 1} }},
		{"failed network has names", func(d *heronv1.AgentDiagnostics) {
			d.FailedCollectors = []heronv1.CollectionComponent{heronv1.CollectionComponent_COLLECTION_COMPONENT_NET}
		}},
		{"short interval", func(d *heronv1.AgentDiagnostics) { d.ReportIntervalMs = 1 }},
		{"long interval", func(d *heronv1.AgentDiagnostics) { d.ReportIntervalMs = 60001 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := proto.Clone(good).(*heronv1.AgentDiagnostics)
			tc.change(d)
			if err := ValidateDiagnostics(d); err == nil {
				t.Fatalf("invalid diagnostics accepted: %v", d)
			}
		})
	}
	for _, epoch := range []string{"", strings.Repeat("a", 64)} {
		if err := ValidateCounterEpoch(epoch); err != nil {
			t.Fatal(err)
		}
	}
	for _, epoch := range []string{strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if err := ValidateCounterEpoch(epoch); err == nil {
			t.Fatalf("invalid epoch accepted: %q", epoch)
		}
	}
}

// 合法截断仍保留真实总数；TCP 计数失败不能被误判成未知类别。
func TestDiagnosticsAcceptStructureAndTCPStats(t *testing.T) {
	d := &heronv1.AgentDiagnostics{NetExclude: []string{"lo"}, NetInterfaces: []string{"eth0"}, NetInterfacesTotal: 1, NetStructureRules: true,
		FailedCollectors: []heronv1.CollectionComponent{heronv1.CollectionComponent_COLLECTION_COMPONENT_TCP_STATS}}
	for i := range MaxDiagnosticInterfaces {
		d.NetStructureExcluded = append(d.NetStructureExcluded, &heronv1.NetStructureExclusion{Name: fmt.Sprintf("tap%03d", i), Reason: heronv1.NetStructureReason_NET_STRUCTURE_REASON_BRIDGE_PORT})
	}
	d.NetStructureExcludedTotal = MaxDiagnosticInterfaces + 5
	if err := ValidateDiagnostics(d); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDiagnostics(&heronv1.AgentDiagnostics{NetExclude: []string{"lo"}, NetInterfaces: []string{"wg0"}, NetInterfacesTotal: 1, NetStructureRules: true, NetStructureFallback: true}); err != nil {
		t.Fatalf("fallback: %v", err)
	}
}

func TestDiagnosticsStructureBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*heronv1.AgentDiagnostics)
	}{
		{"empty name", func(d *heronv1.AgentDiagnostics) { d.NetStructureExcluded[0].Name = "" }},
		{"long name", func(d *heronv1.AgentDiagnostics) {
			d.NetStructureExcluded[0].Name = strings.Repeat("x", MaxInterfaceNameBytes+1)
		}},
		{"invalid utf8 name", func(d *heronv1.AgentDiagnostics) { d.NetStructureExcluded[0].Name = string([]byte{0xff}) }},
		{"nil entry", func(d *heronv1.AgentDiagnostics) { d.NetStructureExcluded[0] = nil }},
		{"negative reason", func(d *heronv1.AgentDiagnostics) { d.NetStructureExcluded[0].Reason = -1 }},
		{"zero total", func(d *heronv1.AgentDiagnostics) { d.NetStructureExcludedTotal = 0 }},
		{"duplicate name", func(d *heronv1.AgentDiagnostics) {
			d.NetStructureExcluded = append(d.NetStructureExcluded, proto.Clone(d.NetStructureExcluded[0]).(*heronv1.NetStructureExclusion))
			d.NetStructureExcludedTotal = 2
		}},
		{"too many exclusions", func(d *heronv1.AgentDiagnostics) {
			d.NetStructureExcluded = nil
			for i := range MaxDiagnosticInterfaces + 1 {
				d.NetStructureExcluded = append(d.NetStructureExcluded, &heronv1.NetStructureExclusion{Name: fmt.Sprintf("tap%03d", i), Reason: heronv1.NetStructureReason_NET_STRUCTURE_REASON_BRIDGE_PORT})
			}
			d.NetStructureExcludedTotal = MaxDiagnosticInterfaces + 1
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &heronv1.AgentDiagnostics{NetStructureRules: true, NetStructureExcludedTotal: 1,
				NetStructureExcluded: []*heronv1.NetStructureExclusion{{Name: "wg0", Reason: heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL}}}
			tc.change(d)
			if err := ValidateDiagnostics(d); err == nil {
				t.Fatalf("invalid structure diagnostics accepted: %v", d)
			}
		})
	}
}
