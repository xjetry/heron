package prober

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// ICMP 每个地址族共享一个 socket，数据报不可用时退到 raw。
// Linux 数据报回包 ID 是本地端口，macOS 的公网回包 ID 也可能被改写；因此只按 payload 匹配。
// raw 与 macOS udp6 会收到自己的请求，macOS 数据报 socket 会收到同进程其他 socket 的回包；
// 每个 socket 一个读协程，只接受 Echo Reply，陌生 nonce 或任务序号不会交给 pending。
// ReadFrom 不带 IP 头依赖 x/net v0.58.0 与 Go 1.27.1 的以下路径，换版本需重新核对：
// Darwin udp4：icmp/listen_posix.go 的 ListenPacket 设置 IP_STRIPHDR。
// Darwin raw v4：ListenPacket 走 net.ListenPacket，不设置 IP_STRIPHDR；
// icmp/endpoint.go 在 darwin 且 p4 非空时转到 ipv4.PacketConn.ReadFrom，
// ipv4/payload_cmsg.go 的 *net.IPConn 分支将头部读入独立缓冲，再按首字节中的头长剥除。
// Linux udp4：内核 ping socket 只交付 ICMP 报文。
// Linux raw v4：net/iprawsock_posix.go 的 net.IPConn.ReadFrom 对 SockaddrInet4 调 stripIPv4Header。
// udp6 与 raw v6：Darwin/Linux 内核均不交付 IPv6 头。
// 连接或发送失败经 classify 区分可达性与本地故障。
type ICMP struct {
	clk       clock.Clock
	log       *slog.Logger
	Targets   Targets
	nonce     [8]byte
	seq       atomic.Uint32
	v4, v6    *icmpConn
	initErr4  []string
	initErr6  []string
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

type icmpSocket interface {
	WriteTo([]byte, net.Addr) (int, error)
	ReadFrom([]byte) (int, net.Addr, error)
	Close() error
}

var _ icmpSocket = (*icmp.PacketConn)(nil)

type icmpConn struct {
	pc      icmpSocket
	raw     bool
	proto   int
	mu      sync.Mutex
	pending map[pendingKey]chan time.Duration
}

type pendingKey struct {
	task uint64
	seq  uint32
}

func NewICMP(clk clock.Clock, log *slog.Logger) *ICMP {
	e := &ICMP{clk: clk, log: log, done: make(chan struct{})}
	rand.Read(e.nonce[:])
	e.v4, e.initErr4 = openICMP("udp4", "ip4:icmp", "0.0.0.0", ipv4.ICMPTypeEchoReply.Protocol())
	e.v6, e.initErr6 = openICMP("udp6", "ip6:ipv6-icmp", "::", ipv6.ICMPTypeEchoReply.Protocol())
	for _, c := range []*icmpConn{e.v4, e.v6} {
		if c != nil {
			e.wg.Add(1)
			go func() { defer e.wg.Done(); e.read(c) }()
		}
	}
	return e
}

// 两种 socket 的失败原因都保留，探测不可用时才能说明权限或协议限制。
func openICMP(dgram, raw, addr string, proto int) (*icmpConn, []string) {
	var diagnostics []string
	if pc, err := icmp.ListenPacket(dgram, addr); err == nil {
		return &icmpConn{pc: pc, proto: proto, pending: map[pendingKey]chan time.Duration{}}, nil
	} else {
		diagnostics = append(diagnostics, dgram+": "+err.Error())
	}
	if pc, err := icmp.ListenPacket(raw, addr); err == nil {
		return &icmpConn{pc: pc, raw: true, proto: proto, pending: map[pendingKey]chan time.Duration{}}, diagnostics
	} else {
		diagnostics = append(diagnostics, raw+": "+err.Error())
	}
	return nil, diagnostics
}

// Available 表示初始化时至少一个地址族成功创建 socket，供 facts 报告能力。
func (e *ICMP) Available() bool { return e.v4 != nil || e.v6 != nil }

// InitErrors 是诊断信息：数据报失败但 raw 成功时也非空，不等于不可用；能力由 Available 判断。
func (e *ICMP) InitErrors() []string { return append(slices.Clone(e.initErr4), e.initErr6...) }

// Close 须在 Scheduler.Stop 之后调用，否则每个间隔都会入队 icmp closed。
func (e *ICMP) Close() {
	e.closeOnce.Do(func() {
		close(e.done)
		for _, c := range []*icmpConn{e.v4, e.v6} {
			if c != nil {
				c.pc.Close()
			}
		}
	})
	e.wg.Wait()
}

const payloadLen = 32 // nonce 8 + task_id 8 + seq 4 + 零填充

func (e *ICMP) payload(task uint64, seq uint32) []byte {
	b := make([]byte, payloadLen)
	copy(b, e.nonce[:])
	binary.BigEndian.PutUint64(b[8:16], task)
	binary.BigEndian.PutUint32(b[16:20], seq)
	return b
}

func (e *ICMP) parsePayload(b []byte) (pendingKey, bool) {
	if len(b) != payloadLen || !bytes.Equal(b[:8], e.nonce[:]) {
		return pendingKey{}, false
	}
	return pendingKey{task: binary.BigEndian.Uint64(b[8:16]), seq: binary.BigEndian.Uint32(b[16:20])}, true
}

func (c *icmpConn) register(key pendingKey, reply chan time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending[key] = reply
}

func (c *icmpConn) unregister(key pendingKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, key)
}

func (c *icmpConn) deliver(key pendingKey, at time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if reply, ok := c.pending[key]; ok {
		select {
		case reply <- at:
		default:
		}
	}
}

func (e *ICMP) Probe(ctx context.Context, t *heronv1.ProbeTask) Outcome {
	select {
	case <-e.done:
		return Outcome{Err: "icmp closed"}
	default:
	}
	if !e.Available() {
		if ip, err := netip.ParseAddr(t.GetTarget()); err == nil {
			return e.unavailable(ip.Unmap().Is6())
		}
		// 没有任何 socket 时，主机名无可选地址族，返回两族初始化诊断，不擅自归因到某一族。
		return Outcome{Err: "icmp unavailable: " + strings.Join(e.InitErrors(), "; ")}
	}
	timeout := time.Duration(t.GetTimeoutMs()) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ip, err := e.Targets.Resolve(ctx, t.GetTarget(), e.v4 != nil, e.v6 != nil)
	if err != nil {
		return Outcome{Err: err.Error()}
	}
	c, typ := e.v4, icmp.Type(ipv4.ICMPTypeEcho)
	if ip.Is6() {
		c, typ = e.v6, ipv6.ICMPTypeEchoRequest
	}
	if c == nil {
		return e.unavailable(ip.Is6())
	}
	seq := e.seq.Add(1)
	key := pendingKey{task: t.GetId(), seq: seq}
	reply := make(chan time.Duration, 1)
	c.register(key, reply)
	defer c.unregister(key)
	msg := icmp.Message{Type: typ, Body: &icmp.Echo{ID: int(seq & 0xffff), Seq: int(seq & 0xffff), Data: e.payload(t.GetId(), seq)}}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return Outcome{Err: err.Error()}
	}
	var dst net.Addr = &net.UDPAddr{IP: ip.AsSlice()}
	if c.raw {
		dst = &net.IPAddr{IP: ip.AsSlice()}
	}
	sent := e.clk.Mono()
	if _, err := c.pc.WriteTo(wire, dst); err != nil {
		return classify(err)
	}
	select {
	case at := <-reply:
		elapsed := at - sent
		if elapsed > timeout {
			return Outcome{Timeout: true}
		}
		return Outcome{RttUs: uint32(elapsed / time.Microsecond)}
	case <-ctx.Done():
		return Outcome{Timeout: true}
	case <-e.done:
		return Outcome{Err: "icmp closed"}
	}
}

func (e *ICMP) unavailable(v6 bool) Outcome {
	diagnostics := e.initErr4
	if v6 {
		diagnostics = e.initErr6
	}
	return Outcome{Err: "icmp unavailable: " + strings.Join(diagnostics, "; ")}
}

// readBackoffMin 与 readBackoffMax 是读失败后的等待上下限。
// 每次失败取 min(max(readBackoffMin, 上次*2), readBackoffMax)；成功必须清零，否则下一次失败从上次的倍数继续。
// 重置窗口、封顶窗口和 Close 不等待退避的上界都由这两端推出。
const (
	readBackoffMin = 10 * time.Millisecond
	readBackoffMax = time.Second
)

func (e *ICMP) read(c *icmpConn) {
	buf := make([]byte, 1500)
	var delay time.Duration
	for {
		n, _, err := c.pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			e.log.Warn("reading ICMP failed", "err", err)
			// 每个失败读取后都等待，持续错误不会空转刷日志；Close 不必等待退避到期。
			delay = min(max(readBackoffMin, delay*2), readBackoffMax)
			timer := time.NewTimer(delay)
			select {
			case <-e.done:
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		delay = 0
		e.handle(c, buf[:n], e.clk.Mono())
	}
}

func (e *ICMP) handle(c *icmpConn, wire []byte, at time.Duration) {
	msg, err := icmp.ParseMessage(c.proto, wire)
	if err != nil || (msg.Type != ipv4.ICMPTypeEchoReply && msg.Type != ipv6.ICMPTypeEchoReply) {
		return
	}
	echo, ok := msg.Body.(*icmp.Echo)
	if !ok {
		return
	}
	if key, ok := e.parsePayload(echo.Data); ok {
		c.deliver(key, at)
	}
}
