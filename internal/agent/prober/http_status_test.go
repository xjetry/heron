package prober

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
)

// 状态码判据决定一次 HTTP 响应算成功还是丢包：不满足判据计入丢包（与超时同一口径）且不携带证书到期时刻；
// 3xx 不跟随，判据看的就是这一个响应——严格 2xx 下反代跳去登录页的 302 是丢包，而不是跟过去拿到登录页的 200。
func TestHTTPStatusPolicy(t *testing.T) {
	var status, requests, followed atomic.Int64
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			followed.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		requests.Add(1)
		if code := int(status.Load()); code >= 300 && code < 400 {
			w.Header().Set("Location", "/login")
			w.WriteHeader(code)
			return
		}
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(s.Close)
	notAfter := s.Certificate().NotAfter.Unix()
	const (
		below400 = heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_UNSPECIFIED
		success  = heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_SUCCESS
		exact    = heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_EXACT
	)
	for _, tc := range []struct {
		name     string
		policy   heronv1.HttpStatusPolicy
		expected uint32
		status   int
		ok       bool
	}{
		{"default_200", below400, 0, 200, true},
		{"default_302", below400, 0, 302, true},
		{"default_399", below400, 0, 399, true},
		{"default_400", below400, 0, 400, false},
		{"default_503", below400, 0, 503, false},
		{"success_200", success, 0, 200, true},
		{"success_204", success, 0, 204, true},
		{"success_299", success, 0, 299, true},
		{"success_301", success, 0, 301, false},
		{"success_302", success, 0, 302, false},
		{"success_404", success, 0, 404, false},
		{"exact_204_hit", exact, 204, 204, true},
		{"exact_204_got_200", exact, 204, 200, false},
		{"exact_302_hit", exact, 302, 302, true},
		{"exact_302_got_301", exact, 302, 301, false},
		{"exact_401_hit", exact, 401, 401, true},
		{"exact_503_hit", exact, 503, 503, true},
		{"exact_503_got_500", exact, 503, 500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status.Store(int64(tc.status))
			before, beforeFollowed := requests.Load(), followed.Load()
			task := httpTask(s.URL + "/app")
			task.HttpStatusPolicy, task.HttpExpectedStatus = tc.policy, tc.expected
			out := certProber(clock.Real(), t).Probe(t.Context(), task)
			if requests.Load() != before+1 || followed.Load() != beforeFollowed {
				t.Fatalf("requests=%d followed=%d, want exactly one request and no redirect followed", requests.Load()-before, followed.Load()-beforeFollowed)
			}
			if out.Err != "" {
				t.Fatalf("outcome=%+v, want no probe error", out)
			}
			if tc.ok {
				if out.Timeout || out.RttUs == 0 || out.CertNotAfter != notAfter {
					t.Fatalf("status %d under %s/%d = %+v, want success carrying cert %d", tc.status, tc.policy, tc.expected, out, notAfter)
				}
				return
			}
			if !out.Timeout || out.RttUs != 0 || out.CertNotAfter != 0 || out.Presented != nil {
				t.Fatalf("status %d under %s/%d = %+v, want loss without cert", tc.status, tc.policy, tc.expected, out)
			}
		})
	}
}

// 状态码不符的丢包不占用证书到期时刻的每小时携带额度：判据随后满足时，同一身份的第一次成功照常带回到期时刻。
func TestHTTPStatusMismatchKeepsCertReportBudget(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusFound)
	s, notAfter := certTestServer(t, &status)
	p := certProber(clock.NewFake(time.Unix(1000, 0)), t)
	task := httpTask(s.URL)
	task.HttpStatusPolicy = heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_SUCCESS
	if out := p.Probe(t.Context(), task); !out.Timeout || out.CertNotAfter != 0 {
		t.Fatalf("302 under strict 2xx = %+v, want loss without cert", out)
	}
	status.Store(http.StatusOK)
	if out := p.Probe(t.Context(), task); out.Timeout || out.CertNotAfter != notAfter {
		t.Fatalf("200 after a mismatch = %+v, want success carrying cert %d", out, notAfter)
	}
}
