package prober

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
)

// 本机通往公网的路由的几种形状：具备某族只看该族有没有公网路由。
var (
	dualStackHost = stack{v4: true, v6: true}
	v6OnlyHost    = stack{v6: true}
	v4OnlyHost    = stack{v4: true}
	noStackHost   = stack{}
)

// bridgedHostAddrs 是装了 Docker 的主机的接口地址：docker0 上的 172.17.0.1 与一个全局 IPv6 地址，两族都有
// 非回环、非链路本地地址。它与上面任一路由形状搭配，接口地址都不能改变本机具备哪些族的结论。
var bridgedHostAddrs = []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1"), netip.MustParseAddr("172.17.0.1"), netip.MustParseAddr("2001:db8::10")}

// route 是按族问路由的接缝的固定答案。
func (s stack) route(v6 bool) bool {
	if v6 {
		return s.v6
	}
	return s.v4
}

// hostTargets 放行回环并固定本机的公网路由与接口地址：解析器把名字答成回环地址，本机具备哪些地址族只由 routes 决定，
// 结果不随运行测试的机器的网卡与路由表变化。
func hostTargets(t *testing.T, r *net.Resolver, routes stack) Targets {
	t.Helper()
	tg := loopbackTargets(t, r)
	tg.HostAddrs = func() ([]netip.Addr, error) { return bridgedHostAddrs, nil }
	tg.PublicRoute = routes.route
	return tg
}

// 自动地址族跟随本机协议栈：纯 IPv6 节点探测双栈名字选 AAAA，不再连向本机发不出去的 IPv4；显式地址族只走该族；
// 任务要求或字面量属于本机不具备的族时报探测错误且不发起连接——计入丢包会把本机能力缺失算成链路故障。
func TestTCPFamilyFollowsTaskAndHostStack(t *testing.T) {
	const (
		auto = heronv1.ProbeIpFamily_PROBE_IP_FAMILY_UNSPECIFIED
		v4   = heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV4
		v6   = heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV6
	)
	resolver := localDNS(t, dnsDual, nil)
	for _, tc := range []struct {
		name    string
		host    stack
		target  string
		family  heronv1.ProbeIpFamily
		dialed  string
		errPart string
	}{
		{"dual_auto_prefers_v4", dualStackHost, "dual.prober.invalid:9", auto, "127.0.0.1:9", ""},
		{"v6_only_auto_picks_aaaa", v6OnlyHost, "dual.prober.invalid:9", auto, "[::1]:9", ""},
		{"v4_only_auto", v4OnlyHost, "dual.prober.invalid:9", auto, "127.0.0.1:9", ""},
		{"dual_explicit_v6", dualStackHost, "dual.prober.invalid:9", v6, "[::1]:9", ""},
		{"dual_explicit_v4", dualStackHost, "dual.prober.invalid:9", v4, "127.0.0.1:9", ""},
		{"v4_only_explicit_v6", v4OnlyHost, "dual.prober.invalid:9", v6, "", "this host has no route to public IPv6 addresses"},
		{"v6_only_explicit_v4", v6OnlyHost, "dual.prober.invalid:9", v4, "", "this host has no route to public IPv4 addresses"},
		{"v6_only_v4_literal", v6OnlyHost, "127.0.0.1:9", auto, "", "this host has no route to public IPv4 addresses"},
		{"v4_only_v6_literal", v4OnlyHost, "[::1]:9", auto, "", "this host has no route to public IPv6 addresses"},
		{"no_stack_auto", noStackHost, "dual.prober.invalid:9", auto, "", "this host has no route to public IPv4 or IPv6 addresses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dialed []string
			p := TCP{Clock: clock.Real(), Targets: hostTargets(t, resolver, tc.host), DialContext: func(_ context.Context, _ string, address string) (net.Conn, error) {
				dialed = append(dialed, address)
				return nil, syscall.ECONNREFUSED
			}}
			probe := tcpTask(tc.target)
			probe.IpFamily = tc.family
			out := p.Probe(t.Context(), probe)
			if tc.errPart != "" {
				if out.Timeout || !strings.Contains(out.Err, tc.errPart) || len(dialed) != 0 {
					t.Fatalf("outcome=%+v dialed=%v, want a probe error containing %q and no connection", out, dialed, tc.errPart)
				}
				return
			}
			if len(dialed) != 1 || dialed[0] != tc.dialed || !out.Timeout {
				t.Fatalf("dialed=%v outcome=%+v, want one connection to %s", dialed, out, tc.dialed)
			}
		})
	}
}

// HTTP 与 TCP 共用同一个取址入口，任务的地址族同样生效：本机没有 IPv6 公网路由时要求 IPv6 的任务报错，不按自动退回 IPv4。
func TestHTTPHonorsTaskFamily(t *testing.T) {
	p := &HTTP{Clock: clock.Real(), Targets: hostTargets(t, localDNS(t, dnsDual, nil), v4OnlyHost), Version: "test"}
	probe := task(1)
	probe.Kind, probe.Target, probe.IpFamily = heronv1.ProbeKind_PROBE_KIND_HTTP, "http://dual.prober.invalid:9/", heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV6
	out := p.Probe(t.Context(), probe)
	if out.Timeout || !strings.Contains(out.Err, "this host has no route to public IPv6 addresses") {
		t.Fatalf("outcome=%+v, want the missing-family probe error", out)
	}
}

// ICMP 的地址族能力是本机公网路由与该族套接字两者都具备：自动模式在纯 IPv6 节点上选 AAAA；
// 显式要求的族缺套接字时报该族的套接字诊断，缺路由时报本机没有该族的公网路由。
func TestICMPFamilyFollowsSocketsAndHostStack(t *testing.T) {
	withSockets := func(t *testing.T, host stack, v4, v6 bool) (*ICMP, *string) {
		t.Helper()
		var destination string
		socket := func() *quietSocket {
			s := newQuietSocket()
			s.write = func(_ []byte, dst net.Addr) (int, error) {
				destination = dst.(*net.UDPAddr).IP.String()
				return 0, syscall.EPERM
			}
			return s
		}
		e := &ICMP{clk: clock.Real(), log: logger(), done: make(chan struct{}), Targets: hostTargets(t, localDNS(t, dnsDual, nil), host),
			initErr4: []string{"udp4: denied"}, initErr6: []string{"udp6: denied"}}
		if v4 {
			e.v4 = &icmpConn{pc: socket(), proto: 1, pending: map[pendingKey]chan time.Duration{}}
		}
		if v6 {
			e.v6 = &icmpConn{pc: socket(), proto: 58, pending: map[pendingKey]chan time.Duration{}}
		}
		t.Cleanup(e.Close)
		return e, &destination
	}
	probe := func(target string, family heronv1.ProbeIpFamily) *heronv1.ProbeTask {
		p := task(1)
		p.Target, p.IpFamily = target, family
		return p
	}

	e, dst := withSockets(t, v6OnlyHost, true, true)
	if out := e.Probe(t.Context(), probe("dual.prober.invalid", heronv1.ProbeIpFamily_PROBE_IP_FAMILY_UNSPECIFIED)); *dst != "::1" || !strings.Contains(out.Err, syscall.EPERM.Error()) {
		t.Fatalf("v6-only auto destination=%q outcome=%+v, want ::1", *dst, out)
	}
	e, dst = withSockets(t, dualStackHost, true, false)
	if out := e.Probe(t.Context(), probe("dual.prober.invalid", heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV6)); *dst != "" || out.Err != "icmp unavailable: udp6: denied" {
		t.Fatalf("explicit v6 without a v6 socket destination=%q outcome=%+v", *dst, out)
	}
	e, dst = withSockets(t, v4OnlyHost, true, true)
	if out := e.Probe(t.Context(), probe("dual.prober.invalid", heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV6)); *dst != "" || out.Timeout || !strings.Contains(out.Err, "this host has no route to public IPv6 addresses") {
		t.Fatalf("explicit v6 without a v6 address destination=%q outcome=%+v", *dst, out)
	}
	// 自动模式：本机有 IPv6 路由但只有 IPv4 套接字，又没有 IPv4 路由——两族都用不了。
	e, dst = withSockets(t, v6OnlyHost, true, false)
	if out := e.Probe(t.Context(), probe("dual.prober.invalid", heronv1.ProbeIpFamily_PROBE_IP_FAMILY_UNSPECIFIED)); *dst != "" || out.Timeout || out.Err == "" {
		t.Fatalf("auto with no usable family destination=%q outcome=%+v", *dst, out)
	}
}

// DNS 的记录类型决定查询与成功判据：查 AAAA 就发 AAAA 问题、应答里要有 AAAA 记录；
// 连解析器走哪一族由 dns_server 字面量决定，本机没有那一族时报探测错误，不发包。
func TestDNSRecordTypeAndResolverFamily(t *testing.T) {
	p := DNS{Clock: clock.Real(), Targets: hostTargets(t, nil, dualStackHost)}
	aaaaOnly, _ := fakeResolver(t, answerAAAA)
	aOnly, _ := fakeResolver(t, answerA)
	aaaa := func(server string) *heronv1.ProbeTask {
		task := dnsTask(server)
		task.DnsRecordType = heronv1.DnsRecordType_DNS_RECORD_TYPE_AAAA
		return task
	}
	if out := p.Probe(t.Context(), aaaa(aaaaOnly)); out.Err != "" || out.Timeout || out.RttUs == 0 {
		t.Fatalf("aaaa query=%+v, want success", out)
	}
	// 解析器只回答 AAAA 问题：缺省任务查的是 A，得到空应答即丢包。
	if out := p.Probe(t.Context(), dnsTask(aaaaOnly)); !out.Timeout || out.Err != "" {
		t.Fatalf("a query to an AAAA-only resolver=%+v, want loss", out)
	}
	// 只带 A 记录的应答不能算 AAAA 查询成功。
	if out := p.Probe(t.Context(), aaaa(aOnly)); !out.Timeout || out.Err != "" {
		t.Fatalf("aaaa query answered with A=%+v, want loss", out)
	}

	_, port, _ := net.SplitHostPort(aOnly)
	noV6 := DNS{Clock: clock.Real(), Targets: hostTargets(t, nil, v4OnlyHost)}
	if out := noV6.Probe(t.Context(), aaaa(net.JoinHostPort("::1", port))); out.Timeout || !strings.Contains(out.Err, "this host has no route to public IPv6 addresses") {
		t.Fatalf("v6 resolver on a v4-only host=%+v, want the missing-family probe error", out)
	}
}
