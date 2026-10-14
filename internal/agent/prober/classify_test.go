package prober

import (
	"context"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/xjetry/heron/internal/clock"
)

func TestClassifyProbeFailures(t *testing.T) {
	for _, tc := range []struct {
		err  error
		lost bool
	}{
		{context.DeadlineExceeded, true}, {&net.DNSError{Err: "deadline", IsTimeout: true}, true},
		{syscall.ECONNREFUSED, true}, {syscall.ECONNRESET, true}, {syscall.ETIMEDOUT, true},
		{syscall.ENETUNREACH, true}, {syscall.EHOSTUNREACH, true}, {syscall.EHOSTDOWN, true},
		{syscall.EMFILE, false}, {syscall.ENFILE, false}, {syscall.EACCES, false}, {syscall.EPERM, false},
		{syscall.EADDRNOTAVAIL, false}, {syscall.ENOBUFS, false}, {syscall.ENETDOWN, false},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			wrapped := fmt.Errorf("probe: %w", tc.err)
			got := classify(wrapped)
			if got.Timeout != tc.lost || got.RttUs != 0 || (tc.lost && got.Err != "") || (!tc.lost && got.Err != wrapped.Error()) {
				t.Fatalf("classification=%+v lost=%v", got, tc.lost)
			}
		})
	}
}

func TestEnginesReportLocalFileExhaustion(t *testing.T) {
	socket := newQuietSocket()
	socket.write = func([]byte, net.Addr) (int, error) { return 0, syscall.EMFILE }
	ic := icmpWithSocket(t, socket)
	tcp := TCP{Clock: clock.Real(), Targets: loopbackTargets(t, nil), DialContext: func(context.Context, string, string) (net.Conn, error) { return nil, syscall.EMFILE }}
	for name, out := range map[string]Outcome{"icmp": ic.Probe(t.Context(), task(1)), "tcp": tcp.Probe(t.Context(), tcpTask("127.0.0.1:9"))} {
		if out.Timeout || !strings.Contains(out.Err, syscall.EMFILE.Error()) {
			t.Errorf("%s local exhaustion=%+v", name, out)
		}
	}
}
