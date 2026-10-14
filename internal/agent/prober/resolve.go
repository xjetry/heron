package prober

import (
	"context"
	"fmt"
	"net"
	"net/netip"
)

// IP 字面量反映射后直接使用；名字的地址族按 socket 可用性选、v4 优先，不看路由；
// 仅 IPv6 的主机上双栈名字会选到 v4（前提是 v4 socket 可创建）。
// 调用方让解析与连接共用超时上下文，解析完成后才开始测量 RTT。
func resolve(ctx context.Context, resolver *net.Resolver, host string, v4, v6 bool) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap(), nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ips, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.Addr{}, err
	}
	if v4 {
		for _, ip := range ips {
			if ip.Unmap().Is4() {
				return ip.Unmap(), nil
			}
		}
	}
	if v6 {
		for _, ip := range ips {
			if ip.Is6() && !ip.Is4In6() {
				return ip, nil
			}
		}
	}
	return netip.Addr{}, fmt.Errorf("no address in an available family for %q", host)
}
