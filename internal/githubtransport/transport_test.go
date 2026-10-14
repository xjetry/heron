package githubtransport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type githubRoundTripFunc func(*http.Request) (*http.Response, error)

func (f githubRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubDialRejectsAllNonPublicAddresses(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "0.0.0.0", "224.0.0.1",
		"100.64.0.1", "192.0.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "240.0.0.1",
		"::1", "::", "fe80::1", "fc00::1", "ff02::1", "::ffff:127.0.0.1", "::ffff:169.254.169.254",
		"64:ff9b::7f00:1", "64:ff9b:1::a00:1", "2001:db8::1", "2002:7f00:1::", "2001::1",
	} {
		t.Run(address, func(t *testing.T) {
			dialer := githubDialer{
				lookup: func(context.Context, string, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr("140.82.112.3"), netip.MustParseAddr(address)}, nil
				},
				dial: func(context.Context, string, string) (net.Conn, error) {
					t.Fatal("unsafe DNS answer reached dial")
					return nil, nil
				},
			}
			_, err := dialer.dialContext(context.Background(), "tcp", "api.github.com:443")
			if err == nil || !strings.Contains(err.Error(), "non-public") {
				t.Fatalf("dial error = %v", err)
			}
		})
	}
}

func TestGitHubDialPinsResolvedIP(t *testing.T) {
	lookupCalls := 0
	var dialed []string
	dialer := githubDialer{
		lookup: func(_ context.Context, network, host string) ([]netip.Addr, error) {
			lookupCalls++
			if network != "ip" || host != "api.github.com" {
				t.Fatalf("lookup %q %q", network, host)
			}
			if lookupCalls > 1 {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("140.82.112.3"), netip.MustParseAddr("2606:50c0:8000::154")}, nil
		},
		dial: func(_ context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" {
				t.Fatalf("network = %s", network)
			}
			dialed = append(dialed, address)
			return nil, errors.New("controlled connection failure")
		},
	}
	_, err := dialer.dialContext(context.Background(), "tcp", "api.github.com:443")
	if err == nil || lookupCalls != 1 || fmt.Sprint(dialed) != "[140.82.112.3:443 [2606:50c0:8000::154]:443]" {
		t.Fatalf("dialed = %v, lookups = %d, err = %v", dialed, lookupCalls, err)
	}
	for _, host := range []string{"api.github.com:80", "api.github.com.evil.test:443", "127.0.0.1:443"} {
		_, err := dialer.dialContext(context.Background(), "tcp", host)
		if err == nil || lookupCalls != 1 {
			t.Fatalf("invalid host %q reached lookup, err = %v", host, err)
		}
	}
}

func TestGitHubTransportRejectsHostOverrideAndProxy(t *testing.T) {
	client := WithTransport(githubRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("forged Host reached transport")
		return nil, nil
	}), time.Second)
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/owner/repo/releases", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1"
	if _, err := client.Do(req); err == nil {
		t.Fatal("forged Host was accepted")
	}
	transport := NewClient(time.Second).Transport.(githubTransport).base.(*http.Transport)
	if transport.Proxy != nil || transport.DialContext == nil || transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("unsafe production transport: %#v", transport)
	}
	if client.Timeout <= 0 {
		t.Fatal("HTTP client lacks a request timeout")
	}
}

func TestGitHubPinnedDialKeepsTLSHostname(t *testing.T) {
	serverNames := make(chan string, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("untrusted TLS certificate reached the HTTP handler")
	}))
	server.TLS = &tls.Config{GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
		serverNames <- info.ServerName
		return nil, nil
	}}
	server.StartTLS()
	defer server.Close()
	dialer := githubDialer{
		lookup: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("140.82.112.3")}, nil
		},
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "140.82.112.3:443" {
				t.Fatalf("dial address = %q", address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	}
	client := NewClient(time.Second)
	transport := client.Transport.(githubTransport).base.(*http.Transport)
	transport.DialContext = dialer.dialContext
	defer transport.CloseIdleConnections()
	_, err := client.Get("https://api.github.com/repos/owner/repo/releases")
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("TLS error = %v", err)
	}
	select {
	case name := <-serverNames:
		if name != "api.github.com" {
			t.Fatalf("TLS ServerName = %q", name)
		}
	default:
		t.Fatal("TLS handshake was not attempted")
	}
}
