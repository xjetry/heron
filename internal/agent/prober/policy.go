package prober

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// defaultDeny 是本地策略的固定默认拒绝集（规则见 Policy）：本机、链路本地（含云厂商的 metadata 地址 169.254.169.254）、组播、
// 广播，以及落在私网段里的两个云 metadata 地址。宿主机自己接口上的地址另由 Targets 在每次探测前枚举，按同样的默认拒绝处理。
// 私网本身不在其中：内网互测是常见用法，是否拒绝由宿主机的 probe_deny 决定。
var defaultDeny = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("255.255.255.255/32"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("100.100.100.200/32"), // 阿里云 metadata，落在 CGNAT 100.64.0.0/10
	netip.MustParsePrefix("fd00:ec2::254/128"),  // AWS Nitro 的 IPv6 metadata，落在 ULA fc00::/7
}

// mapped 是 IPv4 映射的 IPv6 地址段。Check 先把目标还原成 IPv4 再判断，写在这一段里的本地前缀因地址族不同永远匹配不到。
var mapped = netip.MustParsePrefix("::ffff:0:0/96")

// Policy 是宿主机对探测目标地址的本地策略。策略只来自本地配置，hub 改不了它。
//
// 零值即只有 defaultDeny；Targets 每次探测前枚举本机接口，私网目标不在默认拒绝集中。
// ParsePolicy 拒绝非规范前缀、allow/deny 重叠和不可能命中的映射前缀，解析失败不会静默放行地址。
type Policy struct {
	allow, deny []netip.Prefix
}

// ParsePolicy 解析配置里的 probe_allow 与 probe_deny。前缀必须是规范形式（主机位为零）：10.1.2.3/8 这类写法
// 看不出作者要的是 10.0.0.0/8 还是 10.1.2.3/32，按配置错误拒绝。同一前缀同时出现在两个列表里也是配置错误，
// 否则最长前缀匹配在两者等长时没有答案。
func ParsePolicy(allow, deny []string) (Policy, error) {
	parse := func(field string, in []string) ([]netip.Prefix, error) {
		out := make([]netip.Prefix, 0, len(in))
		for _, s := range in {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("%s: %q is not a CIDR prefix", field, s)
			}
			if p != p.Masked() {
				return nil, fmt.Errorf("%s: %q has host bits set; write %s", field, s, p.Masked())
			}
			// 静默不生效的前缀比报错更糟：写 probe_deny 的人以为拒绝了，实际放行。
			if p.Addr().Is6() && p.Bits() >= mapped.Bits() && mapped.Contains(p.Addr()) {
				return nil, fmt.Errorf("%s: %q is an IPv4-mapped prefix, which never matches because targets are compared as IPv4; write %s/%d", field, s, p.Addr().Unmap(), p.Bits()-mapped.Bits())
			}
			out = append(out, p)
		}
		return out, nil
	}
	a, err := parse("probe_allow", allow)
	if err != nil {
		return Policy{}, err
	}
	d, err := parse("probe_deny", deny)
	if err != nil {
		return Policy{}, err
	}
	for _, p := range a {
		if slices.Contains(d, p) {
			return Policy{}, fmt.Errorf("%s is in both probe_allow and probe_deny", p)
		}
	}
	return Policy{allow: a, deny: d}, nil
}

// Check 按最长前缀匹配裁决：本地前缀与默认前缀等长时本地优先，所以 probe_allow 写 127.0.0.0/8 就能放行本机回环。
// host 是宿主机接口上的地址。固定默认集之外的每一个按满长前缀并入默认拒绝：probe_allow 写出同一个 /32（/128）才放行它，
// 更短的放行前缀（如整个私网段）不覆盖它。已落在固定默认集里的（lo 上的 127.0.0.1、链路本地地址）仍由固定前缀管辖，
// 否则满长前缀会压过 probe_allow 的 127.0.0.0/8，放行回环就得逐个写出接口地址。没有任何前缀匹配时放行。
func (p Policy) Check(a netip.Addr, host []netip.Addr) error {
	a = a.Unmap()
	best, allowed := -1, true
	var hit string
	consider := func(q netip.Prefix, allow, local bool, name string) {
		if !q.Contains(a) {
			return
		}
		// 等长时本地前缀覆盖默认前缀；本地两列表之间的等长已被 ParsePolicy 排除。
		if q.Bits() > best || (q.Bits() == best && local) {
			best, allowed, hit = q.Bits(), allow, name
		}
	}
	for _, q := range defaultDeny {
		consider(q, false, false, q.String())
	}
	for _, h := range host {
		h = h.Unmap()
		if !slices.ContainsFunc(defaultDeny, func(q netip.Prefix) bool { return q.Contains(h) }) {
			consider(netip.PrefixFrom(h, h.BitLen()), false, false, "an address of this host")
		}
	}
	for _, q := range p.deny {
		consider(q, false, true, q.String())
	}
	for _, q := range p.allow {
		consider(q, true, true, q.String())
	}
	if allowed {
		return nil
	}
	return fmt.Errorf("target %s denied by local probe policy (%s)", a, hit)
}

// String 供启动日志说明生效的策略。
func (p Policy) String() string {
	join := func(ps []netip.Prefix) string {
		s := make([]string, len(ps))
		for i, q := range ps {
			s[i] = q.String()
		}
		return "[" + strings.Join(s, " ") + "]"
	}
	return "default_deny=" + join(defaultDeny) + "+host_addresses probe_deny=" + join(p.deny) + " probe_allow=" + join(p.allow)
}

// Targets 是探测引擎取目标地址的唯一入口：解析与本地策略检查在同一处，今后新增的探测种类经过它即受策略约束（规则见 Policy）。
// 检查的是解析后实际要连的地址，所以名字无法绕过；首选地址被拒时不换用其他地址，否则攻击者可以借 DNS 挑选。
//
// 宿主机的接口地址每次探测前重新枚举：地址会随 DHCP、网卡增删而变，启动时取一次的快照会漏掉之后加上的地址。
// 连本机的非回环地址走本地路由，常能绕过只挡外部入站的防火墙，所以它们与回环同样默认拒绝。枚举失败时拒绝这次探测。
type Targets struct {
	Resolver *net.Resolver
	Policy   Policy
	// HostAddrs 列出宿主机接口上的地址；nil 取 net.InterfaceAddrs。
	HostAddrs func() ([]netip.Addr, error)
}

func (t Targets) Resolve(ctx context.Context, host string, v4, v6 bool) (netip.Addr, error) {
	ip, err := resolve(ctx, t.Resolver, host, v4, v6)
	if err != nil {
		return netip.Addr{}, err
	}
	list := t.HostAddrs
	if list == nil {
		list = interfaceAddrs
	}
	own, err := list()
	if err != nil {
		return netip.Addr{}, fmt.Errorf("list this host's addresses: %w", err)
	}
	if err := t.Policy.Check(ip, own); err != nil {
		return netip.Addr{}, err
	}
	return ip, nil
}

func interfaceAddrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out, nil
}
