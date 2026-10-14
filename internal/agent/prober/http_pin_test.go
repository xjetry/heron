package prober

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
)

// 本文件的功能矩阵只用在测试内生成的 CA 与叶证书，不读取任何外部证书或网络。

// pinTestCA 是一个测试内 CA，按给出的 key 与有效期签发叶证书。
type pinTestCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

func newPinTestCA(t *testing.T, notBefore, notAfter time.Time) *pinTestCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "pin test CA"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &pinTestCA{cert: cert, key: key, der: der}
}

// leaf 用给定 key 签发一张叶证书，返回解析后的证书与可直接给服务端的 tls.Certificate（链含 CA）。
func (ca *pinTestCA) leaf(t *testing.T, serial int64, key *ecdsa.PrivateKey, notBefore, notAfter time.Time) (*x509.Certificate, tls.Certificate) {
	t.Helper()
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "pin test leaf"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		DNSNames:     []string{"example.test"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return leaf, tls.Certificate{Certificate: [][]byte{der, ca.der}, PrivateKey: key, Leaf: leaf}
}

func pinTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// spki 是钉住与候选共用的指纹：叶证书 SubjectPublicKeyInfo 的 SHA-256。
func spki(cert *x509.Certificate) [32]byte { return sha256.Sum256(cert.RawSubjectPublicKeyInfo) }

// pinnedServer 起一个出示给定证书链的 TLS 服务，返回服务器与请求计数。
func pinnedServer(t *testing.T, cert tls.Certificate, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return pinnedServerWithConns(t, cert, hits, nil)
}

// conns 非 nil 时统计服务端看到的连接数（ConnState 的 StateNew 每接受一个连接计一次）。
func pinnedServerWithConns(t *testing.T, cert tls.Certificate, hits, conns *atomic.Int64) *httptest.Server {
	t.Helper()
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	if conns != nil {
		s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				conns.Add(1)
			}
		}
	}
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func pinnedTask(id uint64, url string, pin, configID []byte) *heronv1.ProbeTask {
	t := httpTask(url)
	t.Id = id
	t.CertSpkiSha256 = pin
	t.ConfigId = configID
	return t
}

// pinnedProber 用默认 TLS 配置的探测器：钉住与未钉两条路径都由任务字段决定，不注入测试配置。
func pinnedProber(clk clock.Clock, t *testing.T) *HTTP {
	return &HTTP{Clock: clk, Targets: loopbackTargets(t, nil), Version: "v"}
}

// --- crypto/tls 行为实测（Go 1.27.1，结论是对这个版本的断言，升级要重跑） ---

// verifyHaltError 是实测 VerifyConnection 中止握手的哨兵类型。
type verifyHaltError struct{ why string }

func (e *verifyHaltError) Error() string { return e.why }

// InsecureSkipVerify 下 VerifyConnection 收到已填充的 PeerCertificates（[0] 是叶证书），
// 返回错误中止握手，Do 的错误链上 errors.As 能取回这个专用错误。
func TestTLSVerifyConnectionContract(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer s.Close()
	var got []byte
	halt := &verifyHaltError{why: "halt"}
	client := s.Client()
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				t.Error("VerifyConnection called without PeerCertificates")
				return nil
			}
			got = cs.PeerCertificates[0].Raw
			return halt
		},
	}
	_, err := client.Get(s.URL)
	var back *verifyHaltError
	if !errors.As(err, &back) || back != halt {
		t.Fatalf("errors.As on Do error chain = %v, want the sentinel from VerifyConnection", err)
	}
	if len(got) == 0 || !bytes.Equal(got, s.Certificate().Raw) {
		t.Fatal("PeerCertificates[0] is not the server leaf certificate")
	}
}

// 默认校验失败时 errors.As 能取回 *tls.CertificateVerificationError，且
// UnverifiedCertificates[0] 是对方出示的叶证书——候选不必另建连接去取。
func TestTLSDefaultVerifyFailureContract(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer s.Close()
	// 不走 s.Client()：它信任了测试证书。默认配置的客户端面对自签证书必须失败。
	_, err := http.DefaultClient.Get(s.URL)
	var verifyErr *tls.CertificateVerificationError
	if !errors.As(err, &verifyErr) {
		t.Fatalf("default verification error chain has no *tls.CertificateVerificationError: %v", err)
	}
	if len(verifyErr.UnverifiedCertificates) == 0 || !bytes.Equal(verifyErr.UnverifiedCertificates[0].Raw, s.Certificate().Raw) {
		t.Fatal("UnverifiedCertificates[0] is not the presented leaf certificate")
	}
}

// 钉住的安全性前提：即使服务端出示的正是钉住的那份证书，只要它没有对应的私钥，握手签名
// 校验就过不去。InsecureSkipVerify 跳过的是链与主机名校验，握手完整性（CertificateVerify
// 用叶证书公钥验签）不跳过，所以钉住公钥指纹等于钉住了私钥持有者。
func TestTLSPinSecurityComesFromHandshakeSignature(t *testing.T) {
	// TLS 1.3 由 CertificateVerify、TLS 1.2 的 ECDHE 套件由 ServerKeyExchange 的签名证明私钥持有；
	// 两个版本都要钉住。对照组用同一条链配正确的私钥，证明失败只来自私钥不符，而不是别的握手差异。
	for _, tc := range []struct {
		name    string
		version uint16
	}{{"tls1.3", tls.VersionTLS13}, {"tls1.2", tls.VersionTLS12}} {
		t.Run(tc.name, func(t *testing.T) {
			clk := clock.NewFake(time.Unix(2000, 0))
			now := clk.Now()
			ca := newPinTestCA(t, now.Add(-time.Hour), now.Add(24*time.Hour))
			leaf, chain := ca.leaf(t, 2, pinTestKey(t), now.Add(-time.Minute), now.Add(time.Hour))
			pin := spki(leaf)
			serve := func(cert tls.Certificate, hits *atomic.Int64) *httptest.Server {
				srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
				srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tc.version, MaxVersion: tc.version}
				srv.StartTLS()
				t.Cleanup(srv.Close)
				return srv
			}

			var legitHits atomic.Int64
			legit := serve(chain, &legitHits)
			// 假时钟不前进，成功结果的 RttUs 为 0；成败看 Err 与 Timeout。
			if out := pinnedProber(clk, t).Probe(t.Context(), pinnedTask(1, legit.URL, pin[:], nil)); out.Err != "" || out.Timeout || legitHits.Load() != 1 {
				t.Fatalf("control with the matching private key = %+v, hits %d; want success", out, legitHits.Load())
			}

			// 服务端持有钉住叶证书的完整链，但私钥换成另一把：冒充者能复制证书，复制不了私钥。
			var imposterHits atomic.Int64
			imposter := serve(tls.Certificate{Certificate: chain.Certificate, PrivateKey: pinTestKey(t)}, &imposterHits)
			out := pinnedProber(clk, t).Probe(t.Context(), pinnedTask(1, imposter.URL, pin[:], nil))
			if (out.Err == "" && !out.Timeout) || imposterHits.Load() != 0 {
				t.Fatalf("probe against key-impersonating server = %+v, hits %d; want failure before any request", out, imposterHits.Load())
			}
			// 实测（Go 1.27.1）：两个版本都报 "tls: invalid signature by the server certificate: ECDSA verification failure"。
			if !strings.Contains(out.Err, "invalid signature by the server certificate") {
				t.Fatalf("outcome = %+v, want the handshake-signature failure surfaced", out)
			}
		})
	}
}

// --- 钉住后的校验 ---

// 钉住后只认公钥：同钥续签（新序列号与有效期、同一把 key）仍成功，成功结果照旧带 cert_not_after_s。
func TestHTTPPinSucceedsAcrossSameKeyRenewal(t *testing.T) {
	clk := clock.NewFake(time.Unix(2000, 0))
	now := clk.Now()
	ca := newPinTestCA(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	key := pinTestKey(t)
	leaf1, chain1 := ca.leaf(t, 2, key, now.Add(-time.Minute), now.Add(time.Hour))
	var hits1 atomic.Int64
	s1 := pinnedServer(t, chain1, &hits1)

	p := pinnedProber(clk, t)
	pin := spki(leaf1)
	task := pinnedTask(1, s1.URL, pin[:], nil)
	// 假钟在探测期间不推进，rtt 是 0；这里只钉成败与证书字段。
	out := p.Probe(t.Context(), task)
	if out.Err != "" || out.Timeout {
		t.Fatalf("pinned probe = %+v, want success", out)
	}
	if out.CertNotAfter != leaf1.NotAfter.Unix() {
		t.Fatalf("CertNotAfter = %d, want %d", out.CertNotAfter, leaf1.NotAfter.Unix())
	}
	// 续签：同一把 key，新的序列号与有效期。
	leaf2, chain2 := ca.leaf(t, 3, key, now.Add(-time.Minute), now.Add(2*time.Hour))
	var hits2 atomic.Int64
	s2 := pinnedServer(t, chain2, &hits2)
	clk.Advance(CertReportInterval) // 越过携带频率上限，第二次成功也带到期时刻。
	out = p.Probe(t.Context(), pinnedTask(2, s2.URL, pin[:], nil))
	if out.Err != "" || out.Timeout {
		t.Fatalf("renewed pinned probe = %+v, want success", out)
	}
	if out.CertNotAfter != leaf2.NotAfter.Unix() {
		t.Fatalf("CertNotAfter after renewal = %d, want %d", out.CertNotAfter, leaf2.NotAfter.Unix())
	}
}

// 换钥但 CA 合法的证书在钉住时是丢包并带回 PIN_MISMATCH 候选（指纹与到期日是对方出示的那份）。
func TestHTTPPinMismatchReturnsCandidate(t *testing.T) {
	clk := clock.NewFake(time.Unix(2000, 0))
	now := clk.Now()
	ca := newPinTestCA(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	pinned, _ := ca.leaf(t, 2, pinTestKey(t), now.Add(-time.Minute), now.Add(time.Hour))
	presented, presentedChain := ca.leaf(t, 3, pinTestKey(t), now.Add(-time.Minute), now.Add(3*time.Hour))
	var hits atomic.Int64
	s := pinnedServer(t, presentedChain, &hits)

	p := pinnedProber(clk, t)
	pin := spki(pinned)
	out := p.Probe(t.Context(), pinnedTask(1, s.URL, pin[:], nil))
	if !out.Timeout || out.Err != "" {
		t.Fatalf("pin mismatch = %+v, want timeout (丢包)", out)
	}
	if out.Presented == nil || out.Presented.Reason != heronv1.PresentedReason_PRESENTED_REASON_PIN_MISMATCH {
		t.Fatalf("Presented = %+v, want PIN_MISMATCH candidate", out.Presented)
	}
	if out.Presented.SPKI != spki(presented) || out.Presented.NotAfterS != presented.NotAfter.Unix() {
		t.Fatalf("candidate = %+v, want presented leaf spki %x notAfter %d", out.Presented, spki(presented), presented.NotAfter.Unix())
	}
	if hits.Load() != 0 {
		t.Fatalf("handler reached %d times; handshake must abort before the request", hits.Load())
	}
}

// 指纹相符但探测时刻不在证书有效期内（未生效、已过期）分别带回 OUTSIDE_VALIDITY。
func TestHTTPPinOutsideValidity(t *testing.T) {
	clk := clock.NewFake(time.Unix(2000, 0))
	now := clk.Now()
	ca := newPinTestCA(t, now.Add(-48*time.Hour), now.Add(48*time.Hour))
	for _, tc := range []struct {
		name                string
		notBefore, notAfter time.Time
	}{
		{"not_yet_valid", now.Add(time.Hour), now.Add(2 * time.Hour)},
		{"expired", now.Add(-2 * time.Hour), now.Add(-time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaf, chain := ca.leaf(t, 2, pinTestKey(t), tc.notBefore, tc.notAfter)
			var hits atomic.Int64
			s := pinnedServer(t, chain, &hits)
			p := pinnedProber(clk, t)
			pin := spki(leaf)
			out := p.Probe(t.Context(), pinnedTask(1, s.URL, pin[:], nil))
			if !out.Timeout || out.Err != "" {
				t.Fatalf("%s = %+v, want timeout", tc.name, out)
			}
			if out.Presented == nil || out.Presented.Reason != heronv1.PresentedReason_PRESENTED_REASON_OUTSIDE_VALIDITY {
				t.Fatalf("%s Presented = %+v, want OUTSIDE_VALIDITY candidate", tc.name, out.Presented)
			}
			if out.Presented.SPKI != spki(leaf) || out.Presented.NotAfterS != leaf.NotAfter.Unix() {
				t.Fatalf("%s candidate = %+v, want presented leaf", tc.name, out.Presented)
			}
		})
	}
}

// 钉住不改变地址策略：策略拒绝仍是 error 且在拨号之前，对端看不到任何连接。
func TestHTTPPinPolicyDeniedBeforeDial(t *testing.T) {
	clk := clock.NewFake(time.Unix(2000, 0))
	now := clk.Now()
	ca := newPinTestCA(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	leaf, chain := ca.leaf(t, 2, pinTestKey(t), now.Add(-time.Minute), now.Add(time.Hour))
	var hits atomic.Int64
	s := pinnedServer(t, chain, &hits)
	p := HTTP{Clock: clk, Targets: Targets{}, Version: "v"} // 零值策略拒绝回环。
	pin := spki(leaf)
	out := p.Probe(t.Context(), pinnedTask(1, s.URL, pin[:], nil))
	if out.Err == "" || out.Timeout || out.Presented != nil {
		t.Fatalf("policy denied = %+v, want error without timeout or candidate", out)
	}
	if hits.Load() != 0 {
		t.Fatalf("dial happened despite policy denial: %d requests", hits.Load())
	}
}

// 未钉的任务按默认校验：自签目标的握手失败带回 CA_VERIFY_FAILED 候选，取自失败链上的叶证书。
func TestHTTPCAVerifyFailureReturnsCandidate(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer s.Close()
	p := pinnedProber(clock.Real(), t)
	out := p.Probe(t.Context(), httpTask(s.URL))
	if !out.Timeout || out.Err != "" {
		t.Fatalf("untrusted self-signed = %+v, want timeout", out)
	}
	if out.Presented == nil || out.Presented.Reason != heronv1.PresentedReason_PRESENTED_REASON_CA_VERIFY_FAILED {
		t.Fatalf("Presented = %+v, want CA_VERIFY_FAILED candidate", out.Presented)
	}
	if out.Presented.SPKI != spki(s.Certificate()) || out.Presented.NotAfterS != s.Certificate().NotAfter.Unix() {
		t.Fatalf("candidate = %+v, want server leaf", out.Presented)
	}
}

// 服务端出示完整链（叶 + 未知 CA）时候选仍是叶证书：UnverifiedCertificates[0] 是叶，
// 不是链尾。面板信任的是目标本身的公钥，带上 CA 的指纹会让管理员钉住签发者。
func TestHTTPCAVerifyFailureCandidateIsLeafNotIssuer(t *testing.T) {
	// 证书有效期相对真实时钟：失败必须纯粹源于未知 CA，而不是撞上有效期。
	now := time.Now()
	ca := newPinTestCA(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	leaf, chain := ca.leaf(t, 2, pinTestKey(t), now.Add(-time.Minute), now.Add(time.Hour))
	var hits atomic.Int64
	s := pinnedServer(t, chain, &hits)
	p := pinnedProber(clock.Real(), t)
	out := p.Probe(t.Context(), httpTask(s.URL))
	if !out.Timeout || out.Presented == nil || out.Presented.Reason != heronv1.PresentedReason_PRESENTED_REASON_CA_VERIFY_FAILED {
		t.Fatalf("unknown-CA chain = %+v, want timeout with CA_VERIFY_FAILED candidate", out)
	}
	if out.Presented.SPKI != spki(leaf) || out.Presented.NotAfterS != leaf.NotAfter.Unix() {
		t.Fatalf("candidate = %+v, want leaf spki %x", out.Presented, spki(leaf))
	}
}

// --- 限频与身份 ---

// 候选限频按 (task_id, config_id)：同一身份一小时内只带一次；换了身份（任务内容变了）立即重新带回；
// 满一小时后也重新带回。
func TestHTTPPresentedRateLimitPerIdentity(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer s.Close()
	clk := clock.NewFake(time.Unix(2000, 0))
	p := pinnedProber(clk, t)
	cfgA := bytes.Repeat([]byte{1}, 16)
	cfgB := bytes.Repeat([]byte{2}, 16)

	out := p.Probe(t.Context(), pinnedTask(1, s.URL, nil, cfgA))
	if out.Presented == nil {
		t.Fatalf("first failure Presented = %+v, want candidate", out)
	}
	clk.Advance(time.Minute)
	if out := p.Probe(t.Context(), pinnedTask(1, s.URL, nil, cfgA)); out.Presented != nil {
		t.Fatalf("failure within interval carried candidate %+v, want suppressed", out.Presented)
	}
	// 改目标等任何内容变化都会产生新身份：限频从首次观测重新开始，立即重新带回。
	if out := p.Probe(t.Context(), pinnedTask(1, s.URL, nil, cfgB)); out.Presented == nil {
		t.Fatal("new config identity did not carry candidate")
	}
	// 新身份同样受限频约束。
	clk.Advance(time.Minute)
	if out := p.Probe(t.Context(), pinnedTask(1, s.URL, nil, cfgB)); out.Presented != nil {
		t.Fatalf("new identity within interval carried %+v, want suppressed", out.Presented)
	}
	clk.Advance(CertReportInterval)
	if out := p.Probe(t.Context(), pinnedTask(1, s.URL, nil, cfgB)); out.Presented == nil {
		t.Fatal("candidate not carried after CertReportInterval")
	}
}

// 成功证书与候选是两个独立时钟：同一 (task_id, config_id) 上先带成功证书不挡候选，反之亦然。
func TestHTTPCertClocksAreIndependent(t *testing.T) {
	clk := clock.NewFake(time.Unix(2000, 0))
	now := clk.Now()
	ca := newPinTestCA(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	leaf, chain := ca.leaf(t, 2, pinTestKey(t), now.Add(-time.Minute), now.Add(time.Hour))
	var hits atomic.Int64
	good := pinnedServer(t, chain, &hits)
	cfg := bytes.Repeat([]byte{1}, 16)

	p := pinnedProber(clk, t)
	pin := spki(leaf)
	out := p.Probe(t.Context(), pinnedTask(1, good.URL, pin[:], cfg))
	if out.CertNotAfter == 0 {
		t.Fatalf("success = %+v, want cert carried", out)
	}
	// 同一任务同一身份随后握手失败（换了一台出示别的证书的服务端）：候选照常带回。
	var badHits atomic.Int64
	_, badChain := ca.leaf(t, 3, pinTestKey(t), now.Add(-time.Minute), now.Add(time.Hour))
	bad := pinnedServer(t, badChain, &badHits)
	out = p.Probe(t.Context(), pinnedTask(1, bad.URL, pin[:], cfg))
	if out.Presented == nil || out.Presented.Reason != heronv1.PresentedReason_PRESENTED_REASON_PIN_MISMATCH {
		t.Fatalf("failure after success on same identity Presented = %+v, want candidate", out.Presented)
	}
}

// 同一任务反复改内容（任务数不变）时，限频状态数量不增长：Apply 只保留当前身份的那一份。
func TestHTTPCertStateBoundedAcrossIdentityChurn(t *testing.T) {
	clk := clock.NewFake(time.Unix(2000, 0))
	now := clk.Now()
	ca := newPinTestCA(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	leaf, chain := ca.leaf(t, 2, pinTestKey(t), now.Add(-time.Minute), now.Add(time.Hour))
	var hits atomic.Int64
	s := pinnedServer(t, chain, &hits)
	p := pinnedProber(clk, t)
	pin := spki(leaf)

	for i := range 10 {
		cfg := bytes.Repeat([]byte{byte(i)}, 16)
		p.pruneTasks(map[uint64]string{1: string(cfg)})
		out := p.Probe(t.Context(), pinnedTask(1, s.URL, pin[:], cfg))
		if out.CertNotAfter == 0 {
			t.Fatalf("probe with fresh identity = %+v, want cert carried", out)
		}
	}
	p.certMu.Lock()
	okKeys, candKeys, current := len(p.certOK), len(p.certCand), len(p.current)
	p.certMu.Unlock()
	if okKeys != 1 || candKeys != 0 || current != 1 {
		t.Fatalf("state after churn: certOK=%d certCand=%d current=%d, want 1/0/1", okKeys, candKeys, current)
	}
}

// 取消后才返回的旧探测：写限频状态前身份已换成另一份，旧键不重建、观测不携带。
func TestHTTPLateProbeKeepsNoOldKey(t *testing.T) {
	clk := clock.NewFake(time.Unix(2000, 0))
	now := clk.Now()
	ca := newPinTestCA(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	leaf, chain := ca.leaf(t, 2, pinTestKey(t), now.Add(-time.Minute), now.Add(time.Hour))
	var hits atomic.Int64
	s := pinnedServer(t, chain, &hits)
	p := pinnedProber(clk, t)
	pin := spki(leaf)
	cfgA := bytes.Repeat([]byte{1}, 16)
	cfgB := bytes.Repeat([]byte{2}, 16)

	p.pruneTasks(map[uint64]string{1: string(cfgA)})
	if out := p.Probe(t.Context(), pinnedTask(1, s.URL, pin[:], cfgA)); out.CertNotAfter == 0 {
		t.Fatal("first probe did not carry cert")
	}
	// 任务内容变化：Apply 换成新身份，旧身份的记录删除。
	p.pruneTasks(map[uint64]string{1: string(cfgB)})
	// 被取消的旧探测此刻才返回：既不携带，也不重建旧键。
	if out := p.Probe(t.Context(), pinnedTask(1, s.URL, pin[:], cfgA)); out.CertNotAfter != 0 {
		t.Fatalf("late probe with stale identity = %+v, want cert not carried", out)
	}
	p.certMu.Lock()
	_, oldKey := p.certOK[certKey{1, string(cfgA)}]
	p.certMu.Unlock()
	if oldKey {
		t.Fatal("late probe recreated the key of the old identity")
	}
	// 新身份的探测不受影响。
	if out := p.Probe(t.Context(), pinnedTask(1, s.URL, pin[:], cfgB)); out.CertNotAfter == 0 {
		t.Fatal("probe with current identity did not carry cert")
	}
}

// 任务删除后晚返回的探测：登记簿里已没有它，既不携带观测也不留键；再来多少轮 prune 也不会泄漏。
func TestHTTPLateProbeAfterTaskRemoval(t *testing.T) {
	clk := clock.NewFake(time.Unix(2000, 0))
	now := clk.Now()
	ca := newPinTestCA(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	leaf, chain := ca.leaf(t, 2, pinTestKey(t), now.Add(-time.Minute), now.Add(time.Hour))
	var hits atomic.Int64
	s := pinnedServer(t, chain, &hits)
	p := pinnedProber(clk, t)
	pin := spki(leaf)
	cfgA := bytes.Repeat([]byte{1}, 16)

	p.pruneTasks(map[uint64]string{1: string(cfgA)})
	if out := p.Probe(t.Context(), pinnedTask(1, s.URL, pin[:], cfgA)); out.CertNotAfter == 0 {
		t.Fatal("registered probe did not carry cert")
	}
	// 任务从清单消失，登记簿与限频表都清空。
	p.pruneTasks(map[uint64]string{})
	// 被取消的旧探测此刻才返回：不携带、不写键。
	if out := p.Probe(t.Context(), pinnedTask(1, s.URL, pin[:], cfgA)); out.CertNotAfter != 0 {
		t.Fatalf("late probe after removal = %+v, want cert not carried", out)
	}
	// 再次 prune 之后状态仍为空：旧键没有借晚探测复活。
	p.pruneTasks(map[uint64]string{})
	p.certMu.Lock()
	left := len(p.certOK) + len(p.certCand)
	p.certMu.Unlock()
	if left != 0 {
		t.Fatalf("state after removal + late probe + prune = %d keys, want 0", left)
	}
}

// 增删交替多轮：状态数量始终有界，不随轮数增长。
func TestHTTPCertStateBoundedAcrossAddDeleteChurn(t *testing.T) {
	clk := clock.NewFake(time.Unix(2000, 0))
	now := clk.Now()
	ca := newPinTestCA(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	leaf, chain := ca.leaf(t, 2, pinTestKey(t), now.Add(-time.Minute), now.Add(time.Hour))
	var hits atomic.Int64
	s := pinnedServer(t, chain, &hits)
	p := pinnedProber(clk, t)
	pin := spki(leaf)

	for i := range 6 {
		cfg := bytes.Repeat([]byte{byte(i)}, 16)
		p.pruneTasks(map[uint64]string{1: string(cfg)})
		if out := p.Probe(t.Context(), pinnedTask(1, s.URL, pin[:], cfg)); out.CertNotAfter == 0 {
			t.Fatalf("round %d: registered probe did not carry cert", i)
		}
		p.pruneTasks(map[uint64]string{})
		// 删除当轮的晚返回探测：不携带、不留键。
		if out := p.Probe(t.Context(), pinnedTask(1, s.URL, pin[:], cfg)); out.CertNotAfter != 0 {
			t.Fatalf("round %d: late probe after removal carried cert", i)
		}
	}
	p.certMu.Lock()
	okKeys, candKeys, current := len(p.certOK), len(p.certCand), len(p.current)
	p.certMu.Unlock()
	if okKeys != 0 || candKeys != 0 || current != 0 {
		t.Fatalf("state after add/delete churn: certOK=%d certCand=%d current=%d, want all 0", okKeys, candKeys, current)
	}
}

// 登记簿建立之前写下的键（引擎脱离调度器使用的阶段）也要按表键清掉：清单为空即全清。
func TestHTTPPruneCleansKeysWithoutRegistration(t *testing.T) {
	clk := clock.NewFake(time.Unix(2000, 0))
	now := clk.Now()
	ca := newPinTestCA(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	leaf, chain := ca.leaf(t, 2, pinTestKey(t), now.Add(-time.Minute), now.Add(time.Hour))
	var hits atomic.Int64
	s := pinnedServer(t, chain, &hits)
	p := pinnedProber(clk, t)
	pin := spki(leaf)
	cfg := bytes.Repeat([]byte{1}, 16)

	// 脱离调度器的使用：没有任何 pruneTasks，探测直接写限频键。
	if out := p.Probe(t.Context(), pinnedTask(1, s.URL, pin[:], cfg)); out.CertNotAfter == 0 {
		t.Fatal("standalone probe did not carry cert")
	}
	p.pruneTasks(map[uint64]string{})
	p.certMu.Lock()
	left := len(p.certOK) + len(p.certCand)
	p.certMu.Unlock()
	if left != 0 {
		t.Fatalf("prune with empty set left %d keys, want 0 (clean by table keys, not only via registry)", left)
	}
}

// 每次探测恰好建立一条连接：候选必须来自这次握手失败的错误链，而不是另建连接去取证书——
// 再拨一次拿到的可能是另一台机器（LB 后面）出示的另一份证书。
func TestHTTPProbeUsesExactlyOneConnection(t *testing.T) {
	clk := clock.NewFake(time.Unix(2000, 0))
	now := clk.Now()
	ca := newPinTestCA(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	p := pinnedProber(clk, t)

	// 钉住但指纹不符：失败路径带回候选。
	_, chainA := ca.leaf(t, 2, pinTestKey(t), now.Add(-time.Minute), now.Add(time.Hour))
	var hitsA, connsA atomic.Int64
	sA := pinnedServerWithConns(t, chainA, &hitsA, &connsA)
	other := pinTestKey(t)
	otherLeaf, _ := ca.leaf(t, 3, other, now.Add(-time.Minute), now.Add(time.Hour))
	otherPin := spki(otherLeaf)
	out := p.Probe(t.Context(), pinnedTask(1, sA.URL, otherPin[:], nil))
	if out.Presented == nil || out.Presented.Reason != heronv1.PresentedReason_PRESENTED_REASON_PIN_MISMATCH {
		t.Fatalf("mismatch probe Presented = %+v, want PIN_MISMATCH candidate", out.Presented)
	}
	if n := connsA.Load(); n != 1 {
		t.Fatalf("mismatch probe used %d connections, want exactly 1", n)
	}

	// 未钉住、默认校验失败：失败路径带回候选。
	var hitsB, connsB atomic.Int64
	_, chainB := ca.leaf(t, 4, pinTestKey(t), now.Add(-time.Minute), now.Add(time.Hour))
	sB := pinnedServerWithConns(t, chainB, &hitsB, &connsB)
	out = p.Probe(t.Context(), pinnedTask(2, sB.URL, nil, nil))
	if out.Presented == nil || out.Presented.Reason != heronv1.PresentedReason_PRESENTED_REASON_CA_VERIFY_FAILED {
		t.Fatalf("CA-failure probe Presented = %+v, want CA_VERIFY_FAILED candidate", out.Presented)
	}
	if n := connsB.Load(); n != 1 {
		t.Fatalf("CA-failure probe used %d connections, want exactly 1", n)
	}

	// 成功路径同样一条连接。
	var hitsC, connsC atomic.Int64
	leafC, chainC := ca.leaf(t, 5, pinTestKey(t), now.Add(-time.Minute), now.Add(time.Hour))
	sC := pinnedServerWithConns(t, chainC, &hitsC, &connsC)
	pinC := spki(leafC)
	if out := p.Probe(t.Context(), pinnedTask(3, sC.URL, pinC[:], nil)); out.Err != "" || out.Timeout {
		t.Fatalf("success probe = %+v", out)
	}
	if n := connsC.Load(); n != 1 {
		t.Fatalf("success probe used %d connections, want exactly 1", n)
	}
}
