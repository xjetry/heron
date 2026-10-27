package tcpstall

import (
	"net"
	"testing"
	"time"
)

func TestBoundRejectsNonPositiveAndIgnoresNonTCP(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if err := Bound(a, time.Second); err != nil {
		t.Fatalf("non-TCP conn: %v", err)
	}
	for _, d := range []time.Duration{0, -time.Second, time.Microsecond} {
		if err := Bound(a, d); err == nil {
			t.Fatalf("Bound(%v) accepted", d)
		}
	}
}
