package prober

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

// certTestServer 起一个自签 TLS 服务，返回链首枚证书的到期时刻供断言比对。
func certTestServer(t *testing.T, status *atomic.Int64) (*httptest.Server, int64) {
	t.Helper()
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(s.Close)
	return s, s.Certificate().NotAfter.Unix()
}

// 自签证书需要跳过链验证才能握手成功；PeerCertificates 与是否验证无关，始终填充。
func certProber(clk clock.Clock, t *testing.T) *HTTP {
	return &HTTP{
		Clock:           clk,
		Targets:         loopbackTargets(t, nil),
		Version:         "v",
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
}

func TestHTTPSuccessCarriesCertNotAfter(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusOK)
	s, wantNotAfter := certTestServer(t, &status)
	p := certProber(clock.Real(), t)
	out := p.Probe(t.Context(), httpTask(s.URL))
	if out.Err != "" || out.Timeout {
		t.Fatalf("https 200=%+v", out)
	}
	if out.CertNotAfter != wantNotAfter {
		t.Fatalf("CertNotAfter = %d, want server cert NotAfter %d", out.CertNotAfter, wantNotAfter)
	}
}

func TestHTTPCertNotCarried(t *testing.T) {
	p := certProber(clock.Real(), t)
	// http:// 目标没有 TLS 握手，不携带。
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer plain.Close()
	if out := p.Probe(t.Context(), httpTask(plain.URL)); out.Err != "" || out.Timeout || out.CertNotAfter != 0 {
		t.Fatalf("http 200=%+v, want success without cert", out)
	}
	// 状态 ≥400 计入丢包，即使握手成功也不携带。
	var status atomic.Int64
	status.Store(http.StatusInternalServerError)
	s, _ := certTestServer(t, &status)
	if out := p.Probe(t.Context(), httpTask(s.URL)); !out.Timeout || out.CertNotAfter != 0 {
		t.Fatalf("https 500=%+v, want timeout without cert", out)
	}
}

// 频率上限：同一任务连续两次成功探测只有第一次携带；单调钟推进 CertReportInterval 后再携带。
func TestHTTPCertReportInterval(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusOK)
	s, wantNotAfter := certTestServer(t, &status)
	clk := clock.NewFake(time.Unix(1000, 0))
	p := certProber(clk, t)
	task := httpTask(s.URL)

	if out := p.Probe(t.Context(), task); out.CertNotAfter != wantNotAfter {
		t.Fatalf("first probe CertNotAfter = %d, want %d", out.CertNotAfter, wantNotAfter)
	}
	clk.Advance(time.Minute)
	if out := p.Probe(t.Context(), task); out.Err != "" || out.Timeout || out.CertNotAfter != 0 {
		t.Fatalf("probe within CertReportInterval = %+v, want success without cert", out)
	}
	// 差一秒不足一小时仍不携带。
	clk.Advance(CertReportInterval - time.Minute - time.Second)
	if out := p.Probe(t.Context(), task); out.CertNotAfter != 0 {
		t.Fatalf("probe one second before interval = %+v, want without cert", out)
	}
	// 距上次携带满一小时：再次携带。
	clk.Advance(time.Second)
	if out := p.Probe(t.Context(), task); out.CertNotAfter != wantNotAfter {
		t.Fatalf("probe after CertReportInterval CertNotAfter = %d, want %d", out.CertNotAfter, wantNotAfter)
	}
}

// 任务从清单消失（pruneTasks）再出现按首次处理；仍在清单里的任务记录不受影响。
func TestHTTPCertReportPrunedWithTaskSet(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusOK)
	s, wantNotAfter := certTestServer(t, &status)
	clk := clock.NewFake(time.Unix(1000, 0))
	p := certProber(clk, t)
	task := httpTask(s.URL)

	if out := p.Probe(t.Context(), task); out.CertNotAfter != wantNotAfter {
		t.Fatalf("first probe CertNotAfter = %d, want %d", out.CertNotAfter, wantNotAfter)
	}
	// 任务仍在清单里且身份未变：记录保留，一小时内不重复携带。
	p.pruneTasks(map[uint64]string{task.GetId(): ""})
	if out := p.Probe(t.Context(), task); out.CertNotAfter != 0 {
		t.Fatalf("probe after prune keeping task = %+v, want without cert", out)
	}
	// 任务从清单消失：记录清掉；登记簿存在后，未登记的任务按过期处理——晚返回的旧探测
	// 既不携带观测，也不留键（键真留下来，此后没有任何 prune 能再清到它）。
	p.pruneTasks(map[uint64]string{})
	if out := p.Probe(t.Context(), task); out.CertNotAfter != 0 {
		t.Fatalf("late probe after task removal = %+v, want without cert", out)
	}
	p.certMu.Lock()
	left := len(p.certOK) + len(p.certCand)
	p.certMu.Unlock()
	if left != 0 {
		t.Fatalf("state left after removal and late probe = %d keys, want 0", left)
	}
}

// ToProto 只在 rtt_us 成功结果上透传证书到期时刻。
func TestToProtoCarriesCertNotAfter(t *testing.T) {
	rs := ToProto([]Result{
		{TaskID: 1, Outcome: Outcome{RttUs: 100, CertNotAfter: 1893456000}},
		{TaskID: 2, Outcome: Outcome{RttUs: 100}},
		{TaskID: 3, Outcome: Outcome{Timeout: true}},
		{TaskID: 4, Outcome: Outcome{Err: "boom"}},
	}, time.Second)
	if got := rs[0].GetCertNotAfterS(); got != 1893456000 {
		t.Fatalf("rtt result CertNotAfterS = %d, want 1893456000", got)
	}
	for i, r := range rs[1:] {
		if r.CertNotAfterS != nil {
			t.Fatalf("result %d (%T) carries cert, want nil", i+2, r.Outcome)
		}
	}
}
