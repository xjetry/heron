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

// X-Forwarded-For 的项接受两种形式：裸地址与 地址:端口（IPv6 为 [addr]:port）。两种都解析不了（端口非数字、
// 越界、空端口、括号无端口）按畸形值处理，回落到对端地址。不带括号的 IPv6（2001:db8::1:443）本身是合法
// 地址，与端口形式无法区分，按地址解析。
func TestClientIPForwardedItemForms(t *testing.T) {
	trusted, _ := ParsePrefixes("10.0.0.0/8")
	for _, c := range []struct {
		name string
		xff  string
		want string
	}{
		{name: "ipv4 with port", xff: "1.2.3.4:5678", want: "1.2.3.4"},
		{name: "bracketed ipv6 with port", xff: "[2001:db8::1]:443", want: "2001:db8::1"},
		{name: "bracketed v4-mapped ipv6 with port is unmapped", xff: "[::ffff:1.2.3.4]:80", want: "1.2.3.4"},
		{name: "port zero still yields the address", xff: "1.2.3.4:0", want: "1.2.3.4"},
		{name: "non-numeric port falls back to peer", xff: "1.2.3.4:abc", want: "10.1.2.3"},
		{name: "brackets without port fall back to peer", xff: "[2001:db8::1]", want: "10.1.2.3"},
		{name: "empty port falls back to peer", xff: "1.2.3.4:", want: "10.1.2.3"},
		{name: "out-of-range port falls back to peer", xff: "1.2.3.4:65536", want: "10.1.2.3"},
		// 2001:db8::1:443 不带括号，无法与端口区分，本身是合法 IPv6，按地址解析。
		{name: "unbracketed ipv6 cannot be told apart from a port", xff: "2001:db8::1:443", want: "2001:db8::1:443"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := ClientIP("10.1.2.3:80", []string{c.xff}, trusted)
			if got != netip.MustParseAddr(c.want) {
				t.Fatalf("ClientIP(xff %q) = %v, want %s", c.xff, got, c.want)
			}
		})
	}
}

// 带端口的项若是可信代理，照常跳过、继续向左取到客户端。
func TestClientIPSkipsTrustedProxyItemWithPort(t *testing.T) {
	trusted, _ := ParsePrefixes("10.0.0.1")
	got := ClientIP("10.0.0.1:9", []string{"1.2.3.4, 10.0.0.1:443"}, trusted)
	if got != netip.MustParseAddr("1.2.3.4") {
		t.Fatalf("got %v, want the client to the left of the ported trusted proxy item", got)
	}
}

func TestParsePrefixesRejectsGarbage(t *testing.T) {
	if _, err := ParsePrefixes("10.0.0.0/8, banana"); err == nil {
		t.Fatal("expected error")
	}
}

// 反代经链路本地 IPv6 连进来时，对端地址带区域标识（[fe80::1%eth0]:port）。可信判定不看区域：协议、
// 对端与 X-Forwarded-For 里的代理项三处都按不带区域的地址比对 fe80::/10。
func TestTrustedProxyMatchesZonedLinkLocalAddress(t *testing.T) {
	trusted, err := ParsePrefixes("fe80::/10")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "http://admin.example", nil)
	r.RemoteAddr = "[fe80::1%eth0]:4444"
	r.Header.Set("X-Forwarded-Proto", "https")
	if got, err := TrustedRequestScheme(r, trusted); got != "https" || err != nil {
		t.Errorf("TrustedRequestScheme via zoned trusted peer = (%q, %v), want (https, nil)", got, err)
	}
	if got := ClientIP("[fe80::1%eth0]:4444", []string{"203.0.113.9"}, trusted); got != netip.MustParseAddr("203.0.113.9") {
		t.Errorf("ClientIP via zoned trusted peer = %v, want the forwarded client 203.0.113.9", got)
	}
	if got := ClientIP("[fe80::1%eth0]:4444", []string{"203.0.113.9, fe80::2%eth0"}, trusted); got != netip.MustParseAddr("203.0.113.9") {
		t.Errorf("ClientIP with a zoned trusted proxy item = %v, want the client to its left, 203.0.113.9", got)
	}
}

// 区域标识只说明 hub 从本机哪个接口收到连接，可信判定不区分接口；配置里写上它会被误读成"只信这个接口"。
func TestParsePrefixesRejectsZonedAddress(t *testing.T) {
	_, err := ParsePrefixes("10.0.0.0/8, fe80::1%eth0")
	if err == nil || !strings.Contains(err.Error(), `"fe80::1%eth0"`) || !strings.Contains(err.Error(), "zone") {
		t.Fatalf("ParsePrefixes(zoned address) err = %v, want an error naming the entry and the zone", err)
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
