package prober

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/probelimit"
)

// loopbackTargets 放行本机回环：引擎测试只连本机的服务，不依赖宿主网络。
func loopbackTargets(t *testing.T, r *net.Resolver) Targets {
	t.Helper()
	p, err := ParsePolicy([]string{"127.0.0.0/8", "::1/128"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return Targets{Resolver: r, Policy: p}
}

func TestPolicyDefaults(t *testing.T) {
	var p Policy
	for _, a := range []string{"0.0.0.0", "0.1.2.3", "127.0.0.1", "127.255.255.254", "169.254.169.254", "224.0.0.1", "239.255.255.250", "255.255.255.255", "::", "::1", "fe80::1", "ff02::1", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "100.100.100.200", "fd00:ec2::254"} {
		if err := p.Check(netip.MustParseAddr(a), nil); err == nil {
			t.Errorf("%s allowed by the zero policy", a)
		}
	}
	for _, a := range []string{"1.1.1.1", "8.8.8.8", "10.0.0.1", "172.16.0.1", "192.168.1.1", "100.64.0.1", "100.100.100.201", "2606:4700::1111", "fd00::1", "fd00:ec2::253", "::2", "255.255.255.254"} {
		if err := p.Check(netip.MustParseAddr(a), nil); err != nil {
			t.Errorf("%s denied by the zero policy: %v", a, err)
		}
	}
}

func TestPolicyLongestPrefixAndLocalOverride(t *testing.T) {
	p, err := ParsePolicy([]string{"127.0.0.0/8", "10.1.0.0/16", "169.254.169.0/24"}, []string{"10.0.0.0/8", "10.1.2.0/24", "fd00::/8"})
	if err != nil {
		t.Fatal(err)
	}
	for a, allowed := range map[string]bool{
		"127.0.0.1":       true,  // 本地放行与默认拒绝等长：本地优先
		"10.9.9.9":        false, // 本地拒绝
		"10.1.9.9":        true,  // 更长的本地放行
		"10.1.2.3":        false, // 再长一级的本地拒绝
		"169.254.169.254": true,  // 本地放行比默认拒绝更长
		"169.254.1.1":     false, // 默认拒绝仍在
		"fd12::1":         false,
		"::1":             false, // 只放行了 v4 回环
		"192.168.0.1":     true,
	} {
		if err := p.Check(netip.MustParseAddr(a), nil); (err == nil) != allowed {
			t.Errorf("%s: err=%v, want allowed=%v", a, err, allowed)
		}
	}
}

func TestParsePolicyRejects(t *testing.T) {
	for _, tc := range []struct {
		name        string
		allow, deny []string
		want        string
	}{
		{"not_cidr", []string{"10.0.0.1"}, nil, `probe_allow: "10.0.0.1" is not a CIDR prefix`},
		{"host_bits", nil, []string{"10.1.2.3/8"}, "has host bits set; write 10.0.0.0/8"},
		{"both", []string{"10.0.0.0/8"}, []string{"10.0.0.0/8"}, "10.0.0.0/8 is in both probe_allow and probe_deny"},
		{"garbage", nil, []string{"x"}, `probe_deny: "x" is not a CIDR prefix`},
		{"mapped_deny", nil, []string{"::ffff:10.0.0.0/104"}, "IPv4-mapped prefix, which never matches because targets are compared as IPv4; write 10.0.0.0/8"},
		{"mapped_all", nil, []string{"::ffff:0:0/96"}, "write 0.0.0.0/0"},
		{"mapped_allow", []string{"::ffff:127.0.0.0/104"}, nil, "write 127.0.0.0/8"},
	} {
		if _, err := ParsePolicy(tc.allow, tc.deny); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", tc.name, err, tc.want)
		}
	}
}

// 拒绝文本要进 ProbeError.message，最长的情形也不能超出 hub 的上限，否则 agent 截断后面板看不到命中的前缀。
func TestDenialMessageFitsProbeError(t *testing.T) {
	p, err := ParsePolicy(nil, []string{"ffff:ffff:ffff:ffff:ffff:ffff:ffff:fff0/128"})
	if err != nil {
		t.Fatal(err)
	}
	longest := netip.MustParseAddr("ffff:ffff:ffff:ffff:ffff:ffff:ffff:fff0")
	for _, err := range []error{p.Check(longest, nil), (Policy{}).Check(longest, []netip.Addr{longest})} {
		if err == nil || len(err.Error()) > probelimit.MaxErrorMessageLen {
			t.Fatalf("denial %q must fit %d bytes", err, probelimit.MaxErrorMessageLen)
		}
	}
}

// 两个引擎都经 Targets：默认策略下回环目标不发包，错误写明地址与前缀；名字解析到被拒地址同样被拒。
func TestEnginesApplyPolicyAfterResolution(t *testing.T) {
	socket := newQuietSocket()
	var sent int
	socket.write = func([]byte, net.Addr) (int, error) { sent++; return 0, syscall.EPERM }
	ic := icmpWithSocket(t, socket)
	ic.Targets = Targets{Resolver: localDNS(t, dnsDual, nil)}
	var dialed int
	tcp := TCP{Clock: clock.Real(), Targets: Targets{Resolver: localDNS(t, dnsDual, nil)}, DialContext: func(context.Context, string, string) (net.Conn, error) {
		dialed++
		return nil, syscall.ECONNREFUSED
	}}
	icmpTask := func(target string) Outcome { p := task(1); p.Target = target; return ic.Probe(t.Context(), p) }
	for name, out := range map[string]Outcome{
		"icmp_literal": icmpTask("127.0.0.1"),
		"icmp_name":    icmpTask("local.prober.invalid"),
		"tcp_literal":  tcp.Probe(t.Context(), tcpTask("127.0.0.1:22")),
		"tcp_name":     tcp.Probe(t.Context(), tcpTask("local.prober.invalid:22")),
		"tcp_mapped":   tcp.Probe(t.Context(), tcpTask("[::ffff:127.0.0.1]:22")),
	} {
		if out.Timeout || out.Err != "target 127.0.0.1 denied by local probe policy (127.0.0.0/8)" {
			t.Errorf("%s: %+v", name, out)
		}
	}
	if sent != 0 || dialed != 0 {
		t.Fatalf("denied targets reached the network: icmp sends=%d tcp dials=%d", sent, dialed)
	}
}

// IPv6 里不在映射段的前缀照常接受，包括覆盖映射段的 ::/0：它说的是 IPv6 地址空间。
func TestParsePolicyAcceptsNonMappedIPv6(t *testing.T) {
	for _, q := range []string{"::/0", "::/64", "::fffe:0:0/95", "fd00::/8"} {
		if _, err := ParsePolicy(nil, []string{q}); err != nil {
			t.Errorf("%s rejected: %v", q, err)
		}
	}
}

// 宿主机接口地址默认拒绝，只有写出同一个满长前缀才放行；落在固定默认集里的本机地址仍由固定前缀管辖。
func TestPolicyHostAddresses(t *testing.T) {
	host := []netip.Addr{netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("2001:db8::5"), netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("fe80::1"), netip.MustParseAddr("::ffff:192.168.1.9")}
	check := func(p Policy, a string) error { return p.Check(netip.MustParseAddr(a), host) }
	var zero Policy
	for _, a := range []string{"10.0.0.5", "2001:db8::5", "192.168.1.9", "::ffff:10.0.0.5", "127.0.0.1", "fe80::1"} {
		if err := check(zero, a); err == nil {
			t.Errorf("%s allowed although it is an address of this host", a)
		}
	}
	if err := check(zero, "10.0.0.5"); err == nil || err.Error() != "target 10.0.0.5 denied by local probe policy (an address of this host)" {
		t.Errorf("denial text: %v", err)
	}
	for _, a := range []string{"10.0.0.6", "2001:db8::6"} {
		if err := check(zero, a); err != nil {
			t.Errorf("%s denied: %v", a, err)
		}
	}
	wide, _ := ParsePolicy([]string{"10.0.0.0/8", "127.0.0.0/8"}, nil)
	if err := check(wide, "10.0.0.5"); err == nil {
		t.Error("a private-range allow must not open this host's own address")
	}
	if err := check(wide, "127.0.0.1"); err != nil {
		t.Errorf("allowing 127.0.0.0/8 must open loopback even though 127.0.0.1 is on lo: %v", err)
	}
	// 固定集里的本机地址随固定前缀：等长或更长的放行打开它，更短的不行。
	ll := []netip.Addr{netip.MustParseAddr("169.254.1.2")}
	for allow, want := range map[string]bool{"169.254.0.0/16": true, "169.254.1.0/24": true, "169.0.0.0/8": false} {
		p, _ := ParsePolicy([]string{allow}, nil)
		if err := p.Check(netip.MustParseAddr("169.254.1.2"), ll); (err == nil) != want {
			t.Errorf("allow %s for a link-local host address: err=%v, want allowed=%v", allow, err, want)
		}
	}
	exact, _ := ParsePolicy([]string{"10.0.0.5/32"}, nil)
	if err := check(exact, "10.0.0.5"); err != nil {
		t.Errorf("an exact allow must open this host's address: %v", err)
	}
}

// 列不出本机地址时拒绝这次探测：放行会让本机地址在这段时间里可探测。
func TestTargetsFailClosedWhenHostAddressesUnknown(t *testing.T) {
	tg := Targets{HostAddrs: func() ([]netip.Addr, error) { return nil, errors.New("netlink down") }}
	if _, err := tg.Resolve(t.Context(), "1.1.1.1", true, true); err == nil || !strings.Contains(err.Error(), "netlink down") {
		t.Fatalf("err = %v, want the enumeration failure", err)
	}
}

// 生产路径（零值 Targets 经 net.InterfaceAddrs 枚举）：本机非回环地址上的监听收不到连接。
func TestTCPDeniesThisHostsOwnAddress(t *testing.T) {
	own, err := interfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var addr netip.Addr
	for _, a := range own {
		if a.Is4() && !a.IsLoopback() && !a.IsLinkLocalUnicast() {
			addr = a
			break
		}
	}
	if !addr.IsValid() {
		t.Skipf("no non-loopback IPv4 address on this host: %v", own)
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
			accepted <- struct{}{}
		}
	}()
	task := tcpTask(ln.Addr().String())
	task.TimeoutMs = probelimit.MaxTimeoutMs
	out := (TCP{Clock: clock.Real()}).Probe(t.Context(), task)
	if out.Err != "target "+addr.String()+" denied by local probe policy (an address of this host)" {
		t.Fatalf("outcome %+v", out)
	}
	select {
	case <-accepted:
		t.Fatal("the listener on this host's own address accepted a connection")
	case <-time.After(50 * time.Millisecond):
	}
}
