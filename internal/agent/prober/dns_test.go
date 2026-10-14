package prober

import (
	"net"
	"sync/atomic"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/probelimit"
)

type resolverAnswer int

const (
	answerA resolverAnswer = iota
	answerNXDOMAIN
	answerSERVFAIL
	answerREFUSED
	answerEmpty
	answerGarbage
	answerWrongID
	answerDrop
)

// 每个解析器只服务自己的回环端口，不替换全局解析器，也不依赖宿主 DNS 或公网。
func fakeResolver(t *testing.T, mode resolverAnswer) (string, *atomic.Int64) {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	packets := &atomic.Int64{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1500)
		for {
			n, peer, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			packets.Add(1)
			if mode == answerDrop {
				continue
			}
			if mode == answerGarbage {
				_, _ = pc.WriteTo([]byte{0xff, 0x00, 0x01}, peer)
				continue
			}
			var q dnsmessage.Message
			if err := q.Unpack(buf[:n]); err != nil || len(q.Questions) != 1 {
				continue
			}
			r := dnsmessage.Message{
				Header:    dnsmessage.Header{ID: q.ID, Response: true, RecursionAvailable: true},
				Questions: q.Questions,
			}
			if mode == answerWrongID {
				r.ID = q.ID + 1
			}
			switch mode {
			case answerA, answerWrongID:
				r.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
					Body:   &dnsmessage.AResource{A: [4]byte{203, 0, 113, 7}},
				}}
			case answerNXDOMAIN:
				r.RCode = dnsmessage.RCodeNameError
			case answerSERVFAIL:
				r.RCode = dnsmessage.RCodeServerFailure
			case answerREFUSED:
				r.RCode = dnsmessage.RCodeRefused
			case answerEmpty:
			}
			packed, err := r.Pack()
			if err != nil {
				continue
			}
			_, _ = pc.WriteTo(packed, peer)
		}
	}()
	t.Cleanup(func() { pc.Close(); <-done })
	return pc.LocalAddr().String(), packets
}

func dnsTask(server string) *heronv1.ProbeTask {
	t := task(1)
	t.Kind, t.Target, t.DnsServer = heronv1.ProbeKind_PROBE_KIND_DNS, "example.com", server
	t.TimeoutMs = probelimit.MaxTimeoutMs
	return t
}

func TestDNSOutcomes(t *testing.T) {
	p := DNS{Clock: clock.Real(), Targets: loopbackTargets(t, nil)}
	addr, packets := fakeResolver(t, answerA)
	if out := p.Probe(t.Context(), dnsTask(addr)); out.Err != "" || out.Timeout || out.RttUs == 0 {
		t.Fatalf("a_record=%+v", out)
	}
	trailingDot := dnsTask(addr)
	trailingDot.Target = "example.com."
	if out := p.Probe(t.Context(), trailingDot); out.Err != "" || out.Timeout || out.RttUs == 0 {
		t.Fatalf("trailing_dot=%+v", out)
	}
	// 单个 UDP 包：不重试、不走 TCP。
	if packets.Load() != 2 {
		t.Fatalf("packets=%d, want one per probe", packets.Load())
	}

	// NXDOMAIN/SERVFAIL/REFUSED、无 A 记录、解不开的应答、张冠李戴的应答都是丢包。
	for name, mode := range map[string]resolverAnswer{
		"nxdomain": answerNXDOMAIN, "servfail": answerSERVFAIL, "refused": answerREFUSED,
		"empty": answerEmpty, "garbage": answerGarbage, "wrong_id": answerWrongID,
	} {
		a, pkts := fakeResolver(t, mode)
		if out := p.Probe(t.Context(), dnsTask(a)); !out.Timeout || out.Err != "" {
			t.Fatalf("%s=%+v", name, out)
		}
		if pkts.Load() != 1 {
			t.Fatalf("%s packets=%d, want exactly one", name, pkts.Load())
		}
	}

	// 不应答：超时计入丢包，预算就是任务预算。
	silent, pkts := fakeResolver(t, answerDrop)
	blackhole := dnsTask(silent)
	blackhole.TimeoutMs = 200
	if out := p.Probe(t.Context(), blackhole); !out.Timeout || out.Err != "" {
		t.Fatalf("drop=%+v", out)
	}
	if pkts.Load() != 1 {
		t.Fatalf("drop packets=%d, want exactly one", pkts.Load())
	}

	// 端口没有监听：ICMP 不可达经 classify 计入丢包。
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unbound := pc.LocalAddr().String()
	pc.Close()
	if out := p.Probe(t.Context(), dnsTask(unbound)); !out.Timeout || out.Err != "" {
		t.Fatalf("unbound=%+v", out)
	}

	// 解析器地址过本地策略：默认策略拒绝回环，拒绝是 error。
	denied := DNS{Clock: clock.Real(), Targets: Targets{}}
	if out := denied.Probe(t.Context(), dnsTask(addr)); out.Err == "" || out.Timeout {
		t.Fatalf("policy=%+v", out)
	}

	badServer := dnsTask("dns.example:53")
	if out := p.Probe(t.Context(), badServer); out.Err == "" || out.Timeout {
		t.Fatalf("named_server=%+v", out)
	}
	noPort := dnsTask("127.0.0.1")
	if out := p.Probe(t.Context(), noPort); out.Err == "" || out.Timeout {
		t.Fatalf("no_port=%+v", out)
	}
}
