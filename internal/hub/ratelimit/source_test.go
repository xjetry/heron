package ratelimit

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

func TestBySourceLimitsBeforeNextAndPassesTheSource(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	var seen []string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from, ok := SourceOf(r.Context())
		if !ok {
			t.Error("next ran without a source address")
		}
		seen = append(seen, from.String())
	})
	h := BySource(New[netip.Addr](2, time.Second), []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, clk, next)
	call := func(peer, xff string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/heron.v1.S/M", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = peer
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body, _ := io.ReadAll(rec.Body)
		return rec.Code, string(body)
	}
	// 可信代理转发的请求按 X-Forwarded-For 计；不可信对端的这个头被忽略，按对端地址计。
	for range 2 {
		if code, body := call("192.0.2.1:5000", "203.0.113.9"); code != http.StatusOK {
			t.Fatalf("within capacity: %d %s", code, body)
		}
	}
	code, body := call("192.0.2.1:5000", "203.0.113.9")
	if code != http.StatusTooManyRequests || !strings.Contains(body, `"code":"resource_exhausted"`) ||
		!strings.Contains(body, "rate limit exceeded for 203.0.113.9: each source (one IPv4 address, or one IPv6 /64) may make 2 requests at once and then one every 1s") {
		t.Fatalf("past capacity: %d %s", code, body)
	}
	if code, _ := call("198.51.100.1:5000", "203.0.113.9"); code != http.StatusOK {
		t.Fatalf("untrusted peer was charged to the forwarded address: %d", code)
	}
	if want := []string{"203.0.113.9", "203.0.113.9", "198.51.100.1"}; strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("next saw sources %v, want %v (the limited request must not reach next)", seen, want)
	}
}

// 可信代理另起一行追加真实地址时，桶取自那一行：客户端每次换一个伪造的第一行也换不了桶。
func TestBySourceReadsEveryForwardedForLine(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	var seen []string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from, _ := SourceOf(r.Context())
		seen = append(seen, from.String())
	})
	h := BySource(New[netip.Addr](2, time.Second), []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, clk, next)
	for i := range 3 {
		req := httptest.NewRequest(http.MethodPost, "/heron.v1.S/M", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.1:5000"
		req.Header.Add("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i+1))
		req.Header.Add("X-Forwarded-For", "198.51.100.9")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		want := http.StatusOK
		if i == 2 {
			want = http.StatusTooManyRequests
		}
		if rec.Code != want {
			t.Fatalf("request %d: %d %s, want %d", i, rec.Code, rec.Body.String(), want)
		}
	}
	if want := "198.51.100.9,198.51.100.9"; strings.Join(seen, ",") != want {
		t.Fatalf("next saw sources %v, want %s", seen, want)
	}
}

// 一台主机通常独占整个 IPv6 /64，逐地址计键等于不限流：IPv6 同一 /64 的地址共用一桶，不同 /64 各自一桶；
// IPv4 仍是一个地址一桶。放行的请求带进 next 的是同一个键。
func TestBySourceKeysIPv4ByAddressAndIPv6ByPrefix64(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	var seen []string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from, _ := SourceOf(r.Context())
		seen = append(seen, from.String())
	})
	h := BySource(New[netip.Addr](1, time.Second), nil, clk, next)
	for _, c := range []struct {
		peer   string
		status int
		body   string
	}{
		{"[2001:db8:1:2::1]:5000", http.StatusOK, ""},
		{"[2001:db8:1:2:ffff:ffff:ffff:ffff]:5000", http.StatusTooManyRequests, "rate limit exceeded for 2001:db8:1:2::/64:"},
		{"[2001:db8:1:3::1]:5000", http.StatusOK, ""},
		{"192.0.2.1:5000", http.StatusOK, ""},
		{"192.0.2.2:5000", http.StatusOK, ""},
		{"192.0.2.1:5001", http.StatusTooManyRequests, "rate limit exceeded for 192.0.2.1:"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/heron.v1.S/M", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = c.peer
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.body) {
			t.Errorf("%s: %d %s, want %d %q", c.peer, rec.Code, rec.Body.String(), c.status, c.body)
		}
	}
	if want := []string{"2001:db8:1:2::", "2001:db8:1:3::", "192.0.2.1", "192.0.2.2"}; strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("next saw sources %v, want %v", seen, want)
	}
}
