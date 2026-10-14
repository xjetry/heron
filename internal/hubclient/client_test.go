package hubclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
)

// 以下测试直接对 connect-go 的实际行为下结论（本仓库 go.mod 固定的版本），换版本要重跑而不是改断言。

// bodyLimit 是多数用例给 New 的正文上限，取 agent 那一档的值。
const bodyLimit = 64 << 10

func report(t *testing.T, c heronv1connect.AgentServiceClient) error {
	t.Helper()
	req := connect.NewRequest(&heronv1.ReportRequest{})
	req.Header().Set("Authorization", "Bearer tok")
	_, err := c.Report(context.Background(), req)
	return err
}

func protoResponder(t *testing.T, msg proto.Message, gz bool) http.HandlerFunc {
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/proto")
		if gz {
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			zw.Write(b)
			zw.Close()
			return
		}
		w.Write(b)
	}
}

func oversizedResponse() *heronv1.ReportResponse {
	return &heronv1.ReportResponse{Tasks: &heronv1.ProbeTasks{Tasks: []*heronv1.ProbeTask{{Target: strings.Repeat("a", bodyLimit)}}}}
}

func TestClientRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(protoResponder(t, oversizedResponse(), false))
	defer srv.Close()
	err := report(t, New(srv.URL, 5*time.Second, bodyLimit))
	if err == nil || !strings.Contains(err.Error(), "hub response body exceeds 65536 bytes") {
		t.Fatalf("err = %v, want the body limit", err)
	}
}

// 上限是 New 的参数而不是包里的常量：同一个构造给更大的上限时，同一份响应必须照常通过。
func TestClientBodyLimitIsAParameter(t *testing.T) {
	srv := httptest.NewServer(protoResponder(t, oversizedResponse(), false))
	defer srv.Close()
	if err := report(t, New(srv.URL, 5*time.Second, 1<<30)); err != nil {
		t.Fatalf("a client with a larger limit rejected the response: %v", err)
	}
}

// 对照组：上限之内的响应照常解码，说明上一个测试红在大小上而不是别的解码失败上。
func TestClientAcceptsResponseWithinLimit(t *testing.T) {
	resp := &heronv1.ReportResponse{ReportIntervalMs: 10000, Tasks: &heronv1.ProbeTasks{Tasks: []*heronv1.ProbeTask{{Target: strings.Repeat("a", bodyLimit-64)}}}}
	srv := httptest.NewServer(protoResponder(t, resp, false))
	defer srv.Close()
	if err := report(t, New(srv.URL, 5*time.Second, bodyLimit)); err != nil {
		t.Fatal(err)
	}
}

// 客户端不声明任何压缩，hub 不顾声明回 gzip 时报错而不是解压。
func TestClientRefusesCompressedResponse(t *testing.T) {
	var accept atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept.Store(r.Header.Get("Accept-Encoding") + "|" + r.Header.Get("Connect-Accept-Encoding"))
		protoResponder(t, &heronv1.ReportResponse{ReportIntervalMs: 10000}, true)(w, r)
	}))
	defer srv.Close()
	err := report(t, New(srv.URL, 5*time.Second, bodyLimit))
	if err == nil {
		t.Fatal("a gzip response was accepted")
	}
	if a := accept.Load().(string); strings.Contains(a, "gzip") {
		t.Fatalf("request advertised compression: %q", a)
	}
}

// allocDuring 返回 f 运行期间的堆分配总量。
func allocDuring(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// gzipBomb 是解压后 n 字节的零，压缩比约千倍。
func gzipBomb(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	chunk := make([]byte, 1<<20)
	for i := 0; i < n/len(chunk); i++ {
		zw.Write(chunk)
	}
	zw.Close()
	return buf.Bytes()
}

const bombBytes = 256 << 20

// 原始字节在上限之内、解压后数十 MiB 的响应不会被解压：否则一个几十 KiB 的响应就能让客户端分配数十 MiB。
func TestClientBoundsDecompressedResponse(t *testing.T) {
	bomb := gzipBomb(t, 48<<20)
	if len(bomb) >= bodyLimit {
		t.Fatalf("bomb is %d bytes; it must fit under the raw limit to exercise decompression", len(bomb))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/proto")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(bomb)
	}))
	defer srv.Close()
	var err error
	alloc := allocDuring(func() { err = report(t, New(srv.URL, 30*time.Second, bodyLimit)) })
	if err == nil {
		t.Fatal("a gzip response was accepted")
	}
	if alloc > 16<<20 {
		t.Fatalf("allocated %d MiB decoding a %d KiB gzip response; the decompressed size is not bounded", alloc>>20, len(bomb)>>10)
	}
}

// 错误响应的正文同样受限：hub 可以回非 200 状态并附一个巨大的正文，或一个解压后巨大的正文。
func TestClientBoundsErrorBody(t *testing.T) {
	bomb := gzipBomb(t, 48<<20)
	for _, tc := range []struct {
		name string
		gz   bool
	}{{"plain", false}, {"gzip", true}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if tc.gz {
				w.Header().Set("Content-Encoding", "gzip")
				w.WriteHeader(http.StatusInternalServerError)
				w.Write(bomb)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"code":"internal","message":"`)
			chunk := strings.Repeat("a", 1<<20)
			for i := 0; i < bombBytes>>20; i++ {
				if _, err := io.WriteString(w, chunk); err != nil {
					return
				}
			}
			io.WriteString(w, `"}`)
		}))
		var err error
		alloc := allocDuring(func() { err = report(t, New(srv.URL, 30*time.Second, bodyLimit)) })
		srv.Close()
		if err == nil {
			t.Fatalf("%s: a 500 response must fail", tc.name)
		}
		if alloc > 16<<20 {
			t.Fatalf("%s: allocated %d MiB reading an error body; the error body is not bounded", tc.name, alloc>>20)
		}
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var reached atomic.Bool
	var leaked atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		leaked.Store(r.Header.Get("Authorization"))
		protoResponder(t, &heronv1.ReportResponse{ReportIntervalMs: 10000}, false)(w, r)
	}))
	defer target.Close()
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+r.URL.Path, code)
		}))
		err := report(t, New(origin.URL, 5*time.Second, bodyLimit))
		origin.Close()
		if err == nil || !strings.Contains(err.Error(), "does not follow redirects") {
			t.Fatalf("%d: err = %v, want the redirect refusal", code, err)
		}
		if reached.Load() {
			t.Fatalf("%d: redirect was followed (Authorization %q reached the target)", code, leaked.Load())
		}
	}
}

// 响应头同样受限：hub 可以在正文之前先送一个巨大的头。对照组是上限之内的头照常通过。
func TestClientBoundsResponseHeaders(t *testing.T) {
	for _, tc := range []struct {
		size int
		ok   bool
	}{{maxResponseHeaderBytes / 2, true}, {maxResponseHeaderBytes * 2, false}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Pad", strings.Repeat("a", tc.size))
			protoResponder(t, &heronv1.ReportResponse{ReportIntervalMs: 10000}, false)(w, r)
		}))
		err := report(t, New(srv.URL, 5*time.Second, bodyLimit))
		srv.Close()
		if (err == nil) != tc.ok {
			t.Fatalf("header of %d bytes: err = %v, want ok=%v", tc.size, err, tc.ok)
		}
	}
}
