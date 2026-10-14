package probelimit

import (
	"slices"
	"strings"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

func TestCheckTask(t *testing.T) {
	for _, tc := range []struct {
		name              string
		kind              heronv1.ProbeKind
		target            string
		interval, timeout uint32
		dnsServer         string
		want              string
	}{
		{"icmp_ip", heronv1.ProbeKind_PROBE_KIND_ICMP, "1.1.1.1", 5, 100, "", ""},
		{"icmp_dns", heronv1.ProbeKind_PROBE_KIND_ICMP, "example.com.", 3600, 5000, "", ""},
		{"icmp_ipv6", heronv1.ProbeKind_PROBE_KIND_ICMP, "::1", 5, 5000, "", ""},
		{"tcp_dns", heronv1.ProbeKind_PROBE_KIND_TCP, "example.com:443", 5, 1000, "", ""},
		{"tcp_ipv6", heronv1.ProbeKind_PROBE_KIND_TCP, "[::1]:80", 5, 1000, "", ""},
		{"interval_low", heronv1.ProbeKind_PROBE_KIND_ICMP, "localhost", 4, 1000, "", "interval_s must be between 5 and 3600"},
		{"interval_high", heronv1.ProbeKind_PROBE_KIND_ICMP, "localhost", 3601, 1000, "", "interval_s must be between 5 and 3600"},
		{"timeout_low", heronv1.ProbeKind_PROBE_KIND_ICMP, "localhost", 5, 99, "", "timeout_ms must be between"},
		{"timeout_high", heronv1.ProbeKind_PROBE_KIND_ICMP, "localhost", 5, 5001, "", "timeout_ms must be between"},
		{"icmp_port", heronv1.ProbeKind_PROBE_KIND_ICMP, "example.com:80", 5, 1000, "", "IP address or a host name"},
		{"bad_label", heronv1.ProbeKind_PROBE_KIND_ICMP, "-bad.example", 5, 1000, "", "IP address or a host name"},
		{"empty", heronv1.ProbeKind_PROBE_KIND_ICMP, "", 5, 1000, "", "IP address or a host name"},
		{"tcp_no_port", heronv1.ProbeKind_PROBE_KIND_TCP, "example.com", 5, 1000, "", "host:port"},
		{"port_low", heronv1.ProbeKind_PROBE_KIND_TCP, "example.com:0", 5, 1000, "", "port must be between"},
		{"port_high", heronv1.ProbeKind_PROBE_KIND_TCP, "example.com:65536", 5, 1000, "", "port must be between"},
		{"kind_missing", heronv1.ProbeKind_PROBE_KIND_UNSPECIFIED, "localhost", 5, 1000, "", "kind must be"},
		{"long_target", heronv1.ProbeKind_PROBE_KIND_ICMP, strings.Repeat("a", 254), 5, 1000, "", "at most 253 bytes"},
		{"label_long", heronv1.ProbeKind_PROBE_KIND_ICMP, strings.Repeat("a", 64), 5, 1000, "", "IP address or a host name"},
		{"label_empty", heronv1.ProbeKind_PROBE_KIND_ICMP, "a..b", 5, 1000, "", "IP address or a host name"},
		{"label_end", heronv1.ProbeKind_PROBE_KIND_ICMP, "bad-.example", 5, 1000, "", "IP address or a host name"},
		{"label_character", heronv1.ProbeKind_PROBE_KIND_ICMP, "a_b.example", 5, 1000, "", "IP address or a host name"},
		{"tcp_bad_host", heronv1.ProbeKind_PROBE_KIND_TCP, "-bad:80", 5, 1000, "", "target host for a TCP task must be an IP address or a host name"},
		{"port_text", heronv1.ProbeKind_PROBE_KIND_TCP, "example.com:http", 5, 1000, "", "port must be between"},
		{"icmp_zone", heronv1.ProbeKind_PROBE_KIND_ICMP, "fe80::1%eth0", 5, 1000, "", "IP address or a host name"},
		{"icmp_zone_control", heronv1.ProbeKind_PROBE_KIND_ICMP, "fe80::1%\n<x>", 5, 1000, "", "IP address or a host name"},
		{"tcp_zone", heronv1.ProbeKind_PROBE_KIND_TCP, "[fe80::1%eth0]:80", 5, 1000, "", "target host for a TCP task must be an IP address or a host name"},
		{"tcp_zone_control", heronv1.ProbeKind_PROBE_KIND_TCP, "[fe80::1%\n<x>]:80", 5, 1000, "", "target host for a TCP task must be an IP address or a host name"},
		{"tcp_bracket_dns", heronv1.ProbeKind_PROBE_KIND_TCP, "[example.com]:80", 5, 1000, "", "canonical host:port"},
		{"tcp_bracket_ipv4", heronv1.ProbeKind_PROBE_KIND_TCP, "[1.1.1.1]:80", 5, 1000, "", "canonical host:port"},
		{"tcp_port_plus", heronv1.ProbeKind_PROBE_KIND_TCP, "example.com:+80", 5, 1000, "", "canonical host:port"},
		{"tcp_port_zeroes", heronv1.ProbeKind_PROBE_KIND_TCP, "example.com:0080", 5, 1000, "", "canonical host:port"},
		{"label_boundary", heronv1.ProbeKind_PROBE_KIND_ICMP, strings.Repeat("a", 63), 5, 1000, "", ""},
		{"target_boundary", heronv1.ProbeKind_PROBE_KIND_ICMP, strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61), 5, 1000, "", ""},
		{"port_min", heronv1.ProbeKind_PROBE_KIND_TCP, "example.com:1", 5, 1000, "", ""},
		{"port_max", heronv1.ProbeKind_PROBE_KIND_TCP, "example.com:65535", 5, 1000, "", ""},
		{"http", heronv1.ProbeKind_PROBE_KIND_HTTP, "http://example.com", 5, 1000, "", ""},
		{"https_path_query_port", heronv1.ProbeKind_PROBE_KIND_HTTP, "https://example.com:8443/p?q=1", 5, 1000, "", ""},
		{"http_target_boundary", heronv1.ProbeKind_PROBE_KIND_HTTP, "http://" + strings.Repeat("a", 505), 5, 1000, "", ""},
		{"http_relative", heronv1.ProbeKind_PROBE_KIND_HTTP, "example.com/x", 5, 1000, "", "absolute http or https URL"},
		{"http_path_only", heronv1.ProbeKind_PROBE_KIND_HTTP, "/x", 5, 1000, "", "absolute http or https URL"},
		{"http_ftp", heronv1.ProbeKind_PROBE_KIND_HTTP, "ftp://example.com/", 5, 1000, "", "absolute http or https URL"},
		{"http_userinfo", heronv1.ProbeKind_PROBE_KIND_HTTP, "https://user@example.com/", 5, 1000, "", "user info"},
		{"http_fragment", heronv1.ProbeKind_PROBE_KIND_HTTP, "https://example.com/#frag", 5, 1000, "", "fragment"},
		{"http_no_host", heronv1.ProbeKind_PROBE_KIND_HTTP, "http:///path", 5, 1000, "", "have a host"},
		{"http_port_low", heronv1.ProbeKind_PROBE_KIND_HTTP, "http://example.com:0/", 5, 1000, "", "port must be between"},
		{"http_port_high", heronv1.ProbeKind_PROBE_KIND_HTTP, "http://example.com:65536/", 5, 1000, "", "port must be between"},
		{"http_too_long", heronv1.ProbeKind_PROBE_KIND_HTTP, "http://" + strings.Repeat("a", 506), 5, 1000, "", "at most 512 bytes"},
		{"dns", heronv1.ProbeKind_PROBE_KIND_DNS, "example.com", 5, 1000, "1.1.1.1:53", ""},
		{"dns_trailing_dot_v6_server", heronv1.ProbeKind_PROBE_KIND_DNS, "example.com.", 5, 1000, "[2001:4860:4860::8888]:53", ""},
		{"dns_server_boundary", heronv1.ProbeKind_PROBE_KIND_DNS, "example.com", 5, 1000, "[ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff]:65535", ""},
		{"dns_target_ipv4", heronv1.ProbeKind_PROBE_KIND_DNS, "1.1.1.1", 5, 1000, "1.1.1.1:53", "not an IP address"},
		{"dns_target_ipv6", heronv1.ProbeKind_PROBE_KIND_DNS, "::1", 5, 1000, "1.1.1.1:53", "not an IP address"},
		{"dns_target_bad_host", heronv1.ProbeKind_PROBE_KIND_DNS, "-bad.example", 5, 1000, "1.1.1.1:53", "must be a DNS name"},
		{"dns_target_too_long", heronv1.ProbeKind_PROBE_KIND_DNS, strings.Repeat("a", 254), 5, 1000, "1.1.1.1:53", "at most 253 bytes"},
		{"dns_server_missing", heronv1.ProbeKind_PROBE_KIND_DNS, "example.com", 5, 1000, "", "dns_server must be ip:port"},
		{"dns_server_no_port", heronv1.ProbeKind_PROBE_KIND_DNS, "example.com", 5, 1000, "8.8.8.8", "dns_server must be ip:port"},
		{"dns_server_name", heronv1.ProbeKind_PROBE_KIND_DNS, "example.com", 5, 1000, "dns.example:53", "IP literal, not a name"},
		{"dns_server_zone", heronv1.ProbeKind_PROBE_KIND_DNS, "example.com", 5, 1000, "[fe80::1%eth0]:53", "zone"},
		{"dns_server_port_low", heronv1.ProbeKind_PROBE_KIND_DNS, "example.com", 5, 1000, "1.1.1.1:0", "port must be between"},
		{"dns_server_port_high", heronv1.ProbeKind_PROBE_KIND_DNS, "example.com", 5, 1000, "1.1.1.1:65536", "port must be between"},
		{"dns_server_leading_zero", heronv1.ProbeKind_PROBE_KIND_DNS, "example.com", 5, 1000, "1.1.1.1:053", "canonical ip:port form; got \"1.1.1.1:053\"; want \"1.1.1.1:53\""},
		{"dns_server_uncompressed_v6", heronv1.ProbeKind_PROBE_KIND_DNS, "example.com", 5, 1000, "[2001:0DB8:0000::1]:53", "canonical ip:port form; got \"[2001:0DB8:0000::1]:53\"; want \"[2001:db8::1]:53\""},
		{"dns_server_too_long", heronv1.ProbeKind_PROBE_KIND_DNS, "example.com", 5, 1000, strings.Repeat("1", 48), "dns_server must be at most 47 bytes; got 48"},
		{"icmp_with_dns_server", heronv1.ProbeKind_PROBE_KIND_ICMP, "localhost", 5, 1000, "1.1.1.1:53", "dns_server only applies to a DNS task"},
		{"http_with_dns_server", heronv1.ProbeKind_PROBE_KIND_HTTP, "http://example.com", 5, 1000, "1.1.1.1:53", "dns_server only applies to a DNS task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckTask(&heronv1.ProbeTask{Kind: tc.kind, Target: tc.target, IntervalS: tc.interval, TimeoutMs: tc.timeout, DnsServer: tc.dnsServer})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid task rejected: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v; want substring %q", err, tc.want)
			}
		})
	}
	t.Run("nil", func(t *testing.T) {
		if err := CheckTask(nil); err == nil || err.Error() != "task: required" {
			t.Fatalf("nil task error=%v", err)
		}
	})
}

// pin 与 config_id 的约束在按种类分支之前检查：DNS 分支提前 return，
// 钉在 DNS 任务上的 pin 必须被拒而不是被分支放行。
// IsHTTPSTarget 按解析出的 scheme 判断，不分大小写；种类不是 HTTP 时一律不是。
func TestIsHTTPSTarget(t *testing.T) {
	for _, tc := range []struct {
		kind   heronv1.ProbeKind
		target string
		want   bool
	}{
		{heronv1.ProbeKind_PROBE_KIND_HTTP, "https://example.com/", true},
		{heronv1.ProbeKind_PROBE_KIND_HTTP, "HTTPS://example.com/", true},
		{heronv1.ProbeKind_PROBE_KIND_HTTP, "Https://example.com/", true},
		{heronv1.ProbeKind_PROBE_KIND_HTTP, "http://example.com/", false},
		{heronv1.ProbeKind_PROBE_KIND_HTTP, "://example.com/", false},
		{heronv1.ProbeKind_PROBE_KIND_TCP, "https://example.com/", false},
		{heronv1.ProbeKind_PROBE_KIND_DNS, "https://example.com/", false},
	} {
		if got := IsHTTPSTarget(tc.kind, tc.target); got != tc.want {
			t.Errorf("IsHTTPSTarget(%s, %q) = %v, want %v", tc.kind, tc.target, got, tc.want)
		}
	}
}

func TestCheckTaskPinAndConfigID(t *testing.T) {
	pin32 := make([]byte, CertSPKISHA256Len)
	cid16 := make([]byte, ConfigIDLen)
	for _, tc := range []struct {
		name          string
		kind          heronv1.ProbeKind
		target        string
		dnsServer     string
		pin, configID []byte
		want          string
	}{
		{"pin_https", heronv1.ProbeKind_PROBE_KIND_HTTP, "https://example.com/", "", pin32, nil, ""},
		// url.Parse 把 scheme 转成小写：大写的 https 同样可钉，与 IsHTTPSTarget 及 agent 取证书的判据一致。
		{"pin_https_uppercase_scheme", heronv1.ProbeKind_PROBE_KIND_HTTP, "HTTPS://example.com/", "", pin32, nil, ""},
		{"pin_http", heronv1.ProbeKind_PROBE_KIND_HTTP, "http://example.com/", "", pin32, nil, "only applies to an HTTP task with an https target"},
		// scheme 为 https 时 pin 检查放行，主机缺失由按种类分支的 URL 检查兜住。
		{"pin_https_no_host", heronv1.ProbeKind_PROBE_KIND_HTTP, "https:///path", "", pin32, nil, "have a host"},
		{"pin_icmp", heronv1.ProbeKind_PROBE_KIND_ICMP, "127.0.0.1", "", pin32, nil, "only applies to an HTTP task"},
		{"pin_tcp", heronv1.ProbeKind_PROBE_KIND_TCP, "example.com:443", "", pin32, nil, "only applies to an HTTP task"},
		{"pin_dns", heronv1.ProbeKind_PROBE_KIND_DNS, "example.com", "1.1.1.1:53", pin32, nil, "only applies to an HTTP task"},
		{"pin_short", heronv1.ProbeKind_PROBE_KIND_HTTP, "https://example.com/", "", pin32[:31], nil, "must be exactly 32 bytes"},
		{"pin_long", heronv1.ProbeKind_PROBE_KIND_HTTP, "https://example.com/", "", append(slices.Clone(pin32), 0), nil, "must be exactly 32 bytes"},
		{"config_id", heronv1.ProbeKind_PROBE_KIND_ICMP, "127.0.0.1", "", nil, cid16, ""},
		{"config_id_short", heronv1.ProbeKind_PROBE_KIND_ICMP, "127.0.0.1", "", nil, cid16[:15], "must be exactly 16 bytes"},
		{"config_id_long", heronv1.ProbeKind_PROBE_KIND_ICMP, "127.0.0.1", "", nil, append(slices.Clone(cid16), 0), "must be exactly 16 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := &heronv1.ProbeTask{Kind: tc.kind, Target: tc.target, IntervalS: 5, TimeoutMs: 1000, DnsServer: tc.dnsServer, CertSpkiSha256: tc.pin, ConfigId: tc.configID}
			err := CheckTask(task)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid task rejected: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v; want substring %q", err, tc.want)
			}
		})
	}
}
