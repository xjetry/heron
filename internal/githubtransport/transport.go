// Package githubtransport 为官方发行资产和主题共享受限的 GitHub HTTPS 传输。
package githubtransport

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// NewClient 禁用环境代理，并将连接固定到验证过的公网地址。timeout 是含读完正文的总时限；0 表示不设，期限只来自
// 请求的 ctx，建连、TLS 握手与等响应头仍各有时限，但已开始传输的正文卡住时只有 ctx 能结束它。
func NewClient(timeout time.Duration) *http.Client {
	dialer := githubDialer{
		lookup: net.DefaultResolver.LookupNetIP,
		dial:   (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}
	return WithTransport(&http.Transport{
		DialContext:           dialer.dialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          8,
		MaxConnsPerHost:       4,
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
	}, timeout)
}

// WithTransport 用同样的 URL 和重定向约束包装传输，供组件注入受控传输。
func WithTransport(transport http.RoundTripper, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: githubTransport{base: transport},
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("GitHub download exceeded 5 redirects")
			}
			return validateGitHubDestination(req)
		},
	}
}

type githubTransport struct{ base http.RoundTripper }

func (t githubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := validateGitHubDestination(req); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

func validateGitHubDestination(req *http.Request) error {
	u := req.URL
	if u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Fragment != "" || !githubHost(u.Hostname()) || u.Port() != "" && u.Port() != "443" || req.Host != "" && req.Host != u.Host {
		return fmt.Errorf("GitHub destination must use HTTPS on an allowed hostname without credentials or Host override")
	}
	return nil
}

func githubHost(host string) bool {
	switch host {
	case "api.github.com", "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return true
	default:
		return false
	}
}

type githubDialer struct {
	lookup func(context.Context, string, string) ([]netip.Addr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
}

func (d githubDialer) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" || !githubHost(host) || network != "tcp" {
		return nil, fmt.Errorf("GitHub destination is not an allowed HTTPS address")
	}
	addresses, err := d.lookup(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve GitHub destination: %w", err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("GitHub destination resolved to no addresses")
	}
	// 全部候选先检查，再只向这些 IP 拨号；HTTP Transport 仍按原始主机名校验 TLS，二次 DNS 不能改写连接目标。
	for _, address := range addresses {
		if !githubPublicIP(address) {
			return nil, fmt.Errorf("GitHub destination resolved to a non-public IP address")
		}
	}
	for _, address := range addresses {
		conn, dialErr := d.dial(ctx, network, net.JoinHostPort(address.Unmap().String(), port))
		if dialErr == nil {
			return conn, nil
		}
		err = dialErr
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("connect to GitHub destination: %w", err)
}

var githubNonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}

func githubPublicIP(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.Zone() != "" {
		return false
	}
	// IPv6 仅允许当前全球单播范围，排除 NAT64 等可把地址重新映射到内网的转换空间。
	if address.Is6() && !netip.MustParsePrefix("2000::/3").Contains(address) {
		return false
	}
	for _, prefix := range githubNonPublicPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
