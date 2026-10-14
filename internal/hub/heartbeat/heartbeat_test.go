package heartbeat

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
)

// fakeSource 是 Source 的替换件：用例摆出每轮的设置与计数。
type fakeSource struct {
	mu       sync.Mutex
	settings store.HeartbeatSettings
	counts   Counts
}

func (f *fakeSource) HeartbeatSettings(context.Context) (store.HeartbeatSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings, nil
}

func (f *fakeSource) HeartbeatCounts(context.Context) (Counts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts, nil
}

func (f *fakeSource) set(s store.HeartbeatSettings) {
	f.mu.Lock()
	f.settings = s
	f.mu.Unlock()
}

type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// received 记一次到达接收端的请求。GET/HEAD 没有正文。
type received struct {
	method      string
	body        string
	contentType string
}

type receiver struct {
	mu    sync.Mutex
	items []received
	// target 记录跳转目标的命中次数：3xx 不跟随，target 必须保持为 0。
	target int
}

func (r *receiver) add(req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, received{method: req.Method, body: string(body), contentType: req.Header.Get("Content-Type")})
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.items)
}

func (r *receiver) at(i int) received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.items[i]
}

func newHeartbeat(source Source, clk clock.Clock, logs *logBuffer) *Heartbeat {
	return New(source, outbound.NewClient(alert.NotifyTimeout), "v9", clk, slog.New(slog.NewTextHandler(logs, nil)))
}

// 三种方法各自的线上形状：GET/HEAD 无正文；POST 是固定的 JSON 字段集合与数值，带 application/json。
func TestHeartbeatRequestShapes(t *testing.T) {
	for _, c := range []struct {
		name        string
		method      store.HeartbeatMethod
		wantMethod  string
		wantContent string
		wantType    string
	}{
		{"GET", store.HeartbeatGet, http.MethodGet, "", ""},
		{"HEAD", store.HeartbeatHead, http.MethodHead, "", ""},
		{"POST", store.HeartbeatPost, http.MethodPost, `{"nodes_total":4,"online":3,"offline":1,"maintenance":1,"rented":0,"firing":2,"hub_version":"v9"}`, "application/json"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rc := &receiver{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/target" {
					rc.mu.Lock()
					rc.target++
					rc.mu.Unlock()
				}
				rc.add(r)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			src := &fakeSource{
				settings: store.HeartbeatSettings{URL: srv.URL + "/ping", IntervalS: 90, Method: c.method},
				counts:   Counts{NodesTotal: 4, Online: 3, Offline: 1, Maintenance: 1, Firing: 2},
			}
			h := newHeartbeat(src, clock.NewFake(time.Unix(1000, 0)), &logBuffer{})
			ctx := t.Context()
			if got := h.round(ctx); got != 90*time.Second {
				t.Fatalf("round wait = %v, want 90s", got)
			}
			if rc.count() != 1 {
				t.Fatalf("requests = %d, want 1", rc.count())
			}
			got := rc.at(0)
			if got.method != c.wantMethod || got.body != c.wantContent || got.contentType != c.wantType {
				t.Fatalf("request = %+v, want method %s body %q content-type %q", got, c.wantMethod, c.wantContent, c.wantType)
			}
		})
	}
}

// POST 正文的字段集合是钉死的：多一个（比如拓扑或节点名）少一个都会红。
func TestHeartbeatPostBodyFields(t *testing.T) {
	rc := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	src := &fakeSource{
		settings: store.HeartbeatSettings{URL: srv.URL, IntervalS: 60, Method: store.HeartbeatPost},
		counts:   Counts{NodesTotal: 1, Online: 1},
	}
	h := newHeartbeat(src, clock.NewFake(time.Unix(0, 0)), &logBuffer{})
	h.round(t.Context())

	var got map[string]any
	if err := json.Unmarshal([]byte(rc.at(0).body), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"nodes_total": float64(1), "online": float64(1), "offline": float64(0), "maintenance": float64(0), "rented": float64(0), "firing": float64(0), "hub_version": "v9"}
	if len(got) != len(want) {
		t.Fatalf("fields = %v, want exactly %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("field %s = %v, want %v", k, got[k], v)
		}
	}
}

// 失败在产生处归类：非法/非绝对地址走 request，连接不上走 transport，非 2xx（含 3xx）走 http_status 并记状态码。
// 3xx 不跟随：跳转目标一次都不被请求。
func TestHeartbeatFailureCategories(t *testing.T) {
	rc := &receiver{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { rc.add(r); w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	mux.HandleFunc("/target", func(w http.ResponseWriter, r *http.Request) {
		rc.mu.Lock()
		rc.target++
		rc.mu.Unlock()
	})
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) { rc.add(r); w.WriteHeader(http.StatusServiceUnavailable) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Run("success is 2xx", func(t *testing.T) {
		clk := clock.NewFake(time.Unix(5000, 0))
		src := &fakeSource{settings: store.HeartbeatSettings{URL: srv.URL + "/ok", IntervalS: 60, Method: store.HeartbeatGet}}
		h := newHeartbeat(src, clk, &logBuffer{})
		h.round(t.Context())
		st := h.Status()
		if st.LastSuccessAt.Unix() != 5000 || !st.LastFailureAt.IsZero() || st.FailureCategory != "" || st.NextAt.Unix() != 5060 {
			t.Fatalf("success status = %+v", st)
		}
	})

	t.Run("transport", func(t *testing.T) {
		down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		downURL := down.URL
		down.Close()
		clk := clock.NewFake(time.Unix(6000, 0))
		src := &fakeSource{settings: store.HeartbeatSettings{URL: downURL + "/ping", IntervalS: 60, Method: store.HeartbeatGet}}
		h := newHeartbeat(src, clk, &logBuffer{})
		h.round(t.Context())
		st := h.Status()
		if st.FailureCategory != CategoryTransport || st.LastFailureAt.Unix() != 6000 || st.FailureHTTPStatus != 0 || !st.LastSuccessAt.IsZero() {
			t.Fatalf("transport status = %+v", st)
		}
	})

	t.Run("http_status and no redirect", func(t *testing.T) {
		clk := clock.NewFake(time.Unix(7000, 0))
		src := &fakeSource{settings: store.HeartbeatSettings{URL: srv.URL + "/boom", IntervalS: 60, Method: store.HeartbeatGet}}
		h := newHeartbeat(src, clk, &logBuffer{})
		h.round(t.Context())
		st := h.Status()
		if st.FailureCategory != CategoryHTTPStatus || st.FailureHTTPStatus != http.StatusServiceUnavailable || st.LastFailureAt.Unix() != 7000 {
			t.Fatalf("http_status status = %+v", st)
		}
		before := rc.count()
		src.set(store.HeartbeatSettings{URL: srv.URL + "/redirect", IntervalS: 60, Method: store.HeartbeatGet})
		h.round(t.Context())
		if st := h.Status(); st.FailureCategory != CategoryHTTPStatus || st.FailureHTTPStatus != http.StatusFound {
			t.Fatalf("redirect status = %+v", st)
		}
		if rc.count() != before+1 {
			t.Fatalf("requests after redirect = %d, want %d (the target must not be requested)", rc.count(), before+1)
		}
		if rc.target != 0 {
			t.Fatalf("redirect target was requested %d times, want 0", rc.target)
		}
	})

	t.Run("request", func(t *testing.T) {
		for _, raw := range []string{"not a url", "ftp://hc.example/x", "https:///no-host"} {
			clk := clock.NewFake(time.Unix(8000, 0))
			src := &fakeSource{settings: store.HeartbeatSettings{URL: raw, IntervalS: 60, Method: store.HeartbeatGet}}
			h := newHeartbeat(src, clk, &logBuffer{})
			h.round(t.Context())
			if st := h.Status(); st.FailureCategory != CategoryRequest || st.LastFailureAt.Unix() != 8000 || st.FailureHTTPStatus != 0 {
				t.Fatalf("url %q status = %+v", raw, st)
			}
		}
	})

	// 连续同一失败只记一行；成功清零后下一次失败再记。用它钉住日志不刷屏。
	t.Run("log only on state change", func(t *testing.T) {
		logs := &logBuffer{}
		clk := clock.NewFake(time.Unix(9000, 0))
		src := &fakeSource{settings: store.HeartbeatSettings{URL: srv.URL + "/boom", IntervalS: 60, Method: store.HeartbeatGet}}
		h := newHeartbeat(src, clk, logs)
		h.round(t.Context())
		h.round(t.Context())
		if got := strings.Count(logs.String(), "heartbeat failed"); got != 1 {
			t.Fatalf("failure log lines = %d, want 1:\n%s", got, logs.String())
		}
		src.set(store.HeartbeatSettings{URL: srv.URL + "/ok", IntervalS: 60, Method: store.HeartbeatGet})
		h.round(t.Context())
		src.set(store.HeartbeatSettings{URL: srv.URL + "/boom", IntervalS: 60, Method: store.HeartbeatGet})
		h.round(t.Context())
		if got := strings.Count(logs.String(), "heartbeat failed"); got != 2 {
			t.Fatalf("failure log lines after success = %d, want 2:\n%s", got, logs.String())
		}
	})
}

// 循环每轮重读设置：启动即发一次，改间隔下一轮生效，清空 url 立即停发。把"每轮重读"改成"循环开头读一次"后，
// 第三次唤醒会带着旧 url 再发一次、且间隔仍是 60s——下面两处断言（d1、清空后计数不变）就会红。
func TestRunRereadsSettingsEveryRound(t *testing.T) {
	rc := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rc.add(r); w.WriteHeader(http.StatusOK) }))
	defer srv.Close()

	clk := clock.NewFake(time.Unix(1000, 0))
	src := &fakeSource{settings: store.HeartbeatSettings{URL: srv.URL + "/ping", IntervalS: 60, Method: store.HeartbeatGet}}
	h := newHeartbeat(src, clk, &logBuffer{})
	wake := make(chan time.Duration, 1)
	proceed := make(chan time.Duration, 1)
	h.wait = func(ctx context.Context, d time.Duration) bool {
		select {
		case wake <- d:
		case <-ctx.Done():
			return false
		}
		select {
		case next := <-proceed:
			clk.Advance(next)
			return true
		case <-ctx.Done():
			return false
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()

	d0 := <-wake // 启动即发一次，随后等待 60s。
	if rc.count() != 1 {
		t.Fatalf("startup requests = %d, want 1", rc.count())
	}
	if d0 != 60*time.Second {
		t.Fatalf("first wait = %v, want 60s", d0)
	}
	src.set(store.HeartbeatSettings{URL: srv.URL + "/ping", IntervalS: 120, Method: store.HeartbeatGet})
	proceed <- d0
	d1 := <-wake
	if d1 != 120*time.Second {
		t.Fatalf("wait after interval change = %v, want 120s (interval must be re-read every round)", d1)
	}
	if rc.count() != 2 {
		t.Fatalf("requests after interval change = %d, want 2", rc.count())
	}
	src.set(store.HeartbeatSettings{URL: "", IntervalS: 120, Method: store.HeartbeatGet})
	proceed <- d1
	d2 := <-wake
	if d2 != 60*time.Second {
		t.Fatalf("wait while disabled = %v, want 60s (poll settings, not the outbound interval)", d2)
	}
	if rc.count() != 2 {
		t.Fatalf("requests after clearing url = %d, want 2 (no request while disabled)", rc.count())
	}
	cancel()
	<-done
}

// 停用态等的不是外呼间隔，而是设置的轮询节奏：间隔可以设到 3600，按它睡会让刚填上的地址最坏一小时才生效。
// 把 round 改回"停用也按 interval 等"后，本用例第一处断言拿到 3600s 而不是 60s，next_at 也不再置零。
func TestRunDisabledPollsSettingsAtFixedCadence(t *testing.T) {
	rc := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rc.add(r); w.WriteHeader(http.StatusOK) }))
	defer srv.Close()

	clk := clock.NewFake(time.Unix(2000, 0))
	src := &fakeSource{settings: store.HeartbeatSettings{URL: "", IntervalS: 3600, Method: store.HeartbeatGet}}
	h := newHeartbeat(src, clk, &logBuffer{})
	wake := make(chan time.Duration, 1)
	proceed := make(chan time.Duration, 1)
	h.wait = func(ctx context.Context, d time.Duration) bool {
		select {
		case wake <- d:
		case <-ctx.Done():
			return false
		}
		select {
		case next := <-proceed:
			clk.Advance(next)
			return true
		case <-ctx.Done():
			return false
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()

	d0 := <-wake // 停用：不发请求，等 60s 后重读设置。
	if rc.count() != 0 {
		t.Fatalf("requests while disabled = %d, want 0", rc.count())
	}
	if d0 != 60*time.Second {
		t.Fatalf("wait while disabled = %v, want 60s (not the 3600s outbound interval)", d0)
	}
	if next := h.Status().NextAt; !next.IsZero() {
		t.Fatalf("next_at while disabled = %v, want zero", next)
	}

	src.set(store.HeartbeatSettings{URL: srv.URL + "/ping", IntervalS: 3600, Method: store.HeartbeatGet})
	proceed <- d0
	d1 := <-wake
	if rc.count() != 1 {
		t.Fatalf("requests after enabling within one poll = %d, want 1", rc.count())
	}
	if d1 != 3600*time.Second {
		t.Fatalf("wait after enabling = %v, want 3600s (outbound interval)", d1)
	}
	if next := h.Status().NextAt; next.IsZero() {
		t.Fatal("next_at after enabling should be set")
	}
	cancel()
	<-done
}
