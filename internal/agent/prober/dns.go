package prober

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
)

// DNS 手组一条 A 查询发给任务指定的解析器，测量从发出到收到应答的耗时。
// 单个 UDP 包，不重试、不走 TCP：探的是"这个解析器此刻能不能答出 A 记录"，重试会把
// 一次失败伪装成一次慢成功。NXDOMAIN/SERVFAIL/REFUSED、无 A 记录、解不开的应答与
// 对不上号的应答都计入丢包；dns_server 非法、地址策略拒绝与 socket/fd 等本机原因是 error。
type DNS struct {
	Clock   clock.Clock
	Targets Targets
}

func (p DNS) Probe(ctx context.Context, t *heronv1.ProbeTask) Outcome {
	timeout := time.Duration(t.GetTimeoutMs()) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	host, port, err := net.SplitHostPort(t.GetDnsServer())
	if err != nil {
		return Outcome{Err: fmt.Sprintf("dns_server %q is not ip:port", t.GetDnsServer())}
	}
	if _, err := netip.ParseAddr(host); err != nil {
		return Outcome{Err: fmt.Sprintf("dns_server %q does not use an IP literal", t.GetDnsServer())}
	}
	// 解析器即使已是 IP 字面量也走 Targets：要发包的地址在解析之后、发包之前按 Policy 检查，跳过这一步会让字面量解析器绕开默认拒绝。
	ip, err := p.Targets.Resolve(ctx, host, true, true)
	if err != nil {
		return Outcome{Err: err.Error()}
	}
	target := t.GetTarget()
	if !strings.HasSuffix(target, ".") {
		target += "." // NewName 接受两种写法，Pack 只认以点结尾的规范形式。
	}
	name, err := dnsmessage.NewName(target)
	if err != nil {
		return Outcome{Err: fmt.Sprintf("target %q is not a DNS name: %v", t.GetTarget(), err)}
	}
	var id [2]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Outcome{Err: fmt.Sprintf("draw query ID: %v", err)}
	}
	query, err := (&dnsmessage.Message{
		Header: dnsmessage.Header{ID: binary.BigEndian.Uint16(id[:]), RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name:  name,
			Type:  dnsmessage.TypeA,
			Class: dnsmessage.ClassINET,
		}},
	}).Pack()
	if err != nil {
		return Outcome{Err: fmt.Sprintf("pack query: %v", err)}
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort(ip.String(), port))
	if err != nil {
		return classify(err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	start := p.Clock.Mono()
	if _, err := conn.Write(query); err != nil {
		return classify(err)
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return classify(err)
	}
	elapsed := p.Clock.Mono() - start
	// CheckTask 把任务超时限制在 hub 接受的 RTT 上限内；测得耗时超过任务预算时只回报超时。
	if elapsed > timeout {
		return Outcome{Timeout: true}
	}
	if !dnsResolved(query, buf[:n]) {
		return Outcome{Timeout: true}
	}
	return Outcome{RttUs: uint32(elapsed / time.Microsecond)}
}

// dnsResolved 裁决应答报文：只有对得上号、NOERROR 且带着 A 记录的应答才是解析成功，
// 其余——拒绝、空应答、残报文、张冠李戴——都是"这一次没有解析成功"，计入丢包。
func dnsResolved(query, answer []byte) bool {
	var q, r dnsmessage.Message
	if err := q.Unpack(query); err != nil {
		return false
	}
	if err := r.Unpack(answer); err != nil || !r.Response || r.ID != q.ID {
		return false
	}
	if r.RCode != dnsmessage.RCodeSuccess {
		return false
	}
	for _, rr := range r.Answers {
		if rr.Header.Type == dnsmessage.TypeA {
			return true
		}
	}
	return false
}
