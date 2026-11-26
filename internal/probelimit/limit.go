// Package probelimit 定义探测任务的硬限制，hub 保存前与 agent 应用前各自调用。
//
// 每个节点的探测速率有上界，hub 失守也不能把节点变成扫描器或反射器。
// 常量只在此处定义一次；两侧各有自己的入口与测试，缺一侧就只剩另一侧在守。
package probelimit

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

const (
	MinIntervalS    = 5
	MaxIntervalS    = 3600
	MinTimeoutMs    = 100
	MaxTimeoutMs    = 5000
	MaxTasksPerNode = 64
	// MaxTargetLen 是 ICMP/TCP/DNS 的 target 上限（主机名与 host:port）；HTTP 的 target 是绝对 URL，
	// 走 MaxHTTPTargetLen。上限按种类分，CheckTask 是 hub 与 agent 两侧共同的裁决点。
	MaxTargetLen = 253
	// MaxHTTPTargetLen 取 512 而不是更大：hub 下发的任何合法清单都必须装进 agent 的响应体上限
	// agentwire.MaxResponseBytes（64 KiB）——每节点至多 MaxTasksPerNode 个任务、每个 target
	// 至多这么长，加上其余字段的满载值必须小于它，由 internal/hub/ingest 的满载不变式测试机械地
	// 守着；该测试按登记表给出放宽任一上限时还剩多少余量。
	MaxHTTPTargetLen = 512
	// MaxDNSServerLen 是 dns_server 的上限：最长规范形式是 39 字节 IPv6 + 方括号 2 + 冒号 1 + 端口 5 位。
	// 上报响应的上界登记表引用这个常量，两处写同一个数迟早漂移。
	MaxDNSServerLen = 47
	// 一次上报须排空 MaxTasksPerNode/MinIntervalS 条每秒的满速产出，上报间隔为 TTL/3。
	// 条数和文本共同约束体积；超出 hub 读上限会被 ResourceExhausted 拒绝并回队，形成永久失败。
	MaxResultsPerReport = 1024
	MaxErrorMessageLen  = 128 // 字节；协议字符串仍须为合法 UTF-8。
	// CertSPKISHA256Len 是钉住指纹的字节数：叶证书 SubjectPublicKeyInfo 的 SHA-256。
	CertSPKISHA256Len = 32
	// ConfigIDLen 是任务配置身份的字节数：crypto/rand 生成的随机数，只比较相等。
	ConfigIDLen = 16
	// MinHTTPStatus 与 MaxHTTPStatus 是精确状态码判据可取的范围：RFC 9110 的 2xx–5xx。1xx 是中间响应，
	// net/http 自行消化 101 以外的 1xx、探测拿到的永远是其后的最终响应，探测又不发 Upgrade 请求，1xx 精确码
	// 永远匹配不上——收下它就是一个注定全丢包、却看不出原因的任务。web 的 HTTP_STATUS_RANGE 与这里同值。
	MinHTTPStatus = 200
	MaxHTTPStatus = 599
	// MaxResultAge 是结果的迟到预算：agent 取走时丢弃更老的，hub 拒收更老的。两侧必须是同一个数，
	// 否则一边收下的结果另一边已经丢掉，或反过来把仍有效的结果丢掉。
	MaxResultAge = 120 * time.Second
)

// 超时上限不超过最短间隔，单个任务的探测不会因超时预算而重叠；放宽边界必须重新审视此关系。
const _ = uint(MinIntervalS*1000 - MaxTimeoutMs)

// CheckTask 校验一个任务的字段。错误文本说清字段、约束与期望取值——agent 手上只有这个字符串。
func CheckTask(t *heronv1.ProbeTask) error {
	if t == nil {
		return errors.New("task: required")
	}
	if t.GetIntervalS() < MinIntervalS || t.GetIntervalS() > MaxIntervalS {
		return fmt.Errorf("interval_s must be between %d and %d; got %d", MinIntervalS, MaxIntervalS, t.GetIntervalS())
	}
	if t.GetTimeoutMs() < MinTimeoutMs || t.GetTimeoutMs() > MaxTimeoutMs {
		return fmt.Errorf("timeout_ms must be between %d and %d; got %d", MinTimeoutMs, MaxTimeoutMs, t.GetTimeoutMs())
	}
	// pin 与身份的检查放在按种类分支之前：DNS 分支提前 return，晚于分支的检查会被它跳过。
	if pin := t.GetCertSpkiSha256(); len(pin) > 0 {
		if len(pin) != CertSPKISHA256Len {
			return fmt.Errorf("cert_spki_sha256 must be exactly %d bytes when set; got %d", CertSPKISHA256Len, len(pin))
		}
		// 钉指纹只对 https 目标有意义：http 没有证书可钉，其他种类同样没有；
		// 静默忽略会让调用方以为钉住了而实际上按默认校验执行，方向是放宽，必须拒绝。
		if t.GetKind() != heronv1.ProbeKind_PROBE_KIND_HTTP {
			return fmt.Errorf("cert_spki_sha256 only applies to an HTTP task with an https target; got kind %s", t.GetKind())
		}
		if !IsHTTPSTarget(t.GetKind(), t.GetTarget()) {
			return fmt.Errorf("cert_spki_sha256 only applies to an HTTP task with an https target; got %q", t.GetTarget())
		}
	}
	if cid := t.GetConfigId(); len(cid) > 0 && len(cid) != ConfigIDLen {
		return fmt.Errorf("config_id must be exactly %d bytes when set; got %d", ConfigIDLen, len(cid))
	}
	// 地址族与记录类型的取值与适用种类同样放在分支之前，理由同上。未定义的枚举值不能按缺省执行：
	// 较新的 hub 发来的新取值若被当成自动或 A，任务就在调用方不知情时测了另一件事。
	switch t.GetIpFamily() {
	case heronv1.ProbeIpFamily_PROBE_IP_FAMILY_UNSPECIFIED, heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV4, heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV6:
	default:
		return fmt.Errorf("ip_family must be PROBE_IP_FAMILY_UNSPECIFIED, PROBE_IP_FAMILY_IPV4 or PROBE_IP_FAMILY_IPV6; got %s", t.GetIpFamily())
	}
	switch t.GetDnsRecordType() {
	case heronv1.DnsRecordType_DNS_RECORD_TYPE_UNSPECIFIED, heronv1.DnsRecordType_DNS_RECORD_TYPE_AAAA:
	default:
		return fmt.Errorf("dns_record_type must be DNS_RECORD_TYPE_UNSPECIFIED or DNS_RECORD_TYPE_AAAA; got %s", t.GetDnsRecordType())
	}
	// 状态码判据同样放在分支之前。未定义的策略不能按缺省执行，理由与地址族相同；精确码以外的策略带着状态码、
	// 或非 HTTP 任务带着判据，都是写了不生效的配置，静默忽略会让调用方以为任务在按它判定。
	switch t.GetHttpStatusPolicy() {
	case heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_UNSPECIFIED, heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_SUCCESS, heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_EXACT:
	default:
		return fmt.Errorf("http_status_policy must be HTTP_STATUS_POLICY_UNSPECIFIED, HTTP_STATUS_POLICY_SUCCESS or HTTP_STATUS_POLICY_EXACT; got %s", t.GetHttpStatusPolicy())
	}
	if t.GetKind() != heronv1.ProbeKind_PROBE_KIND_HTTP && t.GetHttpStatusPolicy() != heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_UNSPECIFIED {
		return fmt.Errorf("http_status_policy only applies to an HTTP task; got %s", t.GetHttpStatusPolicy())
	}
	if t.GetHttpStatusPolicy() == heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_EXACT {
		if s := t.GetHttpExpectedStatus(); s < MinHTTPStatus || s > MaxHTTPStatus {
			return fmt.Errorf("http_expected_status must be between %d and %d with HTTP_STATUS_POLICY_EXACT (a 1xx is never the final response); got %d", MinHTTPStatus, MaxHTTPStatus, s)
		}
	} else if s := t.GetHttpExpectedStatus(); s != 0 {
		return fmt.Errorf("http_expected_status only applies with HTTP_STATUS_POLICY_EXACT; got %d", s)
	}
	// DNS 任务连向解析器的地址族由 dns_server 字面量唯一决定，查询的记录类型另由 dns_record_type 表达；
	// 两种语义混进同一字段就分不清"经 IPv6 连解析器"与"查 AAAA"，所以 DNS 任务带 ip_family 即拒绝。其余种类没有
	// 记录类型可查，带 dns_record_type 同样拒绝而不是静默忽略——忽略会让调用方以为它生效了。
	if t.GetKind() == heronv1.ProbeKind_PROBE_KIND_DNS {
		if t.GetIpFamily() != heronv1.ProbeIpFamily_PROBE_IP_FAMILY_UNSPECIFIED {
			return fmt.Errorf("ip_family does not apply to a DNS task; dns_server decides the resolver's family and dns_record_type the queried record; got %s", t.GetIpFamily())
		}
	} else if t.GetDnsRecordType() != heronv1.DnsRecordType_DNS_RECORD_TYPE_UNSPECIFIED {
		return fmt.Errorf("dns_record_type only applies to a DNS task; got %s", t.GetDnsRecordType())
	}
	switch t.GetKind() {
	case heronv1.ProbeKind_PROBE_KIND_ICMP:
		if err := checkTargetLen(t.GetTarget(), MaxTargetLen); err != nil {
			return err
		}
		if !validHost(t.GetTarget()) {
			return fmt.Errorf("target for an ICMP task must be an IP address or a host name; got %q", t.GetTarget())
		}
		if err := checkLiteralFamily(t.GetTarget(), t.GetIpFamily()); err != nil {
			return err
		}
	case heronv1.ProbeKind_PROBE_KIND_TCP:
		if err := checkTargetLen(t.GetTarget(), MaxTargetLen); err != nil {
			return err
		}
		host, port, err := net.SplitHostPort(t.GetTarget())
		if err != nil {
			return fmt.Errorf("target for a TCP task must be host:port; got %q", t.GetTarget())
		}
		if !validHost(host) {
			return fmt.Errorf("target host for a TCP task must be an IP address or a host name; got %q", host)
		}
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return fmt.Errorf("target port must be between 1 and 65535; got %q", port)
		}
		if canonical := net.JoinHostPort(host, strconv.Itoa(p)); canonical != t.GetTarget() {
			return fmt.Errorf("target for a TCP task must use canonical host:port form; got %q; want %q", t.GetTarget(), canonical)
		}
		if err := checkLiteralFamily(host, t.GetIpFamily()); err != nil {
			return err
		}
	case heronv1.ProbeKind_PROBE_KIND_HTTP:
		if err := checkTargetLen(t.GetTarget(), MaxHTTPTargetLen); err != nil {
			return err
		}
		if err := checkHTTPURL(t.GetTarget()); err != nil {
			return err
		}
		// checkHTTPURL 已确认可解析且有主机；Hostname 去掉 IPv6 字面量的方括号，与 agent 取连接地址的写法相同。
		u, _ := url.Parse(t.GetTarget())
		if err := checkLiteralFamily(u.Hostname(), t.GetIpFamily()); err != nil {
			return err
		}
	case heronv1.ProbeKind_PROBE_KIND_DNS:
		if err := checkTargetLen(t.GetTarget(), MaxTargetLen); err != nil {
			return err
		}
		if !validHost(t.GetTarget()) {
			return fmt.Errorf("target for a DNS task must be a DNS name; got %q", t.GetTarget())
		}
		// IP 字面量没有解析可言：任务配成 DNS 名才是探测解析路径，探测 IP 字面量量的是本机组包。
		if _, err := netip.ParseAddr(t.GetTarget()); err == nil {
			return fmt.Errorf("target for a DNS task must be a DNS name, not an IP address; got %q", t.GetTarget())
		}
		return checkDNSServer(t.GetDnsServer())
	default:
		return fmt.Errorf("kind must be PROBE_KIND_ICMP, PROBE_KIND_TCP, PROBE_KIND_HTTP or PROBE_KIND_DNS; got %s", t.GetKind())
	}
	// dns_server 只对 DNS 任务有意义：其他种类携带即拒绝，静默忽略会让调用方以为它生效了。
	if t.GetDnsServer() != "" {
		return fmt.Errorf("dns_server only applies to a DNS task; got %q", t.GetDnsServer())
	}
	return nil
}

func checkTargetLen(target string, maxLen int) error {
	if len(target) > maxLen {
		return fmt.Errorf("target must be at most %d bytes; got %d", maxLen, len(target))
	}
	return nil
}

// IsHTTPSTarget 判断任务是不是 https 目标的 HTTP 任务；钉指纹、证书观测与证书到期规则都只对这类任务成立。
// 判据是 url.Parse 解析出的 scheme（解析时已转成小写）等于 https，与 agent 发请求、取证书时依据的 scheme 相同
// （agent/prober/http.go）。hub 的各个读者都调用这一个函数，不另用 target 的字符串前缀判断："HTTPS://" 这类
// 目标通过准入、agent 按 https 执行，前缀判据却认定它不是 https，同一个任务在两侧得到相反的结论。
func IsHTTPSTarget(kind heronv1.ProbeKind, target string) bool {
	if kind != heronv1.ProbeKind_PROBE_KIND_HTTP {
		return false
	}
	u, err := url.Parse(target)
	return err == nil && u.Scheme == "https"
}

// HTTP 的 target 是绝对 http(s) URL：有主机、不含用户信息、不含片段；端口若给出在 1–65535。
// 不跟随重定向由执行侧保证，这里只管形状。
func checkHTTPURL(target string) error {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("target for an HTTP task must be an absolute http or https URL; got %q", target)
	}
	if u.Host == "" {
		return fmt.Errorf("target for an HTTP task must have a host; got %q", target)
	}
	if u.User != nil {
		return fmt.Errorf("target for an HTTP task must not contain user info; got %q", target)
	}
	if u.Fragment != "" {
		return fmt.Errorf("target for an HTTP task must not contain a fragment; got %q", target)
	}
	if port := u.Port(); port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return fmt.Errorf("target port must be between 1 and 65535; got %q", port)
		}
	}
	return nil
}

// dns_server 是解析器的 ip:port 规范形式，只允许 IP 字面量（协议注释给了原因）；非规范写法
// 看不出作者要的值（前导零、未压缩的 IPv6），按配置错误拒绝并给出规范形式。
func checkDNSServer(s string) error {
	if len(s) > MaxDNSServerLen {
		return fmt.Errorf("dns_server must be at most %d bytes; got %d", MaxDNSServerLen, len(s))
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("dns_server must be ip:port; got %q", s)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("dns_server must use an IP literal, not a name; got %q", host)
	}
	// zone 是单机接口标识，不能随任务下发到多个节点。
	if addr.Zone() != "" {
		return fmt.Errorf("dns_server must not have a zone; got %q", host)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("dns_server port must be between 1 and 65535; got %q", port)
	}
	if canonical := net.JoinHostPort(addr.String(), strconv.Itoa(p)); canonical != s {
		return fmt.Errorf("dns_server must use canonical ip:port form; got %q; want %q", s, canonical)
	}
	return nil
}

// checkLiteralFamily 拒绝与目标 IP 字面量不同族的显式地址族：这样的任务在任何节点上都只能报错，保存时就该指出。
// 判族前先还原 IPv4 映射地址，与 agent 发包前的还原（prober 的 resolve）一致，否则映射地址在两侧被判成不同的族。
// 名字与自动地址族不受此约束：名字解析成什么由节点所在网络决定。
func checkLiteralFamily(host string, family heronv1.ProbeIpFamily) error {
	addr, err := netip.ParseAddr(host)
	if err != nil || family == heronv1.ProbeIpFamily_PROBE_IP_FAMILY_UNSPECIFIED {
		return nil
	}
	is4 := addr.Unmap().Is4()
	if is4 != (family == heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV4) {
		return fmt.Errorf("ip_family %s conflicts with target address %s", family, host)
	}
	return nil
}

// validHost 接受无 zone 的 IP 字面量或 DNS 名：标签由字母、数字、连字符组成，不以连字符开头或结尾。
// 不做解析：hub 不替 agent 决定名字在 agent 所在网络里解析成什么。
func validHost(h string) bool {
	if h == "" {
		return false
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		// zone 是单机接口标识，不能在分配到多个节点的任务中共享；ParseAddr 不替此处约束 zone 字符。
		return addr.Zone() == ""
	}
	for _, label := range strings.Split(strings.TrimSuffix(h, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
