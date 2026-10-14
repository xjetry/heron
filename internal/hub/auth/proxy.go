package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// TrustedRequestScheme 拒绝转发协议的重复、冲突及多跳值；来源校验和 cookie 必须采信同一可信协议。
func TrustedRequestScheme(r *http.Request, trusted []netip.Prefix) (string, error) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if !inAny(peerIP(r.RemoteAddr), trusted) {
		return scheme, nil
	}
	var forwarded string
	xfp := r.Header.Values("X-Forwarded-Proto")
	if len(xfp) > 1 {
		return "", errors.New("ambiguous forwarded protocol")
	}
	if len(xfp) == 1 {
		forwarded = strings.ToLower(strings.TrimSpace(xfp[0]))
		if forwarded != "http" && forwarded != "https" {
			return "", errors.New("invalid forwarded protocol")
		}
	}
	fwd := r.Header.Values("Forwarded")
	if len(fwd) > 1 || (len(fwd) == 1 && strings.Contains(fwd[0], ",")) {
		return "", errors.New("ambiguous forwarded protocol")
	}
	if len(fwd) == 1 {
		proto := ""
		for _, part := range strings.Split(fwd[0], ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok || !strings.EqualFold(key, "proto") {
				continue
			}
			if proto != "" {
				return "", errors.New("ambiguous forwarded protocol")
			}
			value = strings.TrimSpace(value)
			if strings.HasPrefix(value, "\"") {
				if len(value) < 2 || !strings.HasSuffix(value, "\"") {
					return "", errors.New("invalid forwarded protocol")
				}
				value = value[1 : len(value)-1]
			}
			proto = strings.ToLower(value)
			if proto != "http" && proto != "https" {
				return "", errors.New("invalid forwarded protocol")
			}
		}
		if proto == "" || (forwarded != "" && proto != forwarded) {
			return "", errors.New("conflicting forwarded protocol")
		}
		forwarded = proto
	}
	if forwarded != "" {
		scheme = forwarded
	}
	return scheme, nil
}

// ClientIP 决定一个请求的来源地址。
//
// 只有 TCP 对端落在可信列表内时，X-Forwarded-For 才被采信；此时从右向左跳过
// 可信地址，第一个不可信的就是客户端。空列表 = 不信任任何转发头、一律用对端
// 地址，是收紧方向。畸形头同样回落到对端地址。hub 不从请求头推断自己是否
// 在反代之后。
//
// xff 是该头的全部字段行（Header.Values），按出现顺序：同名的多行字段等价于按顺序用逗号拼接
// （RFC 9110 第 5.3 节），代理可以不动客户端自带的那一行、另起一行追加它看到的地址。只读第一行，
// "从右向左"就是在客户端写的那一行里找，客户端改一个头就能换来源。只传一行（如 []string{Header.Get(…)}）
// 照样能编译，"每一行都被读到"由多行用例钉住：ratelimit 的 TestBySourceReadsEveryForwardedForLine、
// auth 的 TestClientIPReadsEveryForwardedForLine、api 的 TestLoginLockoutKeysOnEveryForwardedForLine、
// ingest 的 TestReportRecordsTheSourceHubSees/可信代理追加的转发头覆盖客户端伪造的第一行。
func ClientIP(peerAddr string, xff []string, trusted []netip.Prefix) netip.Addr {
	peer := peerIP(peerAddr)
	if !peer.IsValid() || !inAny(peer, trusted) {
		return peer
	}
	parts := strings.Split(strings.Join(xff, ","), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		s := strings.TrimSpace(parts[i])
		if s == "" {
			continue
		}
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return peer
		}
		ip = ip.Unmap()
		if !inAny(ip, trusted) {
			return ip
		}
	}
	return peer
}

// SourceText 把 ClientIP 得到的来源地址写成存储与展示用的规范文本：IPv4 映射地址还原成 IPv4，点分；IPv6 为 RFC 5952 的
// 压缩形式（netip 的 String）；区域标识（%eth0）去掉——它只在 hub 本机有意义。取不到对端时为空串。live 层照常用它
// 覆盖条目里的来源，"空串不覆盖已落盘的值"由 store.WriteMinuteBatch 那条 UPDATE 里 COALESCE(NULLIF(?, 空串), last_source)
// 承载，不是这里或调用方；hub 只监听 TCP（serve 的 listen），这种输入在生产上不会出现。
func SourceText(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.Unmap().WithZone("").String()
}

// SourceKey 把 ClientIP 得到的来源地址归一化成按来源计数的键：IPv4 按单个地址，IPv6 截到所在 /64 的网络地址。
// 一台主机通常独占整个 /64（SLAAC 与隐私扩展地址随时可换），逐地址计键等于在 /64 里换个地址就换一份计数。
// 按来源计数的三处都经它：匿名入口的限流（ratelimit.BySource）、Register 的窗口失败计数与登录失败锁定（failureTracker）。
// IPv4 映射地址先还原成 IPv4：它们的前 64 位全是 0，不还原就全部落进 ::/64。netip.PrefixFrom 丢掉区域标识（%eth0），
// 键里没有它。无效地址（取不到对端）原样返回，这类请求共用一个键。
func SourceKey(a netip.Addr) netip.Addr {
	a = a.Unmap()
	if !a.Is6() {
		return a
	}
	return netip.PrefixFrom(a, 64).Masked().Addr()
}

// DescribeSource 把 SourceKey 的结果写成给人看的形式：IPv6 的键带上 /64，免得被读成一个具体地址。
func DescribeSource(key netip.Addr) string {
	if key.Is6() {
		return netip.PrefixFrom(key, 64).String()
	}
	return key.String()
}

func peerIP(addr string) netip.Addr {
	if ap, err := netip.ParseAddrPort(addr); err == nil {
		return ap.Addr().Unmap()
	}
	if ip, err := netip.ParseAddr(addr); err == nil {
		return ip.Unmap()
	}
	return netip.Addr{}
}

func inAny(ip netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ParsePrefixes 解析逗号分隔的 CIDR 列表；裸地址按单主机前缀处理。
func ParsePrefixes(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range strings.Split(list, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p)
			continue
		}
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: not a CIDR or address", s)
		}
		out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
	}
	return out, nil
}
