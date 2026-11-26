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

// 探测地址族只对 ICMP、TCP、HTTP 生效，DNS 用自己的记录类型；目标是 IP 字面量时显式地址族必须与之同族。
// hub 保存与 agent 应用都调用 CheckTask，所以这里的每个拒绝在两侧一致。
func TestCheckTaskFamilyAndRecordType(t *testing.T) {
	const (
		auto = heronv1.ProbeIpFamily_PROBE_IP_FAMILY_UNSPECIFIED
		v4   = heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV4
		v6   = heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV6
		a    = heronv1.DnsRecordType_DNS_RECORD_TYPE_UNSPECIFIED
		aaaa = heronv1.DnsRecordType_DNS_RECORD_TYPE_AAAA
	)
	icmp, tcp, http, dns := heronv1.ProbeKind_PROBE_KIND_ICMP, heronv1.ProbeKind_PROBE_KIND_TCP, heronv1.ProbeKind_PROBE_KIND_HTTP, heronv1.ProbeKind_PROBE_KIND_DNS
	for _, tc := range []struct {
		name      string
		kind      heronv1.ProbeKind
		target    string
		dnsServer string
		family    heronv1.ProbeIpFamily
		record    heronv1.DnsRecordType
		want      string
	}{
		{"icmp_name_v6", icmp, "example.com", "", v6, a, ""},
		{"icmp_name_v4", icmp, "example.com", "", v4, a, ""},
		{"icmp_literal_auto", icmp, "2001:db8::1", "", auto, a, ""},
		{"icmp_literal_same_family", icmp, "2001:db8::1", "", v6, a, ""},
		{"icmp_v6_literal_v4_family", icmp, "2001:db8::1", "", v4, a, "ip_family PROBE_IP_FAMILY_IPV4 conflicts with target address 2001:db8::1"},
		{"icmp_v4_literal_v6_family", icmp, "192.0.2.1", "", v6, a, "ip_family PROBE_IP_FAMILY_IPV6 conflicts with target address 192.0.2.1"},
		// 映射地址在 agent 侧还原成 IPv4 发包，所以按 IPv4 判族。
		{"icmp_mapped_literal_v6_family", icmp, "::ffff:192.0.2.1", "", v6, a, "conflicts with target address"},
		{"icmp_mapped_literal_v4_family", icmp, "::ffff:192.0.2.1", "", v4, a, ""},
		{"tcp_v6_literal_v4_family", tcp, "[2001:db8::1]:443", "", v4, a, "conflicts with target address 2001:db8::1"},
		{"tcp_v4_literal_v4_family", tcp, "192.0.2.1:443", "", v4, a, ""},
		{"http_v4_literal_v6_family", http, "http://192.0.2.1/x", "", v6, a, "conflicts with target address 192.0.2.1"},
		{"http_v6_literal_v6_family", http, "https://[2001:db8::1]:8443/", "", v6, a, ""},
		{"http_name_v6", http, "https://example.com/", "", v6, a, ""},
		{"family_undefined", icmp, "example.com", "", heronv1.ProbeIpFamily(3), a, "ip_family must be PROBE_IP_FAMILY_UNSPECIFIED, PROBE_IP_FAMILY_IPV4 or PROBE_IP_FAMILY_IPV6"},
		{"dns_aaaa", dns, "example.com", "1.1.1.1:53", auto, aaaa, ""},
		{"dns_aaaa_via_v6_resolver", dns, "example.com", "[2606:4700:4700::1111]:53", auto, aaaa, ""},
		{"dns_a_via_v6_resolver", dns, "example.com", "[2606:4700:4700::1111]:53", auto, a, ""},
		{"dns_family", dns, "example.com", "1.1.1.1:53", v4, a, "ip_family does not apply to a DNS task"},
		{"dns_family_v6", dns, "example.com", "1.1.1.1:53", v6, aaaa, "ip_family does not apply to a DNS task"},
		{"record_undefined", dns, "example.com", "1.1.1.1:53", auto, heronv1.DnsRecordType(2), "dns_record_type must be DNS_RECORD_TYPE_UNSPECIFIED or DNS_RECORD_TYPE_AAAA"},
		{"icmp_record", icmp, "example.com", "", auto, aaaa, "dns_record_type only applies to a DNS task"},
		{"tcp_record", tcp, "example.com:80", "", auto, aaaa, "dns_record_type only applies to a DNS task"},
		{"http_record", http, "http://example.com/", "", auto, aaaa, "dns_record_type only applies to a DNS task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckTask(&heronv1.ProbeTask{Kind: tc.kind, Target: tc.target, IntervalS: 60, TimeoutMs: 1000, DnsServer: tc.dnsServer, IpFamily: tc.family, DnsRecordType: tc.record})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("CheckTask = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckTask = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

// 状态码判据只对 HTTP 任务生效：缺省（< 400）、严格 2xx 或单个精确状态码。精确码以外的策略不得带状态码，
// 精确码必须在 200–599：net/http 自行消化 101 以外的 1xx 中间响应，探测拿到的永远是最终响应，1xx 精确码
// 永远匹配不上。其余种类带任何非缺省值都拒绝。hub 保存与 agent 应用都调用 CheckTask。
func TestCheckTaskHTTPStatusPolicy(t *testing.T) {
	const (
		below400 = heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_UNSPECIFIED
		success  = heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_SUCCESS
		exact    = heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_EXACT
	)
	icmp, tcp, http, dns := heronv1.ProbeKind_PROBE_KIND_ICMP, heronv1.ProbeKind_PROBE_KIND_TCP, heronv1.ProbeKind_PROBE_KIND_HTTP, heronv1.ProbeKind_PROBE_KIND_DNS
	targets := map[heronv1.ProbeKind][2]string{icmp: {"example.com", ""}, tcp: {"example.com:443", ""}, http: {"https://example.com/", ""}, dns: {"example.com", "1.1.1.1:53"}}
	for _, tc := range []struct {
		name   string
		kind   heronv1.ProbeKind
		policy heronv1.HttpStatusPolicy
		status uint32
		want   string
	}{
		{"http_default", http, below400, 0, ""},
		{"http_success", http, success, 0, ""},
		{"http_exact_lowest", http, exact, 200, ""},
		{"http_exact_highest", http, exact, 599, ""},
		{"http_exact_204", http, exact, 204, ""},
		{"http_exact_missing", http, exact, 0, "http_expected_status must be between 200 and 599 with HTTP_STATUS_POLICY_EXACT (a 1xx is never the final response); got 0"},
		{"http_exact_interim", http, exact, 100, "http_expected_status must be between 200 and 599"},
		{"http_exact_below", http, exact, 199, "http_expected_status must be between 200 and 599 with HTTP_STATUS_POLICY_EXACT (a 1xx is never the final response); got 199"},
		{"http_exact_above", http, exact, 600, "http_expected_status must be between 200 and 599"},
		{"http_success_with_status", http, success, 200, "http_expected_status only applies with HTTP_STATUS_POLICY_EXACT; got 200"},
		{"http_default_with_status", http, below400, 302, "http_expected_status only applies with HTTP_STATUS_POLICY_EXACT; got 302"},
		{"policy_undefined", http, heronv1.HttpStatusPolicy(3), 0, "http_status_policy must be HTTP_STATUS_POLICY_UNSPECIFIED, HTTP_STATUS_POLICY_SUCCESS or HTTP_STATUS_POLICY_EXACT"},
		{"icmp_policy", icmp, success, 0, "http_status_policy only applies to an HTTP task"},
		{"tcp_policy", tcp, exact, 200, "http_status_policy only applies to an HTTP task"},
		{"dns_policy", dns, success, 0, "http_status_policy only applies to an HTTP task"},
		{"dns_status_only", dns, below400, 200, "http_expected_status only applies"},
		{"tcp_status_only", tcp, below400, 200, "http_expected_status only applies"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := targets[tc.kind]
			err := CheckTask(&heronv1.ProbeTask{Kind: tc.kind, Target: target[0], DnsServer: target[1], IntervalS: 60, TimeoutMs: 1000, HttpStatusPolicy: tc.policy, HttpExpectedStatus: tc.status})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("CheckTask = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckTask = %v, want error containing %q", err, tc.want)
			}
		})
	}
}
