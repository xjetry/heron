package prober

import (
	"context"
	"fmt"
	"net"
	"net/netip"
)

// IP 字面量反映射后直接使用；名字只在 v4、v6 指明的族里取地址，v4 优先，不看路由。
// 哪些族可用由 Targets.Resolve 按任务地址族、本机地址与引擎能力算出，这里不再判断。
// 单族时仍查 A 与 AAAA 两类记录再按族挑：AAAA 里的 IPv4 映射地址还原后是 IPv4 地址，只查 A 会漏掉它。
// 调用方让解析与连接共用超时上下文，解析完成后才开始测量 RTT。
func resolve(ctx context.Context, resolver *net.Resolver, host string, v4, v6 bool) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap(), nil
	}
	if !v4 && !v6 {
		return netip.Addr{}, fmt.Errorf("no address in an available family for %q", host)
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
