// Package netaddr 定义共享的公网地址判定。
package netaddr

import (
	"net/netip"
	"slices"
)

// special 是 netip 的谓词（见 IsPublic）之外的非公网段。取舍的标准是 IANA 的两张登记表——IANA IPv4 Special-Purpose
// Address Registry 与 IANA IPv6 Special-Purpose Address Registry——里 Globally Reachable 为 False 的条目：这些地址
// 在公网上没有归属，自然也没有国家。段内登记表另标为全球可达的更细分配列在 except，按公网处理。
//
// 192.88.99.0/24 是已废弃的 6to4 中继任播（RFC 7526），登记表里它的 Globally Reachable 为空而不是 False，按公网；
// 其中单列的 192.88.99.2/32（6a44 中继任播）是 False，列在下面。
//
// 登记表之外的取舍：
//   - fec0::/10 是 RFC 3879 废弃的站点本地地址，不在特殊用途登记表里（记在 IPv6 地址空间登记表），语义与 ULA 相同，
//     按非公网处理。
//   - 6to4（2002::/16）与 Teredo（2001::/32）登记表标为 N/A，NAT64 知名前缀（64:ff9b::/96）标为全球可达，三者都按
//     公网处理：它们把 IPv4 地址编进 IPv6 地址，这里按外层地址判定、不解出内嵌的 IPv4。按各自的定义，内嵌的都是
//     公网 IPv4：6to4 编进站点全球唯一的 IPv4（RFC 3056），Teredo 编进 Teredo 服务器与客户端 NAT 的外部地址
//     （RFC 4380），知名前缀不得用来表示非全球的 IPv4（RFC 6052 第 3.1 节）。内嵌私网 IPv4 的写法（如 2002:a00:1::）
//     不是有效部署，出现时照查。
var special = []struct {
	prefix netip.Prefix
	except []netip.Prefix
}{
	{prefix: netip.MustParsePrefix("0.0.0.0/8")},     // "本网络"，RFC 791；含未指定地址
	{prefix: netip.MustParsePrefix("100.64.0.0/10")}, // CGNAT，RFC 6598
	{prefix: netip.MustParsePrefix("192.0.0.0/24"), except: []netip.Prefix{ // IETF 协议分配，RFC 6890
		netip.MustParsePrefix("192.0.0.9/32"),  // PCP 任播，RFC 7723
		netip.MustParsePrefix("192.0.0.10/32"), // TURN 任播，RFC 8155
	}},
	{prefix: netip.MustParsePrefix("192.0.2.0/24")},    // 文档，RFC 5737
	{prefix: netip.MustParsePrefix("192.88.99.2/32")},  // 6a44 中继任播，RFC 6751；所在的 /24 见上
	{prefix: netip.MustParsePrefix("198.18.0.0/15")},   // 基准测试，RFC 2544
	{prefix: netip.MustParsePrefix("198.51.100.0/24")}, // 文档，RFC 5737
	{prefix: netip.MustParsePrefix("203.0.113.0/24")},  // 文档，RFC 5737
	{prefix: netip.MustParsePrefix("240.0.0.0/4")},     // 保留，RFC 1112；含受限广播
	{prefix: netip.MustParsePrefix("64:ff9b:1::/48")},  // 本地 NAT64，RFC 8215
	{prefix: netip.MustParsePrefix("100::/64")},        // 丢弃，RFC 6666
	{prefix: netip.MustParsePrefix("100:0:0:1::/64")},  // Dummy IPv6 Prefix，RFC 9780
	{prefix: netip.MustParsePrefix("2001::/23"), except: []netip.Prefix{ // IETF 协议分配，RFC 2928；含基准测试 2001:2::/48（RFC 5180）
		netip.MustParsePrefix("2001::/32"),       // Teredo，RFC 4380：登记表标为 N/A，按公网处理（见上）
		netip.MustParsePrefix("2001:1::1/128"),   // PCP 任播，RFC 7723
		netip.MustParsePrefix("2001:1::2/128"),   // TURN 任播，RFC 8155
		netip.MustParsePrefix("2001:1::3/128"),   // DNS-SD 服务注册协议任播，RFC 9665
		netip.MustParsePrefix("2001:3::/32"),     // AMT，RFC 7450
		netip.MustParsePrefix("2001:4:112::/48"), // AS112-v6，RFC 7535
		netip.MustParsePrefix("2001:20::/28"),    // ORCHIDv2，RFC 7343
		netip.MustParsePrefix("2001:30::/28"),    // 无人机远程识别实体标签（DET），RFC 9374
	}},
	{prefix: netip.MustParsePrefix("2001:db8::/32")}, // 文档，RFC 3849
	{prefix: netip.MustParsePrefix("3fff::/20")},     // 文档，RFC 9637
	{prefix: netip.MustParsePrefix("5f00::/16")},     // SRv6 SID，RFC 9602
	{prefix: netip.MustParsePrefix("fec0::/10")},     // 废弃的站点本地，RFC 3879（见上）
}

// IsPublic 判定公网地址，供国家查询、出口探测与上报准入共用。RFC 1918 与 ULA（IsPrivate）、回环、链路本地、组播、未指定由 netip 的谓词判定，其余非公网段列在
// special。IPv4 映射的 IPv6 地址按其 IPv4 判定：不同调用方传入的写法可能不同，还原后对同一个地址给出同一个答案。
func IsPublic(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() || addr.IsPrivate() {
		return false
	}
	for _, sp := range special {
		if sp.prefix.Contains(addr) && !slices.ContainsFunc(sp.except, func(p netip.Prefix) bool { return p.Contains(addr) }) {
			return false
		}
	}
	return true
}

// ParsePublicFamily 不接受映射或 zone 写法：出口字段的地址族由协议字段决定，不依赖文本的隐式转换。
func ParsePublicFamily(text string, ipv4 bool) (netip.Addr, bool) {
	address, err := netip.ParseAddr(text)
	return address, err == nil && address.Zone() == "" && !address.Is4In6() && address.Is4() == ipv4 && IsPublic(address)
}
