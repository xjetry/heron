package prober

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/probelimit"
	"github.com/xjetry/heron/internal/testwait"
)

type dnsAnswer int

const (
	dnsMissing dnsAnswer = iota
	dnsDual
	dnsMapped
)

// 每个解析器只访问自己的回环服务，不替换全局解析器，也不依赖宿主 DNS 或公网。
func localDNS(t *testing.T, answer dnsAnswer, before func()) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp4", pc.LocalAddr().String())
	}}
	var once sync.Once
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1500)
		for {
			n, peer, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 17 {
				continue
			}
			pos := 12
			for pos < n && buf[pos] != 0 {
				pos += int(buf[pos]) + 1
			}
			if pos+5 > n {
				continue
			}
			end := pos + 5
			qtype := binary.BigEndian.Uint16(buf[pos+1 : pos+3])
			msg := append([]byte(nil), buf[:end]...)
			msg[2], msg[3] = 0x81, 0x80
			clear(msg[6:12])
			if (answer == dnsDual && (qtype == 1 || qtype == 28)) || (answer == dnsMapped && qtype == 28) {
				msg[7] = 1
				ip := net.ParseIP("127.0.0.1").To4()
				if qtype == 28 {
					ip = net.ParseIP("::1").To16()
					if answer == dnsMapped {
						ip = net.ParseIP("::ffff:127.0.0.1").To16()
					}
				}
				msg = append(msg, 0xc0, 0x0c, 0, byte(qtype), 0, 1, 0, 0, 0, 0, 0, byte(len(ip)))
				msg = append(msg, ip...)
			} else if answer == dnsMissing {
				msg[3] = 0x83
			}
			if before != nil {
				once.Do(before)
			}
			_, _ = pc.WriteTo(msg, peer)
		}
	}()
	t.Cleanup(func() { pc.Close(); <-done })
	return resolver
}

func tcpListener(t *testing.T) (string, <-chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
		close(accepted)
		for c := range accepted {
			c.Close()
		}
	})
	return ln.Addr().String(), accepted
}

func tcpTask(target string) *heronv1.ProbeTask {
	t := task(1)
	t.Kind, t.Target = heronv1.ProbeKind_PROBE_KIND_TCP, target
	return t
}

type measuredClock struct {
	*clock.Fake
	mu           sync.Mutex
	calls        int
	step         time.Duration
	beforeSecond func()
}

func (c *measuredClock) Mono() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls == 2 {
		if c.beforeSecond != nil {
			c.beforeSecond()
		}
		c.Advance(c.step)
	}
	return c.Fake.Mono()
}

func TestTCPOutcomes(t *testing.T) {
	addr, _ := tcpListener(t)
	p := TCP{Clock: clock.Real(), Targets: loopbackTargets(t, nil)}
	loopback := tcpTask(addr)
	// 等的是本机回包，性质不是时长。用产品允许的超时上限，负载下连接变慢不会被判成探测超时。
	loopback.TimeoutMs = probelimit.MaxTimeoutMs
	if out := p.Probe(t.Context(), loopback); out.Err != "" || out.Timeout || out.RttUs == 0 {
		t.Fatalf("loopback=%+v", out)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	ln.Close()
	if out := p.Probe(t.Context(), tcpTask(closed)); !out.Timeout || out.Err != "" {
		t.Fatalf("refused=%+v", out)
	}
	p.Targets.Resolver = localDNS(t, dnsMissing, nil)
	if out := p.Probe(t.Context(), tcpTask("missing.prober.invalid:9")); !strings.Contains(out.Err, "missing.prober.invalid") || out.Timeout {
		t.Fatalf("dns=%+v", out)
	}
	if out := p.Probe(t.Context(), tcpTask("no-port")); out.Err == "" || out.Timeout {
		t.Fatalf("address=%+v", out)
	}
	blackhole := tcpTask("192.0.2.1:9")
	blackhole.TimeoutMs = 100
	p.DialContext = func(ctx context.Context, network, target string) (net.Conn, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 100*time.Millisecond || network != "tcp" || target != blackhole.Target {
			t.Errorf("dial budget/address: deadline=%v valid=%v network=%s target=%s", deadline, ok, network, target)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		// 拨号受任务超时约束由上面的 deadline 检查锁住。这里只在 ctx 从不取消时结束，防止挂死。
		case <-time.After(testwait.Bound):
			t.Error("dial context was not canceled")
			return nil, context.DeadlineExceeded
		}
	}
	start := time.Now()
	out := p.Probe(t.Context(), blackhole)
	if !out.Timeout || out.Err != "" || time.Since(start) >= 10*time.Duration(blackhole.TimeoutMs)*time.Millisecond {
		t.Fatalf("blackhole=%+v elapsed=%v", out, time.Since(start))
	}
}

func TestTCPMeasuresOnlyConnectionAndCapsRTT(t *testing.T) {
	addr, accepted := tcpListener(t)
	_, port, _ := net.SplitHostPort(addr)
	clk := &measuredClock{Fake: clock.NewFake(time.Unix(0, 0)), step: time.Millisecond}
	resolver := localDNS(t, dnsDual, func() { clk.Advance(50 * time.Millisecond) })
	var closedEarly bool
	clk.beforeSecond = func() {
		c := receive(t, accepted)
		defer c.Close()
		// 负向窗口：这段时间内连接不应已关闭。窗口短只会漏掉稍晚才关掉的缺陷，不会把尚未关闭的连接判失败；拉长会让尚未关闭的成功路径空等。
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
		_, err := c.Read(make([]byte, 1))
		closedEarly = err == io.EOF
	}
	connected := tcpTask(net.JoinHostPort("local.prober.invalid", port))
	connected.TimeoutMs = probelimit.MaxTimeoutMs
	out := (TCP{Clock: clk, Targets: loopbackTargets(t, resolver)}).Probe(t.Context(), connected)
	if out != (Outcome{RttUs: 1000}) || closedEarly {
		t.Fatalf("connection=%+v closed_before_measurement=%v", out, closedEarly)
	}
	late := &measuredClock{Fake: clock.NewFake(time.Unix(0, 0)), step: 1001 * time.Millisecond}
	if out := (TCP{Clock: late, Targets: loopbackTargets(t, nil)}).Probe(t.Context(), tcpTask(addr)); !out.Timeout || out.Err != "" || out.RttUs != 0 {
		t.Fatalf("late=%+v", out)
	}
}

func TestResolveChoosesAvailableFamilies(t *testing.T) {
	resolver := localDNS(t, dnsDual, nil)
	for _, tc := range []struct {
		host   string
		v4, v6 bool
		want   string
	}{
		{"127.0.0.1", true, false, "127.0.0.1"},
		{"::1", false, true, "::1"},
		{"local.prober.invalid", true, true, "127.0.0.1"},
		{"local.prober.invalid", false, true, "::1"},
	} {
		ip, err := resolve(t.Context(), resolver, tc.host, tc.v4, tc.v6)
		if err != nil || ip.String() != tc.want {
			t.Fatalf("resolve %+v = %v/%v", tc, ip, err)
		}
	}
	if _, err := resolve(t.Context(), resolver, "local.prober.invalid", false, false); err == nil {
		t.Fatal("resolved without an available family")
	}
}

func TestResolveMappedAddresses(t *testing.T) {
	resolver := localDNS(t, dnsMapped, nil)
	t.Run("literal", func(t *testing.T) {
		ip, err := resolve(t.Context(), resolver, "::ffff:127.0.0.1", true, false)
		if err != nil || ip.String() != "127.0.0.1" {
			t.Fatalf("mapped literal=%v err=%v", ip, err)
		}
	})
	t.Run("dns-v4", func(t *testing.T) {
		ip, err := resolve(t.Context(), resolver, "mapped.prober.invalid", true, false)
		if err != nil || ip.String() != "127.0.0.1" {
			t.Fatalf("mapped DNS v4=%v err=%v", ip, err)
		}
	})
	t.Run("dns-v6-only", func(t *testing.T) {
		ip, err := resolve(t.Context(), resolver, "mapped.prober.invalid", false, true)
		if err == nil {
			t.Fatalf("mapped DNS accepted as v6=%v", ip)
		}
	})
}

func TestResolversRemainIndependent(t *testing.T) {
	previous := net.DefaultResolver
	dual := localDNS(t, dnsDual, nil)
	mapped := localDNS(t, dnsMapped, nil)
	if net.DefaultResolver != previous {
		t.Fatal("local DNS replaced the global resolver")
	}
	for _, tc := range []struct {
		name      string
		resolver  *net.Resolver
		wantError bool
	}{
		{"dual", dual, false},
		{"mapped", mapped, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ip, err := resolve(t.Context(), tc.resolver, "local.prober.invalid", false, true)
			if (err != nil) != tc.wantError || (!tc.wantError && ip.String() != "::1") {
				t.Fatalf("independent resolver %s=%v err=%v", tc.name, ip, err)
			}
		})
	}
}

func TestTCPUsesInjectedResolver(t *testing.T) {
	var destination string
	p := TCP{Clock: clock.Real(), Targets: loopbackTargets(t, localDNS(t, dnsMapped, nil)),
		DialContext: func(_ context.Context, _ string, target string) (net.Conn, error) {
			destination = target
			return nil, syscall.ENETUNREACH
		}}
	out := p.Probe(t.Context(), tcpTask("mapped.prober.invalid:9"))
	if destination != "127.0.0.1:9" || !out.Timeout || out.Err != "" {
		t.Fatalf("injected resolver destination=%q outcome=%+v", destination, out)
	}
}
