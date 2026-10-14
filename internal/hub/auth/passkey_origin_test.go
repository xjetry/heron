package auth

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestPasskeyOriginNormalization(t *testing.T) {
	for _, tc := range []struct{ input, origin, rp string }{
		{"https://ADMIN.example.:443/", "https://admin.example", "admin.example"},
		{"https://例子.测试:08443", "https://xn--fsqu00a.xn--0zwm56d:8443", "xn--fsqu00a.xn--0zwm56d"},
		{"http://localhost:80", "http://localhost", "localhost"},
		{"http://[::1]:8080", "http://[::1]:8080", "::1"},
		{"https://admin.example:8443", "https://admin.example:8443", "admin.example"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			origin, rp, err := ParsePasskeyOrigin(tc.input)
			if err != nil || origin != tc.origin || rp != tc.rp {
				t.Fatalf("normalization = (%q,%q,%v), want (%q,%q)", origin, rp, err, tc.origin, tc.rp)
			}
		})
	}
	for _, input := range []string{"http://admin.example", "https://192.0.2.1", "https://[2001:db8::1]", "https://127.1", "https://0x7f.0.0.1", "https://127.000.0.1", "https://admin.example.123", "https://admin.example.0xff", "https://bad_host.example", "https://-bad.example", "https://bad..example", "https://admin.example:0", "https://admin.example:65536", "https://admin.example:", "https://admin.example?", "https://admin.example#", "https://[admin.example]", "https://admin.example/x", "https://user@admin.example", "null"} {
		t.Run(input, func(t *testing.T) {
			if _, _, err := ParsePasskeyOrigin(input); err == nil {
				t.Fatalf("invalid origin accepted: %s", input)
			}
		})
	}
}

func TestSameOriginRequestSeparatesAuthenticationFromPasskeyEligibility(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	for _, tc := range []struct {
		name, host, proto string
		origins           []string
		want              bool
	}{
		{name: "command line", host: "admin.example", want: true},
		{name: "ordinary HTTP", host: "admin.example", origins: []string{"http://admin.example"}, want: true},
		{name: "IP HTTP", host: "198.51.100.1:8080", origins: []string{"http://198.51.100.1:8080"}, want: true},
		{name: "canonical HTTPS", host: "ADMIN.example.:443", proto: "https", origins: []string{"https://admin.example"}, want: true},
		{name: "sandbox", host: "admin.example", proto: "https", origins: []string{"null"}},
		{name: "foreign", host: "admin.example", proto: "https", origins: []string{"https://foreign.example"}},
		{name: "duplicate", host: "admin.example", proto: "https", origins: []string{"https://admin.example", "https://admin.example"}},
		{name: "protocol mismatch", host: "admin.example", origins: []string{"https://admin.example"}},
		{name: "port mismatch", host: "admin.example:8443", proto: "https", origins: []string{"https://admin.example"}},
		{name: "ambiguous protocol", host: "admin.example", proto: "https,http", origins: []string{"https://admin.example"}},
		{name: "invalid host path", host: "admin.example/", proto: "https", origins: []string{"https://admin.example"}},
		{name: "invalid host fragment", host: "admin.example#", proto: "https", origins: []string{"https://admin.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://admin.example/security", nil)
			r.Host, r.RemoteAddr = tc.host, "192.0.2.1:1234"
			r.Header["Origin"] = tc.origins
			if tc.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			if got := SameOriginRequest(r, trusted); got != tc.want {
				t.Fatalf("same origin = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWebAuthnContextValidatesRequestOrigin(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	for _, tc := range []struct {
		name, host, origin, proto, peer, wantOrigin, reason string
		tls                                                 bool
	}{
		{name: "direct TLS", host: "admin.example", origin: "https://admin.example", tls: true, wantOrigin: "https://admin.example"},
		{name: "trusted proxy", host: "admin.example:8443", origin: "https://admin.example:8443", proto: "https", peer: "192.0.2.1:1234", wantOrigin: "https://admin.example:8443"},
		{name: "canonical host", host: "ADMIN.example.:443", origin: "https://admin.example", tls: true, wantOrigin: "https://admin.example"},
		{name: "untrusted proxy", host: "admin.example", origin: "https://admin.example", proto: "https", peer: "198.51.100.1:1234", reason: "untrusted_proxy"},
		{name: "insecure", host: "admin.example", origin: "http://admin.example", reason: "https_required"},
		{name: "localhost", host: "localhost:8080", origin: "http://localhost:8080", wantOrigin: "http://localhost:8080"},
		{name: "missing origin", host: "admin.example", tls: true, wantOrigin: "https://admin.example", reason: "request_origin_required"},
		{name: "sandbox origin", host: "admin.example", origin: "null", tls: true, wantOrigin: "https://admin.example", reason: "request_origin_mismatch"},
		{name: "foreign origin", host: "admin.example", origin: "https://evil.example", tls: true, wantOrigin: "https://admin.example", reason: "request_origin_mismatch"},
		{name: "different port", host: "admin.example:8443", origin: "https://admin.example", tls: true, wantOrigin: "https://admin.example:8443", reason: "request_origin_mismatch"},
		{name: "ambiguous proxy", host: "admin.example", origin: "https://admin.example", proto: "https,http", peer: "192.0.2.1:1234", reason: "invalid_forwarded_proto"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://admin.example/security", nil)
			r.Host = tc.host
			r.RemoteAddr = tc.peer
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			r.Header.Set("X-Forwarded-Host", "ignored.example")
			called := false
			WebAuthnContext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				origin, reason := currentWebAuthnOrigin(r.Context())
				if origin != tc.wantOrigin || reason != tc.reason {
					t.Fatalf("context = (%q, %q), want (%q, %q)", origin, reason, tc.wantOrigin, tc.reason)
				}
			}), trusted).ServeHTTP(httptest.NewRecorder(), r)
			if !called {
				t.Fatal("ordinary password and recovery requests must remain reachable")
			}
		})
	}
	ctx := WithWebAuthnOrigin(context.Background(), "http://admin.example")
	if origin, reason := currentWebAuthnOrigin(ctx); origin != "" || reason != "invalid_origin" {
		t.Fatal("invalid explicitly supplied context accepted")
	}
}
