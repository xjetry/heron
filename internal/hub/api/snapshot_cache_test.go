package api

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/testwait"
)

// observed 是一次响应里缓存必须与直连 connect 一致的部分；gzip 正文先解开再比，压缩字节本身不是契约。
type observed struct {
	status                             int
	contentType, contentEncoding, vary string
	body                               string
}

func observe(t *testing.T, h http.Handler, newReq func() *http.Request) observed {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newReq())
	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body, err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
	}
	return observed{resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"), resp.Header.Get("Vary"), string(body)}
}

func snapshotRequest(method, query, body string, header map[string]string) func() *http.Request {
	return func() *http.Request {
		target := heronv1connect.PublicServiceGetSnapshotProcedure
		if query != "" {
			target += "?" + query
		}
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		// 值里的换行拆成同名头的多行：有的形态要测多行同名头。
		for k, v := range header {
			for _, line := range strings.Split(v, "\n") {
				req.Header.Add(k, line)
			}
		}
		return req
	}
}

// 缓存对每一种请求形态的应答都与直连 connect 相同：规范形态由缓存回答（第二次是命中），
// 其余形态原样交给 connect；缓存不替 connect 接受它会拒绝的请求，也不给请求方它没要的压缩。
func TestSnapshotCacheIsTransparent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "pub")
	h.setPublic(t, id, "pub", true)
	_, direct := h.pub.connectHandler()
	const json = "connect=v1&encoding=json&message=%7B%7D"
	jsonCT := map[string]string{"Content-Type": "application/json"}
	primes := canonicalSnapshotRequests()
	// hit 标出规范形态：键已有缓存时两次都不进 connect。其余形态每次都交给 connect。
	// chunked 让请求不带 Content-Length（ContentLength = -1），正文照发。
	for _, c := range []struct {
		name, method, query, body string
		header                    map[string]string
		hit                       bool
		chunked                   bool
	}{
		{"json", http.MethodGet, json, "", nil, true, false},
		{"json without connect", http.MethodGet, "encoding=json&message=%7B%7D", "", nil, true, false},
		{"json gzip", http.MethodGet, json, "", map[string]string{"Accept-Encoding": "gzip"}, true, false},
		{"json br then gzip", http.MethodGet, json, "", map[string]string{"Accept-Encoding": "br, gzip"}, true, false},
		{"json gzip q=0", http.MethodGet, json, "", map[string]string{"Accept-Encoding": "gzip;q=0"}, true, false},
		{"json identity line then gzip line", http.MethodGet, json, "", map[string]string{"Accept-Encoding": "identity\ngzip"}, true, false},
		{"json base64 unpadded", http.MethodGet, "connect=v1&encoding=json&base64=1&message=e30", "", nil, false, false},
		{"json base64 padded", http.MethodGet, "connect=v1&encoding=json&base64=1&message=e30%3D", "", nil, false, false},
		{"json base64 flag with literal", http.MethodGet, "connect=v1&encoding=json&base64=1&message=%7B%7D", "", nil, false, false},
		{"json base64=0", http.MethodGet, "connect=v1&encoding=json&base64=0&message=%7B%7D", "", nil, false, false},
		{"json whitespace", http.MethodGet, "connect=v1&encoding=json&message=%7B%20%7D", "", nil, false, false},
		{"json unknown field", http.MethodGet, "connect=v1&encoding=json&message=%7B%22x%22%3A1%7D", "", nil, false, false},
		{"json without message", http.MethodGet, "connect=v1&encoding=json", "", nil, false, false},
		{"proto base64", http.MethodGet, "connect=v1&encoding=proto&base64=1&message=", "", nil, true, false},
		{"proto raw", http.MethodGet, "connect=v1&encoding=proto&message=", "", nil, true, false},
		{"proto gzip", http.MethodGet, "connect=v1&encoding=proto&message=", "", map[string]string{"Accept-Encoding": "gzip"}, true, false},
		{"proto unknown field", http.MethodGet, "connect=v1&encoding=proto&base64=1&message=CAE", "", nil, false, false},
		{"proto invalid", http.MethodGet, "connect=v1&encoding=proto&base64=1&message=AA", "", nil, false, false},
		{"duplicate encoding", http.MethodGet, "connect=v1&encoding=json&encoding=proto&message=%7B%7D", "", nil, false, false},
		{"connect v2", http.MethodGet, "connect=v2&encoding=json&message=%7B%7D", "", nil, false, false},
		{"compressed request", http.MethodGet, "connect=v1&encoding=json&compression=gzip&message=%7B%7D", "", nil, false, false},
		{"timeout header", http.MethodGet, json, "", map[string]string{"Connect-Timeout-Ms": "abc"}, false, false},
		// connect 以 415 拒绝带正文的 GET，与查询串是否规范无关。
		{"get json with body", http.MethodGet, json, "x", nil, false, false},
		{"get json with chunked body", http.MethodGet, json, "x", nil, false, true},
		{"get proto with body", http.MethodGet, "connect=v1&encoding=proto&message=", "x", nil, false, false},
		{"post json", http.MethodPost, "", "{}", jsonCT, true, false},
		{"post json gzip", http.MethodPost, "", "{}", map[string]string{"Content-Type": "application/json", "Accept-Encoding": "gzip"}, true, false},
		{"post proto", http.MethodPost, "", "", map[string]string{"Content-Type": "application/proto"}, true, false},
		{"post proto gzip", http.MethodPost, "", "", map[string]string{"Content-Type": "application/proto", "Accept-Encoding": "gzip"}, true, false},
		{"post json whitespace", http.MethodPost, "", "{ }", jsonCT, false, false},
		{"post json unknown field", http.MethodPost, "", `{"x":1}`, jsonCT, false, false},
		{"post json charset", http.MethodPost, "", "{}", map[string]string{"Content-Type": "application/json; charset=utf-8"}, false, false},
		{"post protocol version 2", http.MethodPost, "", "{}", map[string]string{"Content-Type": "application/json", "Connect-Protocol-Version": "2"}, false, false},
		{"post text", http.MethodPost, "", "{}", map[string]string{"Content-Type": "text/plain"}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			cached := newSnapshotCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				direct.ServeHTTP(w, r)
			}), h.clk)
			// 先让每个键都有缓存：错把某种形态当成规范形态的缺陷，只在它的键已有缓存时才会下发错的字节。
			for _, prime := range primes {
				observe(t, cached, prime)
			}
			if n := len(cached.(*snapshotCache).entries); n != 8 {
				t.Fatalf("priming filled %d keys, want all 8", n)
			}
			calls = 0
			req := snapshotRequest(c.method, c.query, c.body, c.header)
			if c.chunked {
				build := req
				req = func() *http.Request { r := build(); r.ContentLength = -1; return r }
			}
			want := observe(t, direct, req)
			for i := range 2 {
				if got := observe(t, cached, req); got != want {
					t.Fatalf("call %d through the cache:\n got %+v\nwant %+v", i+1, got, want)
				}
			}
			if n := len(cached.(*snapshotCache).entries); n != 8 {
				t.Fatalf("cache holds %d keys, want at most the 8 of the key space", n)
			}
			if wantCalls := map[bool]int{true: 0, false: 2}[c.hit]; calls != wantCalls {
				t.Fatalf("connect ran %d times for two requests, want %d", calls, wantCalls)
			}
		})
	}
}

// canonicalSnapshotRequests 按键空间 {GET, POST} × {json, proto} × {identity, gzip} 逐个构造规范形态的请求，
// 每个键一个：预热由构造覆盖全部键，而不是靠手写清单。
func canonicalSnapshotRequests() []func() *http.Request {
	var out []func() *http.Request
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, codec := range []string{"json", "proto"} {
			for _, accept := range []map[string]string{nil, {"Accept-Encoding": "gzip"}} {
				header := maps.Clone(accept)
				if header == nil {
					header = map[string]string{}
				}
				message := map[string]string{"json": "{}", "proto": ""}[codec]
				if method == http.MethodGet {
					out = append(out, snapshotRequest(method, "connect=v1&encoding="+codec+"&message="+url.QueryEscape(message), "", header))
					continue
				}
				header["Content-Type"] = "application/" + codec
				out = append(out, snapshotRequest(method, "", message, header))
			}
		}
	}
	return out
}

// 规范形态的构造恰好覆盖 8 个不同的键，也就是缓存的全部键空间。
func TestCanonicalSnapshotRequestsCoverTheKeySpace(t *testing.T) {
	t.Parallel()
	keys := map[snapshotKey]bool{}
	for _, req := range canonicalSnapshotRequests() {
		key, ok := canonicalSnapshotRequest(req())
		if !ok {
			t.Fatal("a constructed canonical request was not recognized")
		}
		keys[key] = true
	}
	if len(keys) != 8 {
		t.Fatalf("keys = %v, want 8 distinct", keys)
	}
}

// 窗口从填充开始计：填充期间过去的时间算在窗口里，在途填充读到的旧快照也最多再下发 1 秒。
func TestSnapshotCacheWindowStartsBeforeTheFill(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	calls := 0
	cache := newSnapshotCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		clk.Advance(500 * time.Millisecond)
		fmt.Fprintf(w, "serialization %d", calls)
	}), clk)
	serve := func() string {
		rec := httptest.NewRecorder()
		cache.ServeHTTP(rec, snapshotRequest(http.MethodGet, "connect=v1&encoding=json&message=%7B%7D", "", nil)())
		return rec.Body.String()
	}
	serve()
	clk.Advance(500 * time.Millisecond)
	if got := serve(); got != "serialization 2" {
		t.Fatalf("one second after the fill started: %q, want a new serialization", got)
	}
}

// 外层在调用前设的 Vary 保留，Accept-Encoding 追加在后面：命中与直连 connect 一样。
func TestSnapshotCacheAppendsVary(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	_, direct := h.pub.connectHandler()
	outer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Vary", "Origin")
			next.ServeHTTP(w, r)
		})
	}
	req := snapshotRequest(http.MethodGet, "connect=v1&encoding=json&message=%7B%7D", "", nil)
	vary := func(h http.Handler) []string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req())
		return rec.Header().Values("Vary")
	}
	want := vary(outer(direct))
	cached := outer(newSnapshotCache(direct, h.clk))
	for i := range 2 {
		if got := vary(cached); !slices.Equal(got, want) {
			t.Fatalf("call %d through the cache: Vary %q, want %q", i+1, got, want)
		}
	}
}

// 同一个键在窗口内只进一次处理器：并发的同键请求排队，拿到同一份字节。
func TestSnapshotCacheSerializesOnceForConcurrentRequests(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	var calls atomic.Int64
	allArrived := make(chan struct{})
	cache := newSnapshotCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		<-allArrived
		fmt.Fprintf(w, "serialization %d", n)
	}), clk)
	const n = 16
	var arrived atomic.Int64
	bodies := make(chan string, n)
	for range n {
		go func() {
			arrived.Add(1)
			rec := httptest.NewRecorder()
			cache.ServeHTTP(rec, snapshotRequest(http.MethodGet, "connect=v1&encoding=json&message=%7B%7D", "", nil)())
			bodies <- rec.Body.String()
		}()
	}
	testwait.Until(t, time.Millisecond, func() bool { return arrived.Load() == n }, "only %v of %d requests arrived",
		testwait.When(func() string { return fmt.Sprint(arrived.Load()) }), n)
	close(allArrived)
	for range n {
		select {
		case b := <-bodies:
			if b != "serialization 1" {
				t.Errorf("body %q, want the single serialization", b)
			}
		case <-time.After(testwait.Bound):
			t.Fatal("a request did not finish")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler ran %d times for one key within one window", got)
	}
}

func TestSnapshotCacheKeysWindowAndWhatItStores(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	var calls int
	gzipHonest, fail := true, false
	cache := newSnapshotCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if gzipHonest && negotiatedCompression(r.Header.Get("Accept-Encoding")) == "gzip" {
			w.Header().Set("Content-Encoding", "gzip")
		}
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		fmt.Fprintf(w, "serialization %d", calls)
	}), clk)
	serve := func(method, query, body string, header map[string]string) string {
		rec := httptest.NewRecorder()
		cache.ServeHTTP(rec, snapshotRequest(method, query, body, header)())
		return rec.Body.String()
	}
	const json = "connect=v1&encoding=json&message=%7B%7D"
	acceptGzip := map[string]string{"Accept-Encoding": "gzip"}
	for _, s := range []struct {
		name, method, query, body string
		header                    map[string]string
		want                      string
	}{
		{"json GET", http.MethodGet, json, "", nil, "serialization 1"},
		{"json GET again", http.MethodGet, json, "", nil, "serialization 1"},
		{"proto GET", http.MethodGet, "connect=v1&encoding=proto&base64=1&message=", "", nil, "serialization 2"},
		{"proto GET without base64 flag", http.MethodGet, "encoding=proto&message=", "", nil, "serialization 2"},
		{"gzip GET", http.MethodGet, json, "", acceptGzip, "serialization 3"},
		{"json POST", http.MethodPost, "", "{}", map[string]string{"Content-Type": "application/json"}, "serialization 4"},
		{"proto POST", http.MethodPost, "", "", map[string]string{"Content-Type": "application/proto"}, "serialization 5"},
	} {
		if got := serve(s.method, s.query, s.body, s.header); got != s.want {
			t.Fatalf("%s: %q, want %q", s.name, got, s.want)
		}
	}
	// 窗口是字面值 1 秒，不引用 snapshotTTL：常量改了，这里要红。
	clk.Advance(time.Second - time.Nanosecond)
	if got := serve(http.MethodGet, json, "", nil); got != "serialization 1" {
		t.Fatalf("expired before one second: %q", got)
	}
	clk.Advance(time.Nanosecond)
	if got := serve(http.MethodGet, json, "", nil); got != "serialization 6" {
		t.Fatalf("served after one second: %q", got)
	}
	// connect 实际的压缩与键不符时不入缓存：下一次仍进处理器。
	gzipHonest = false
	clk.Advance(time.Second)
	for _, want := range []string{"serialization 7", "serialization 8"} {
		if got := serve(http.MethodGet, json, "", acceptGzip); got != want {
			t.Fatalf("mismatched compression was cached: %q, want %q", got, want)
		}
	}
	// 失败的响应不入缓存。
	fail = true
	for _, want := range []string{"serialization 9", "serialization 10"} {
		if got := serve(http.MethodGet, "connect=v1&encoding=proto&message=", "", nil); got != want {
			t.Fatalf("failed response was cached: %q, want %q", got, want)
		}
	}
}

// 节点改为非公开之后：已缓存的快照最多再下发 1 秒（字面值，不引用 snapshotTTL），之后不再出现；
// 历史查询不经这层缓存，立即 NotFound；此前没有缓存的键（POST）立即反映改动。
func TestSnapshotCacheWindowAfterNodeTurnsPrivate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "flip")
	h.setPublic(t, id, "flip", true)
	snapshot := jsonQuery("{}")
	before := pubGet(t, h, "GetSnapshot", snapshot, nil)
	if before.status != http.StatusOK || !bytes.Contains(before.body, []byte(`"flip"`)) {
		t.Fatalf("public node missing: %d %s", before.status, before.body)
	}
	h.setPublic(t, id, "flip", false)
	if within := pubGet(t, h, "GetSnapshot", snapshot, nil); !bytes.Equal(within.body, before.body) {
		t.Fatalf("snapshot re-serialized within the window: %s", within.body)
	}
	window := fmt.Sprintf(`{"nodeId":"%d","from":"0","to":"3600"}`, id)
	if got := pubGet(t, h, "QueryMetrics", jsonQuery(window), nil); got.status != http.StatusNotFound {
		t.Fatalf("history of a private node: %d %s", got.status, got.body)
	}
	if got := pubPost(t, h, "GetSnapshot", "{}", nil); bytes.Contains(got.body, []byte(`"flip"`)) {
		t.Fatalf("uncached key still lists the private node: %s", got.body)
	}
	h.clk.Advance(time.Second)
	if after := pubGet(t, h, "GetSnapshot", snapshot, nil); after.status != http.StatusOK || bytes.Contains(after.body, []byte(`"flip"`)) {
		t.Fatalf("private node still served after the window: %d %s", after.status, after.body)
	}
}

// 快照缓存的命中同样计数：限流包在缓存外面。
func TestPublicRateLimitCountsSnapshotCacheHits(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	for i := range 60 {
		if got := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil); got.status != http.StatusOK {
			t.Fatalf("request %d: %d %s", i+1, got.status, got.body)
		}
	}
	if got := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil); got.status != http.StatusTooManyRequests {
		t.Fatalf("cache hit past the burst: %d", got.status)
	}
}
