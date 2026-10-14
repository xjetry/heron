package prober

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
)

// CertReportInterval 是同一配置身份两次携带证书观测的最小间隔：证书到期日按天变化，
// 每次探测都带是无意义的重复字节；一小时内能看到更换后的新到期日已经足够。测试引用它。
const CertReportInterval = time.Hour

// HTTP 测量一次 GET 从拨号到收到响应头的耗时；解析与整个请求（连接、TLS、等响应头）共用预算。
// URL 非法、解析失败与本地策略拒绝是 error；状态 ≥400 与 TLS 握手失败（含证书错误）计入丢包，
// 连接失败经 classify 区分。不跟随重定向（3xx 本身就是答案）、不复用连接、不读正文：
// 探的是"这个 URL 能不能给出 <400 的响应头"，不是内容。
//
// 任务钉了 cert_spki_sha256 时跳过默认的链与主机名校验（InsecureSkipVerify），改在
// VerifyConnection 里只比对叶证书公钥指纹与有效期：钉住即把信任锚定在这份公钥上，
// 同钥续签不受影响，链与名字不再提供额外约束。证书相关的丢包（默认校验失败、指纹不符、
// 不在有效期）带回对方叶证书作为信任候选，与成功结果顺带带回的到期时刻共用每小时一次的
// 频率上限，两个时钟各自独立、按 (task_id, config_id) 记。
//
// Probe 是指针接收者：证书携带的频率上限记录内嵌在 HTTP 里（certMu 保护的三张表），任何构造方式
// （零值 &HTTP{...} 即可用，map 在锁内按需建立）上限都成立，不需要装配侧为字面量构造打补丁。
type HTTP struct {
	Clock   clock.Clock
	Targets Targets
	Version string // User-Agent 用 heron-agent/<Version>，与 heron-agent version 命令同源。
	// TLSClientConfig 只在测试里注入（如限死 TLS 版本构造对端 alert 的握手失败）；生产为 nil，用默认配置。
	// 任务钉了指纹时以它为底克隆一份再打开 InsecureSkipVerify 与 VerifyConnection，注入的字段仍生效。
	TLSClientConfig *tls.Config
	// certOK 与 certCand 是两个独立的携带时钟（成功证书 / 候选），键是 (task_id, config_id)：
	// 任务内容一变（config_id 变）即另一份配置，限频从首次观测重新开始。
	// 它只影响是否重复携带，不影响任何测量值，因此不属于速率基线，不参与休眠检测的 ResetRates/Clear。
	// current 记录 pruneTasks 告知的每任务当前身份：被取消后才返回的旧探测据此识别出自己
	// 已属旧身份，不重建已删的限频键。没有身份记录（引擎未经 Scheduler 使用）按当前处理。
	certMu   sync.Mutex
	certOK   map[certKey]time.Duration
	certCand map[certKey]time.Duration
	current  map[uint64]string
}

// certKey 标识一次证书观测所属的配置身份：cfg 是 ProbeTask.config_id 原样字节（空 = 身份未知）。
type certKey struct {
	task uint64
	cfg  string
}

// pinError 是钉住校验失败的专用错误：指纹不符或证书不在有效期。classify 把它归为丢包，
// presentedFrom 从它取回对方出示的叶证书；握手由返回错误中止，不会走到读响应。
type pinError struct {
	reason   heronv1.PresentedReason // 只取 PIN_MISMATCH / OUTSIDE_VALIDITY
	spki     [32]byte
	notAfter time.Time
}

func (e *pinError) Error() string {
	switch e.reason {
	case heronv1.PresentedReason_PRESENTED_REASON_PIN_MISMATCH:
		return fmt.Sprintf("pinned certificate public key mismatch: presented sha256/%x", e.spki)
	case heronv1.PresentedReason_PRESENTED_REASON_OUTSIDE_VALIDITY:
		return fmt.Sprintf("pinned certificate not valid at probe time (expires %s)", e.notAfter.UTC())
	}
	return fmt.Sprintf("pinned certificate rejected (reason %s)", e.reason)
}

func (p *HTTP) Probe(ctx context.Context, t *heronv1.ProbeTask) Outcome {
	timeout := time.Duration(t.GetTimeoutMs()) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u, err := url.Parse(t.GetTarget())
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return Outcome{Err: fmt.Sprintf("target %q is not an absolute http or https URL", t.GetTarget())}
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	ip, err := p.Targets.Resolve(ctx, u.Hostname(), true, true)
	if err != nil {
		return Outcome{Err: err.Error()}
	}
	start := p.Clock.Mono()
	// 只连解析出的那个地址：DialContext 忽略传入地址，SNI 与 Host 仍是 URL 里的名字。
	transport := &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   p.tlsConfig(t.GetCertSpkiSha256()),
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Outcome{Err: err.Error()}
	}
	req.Header.Set("User-Agent", "heron-agent/"+p.Version)
	resp, err := client.Do(req)
	if err != nil {
		out := classify(err)
		// 证书相关的丢包带回对方出示的叶证书作信任候选，每 (任务, 身份) 每小时至多一次；
		// 握手在出示证书之前失败（连接被拒、对端不讲 TLS）时没有候选可带。
		if out.Timeout {
			if presented := presentedFrom(err); presented != nil && p.reportPresented(t.GetId(), t.GetConfigId(), p.Clock.Mono()) {
				out.Presented = presented
			}
		}
		return out
	}
	now := p.Clock.Mono()
	elapsed := now - start
	// 拿到响应头即完成测量，不读正文。
	resp.Body.Close()
	// CheckTask 把任务超时限制在 hub 接受的 RTT 上限内；测得耗时超过任务预算时只回报超时。
	if elapsed > timeout {
		return Outcome{Timeout: true}
	}
	if resp.StatusCode >= 400 {
		return Outcome{Timeout: true}
	}
	out := Outcome{RttUs: uint32(elapsed / time.Microsecond)}
	// HTTPS 握手成功（能走到这里即已完成握手）时顺带带回链首枚证书的到期时刻，
	// 每 (任务, 身份) 至多每小时一次（reportCert 承载频率上限）；状态 ≥400 已在上面折返，不携带。
	if u.Scheme == "https" && resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 && p.reportCert(t.GetId(), t.GetConfigId(), now) {
		out.CertNotAfter = resp.TLS.PeerCertificates[0].NotAfter.Unix()
	}
	return out
}

// tlsConfig 返回钉住校验用的 TLS 配置；未钉时原样返回注入配置（生产为 nil）。
func (p *HTTP) tlsConfig(pin []byte) *tls.Config {
	if len(pin) == 0 {
		return p.TLSClientConfig
	}
	cfg := &tls.Config{}
	if p.TLSClientConfig != nil {
		cfg = p.TLSClientConfig.Clone()
	}
	// 钉住后默认的链与主机名校验必须关掉，否则自签目标在比对指纹之前就被拒；
	// 安全边界由 VerifyConnection 里的指纹比对承载，不是被删除。
	cfg.InsecureSkipVerify = true
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		return verifyPin(pin, p.Clock.Now(), cs)
	}
	return cfg
}

// verifyPin 是钉住校验的全部内容：叶证书公钥指纹相符，且探测时刻落在证书有效期内。
// 不查证书链与主机名。PeerCertificates 在 InsecureSkipVerify 下同样填充（crypto/tls 实测，
// Go 1.27.1），[0] 是叶证书。
func verifyPin(pin []byte, now time.Time, cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("tls: server presented no certificate")
	}
	leaf := cs.PeerCertificates[0]
	spki := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	if !bytes.Equal(spki[:], pin) {
		return &pinError{reason: heronv1.PresentedReason_PRESENTED_REASON_PIN_MISMATCH, spki: spki, notAfter: leaf.NotAfter}
	}
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return &pinError{reason: heronv1.PresentedReason_PRESENTED_REASON_OUTSIDE_VALIDITY, spki: spki, notAfter: leaf.NotAfter}
	}
	return nil
}

// presentedFrom 从握手失败的原因链取出对方出示的叶证书观测；取不到（失败发生在出示证书之前）返回 nil。
// 钉住校验失败走自己的 pinError；默认校验失败从 *tls.CertificateVerificationError 的
// UnverifiedCertificates[0] 取叶证书，不另建连接。
func presentedFrom(err error) *PresentedCert {
	var pinErr *pinError
	if errors.As(err, &pinErr) {
		return &PresentedCert{SPKI: pinErr.spki, NotAfterS: pinErr.notAfter.Unix(), Reason: pinErr.reason}
	}
	var certVerify *tls.CertificateVerificationError
	if errors.As(err, &certVerify) && len(certVerify.UnverifiedCertificates) > 0 {
		leaf := certVerify.UnverifiedCertificates[0]
		return &PresentedCert{SPKI: sha256.Sum256(leaf.RawSubjectPublicKeyInfo), NotAfterS: leaf.NotAfter.Unix(), Reason: heronv1.PresentedReason_PRESENTED_REASON_CA_VERIFY_FAILED}
	}
	return nil
}

// reportCert 判定本次成功探测是否携带证书到期时刻；判定携带时把 now 记为该键的上次携带时刻。
func (p *HTTP) reportCert(id uint64, configID []byte, now time.Duration) bool {
	p.certMu.Lock()
	defer p.certMu.Unlock()
	if p.staleLocked(id, configID) {
		return false
	}
	if p.certOK == nil {
		p.certOK = map[certKey]time.Duration{}
	}
	return reportOnce(p.certOK, certKey{id, string(configID)}, now)
}

// reportPresented 是候选时钟上的同一个判定。
func (p *HTTP) reportPresented(id uint64, configID []byte, now time.Duration) bool {
	p.certMu.Lock()
	defer p.certMu.Unlock()
	if p.staleLocked(id, configID) {
		return false
	}
	if p.certCand == nil {
		p.certCand = map[certKey]time.Duration{}
	}
	return reportOnce(p.certCand, certKey{id, string(configID)}, now)
}

// staleLocked 判定写者是否属于已被换下或从未登记的身份：登记簿存在时（pruneTasks 已被调用过，
// 即引擎由调度器管理）未登记即过期——Apply 先登记身份再启动探测，登记簿查不到的探测一定来自
// 已被换下或已删除的旧任务，不得为它携带观测或重建键。登记簿不存在（引擎脱离调度器使用，
// 没有清单概念）时维持按当前处理。调用者持 certMu。
func (p *HTTP) staleLocked(id uint64, configID []byte) bool {
	if p.current == nil {
		return false
	}
	cur, ok := p.current[id]
	return !ok || cur != string(configID)
}

// reportOnce 承载频率上限 CertReportInterval：一小时内能看到更换后的新证书已经足够，每次都带是重复字节。
// 调用者持锁。
func reportOnce(m map[certKey]time.Duration, key certKey, now time.Duration) bool {
	if last, ok := m[key]; ok && now-last < CertReportInterval {
		return false
	}
	m[key] = now
	return true
}

// pruneTasks 让限频状态只剩清单内任务当前身份的记录，由 Scheduler.Apply 在任务集更新后、
// 启动新探测之前调用。清理以两张携带表自己的键为准：任务不在清单里，或键的身份与当前登记的
// 不同，都删——登记簿只服务探测侧的过期判定，清理不经过它，登记簿缺失也不漏清。
// 被拒任务不在 alive 里，其记录一并清掉；任务消失再出现时按首次探测处理。
func (p *HTTP) pruneTasks(alive map[uint64]string) {
	p.certMu.Lock()
	defer p.certMu.Unlock()
	if p.current == nil {
		p.current = map[uint64]string{}
	}
	for key := range p.certOK {
		if cfg, ok := alive[key.task]; !ok || cfg != key.cfg {
			delete(p.certOK, key)
		}
	}
	for key := range p.certCand {
		if cfg, ok := alive[key.task]; !ok || cfg != key.cfg {
			delete(p.certCand, key)
		}
	}
	for id := range p.current {
		if _, ok := alive[id]; !ok {
			delete(p.current, id)
		}
	}
	for id, cfg := range alive {
		p.current[id] = cfg
	}
}
