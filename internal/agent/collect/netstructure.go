package collect

import (
	"bytes"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// tunnelARPTypes 是三层隧道的 ARPHRD 值（linux/if_arp.h）：NONE（tun、WireGuard）、TUNNEL（ipip）、TUNNEL6（ip6tnl）、
// SIT、IPGRE、IP6GRE。隧道里的字节封装后又计在承载网卡上。其余类型（PPP、InfiniBand 等）可能就是上联口，不在此列。
// 前四个值在内核 7.0.14-orbstack 的 alpine 3.22 privileged 容器里用 iproute2 建网卡实测过；
// IPGRE、IP6GRE 取自头文件，该内核没有这两个模块。GitHub 托管 runner（内核 6.17.0-1022-azure，
// Docker 28.0.4，alpine 3.21）上二者能建成，并按隧道计入。
var tunnelARPTypes = map[uint16]bool{65534: true, 768: true, 769: true, 776: true, 778: true, 823: true}

// structureReason 判定一张网卡的字节是否已计在另一张网卡上；UNSPECIFIED 表示照常计入。
// lower_* 只在同一网络命名空间内建立（内核 7.0.14-orbstack 的 alpine 3.22 privileged 容器：把 macvlan 与 VLAN
// 移进另一个网络命名空间后，那里没有 lower_*，原命名空间的 upper_* 也消失），所以移进容器的 VLAN、macvlan 在容器里没有它、照常计入；
// 出于同一原因不按 DEVTYPE=vlan 排除。挂在网桥上的物理口有 device，照常计入。
func structureReason(s netStructure) heronv1.NetStructureReason {
	switch {
	case s.stacked:
		return heronv1.NetStructureReason_NET_STRUCTURE_REASON_STACKED
	case s.bridgePort && !s.hasDevice:
		return heronv1.NetStructureReason_NET_STRUCTURE_REASON_BRIDGE_PORT
	case s.devType == "vxlan" || s.devType == "geneve":
		return heronv1.NetStructureReason_NET_STRUCTURE_REASON_OVERLAY
	case tunnelARPTypes[s.arpType]:
		return heronv1.NetStructureReason_NET_STRUCTURE_REASON_TUNNEL
	}
	return heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED
}

// parseUeventDevType 取 uevent 里 DEVTYPE= 行的值；没有该行（物理网卡、veth、macvlan 等）返回空串。
func parseUeventDevType(b []byte) string {
	for line := range bytes.SplitSeq(b, []byte{'\n'}) {
		if v, ok := bytes.CutPrefix(line, []byte("DEVTYPE=")); ok {
			return string(v)
		}
	}
	return ""
}
