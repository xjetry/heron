package auth

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestTrustedRequestSchemeRejectsAmbiguousProxyHeaders(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	for _, tc := range []struct {
		name, want string
		xfp, fwd   []string
		bad        bool
	}{
		{name: "no headers", want: "http"},
		{name: "single protocol", xfp: []string{"https"}, want: "https"},
		{name: "Forwarded", fwd: []string{`for=198.51.100.1;proto="https";host=untrusted.example`}, want: "https"},
		{name: "agreeing headers", xfp: []string{"https"}, fwd: []string{"proto=https"}, want: "https"},
		{name: "duplicate header", xfp: []string{"https", "https"}, bad: true},
		{name: "multiple hops", xfp: []string{"https,http"}, bad: true},
		{name: "conflicting", xfp: []string{"https"}, fwd: []string{"proto=http"}, bad: true},
		{name: "repeated Forwarded", fwd: []string{"proto=https", "proto=https"}, bad: true},
		{name: "repeated parameter", fwd: []string{"proto=https;proto=https"}, bad: true},
		{name: "Forwarded chain", fwd: []string{"proto=https,proto=https"}, bad: true},
		{name: "missing proto", fwd: []string{"host=admin.example"}, bad: true},
		{name: "unclosed quote", fwd: []string{`proto="https`}, bad: true},
		{name: "unsupported protocol", xfp: []string{"wss"}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://admin.example", nil)
			r.RemoteAddr = "192.0.2.1:1234"
			r.Header["X-Forwarded-Proto"] = tc.xfp
			r.Header["Forwarded"] = tc.fwd
			got, err := TrustedRequestScheme(r, trusted)
			if (err != nil) != tc.bad || (!tc.bad && got != tc.want) {
				t.Fatalf("scheme = %q, err = %v; want %q, bad = %v", got, err, tc.want, tc.bad)
			}
			r.RemoteAddr = "198.51.100.1:1234"
			if got, err := TrustedRequestScheme(r, trusted); got != "http" || err != nil {
				t.Fatal("untrusted proxy affected cleartext scheme")
			}
			r.TLS = &tls.ConnectionState{}
			if got, err := TrustedRequestScheme(r, trusted); got != "https" || err != nil {
				t.Fatal("untrusted proxy affected direct TLS scheme")
			}
		})
	}
}

func TestClientIPIgnoresForwardedHeaderFromUntrustedPeer(t *testing.T) {
	got := ClientIP("198.51.100.7:4321", []string{"203.0.113.9"}, nil)
	if got != netip.MustParseAddr("198.51.100.7") {
		t.Fatalf("got %v", got)
	}
}

func TestClientIPUsesRightmostUntrustedForwardedAddress(t *testing.T) {
	trusted, _ := ParsePrefixes("10.0.0.0/8, 127.0.0.1")
	got := ClientIP("10.1.2.3:80", []string{"203.0.113.9, 10.9.9.9"}, trusted)
	if got != netip.MustParseAddr("203.0.113.9") {
		t.Fatalf("got %v", got)
	}
}

// 可信代理不动客户端自带的那一行、另起一行追加真实地址：两行按顺序拼接后从右向左取，伪造的第一行不起作用。
func TestClientIPReadsEveryForwardedForLine(t *testing.T) {
	trusted, _ := ParsePrefixes("10.0.0.0/8")
	got := ClientIP("10.1.2.3:80", []string{"203.0.113.66", "198.51.100.9, 10.9.9.9"}, trusted)
	if got != netip.MustParseAddr("198.51.100.9") {
		t.Fatalf("got %v, want the address the trusted proxy appended on its own line", got)
	}
}

func TestClientIPMalformedHeaderFallsBackToPeer(t *testing.T) {
	trusted, _ := ParsePrefixes("10.0.0.0/8")
	got := ClientIP("10.1.2.3:80", []string{"not-an-ip"}, trusted)
	if got != netip.MustParseAddr("10.1.2.3") {
		t.Fatalf("got %v", got)
	}
}

func TestClientIPUnmapsIPv4InIPv6(t *testing.T) {
	got := ClientIP("[::ffff:198.51.100.7]:1", nil, nil)
	if got != netip.MustParseAddr("198.51.100.7") {
		t.Fatalf("got %v", got)
	}
}

func TestParsePrefixesRejectsGarbage(t *testing.T) {
	if _, err := ParsePrefixes("10.0.0.0/8, banana"); err == nil {
		t.Fatal("expected error")
	}
}

func TestTrustedRequestScheme(t *testing.T) {
	trusted, _ := ParsePrefixes("10.0.0.0/8")
	cases := []struct {
		peer string
		xfp  []string
		want string
		bad  bool
	}{
		{"10.1.2.3:4444", []string{"https"}, "https", false},
		{"10.1.2.3:4444", []string{"HTTPS, http"}, "", true},
		{"10.1.2.3:4444", []string{"http"}, "http", false},
		{"10.1.2.3:4444", nil, "http", false},
		{"10.1.2.3:4444", []string{"https", "http"}, "", true},
		{"203.0.113.9:4444", []string{"https"}, "http", false},
		{"garbage", []string{"https"}, "http", false},
	}
	for _, c := range cases {
		t.Run(c.peer+"/"+strings.Join(c.xfp, "|"), func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://admin.example", nil)
			r.RemoteAddr = c.peer
			r.Header["X-Forwarded-Proto"] = c.xfp
			if got, err := TrustedRequestScheme(r, trusted); got != c.want || (err != nil) != c.bad {
				t.Fatalf("TrustedRequestScheme(%q, %q) = (%q, %v), want (%q, bad=%v)", c.peer, c.xfp, got, err, c.want, c.bad)
			}
		})
	}
}

// 按来源计数的键：IPv4 一个地址一个键；IPv6 同一 /64 的地址合成一个键、不同 /64 各自一个；IPv4 映射地址按 IPv4 算，
// 不还原的话它们的前 64 位全是 0，会全部合进 ::/64。
func TestSourceKey(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"192.0.2.1", "192.0.2.1"},
		{"192.0.2.2", "192.0.2.2"},
		{"::ffff:192.0.2.1", "192.0.2.1"},
		{"2001:db8:1:2::1", "2001:db8:1:2::"},
		{"2001:db8:1:2:ffff:ffff:ffff:ffff", "2001:db8:1:2::"},
		{"2001:db8:1:3::1", "2001:db8:1:3::"},
		{"fe80::1%eth0", "fe80::"},
	} {
		if got := SourceKey(netip.MustParseAddr(c.in)).String(); got != c.want {
			t.Errorf("SourceKey(%s) = %s, want %s", c.in, got, c.want)
		}
	}
	if got := SourceKey(netip.Addr{}); got.IsValid() {
		t.Errorf("SourceKey(invalid) = %s, want the invalid address back", got)
	}
}

// IPv6 的键写成前缀，免得在报错与日志里被读成一个具体地址。
func TestDescribeSource(t *testing.T) {
	for in, want := range map[string]string{"192.0.2.1": "192.0.2.1", "2001:db8:1:2::": "2001:db8:1:2::/64"} {
		if got := DescribeSource(netip.MustParseAddr(in)); got != want {
			t.Errorf("DescribeSource(%s) = %s, want %s", in, got, want)
		}
	}
}
