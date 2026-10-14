//go:build linux && netstructurelive

package collect

import (
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
)

// 由 scripts/netstructure-live.sh 在 privileged 容器里运行：setup 脚本现建各类网卡，把建成的名字放进
// HERON_LIVE_IFACES；建不出的（内核没有该模块）不在其中。这里用与生产相同的 ProcFS 读真实 sysfs，核对
// structureReason 的每条判定：叠加、无 device 的网桥口、vxlan/geneve、隧道 ARP 类型是已在别处计过的字节，其余照常计入。期望表没有的名字让测试失败：setup 新增网卡必须同时给出期望。
var liveWant = map[string]heronv1.NetStructureReason{
	"br0": heronv1.NetStructureReason_NET_STRUCTURE_REASON_STACKED, "bond0": heronv1.NetStructureReason_NET_STRUCTURE_REASON_STACKED,
	"d1.100": heronv1.NetStructureReason_NET_STRUCTURE_REASON_STACKED, "mv0": heronv1.NetStructureReason_NET_STRUCTURE_REASON_STACKED,
	"ipv0": heronv1.NetStructureReason_NET_STRUCTURE_REASON_STACKED, "vxd": heronv1.NetStructureReason_NET_STRUCTURE_REASON_STACKED,
	"tapb": heronv1.NetStructureReason_NET_STRUCTURE_REASON_BRIDGE_PORT, "va": heronv1.NetStructureReason_NET_STRUCTURE_REASON_BRIDGE_PORT,
	"d0":  heronv1.NetStructureReason_NET_STRUCTURE_REASON_BRIDGE_PORT,
	"vxn": heronv1.NetStructureReason_NET_STRUCTURE_REASON_OVERLAY, "gnv0": heronv1.NetStructureReason_NET_STRUCTURE_REASON_OVERLAY,
	"tun0": heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL, "wg0": heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL,
	"ipip0": heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL, "sit1": heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL,
	"ip6t0": heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL, "gre1": heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL,
	"ip6g0": heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL,
	"eth0":  heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED, "d1": heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED,
	"d2": heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED, "d3": heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED,
	"vb": heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED,
	// 移进另一个网络命名空间的 macvlan 与 VLAN：在那里没有 lower_*，照常计入（内核 7.0.14-orbstack 的 alpine 容器实测；GitHub 托管 runner 内核 6.17.0-1022-azure、Docker 28.0.4、alpine 3.21 上复现）。
	"mvx": heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED, "d1.200": heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED,
}

func TestLiveNetStructure(t *testing.T) {
	names := strings.Fields(os.Getenv("HERON_LIVE_IFACES"))
	if len(names) == 0 {
		t.Fatal("HERON_LIVE_IFACES is empty: run through scripts/netstructure-live.sh")
	}
	p, err := newProcFS("", unix.Openat2)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	for _, name := range names {
		want, ok := liveWant[name]
		if !ok {
			t.Fatalf("no expectation for created interface %q", name)
		}
		s, err := p.netStructure(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := structureReason(s); got != want {
			t.Errorf("%s: %+v classified %v, want %v", name, s, got, want)
		}
	}
	// 整条采集路径，双向核对。正向：setup 建成的每张网卡，名字规则排除的既不计入也不进结构诊断；结构上应计入的
	// 必须在计入集合里；结构上应排除的必须在排除诊断里且原因一致。反向：计入集合与排除诊断里出现的已知名字必须是
	// setup 建成的（建成名单记错时不会静默少测）。加载 sit、ipip、gre 等模块时内核在每个命名空间自建的回退设备
	// （sit0、tunl0、ip6tnl0、gre0……）不由 setup 创建、随内核而异，只记录不断言。
	c := &Collector{Host: p, Clock: clock.NewFake(time.Unix(0, 0))}
	if _, err := c.Metrics(c.Identify()); err != nil && strings.Contains(err.Error(), "net:") {
		t.Fatalf("network reading failed: %v", err)
	}
	d := c.Facts(c.Identify()).GetDiagnostics()
	if !d.GetNetStructureRules() || d.GetNetStructureFallback() {
		t.Fatalf("structure rules=%v fallback=%v, want enabled without fallback", d.GetNetStructureRules(), d.GetNetStructureFallback())
	}
	created := map[string]bool{}
	for _, name := range names {
		created[name] = true
	}
	counted := map[string]bool{}
	for _, name := range d.GetNetInterfaces() {
		counted[name] = true
	}
	excluded := map[string]heronv1.NetStructureReason{}
	for _, e := range d.GetNetStructureExcluded() {
		excluded[e.GetName()] = e.GetReason()
	}
	if int(d.GetNetStructureExcludedTotal()) != len(excluded) {
		t.Fatalf("excluded total %d but %d listed; the live setup stays under the display limit", d.GetNetStructureExcludedTotal(), len(excluded))
	}
	nameRules := p.defaultNetExclude()
	for _, name := range names {
		want := liveWant[name]
		got, listed := excluded[name]
		switch {
		case matchAny(nameRules, name):
			if counted[name] || listed {
				t.Errorf("%s is excluded by name and must reach neither the counted set nor the structure diagnostics", name)
			}
		case want == heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED:
			if !counted[name] || listed {
				t.Errorf("%s: counted=%v listed=%v, want counted and not listed", name, counted[name], listed)
			}
		default:
			if !listed || got != want || counted[name] {
				t.Errorf("%s: listed=%v as %v, counted=%v; want listed as %v and not counted", name, listed, got, counted[name], want)
			}
		}
	}
	for name := range counted {
		if _, known := liveWant[name]; !known {
			t.Logf("kernel-created %s counted", name)
		} else if !created[name] {
			t.Errorf("%s counted but setup did not report creating it", name)
		}
	}
	for name, reason := range excluded {
		if _, known := liveWant[name]; !known {
			t.Logf("kernel-created %s excluded as %v", name, reason)
		} else if !created[name] {
			t.Errorf("%s excluded but setup did not report creating it", name)
		}
	}
	t.Logf("counted %v, excluded %v", d.GetNetInterfaces(), d.GetNetStructureExcluded())
}
