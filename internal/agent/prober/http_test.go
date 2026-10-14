package prober

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/probelimit"
)

func httpTask(target string) *heronv1.ProbeTask {
	t := task(1)
	t.Kind, t.Target = heronv1.ProbeKind_PROBE_KIND_HTTP, target
	t.TimeoutMs = probelimit.MaxTimeoutMs
	return t
}

func TestHTTPOutcomes(t *testing.T) {
	p := HTTP{Clock: clock.Real(), Targets: loopbackTargets(t, nil), Version: "test-version"}
	var requests atomic.Int64
	var gotHost, gotUA string
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		gotHost, gotUA = r.Host, r.UserAgent()
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	if out := p.Probe(t.Context(), httpTask(ok.URL)); out.Err != "" || out.Timeout || out.RttUs == 0 {
		t.Fatalf("200=%+v", out)
	}
	// URL 的主机名出现在 Host 头与 User-Agent 里：探测连的是解析出的地址，名字仍是 URL 的。
	if requests.Load() != 1 || gotUA != "heron-agent/test-version" {
		t.Fatalf("requests=%d ua=%q", requests.Load(), gotUA)
	}
	if !strings.HasPrefix(gotHost, "127.0.0.1:") {
		t.Fatalf("host=%q", gotHost)
	}

	var redirects atomic.Int64
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirects.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("method=%s", r.Method)
		}
		http.Redirect(w, r, "/elsewhere", http.StatusMovedPermanently)
	}))
	defer redirect.Close()
	// 301 本身就是答案，不跟随：接收端只见一次 GET。
	if out := p.Probe(t.Context(), httpTask(redirect.URL)); out.Err != "" || out.Timeout || out.RttUs == 0 {
		t.Fatalf("301=%+v", out)
	}
	if redirects.Load() != 1 {
		t.Fatalf("redirect followed: requests=%d", redirects.Load())
	}

	for status, name := range map[int]string{http.StatusNotFound: "404", http.StatusInternalServerError: "500"} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		if out := p.Probe(t.Context(), httpTask(s.URL)); !out.Timeout || out.Err != "" {
			t.Fatalf("%s=%+v", name, out)
		}
		s.Close()
	}

	// 自签证书：TLS 握手失败计入丢包，不是本机 error。
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer tlsSrv.Close()
	if out := p.Probe(t.Context(), httpTask(tlsSrv.URL)); !out.Timeout || out.Err != "" {
		t.Fatalf("untrusted_tls=%+v", out)
	}

	// 对端只回 alert 的握手失败同样是丢包：服务端只要 TLS 1.3，探测器最多给 1.2，
	// 握手死于版本不兼容——服务在、给不出一次有效握手，与证书错误同一类可达性事实。
	tls13 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	tls13.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	tls13.StartTLS()
	defer tls13.Close()
	p12 := HTTP{Clock: clock.Real(), Targets: loopbackTargets(t, nil), Version: "test-version", TLSClientConfig: &tls.Config{MaxVersion: tls.VersionTLS12}}
	if out := p12.Probe(t.Context(), httpTask(tls13.URL)); !out.Timeout || out.Err != "" {
		t.Fatalf("remote_alert=%+v", out)
	}

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + ln.Addr().String() + "/"
	ln.Close()
	if out := p.Probe(t.Context(), httpTask(closed)); !out.Timeout || out.Err != "" {
		t.Fatalf("refused=%+v", out)
	}

	// 默认策略拒绝回环：地址策略拒绝是 error，照常调度由调用方负责。
	denied := HTTP{Clock: clock.Real(), Targets: Targets{}, Version: "test-version"}
	if out := denied.Probe(t.Context(), httpTask(ok.URL)); out.Err == "" || out.Timeout {
		t.Fatalf("policy=%+v", out)
	}

	if out := p.Probe(t.Context(), httpTask("ftp://example.com/")); out.Err == "" || out.Timeout {
		t.Fatalf("scheme=%+v", out)
	}
	if out := p.Probe(t.Context(), httpTask("://bad")); out.Err == "" || out.Timeout {
		t.Fatalf("malformed=%+v", out)
	}
}

// 探测连的是解析出的地址，Host 头与 SNI 用的仍是 URL 里的名字。
func TestHTTPDialsResolvedAddressWithURLName(t *testing.T) {
	var gotHost string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
	}))
	defer s.Close()
	_, port, _ := net.SplitHostPort(s.Listener.Addr().String())
	p := HTTP{Clock: clock.Real(), Targets: loopbackTargets(t, localDNS(t, dnsDual, nil)), Version: "v"}
	if out := p.Probe(t.Context(), httpTask("http://local.prober.invalid:"+port+"/")); out.Err != "" || out.Timeout {
		t.Fatalf("named=%+v", out)
	}
	if gotHost != "local.prober.invalid:"+port {
		t.Fatalf("host=%q", gotHost)
	}
}

// 不读正文：拿到响应头就返回。接收端给完响应头后挂起，读正文的实现会等到超时、被判成丢包。
func TestHTTPDoesNotReadBody(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	p := HTTP{Clock: clock.Real(), Targets: loopbackTargets(t, nil), Version: "v"}
	task := httpTask(s.URL)
	task.TimeoutMs = 1000
	if out := p.Probe(t.Context(), task); out.Err != "" || out.Timeout || out.RttUs == 0 {
		t.Fatalf("headers_only=%+v", out)
	}
}
