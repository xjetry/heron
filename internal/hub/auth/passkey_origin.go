package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/net/idna"
)

type webAuthnOriginKey struct{}
type webAuthnOrigin struct{ origin, reason string }

// WithWebAuthnOrigin 为已经确认可信的来源建立认证上下文；HTTP 入口应使用 WebAuthnContext。
func WithWebAuthnOrigin(ctx context.Context, origin string) context.Context {
	normalized, _, err := normalizePasskeyOrigin(origin)
	if err != nil {
		return context.WithValue(ctx, webAuthnOriginKey{}, webAuthnOrigin{reason: "invalid_origin"})
	}
	return context.WithValue(ctx, webAuthnOriginKey{}, webAuthnOrigin{origin: normalized})
}

// WebAuthnContext 仅从保留的外部 Host 与可信协议建立候选来源，不采用客户端自报的转发主机名。
func WebAuthnContext(next http.Handler, trusted []netip.Prefix) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := webAuthnOrigin{}
		scheme, err := TrustedRequestScheme(r, trusted)
		if err != nil {
			current.reason = "invalid_forwarded_proto"
		} else {
			candidate, e := requestHTTPOrigin(r, scheme)
			err = e
			if err == nil {
				current.origin, _, err = normalizePasskeyOrigin(candidate)
			}
			if err != nil {
				current.reason = "https_required"
				if scheme == "http" && r.TLS == nil && len(r.Header.Values("X-Forwarded-Proto"))+len(r.Header.Values("Forwarded")) > 0 && !inAny(peerIP(r.RemoteAddr), trusted) {
					current.reason = "untrusted_proxy"
				}
			} else {
				origins := r.Header.Values("Origin")
				if len(origins) != 1 {
					current.reason = "request_origin_required"
				} else if origin, _, e := normalizePasskeyOrigin(origins[0]); e != nil || origin != current.origin {
					current.reason = "request_origin_mismatch"
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), webAuthnOriginKey{}, current)))
	})
}

// SameOriginRequest 对浏览器显式来源逐项校验；没有 Origin 的非浏览器客户端不因此失去访问入口。
func SameOriginRequest(r *http.Request, trusted []netip.Prefix) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	scheme, err := TrustedRequestScheme(r, trusted)
	if err != nil {
		return false
	}
	requestOrigin, err := requestHTTPOrigin(r, scheme)
	if err != nil {
		return false
	}
	origin, _, err := normalizeHTTPOrigin(origins[0])
	return err == nil && origin == requestOrigin
}

func requestHTTPOrigin(r *http.Request, scheme string) (string, error) {
	if strings.ContainsAny(r.Host, "/\\?#@") {
		return "", errors.New("invalid request host")
	}
	origin, _, err := normalizeHTTPOrigin(scheme + "://" + r.Host)
	return origin, err
}

func currentWebAuthnOrigin(ctx context.Context) (string, string) {
	current, ok := ctx.Value(webAuthnOriginKey{}).(webAuthnOrigin)
	if !ok {
		return "", "request_origin_required"
	}
	return current.origin, current.reason
}

// ParsePasskeyOrigin 是旧 --admin-origin 的格式校验，返回规范化的 Origin 与由它推出的 RP ID。serve 的 parseServeOptions
// 在打开数据库之前经它拒绝非法值（不看持久绑定）；ConfigureWebAuthn 在无持久绑定时也经它校验导入来源，两处是同一个判定，
// 非法值的错误文案只有这一份。
func ParsePasskeyOrigin(origin string) (normalized, rpID string, err error) {
	return normalizePasskeyOrigin(origin)
}

func normalizePasskeyOrigin(origin string) (string, string, error) {
	normalized, host, err := normalizeHTTPOrigin(origin)
	if err != nil {
		return "", "", err
	}
	local := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if _, err := netip.ParseAddr(host); err == nil && !local {
		return "", "", errors.New("passkey origin requires a hostname")
	}
	if !strings.HasPrefix(normalized, "https://") && !local {
		return "", "", errors.New("passkey origin requires HTTPS except localhost")
	}
	return normalized, host, nil
}

func normalizeHTTPOrigin(origin string) (string, string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" || strings.ContainsAny(origin, "?#") || (u.Path != "" && u.Path != "/") {
		return "", "", errors.New("invalid HTTP origin")
	}
	host, err := securityHostname(u.Hostname())
	if err != nil || host == "" {
		return "", "", errors.New("invalid HTTP hostname")
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.Zone() != "" {
		return "", "", errors.New("invalid HTTP hostname")
	} else if err != nil && strings.HasPrefix(u.Host, "[") {
		return "", "", errors.New("invalid HTTP hostname")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", "", errors.New("invalid HTTP scheme")
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return "", "", errors.New("invalid passkey port")
	}
	if port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return "", "", errors.New("invalid passkey port")
		}
		port = strconv.Itoa(n)
		if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
			port = ""
		}
	}
	authority := host
	if port != "" {
		authority = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	return u.Scheme + "://" + authority, host, nil
}

func securityHostname(host string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.String(), nil
	}
	host, err := idna.Lookup.ToASCII(host)
	if err != nil || len(host) > 253 {
		return "", errors.New("invalid hostname")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid hostname")
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", errors.New("invalid hostname")
			}
		}
	}
	// 浏览器把末段为数字的主机按 IPv4 解析；只有 netip 已识别的规范 IP 才能保持两端口径一致。
	labels := strings.Split(host, ".")
	last := labels[len(labels)-1]
	digits := "0123456789"
	if strings.HasPrefix(last, "0x") {
		last, digits = strings.TrimPrefix(last, "0x"), "0123456789abcdef"
	}
	if strings.Trim(last, digits) == "" {
		return "", errors.New("invalid numeric hostname")
	}
	return host, nil
}

func validPasskeyBinding(origin, rpID string) bool {
	normalized, host, err := normalizePasskeyOrigin(origin)
	return err == nil && normalized == origin && host == rpID
}

func webAuthnForBinding(origin, rpID string) (*webauthn.WebAuthn, error) {
	if !validPasskeyBinding(origin, rpID) {
		return nil, ErrSecurity
	}
	return webauthn.New(&webauthn.Config{RPDisplayName: "Heron", RPID: rpID, RPOrigins: []string{origin}, AuthenticatorSelection: protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementRequired, UserVerification: protocol.VerificationRequired}})
}

func passkeyOrigin(ctx context.Context, s securityState, allowUnbound bool) (string, error) {
	origin, reason := currentWebAuthnOrigin(ctx)
	if reason != "" || origin == "" || (s.Origin != "" && s.Origin != origin) || (s.Origin == "" && (!allowUnbound || len(s.Passkeys) != 0)) {
		return "", ErrSecurity
	}
	if (s.Origin != "" || s.RPID != "") && !validPasskeyBinding(s.Origin, s.RPID) {
		return "", ErrSecurity
	}
	return origin, nil
}

func challengeWebAuthn(ctx context.Context, c securityChallenge) (*webauthn.WebAuthn, error) {
	origin, reason := currentWebAuthnOrigin(ctx)
	if reason != "" || origin == "" || origin != c.Origin || c.Data == nil {
		return nil, ErrSecurity
	}
	return webAuthnForBinding(c.Origin, c.RPID)
}
