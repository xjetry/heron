package collect

import (
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
)

// iface 往夹具里放一张网卡：计数、type 与 uevent 是真实 sysfs 里每张网卡都有的三样，其余属性按需给出。
func iface(fsys fstest.MapFS, name string, arpType int, devType string, extra ...string) {
	dir := "sys/class/net/" + name
	fsys[dir+"/statistics/rx_bytes"] = &fstest.MapFile{Data: []byte("100\n")}
	fsys[dir+"/statistics/tx_bytes"] = &fstest.MapFile{Data: []byte("100\n")}
	fsys[dir+"/type"] = &fstest.MapFile{Data: []byte(fmt.Sprintf("%d\n", arpType))}
	ue := "INTERFACE=" + name + "\nIFINDEX=7\n"
	if devType != "" {
		ue = "DEVTYPE=" + devType + "\n" + ue
	}
	fsys[dir+"/uevent"] = &fstest.MapFile{Data: []byte(ue)}
	for _, e := range extra {
		switch {
		case e == "brport":
			fsys[dir+"/brport/state"] = &fstest.MapFile{Data: []byte("3\n")}
		case e == "device":
			fsys[dir+"/device"] = &fstest.MapFile{Mode: fs.ModeSymlink}
		case strings.HasPrefix(e, "lower_"):
			fsys[dir+"/"+e] = &fstest.MapFile{Mode: fs.ModeSymlink}
		}
	}
}

func TestStructureReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    netStructure
		want heronv1.NetStructureReason
	}{
		{"physical", netStructure{arpType: 1, hasDevice: true}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED},
		{"veth in a container", netStructure{arpType: 1}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED},
		{"physical bridge port", netStructure{arpType: 1, bridgePort: true, hasDevice: true}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED},
		{"stacked", netStructure{arpType: 1, stacked: true}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_STACKED},
		{"virtual bridge port", netStructure{arpType: 1, bridgePort: true}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_BRIDGE_PORT},
		{"vxlan without dev", netStructure{arpType: 1, devType: "vxlan"}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_OVERLAY},
		{"geneve", netStructure{arpType: 1, devType: "geneve"}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_OVERLAY},
		{"vlan moved into a container", netStructure{arpType: 1, devType: "vlan"}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED},
		{"wireguard", netStructure{arpType: 65534, devType: "wireguard"}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL},
		{"ipip", netStructure{arpType: 768}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL},
		{"ip6tnl", netStructure{arpType: 769}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL},
		{"sit", netStructure{arpType: 776}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL},
		{"gre", netStructure{arpType: 778}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL},
		{"ip6gre", netStructure{arpType: 823}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL},
		{"ppp may be the uplink", netStructure{arpType: 512}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED},
		{"infiniband may be the uplink", netStructure{arpType: 32, hasDevice: true}, heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED},
	} {
		if got := structureReason(tc.s); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func structProc(fsys fstest.MapFS) *ProcFS {
	return hostProc(fsys, func() (uint64, uint64, error) { return 0, 0, nil })
}

// Proxmox 形状的主机：物理口挂在 vmbr0 上，虚拟机的 tap 也挂在上面，另有 VLAN 与 WireGuard。
// 只有物理口计入；tap 先被名字规则排除、不进结构诊断，其余按原因排除；诊断按名称排序。
func TestStructureRulesKeepOnlyTheCarrier(t *testing.T) {
	fsys := fstest.MapFS{}
	iface(fsys, "eno1", 1, "", "brport", "device")
	iface(fsys, "vmbr0", 1, "bridge", "lower_eno1", "lower_tap100i0")
	iface(fsys, "tap100i0", 1, "", "brport")
	iface(fsys, "eno1.20", 1, "vlan", "lower_eno1")
	iface(fsys, "wg0", 65534, "wireguard")
	iface(fsys, "lo", 772, "")
	c := &Collector{Host: structProc(fsys), Clock: clock.NewFake(time.Unix(0, 0))}
	m, _ := c.Metrics(c.Identify())
	if m.GetNetRxTotal() != 100 {
		t.Fatalf("rx total = %d, want eno1 only", m.GetNetRxTotal())
	}
	d := c.Facts(c.Identify()).GetDiagnostics()
	if !d.GetNetStructureRules() || d.GetNetStructureFallback() {
		t.Fatalf("rules=%v fallback=%v, want rules on without fallback", d.GetNetStructureRules(), d.GetNetStructureFallback())
	}
	got := []string{}
	for _, e := range d.GetNetStructureExcluded() {
		got = append(got, e.GetName()+":"+e.GetReason().String())
	}
	want := "eno1.20:NET_STRUCTURE_REASON_STACKED vmbr0:NET_STRUCTURE_REASON_STACKED wg0:NET_STRUCTURE_REASON_TUNNEL"
	if strings.Join(got, " ") != want || d.GetNetStructureExcludedTotal() != 3 {
		t.Fatalf("excluded = %v (total %d), want %s", got, d.GetNetStructureExcludedTotal(), want)
	}
	if err := agentwire.ValidateDiagnostics(d); err != nil {
		t.Fatalf("diagnostics rejected: %v", err)
	}
}

// 显式规则整个替换默认规则：结构规则不生效，诊断标为未启用。
func TestExplicitRulesDisableStructureRules(t *testing.T) {
	fsys := fstest.MapFS{}
	iface(fsys, "eth0", 1, "", "device")
	iface(fsys, "wg0", 65534, "wireguard")
	for _, c := range []*Collector{
		{Host: structProc(fsys), Clock: clock.NewFake(time.Unix(0, 0)), NetExclude: []string{"lo"}},
		{Host: structProc(fsys), Clock: clock.NewFake(time.Unix(0, 0)), NetExclude: []string{}},
		{Host: structProc(fsys), Clock: clock.NewFake(time.Unix(0, 0)), NetInclude: []string{"*"}},
	} {
		m, _ := c.Metrics(c.Identify())
		d := c.Facts(c.Identify()).GetDiagnostics()
		if m.GetNetRxTotal() != 200 || d.GetNetStructureRules() || d.GetNetStructureExcludedTotal() != 0 {
			t.Fatalf("rx=%d rules=%v excluded=%d, want both counted and rules off", m.GetNetRxTotal(), d.GetNetStructureRules(), d.GetNetStructureExcludedTotal())
		}
	}
}

// 结构规则排空名字规则放行的网卡时退回名字规则；进入与离开退回各记一行日志，中间不重复记。
func TestStructureFallbackWhenEverythingIsExcluded(t *testing.T) {
	fsys := fstest.MapFS{}
	iface(fsys, "wg0", 65534, "wireguard")
	var logs strings.Builder
	c := &Collector{Host: structProc(fsys), Clock: clock.NewFake(time.Unix(0, 0)), Log: slog.New(slog.NewTextHandler(&logs, nil))}
	for range 2 {
		m, err := c.Metrics(c.Identify())
		if err != nil && strings.Contains(err.Error(), "net:") {
			t.Fatalf("fallback must not fail the network reading: %v", err)
		}
		if m.GetNetRxTotal() != 100 {
			t.Fatalf("rx = %d, want wg0 counted by fallback", m.GetNetRxTotal())
		}
	}
	d := c.Facts(c.Identify()).GetDiagnostics()
	if !d.GetNetStructureFallback() || d.GetNetStructureExcludedTotal() != 0 {
		t.Fatalf("fallback=%v excluded=%d", d.GetNetStructureFallback(), d.GetNetStructureExcludedTotal())
	}
	if err := agentwire.ValidateDiagnostics(d); err != nil {
		t.Fatalf("fallback diagnostics rejected: %v", err)
	}
	iface(fsys, "eth0", 1, "", "device")
	c.Metrics(c.Identify())
	if n := strings.Count(logs.String(), "structure"); n != 2 {
		t.Fatalf("%d structure log lines, want one on entering and one on leaving the fallback:\n%s", n, logs.String())
	}
}

// 列出计数之后网卡被删除：读结构时网卡目录已不在，按不存在的成员略过。
func TestVanishedInterfaceIsSkippedWhenReadingStructure(t *testing.T) {
	fsys := fstest.MapFS{}
	iface(fsys, "eth0", 1, "", "device")
	iface(fsys, "veth9", 1, "")
	h := &vanishingStructure{ProcFS: structProc(fsys), gone: "eth1"}
	iface(fsys, "eth1", 1, "")
	c := &Collector{Host: h, Clock: clock.NewFake(time.Unix(0, 0))}
	m, err := c.Metrics(c.Identify())
	if err != nil && strings.Contains(err.Error(), "net:") {
		t.Fatalf("a vanished interface must not fail the network reading: %v", err)
	}
	if m.GetNetRxTotal() != 100 {
		t.Fatalf("rx = %d, want eth0 only", m.GetNetRxTotal())
	}
}

type vanishingStructure struct {
	*ProcFS
	gone string
}

func (h *vanishingStructure) netStructure(name string) (netStructure, error) {
	if name == h.gone {
		return netStructure{}, errIfaceGone
	}
	return h.ProcFS.netStructure(name)
}

// 网卡还在而结构属性读不出：整个网络读数缺失，不把一张本该排除的网卡静默计入。
// 用 plainProc：hostProc 的夹具补齐（collecttest.EthernetDefaults）会把删掉的 type 补回来。
func TestUnreadableStructureFailsTheNetworkReading(t *testing.T) {
	fsys := fstest.MapFS{}
	iface(fsys, "eth0", 1, "", "device")
	iface(fsys, "wg0", 65534, "wireguard")
	c := &Collector{Host: plainProc(fsys), Clock: clock.NewFake(time.Unix(0, 0))}
	c.Metrics(c.Identify())
	delete(fsys, "sys/class/net/eth0/type")
	m, err := c.Metrics(c.Identify())
	if m.NetRxTotal != nil || err == nil || !strings.Contains(err.Error(), "net:") {
		t.Fatalf("rx=%v err=%v, want the network reading missing", m.NetRxTotal, err)
	}
	d := c.Facts(c.Identify()).GetDiagnostics()
	if !d.GetNetStructureRules() {
		t.Fatal("failed sampling must still report that structure rules are enabled")
	}
	if err := agentwire.ValidateDiagnostics(d); err != nil {
		t.Fatalf("failed network diagnostics rejected: %v", err)
	}
}

// vanishOnType 在 dir/type 被打开的那一刻让整个 dir 消失：网卡在 ReadDir 之后、读 type 之前被删除。
type vanishOnType struct {
	fs.FS
	dir  string
	gone bool
}

func (v *vanishOnType) Open(name string) (fs.File, error) {
	if name == v.dir+"/type" {
		v.gone = true
	}
	if v.gone && (name == v.dir || strings.HasPrefix(name, v.dir+"/")) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return v.FS.Open(name)
}

// 读结构属性的中途网卡被删除：type 打不开时目录也已不在，按已删除略过，不让整个网络读数缺失。
func TestInterfaceRemovedWhileReadingStructureIsSkipped(t *testing.T) {
	fsys := fstest.MapFS{}
	iface(fsys, "eth0", 1, "", "device")
	iface(fsys, "eth1", 1, "")
	c := &Collector{Host: plainProc(&vanishOnType{FS: fsys, dir: "sys/class/net/eth1"}), Clock: clock.NewFake(time.Unix(0, 0))}
	m, err := c.Metrics(c.Identify())
	if err != nil && strings.Contains(err.Error(), "net:") {
		t.Fatalf("an interface removed mid-read must not fail the network reading: %v", err)
	}
	if m.GetNetRxTotal() != 100 {
		t.Fatalf("rx = %d, want eth0 only", m.GetNetRxTotal())
	}
}

// 超过 128 张被结构排除时诊断截断、总数如实，且仍通过诊断准入。
func TestStructureDiagnosticsAreTruncatedWithTheTrueTotal(t *testing.T) {
	fsys := fstest.MapFS{}
	iface(fsys, "eth0", 1, "", "device")
	for i := range 130 {
		iface(fsys, fmt.Sprintf("fwpr%03dp0", i), 1, "", "brport")
	}
	c := &Collector{Host: structProc(fsys), Clock: clock.NewFake(time.Unix(0, 0))}
	c.Metrics(c.Identify())
	d := c.Facts(c.Identify()).GetDiagnostics()
	if d.GetNetStructureExcludedTotal() != 130 || len(d.GetNetStructureExcluded()) != agentwire.MaxDiagnosticInterfaces {
		t.Fatalf("total=%d listed=%d", d.GetNetStructureExcludedTotal(), len(d.GetNetStructureExcluded()))
	}
	if err := agentwire.ValidateDiagnostics(d); err != nil {
		t.Fatal(err)
	}
}

// darwin 不提供结构属性：规则不启用，诊断如实标为未启用。
func TestNoStructureRulesWithoutPlatformSupport(t *testing.T) {
	c := &Collector{Host: &ifacesOnly{list: []ifaceCounters{{"en0", 1, 1}}}, Clock: clock.NewFake(time.Unix(0, 0))}
	c.Metrics(c.Identify())
	if c.Facts(c.Identify()).GetDiagnostics().GetNetStructureRules() {
		t.Fatal("rules must be off on a platform without structure attributes")
	}
}

func TestParseUeventDevType(t *testing.T) {
	if got := parseUeventDevType([]byte("DEVTYPE=vxlan\nINTERFACE=vx0\nIFINDEX=9\n")); got != "vxlan" {
		t.Fatalf("got %q", got)
	}
	if got := parseUeventDevType([]byte("INTERFACE=eth0\nIFINDEX=2\n")); got != "" {
		t.Fatalf("got %q", got)
	}
}
