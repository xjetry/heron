package prober

import (
	"errors"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/probelimit"
	"github.com/xjetry/heron/internal/testwait"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

func TestICMPDatagramWireAndPayload(t *testing.T) {
	e := availableICMP(t, clock.Real())
	for _, family := range []struct {
		network, bind, target  string
		proto                  int
		request                icmp.Type
		requestByte, replyByte byte
	}{
		{"udp4", "0.0.0.0", "127.0.0.1", 1, ipv4.ICMPTypeEcho, 0x08, 0x00},
		{"udp6", "::", "::1", 58, ipv6.ICMPTypeEchoRequest, 0x80, 0x81},
	} {
		t.Run(family.network, func(t *testing.T) {
			pc, err := icmp.ListenPacket(family.network, family.bind)
			if err != nil {
				t.Fatal(err)
			}
			defer pc.Close()
			if err := pc.SetDeadline(time.Now().Add(testwait.Bound)); err != nil {
				t.Fatal(err)
			}
			key := pendingKey{task: 987654321, seq: 123456}
			c := &icmpConn{proto: family.proto, pending: map[pendingKey]chan time.Duration{}}
			reply := make(chan time.Duration, 1)
			c.register(key, reply)
			wire, err := (&icmp.Message{Type: family.request, Body: &icmp.Echo{ID: 0x1234, Seq: 7, Data: e.payload(key.task, key.seq)}}).Marshal(nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pc.WriteTo(wire, &net.UDPAddr{IP: net.ParseIP(family.target)}); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 1500)
			for {
				n, peer, err := pc.ReadFrom(buf)
				if err != nil {
					t.Fatal(err)
				}
				if n == 0 {
					t.Fatal("empty ICMP datagram")
				}
				if buf[0] == family.requestByte {
					continue
				}
				if buf[0] != family.replyByte {
					t.Fatalf("first_byte=0x%02x want=0x%02x raw_len=%d", buf[0], family.replyByte, n)
				}
				e.handle(c, buf[:n], time.Second)
				select {
				case at := <-reply:
					if at != time.Second {
						t.Fatalf("payload match time=%v", at)
					}
				default:
					t.Fatal("loopback payload did not match pending key")
				}
				t.Logf("network=%s first_byte=0x%02x raw_len=%d peer=%s payload_match=true", family.network, buf[0], n, peer)
				break
			}
		})
	}
}

func availableICMP(t *testing.T, clk clock.Clock) *ICMP {
	t.Helper()
	e := NewICMP(clk, logger())
	e.Targets = loopbackTargets(t, nil)
	t.Cleanup(e.Close)
	if !e.Available() {
		t.Skipf("ICMP unavailable: %v", e.InitErrors())
	}
	return e
}

func TestICMPLoopbackAndConcurrentTasks(t *testing.T) {
	e := availableICMP(t, clock.Real())
	for _, target := range []string{"127.0.0.1", "::1"} {
		probe := task(1)
		probe.Target = target
		// 等的是真实回包，性质不是时长。用产品允许的超时上限，负载下回包变慢不会被判成探测超时。
		probe.TimeoutMs = probelimit.MaxTimeoutMs
		if out := e.Probe(t.Context(), probe); out.Err != "" || out.Timeout || out.RttUs == 0 {
			t.Fatalf("target=%s outcome=%+v", target, out)
		}
	}
	results := make(chan Outcome, 40)
	var wg sync.WaitGroup
	for _, id := range []uint64{11, 22} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			probe := task(id)
			probe.TimeoutMs = probelimit.MaxTimeoutMs
			for range 20 {
				results <- e.Probe(t.Context(), probe)
			}
		}()
	}
	wg.Wait()
	close(results)
	for out := range results {
		if out.Err != "" || out.Timeout || out.RttUs == 0 {
			t.Fatalf("concurrent outcome=%+v", out)
		}
	}
}

func TestICMPTimeoutAndClose(t *testing.T) {
	socket := newQuietSocket()
	e := icmpWithSocket(t, socket)
	probe := task(1)
	probe.TimeoutMs = 100
	start := time.Now()
	out := e.Probe(t.Context(), probe)
	if !out.Timeout || out.Err != "" || time.Since(start) >= 10*time.Duration(probe.TimeoutMs)*time.Millisecond {
		t.Fatalf("quiet socket=%+v elapsed=%v", out, time.Since(start))
	}
	e.Close()
	if out := e.Probe(t.Context(), task(1)); out.Err == "" || out.Timeout {
		t.Fatalf("closed=%+v", out)
	}
}

func TestICMPCloseWakesPendingProbe(t *testing.T) {
	socket := newQuietSocket()
	e := icmpWithSocket(t, socket)
	probe := task(1)
	probe.TimeoutMs = 5000
	result := make(chan Outcome, 1)
	go func() { result <- e.Probe(t.Context(), probe) }()
	receive(t, socket.written)
	start := time.Now()
	e.Close()
	// 要验证的是被 Close 唤醒，而不是等到探测超时。窗口小于 TimeoutMs 就能区分；
	// out.Timeout 与 Err 另锁这条性质。取一半给调度留余量，不是挂死上界。
	limit := time.Duration(probe.TimeoutMs) * time.Millisecond / 2
	select {
	case out := <-result:
		if out.Err != "icmp closed" || out.Timeout || time.Since(start) >= limit {
			t.Fatalf("pending close=%+v elapsed=%v, want wake before %v", out, time.Since(start), limit)
		}
	case <-time.After(testwait.Bound):
		t.Fatal("Close did not wake pending probe")
	}
}

func TestICMPRTTLimit(t *testing.T) {
	late := availableICMP(t, &measuredClock{Fake: clock.NewFake(time.Unix(0, 0)), step: 1001 * time.Millisecond})
	if out := late.Probe(t.Context(), task(1)); !out.Timeout || out.Err != "" || out.RttUs != 0 {
		t.Fatalf("late=%+v", out)
	}
}

type quietSocket struct {
	closed  chan struct{}
	written chan struct{}
	once    sync.Once
	read    func([]byte) (int, net.Addr, error)
	write   func([]byte, net.Addr) (int, error)
}

func newQuietSocket() *quietSocket {
	return &quietSocket{closed: make(chan struct{}), written: make(chan struct{}, 1)}
}

func (s *quietSocket) WriteTo(b []byte, dst net.Addr) (int, error) {
	select {
	case s.written <- struct{}{}:
	default:
	}
	if s.write != nil {
		return s.write(b, dst)
	}
	return len(b), nil
}

func (s *quietSocket) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case <-s.closed:
		return 0, nil, net.ErrClosed
	default:
	}
	if s.read != nil {
		return s.read(b)
	}
	<-s.closed
	return 0, nil, net.ErrClosed
}

func (s *quietSocket) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func icmpWithSocket(t *testing.T, socket icmpSocket) *ICMP {
	t.Helper()
	e := &ICMP{clk: clock.Real(), log: logger(), done: make(chan struct{}),
		v4: &icmpConn{pc: socket, proto: 1, pending: map[pendingKey]chan time.Duration{}}, Targets: loopbackTargets(t, nil)}
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.read(e.v4) }()
	t.Cleanup(e.Close)
	return e
}

func TestICMPSendErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		err     error
		timeout bool
	}{
		{syscall.ENETUNREACH, true},
		{syscall.EHOSTUNREACH, true},
		{syscall.EPERM, false},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			socket := newQuietSocket()
			socket.write = func([]byte, net.Addr) (int, error) {
				return 0, &net.OpError{Op: "write", Net: "udp4", Err: tc.err}
			}
			out := icmpWithSocket(t, socket).Probe(t.Context(), task(1))
			if out.Timeout != tc.timeout || (tc.timeout && out.Err != "") || (!tc.timeout && !strings.Contains(out.Err, tc.err.Error())) {
				t.Fatalf("send %v outcome=%+v", tc.err, out)
			}
		})
	}
}

func TestICMPReadErrorsBackOff(t *testing.T) {
	socket := newQuietSocket()
	calls := make(chan time.Time, 64)
	socket.read = func([]byte) (int, net.Addr, error) {
		select {
		case calls <- time.Now():
		default:
		}
		return 0, nil, errors.New("read unavailable")
	}
	e := icmpWithSocket(t, socket)
	first := receive(t, calls)
	// 测量窗口：数这段时间内的重试次数，用来核对退避，不是等一件预期会发生的事。
	time.Sleep(120 * time.Millisecond)
	e.Close()
	close(calls)
	retries := 0
	for at := range calls {
		if at.Sub(first) <= 120*time.Millisecond {
			retries++
		}
	}
	if retries > 4 {
		t.Fatalf("read retries=%d in 120ms; want <=4", retries)
	}
}

func TestICMPReadBackoffResetsAndCaps(t *testing.T) {
	for _, reset := range []bool{true, false} {
		name := "cap"
		if reset {
			name = "reset"
		}
		t.Run(name, func(t *testing.T) {
			// 前 failuresBeforeSuccess 次失败把退避推到 readBackoffMin·2^(n-1)。下一次成功若把 delay 清零，
			// 再失败后的等待是 readBackoffMin。若没清零，那次失败会再翻倍，等待是 readBackoffMin·2^n（受 readBackoffMax 封顶）。
			const failuresBeforeSuccess = 5
			socket := newQuietSocket()
			calls := make(chan time.Time, 16)
			n := 0
			socket.read = func(b []byte) (int, net.Addr, error) {
				n++
				calls <- time.Now()
				if reset && n == failuresBeforeSuccess+1 {
					b[0] = 0xff
					return 1, &net.UDPAddr{}, nil
				}
				return 0, nil, errors.New("read unavailable")
			}
			e := icmpWithSocket(t, socket)
			if reset {
				// 窗口必须落在重置后的 readBackoffMin 与未重置退避之间：长于未重置会把未重置判成通过，过短会在负载下把按时到来的读取判失败。
				unreset := min(readBackoffMin<<failuresBeforeSuccess, readBackoffMax)
				window := readBackoffMin + (unreset-readBackoffMin)/2
				if window <= readBackoffMin || window >= unreset {
					t.Fatalf("reset window %v is not strictly between %v and %v", window, readBackoffMin, unreset)
				}
				for range failuresBeforeSuccess + 2 {
					receive(t, calls)
				}
				select {
				case <-calls:
				case <-time.After(window):
					t.Fatalf("successful read did not reset backoff to %v", readBackoffMin)
				}
			} else {
				// 已看到 9 次失败读取；下一次读取前的等待由第 9 次失败设置。
				// 封顶后是 readBackoffMax。不封顶时是 readBackoffMin·2^8。
				// 窗口必须长于封顶值才能看见下一次读取，短于不封顶的那一次，否则没封顶也会在窗口内到来。
				const failuresSeen = 9
				uncapped := readBackoffMin << (failuresSeen - 1)
				window := readBackoffMax + (uncapped-readBackoffMax)/2
				if window <= readBackoffMax || window >= uncapped {
					t.Fatalf("cap window %v is not strictly between %v and %v", window, readBackoffMax, uncapped)
				}
				for range failuresSeen {
					receive(t, calls)
				}
				select {
				case <-calls:
				case <-time.After(window):
					t.Fatal("read backoff exceeded cap")
				}
			}
			start := time.Now()
			e.Close()
			if time.Since(start) >= readBackoffMax {
				t.Fatalf("Close waited for read backoff: %v", time.Since(start))
			}
		})
	}
}

func TestICMPPayloadAndPendingIsolation(t *testing.T) {
	e := &ICMP{nonce: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}}
	key := pendingKey{task: 0x123456789abcdef0, seq: 0x12345678}
	payload := e.payload(key.task, key.seq)
	if got, ok := e.parsePayload(payload); !ok || got != key || len(payload) != payloadLen {
		t.Fatalf("roundtrip=%v/%v len=%d", got, ok, len(payload))
	}
	for _, b := range [][]byte{nil, payload[:payloadLen-1], append(append([]byte(nil), payload...), 0)} {
		if _, ok := e.parsePayload(b); ok {
			t.Fatalf("accepted length=%d", len(b))
		}
	}
	c := &icmpConn{proto: 1, pending: map[pendingKey]chan time.Duration{}}
	reply := make(chan time.Duration, 1)
	c.register(key, reply)
	foreign := append([]byte(nil), payload...)
	foreign[0] ^= 1
	if parsed, ok := e.parsePayload(foreign); ok {
		c.deliver(parsed, time.Second)
	}
	select {
	case <-reply:
		t.Fatal("foreign nonce delivered to own pending key")
	default:
	}
	c.deliver(pendingKey{task: key.task + 1, seq: key.seq}, time.Second)
	c.deliver(pendingKey{task: key.task, seq: key.seq + 1}, time.Second)
	select {
	case <-reply:
		t.Fatal("foreign key delivered")
	default:
	}
	c.deliver(key, 2*time.Second)
	c.deliver(key, 3*time.Second)
	if at := receive(t, reply); at != 2*time.Second {
		t.Fatalf("duplicate delivery=%v", at)
	}
	c.unregister(key)
	c.deliver(key, time.Second)
	select {
	case <-reply:
		t.Fatal("unregistered key delivered")
	default:
	}
}

func TestICMPAcceptsOnlyEchoRepliesRegardlessOfID(t *testing.T) {
	e := &ICMP{nonce: [8]byte{42}}
	for _, family := range []struct {
		proto          int
		request, reply icmp.Type
	}{
		{1, ipv4.ICMPTypeEcho, ipv4.ICMPTypeEchoReply},
		{58, ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply},
	} {
		c := &icmpConn{proto: family.proto, pending: map[pendingKey]chan time.Duration{}}
		ch := make(chan time.Duration, 1)
		c.register(pendingKey{task: 9, seq: 123}, ch)
		wire, err := (&icmp.Message{Type: family.request, Body: &icmp.Echo{ID: 999, Seq: 1, Data: e.payload(9, 123)}}).Marshal(nil)
		if err != nil {
			t.Fatal(err)
		}
		e.handle(c, wire, time.Second)
		select {
		case <-ch:
			t.Fatal("echo request treated as reply")
		default:
		}
		wire, err = (&icmp.Message{Type: family.reply, Body: &icmp.Echo{ID: 999, Seq: 1, Data: e.payload(9, 123)}}).Marshal(nil)
		if err != nil {
			t.Fatal(err)
		}
		e.handle(c, wire, 2*time.Second)
		if at := receive(t, ch); at != 2*time.Second {
			t.Fatalf("echo reply time=%v", at)
		}
	}
}

func TestICMPUnavailableExplainsSocketFailures(t *testing.T) {
	e := &ICMP{clk: clock.Real(), initErr4: []string{"udp4: denied", "ip4:icmp: denied"}}
	out := e.Probe(t.Context(), task(1))
	if e.Available() || out.Timeout || !strings.Contains(out.Err, "udp4: denied; ip4:icmp: denied") {
		t.Fatalf("unavailable=%+v", out)
	}
	errs := e.InitErrors()
	errs[0] = "changed"
	if e.InitErrors()[0] != "udp4: denied" {
		t.Fatal("initialization diagnostics aliased")
	}
}

func TestICMPUnavailableReportsRelevantFamily(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		v4, v6 bool
		want   string
	}{
		{"literal-v4", "127.0.0.1", false, false, "udp4: denied; ip4:icmp: denied"},
		{"literal-mapped", "::ffff:127.0.0.1", false, false, "udp4: denied; ip4:icmp: denied"},
		{"literal-v6", "::1", false, false, "udp6: denied; ip6:ipv6-icmp: denied"},
		{"hostname-no-family", "node.prober.invalid", false, false, "udp4: denied; ip4:icmp: denied; udp6: denied; ip6:ipv6-icmp: denied"},
		{"selected-v6", "::1", true, false, "udp6: denied; ip6:ipv6-icmp: denied"},
		{"selected-v4", "127.0.0.1", false, true, "udp4: denied; ip4:icmp: denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &ICMP{clk: clock.Real(), done: make(chan struct{}), Targets: loopbackTargets(t, nil),
				initErr4: []string{"udp4: denied", "ip4:icmp: denied"},
				initErr6: []string{"udp6: denied", "ip6:ipv6-icmp: denied"}}
			if tc.v4 {
				e.v4 = &icmpConn{pc: newQuietSocket()}
			}
			if tc.v6 {
				e.v6 = &icmpConn{pc: newQuietSocket()}
			}
			t.Cleanup(e.Close)
			probe := task(1)
			probe.Target = tc.target
			out := e.Probe(t.Context(), probe)
			if out.Err != "icmp unavailable: "+tc.want || out.Timeout {
				t.Fatalf("target=%s available=%v/%v outcome=%+v want=%s", tc.target, tc.v4, tc.v6, out, tc.want)
			}
		})
	}
}

func TestICMPDiagnosticsDoNotImplyUnavailable(t *testing.T) {
	e := icmpWithSocket(t, newQuietSocket())
	e.v4.raw = true
	e.initErr4 = make([]string, 1, 4)
	e.initErr4[0] = "udp4: denied"
	e.initErr6 = []string{"udp6: denied", "ip6:ipv6-icmp: denied"}
	if !e.Available() || len(e.InitErrors()) != 3 {
		t.Fatalf("raw fallback available=%v diagnostics=%v", e.Available(), e.InitErrors())
	}
	diagnostics := e.InitErrors()
	diagnostics[0], diagnostics[1] = "changed", "changed"
	if got := e.InitErrors(); got[0] != "udp4: denied" || got[1] != "udp6: denied" {
		t.Fatalf("diagnostics alias family storage: %v", got)
	}
}

func TestICMPUsesInjectedResolver(t *testing.T) {
	socket := newQuietSocket()
	var destination string
	socket.write = func(_ []byte, dst net.Addr) (int, error) {
		destination = dst.(*net.UDPAddr).IP.String()
		return 0, syscall.EPERM
	}
	e := icmpWithSocket(t, socket)
	e.Targets = loopbackTargets(t, localDNS(t, dnsMapped, nil))
	probe := task(1)
	probe.Target = "mapped.prober.invalid"
	out := e.Probe(t.Context(), probe)
	if destination != "127.0.0.1" || !strings.Contains(out.Err, syscall.EPERM.Error()) {
		t.Fatalf("injected resolver destination=%q outcome=%+v", destination, out)
	}
}
