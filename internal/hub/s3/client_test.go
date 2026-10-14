package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/outbound"
)

var signingTime = time.Date(2026, 9, 28, 1, 2, 3, 0, time.UTC)

func clientFor(t *testing.T, endpoint string) *Client {
	t.Helper()
	c, err := New(Config{Endpoint: endpoint, Bucket: "backups", Region: "auto", AccessKey: "access", Secret: "secret"}, clock.NewFake(signingTime))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// deadline 给每个操作一个截止时间：客户端首字节之后不限时，操作入口拒绝没有截止时间的 context。
func deadline(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// dialTo 让任意主机名的请求都连到 srv：虚拟主机寻址与带端口的 endpoint 不必真的解析。
func dialTo(c *Client, srv *httptest.Server) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	c.http.Transport = tr
	return tr
}

func TestObjectOperations(t *testing.T) {
	for _, virtual := range []bool{false, true} {
		t.Run(fmt.Sprint(virtual), func(t *testing.T) {
			var methods []string
			key := "config/a//../sp ace+雪%2F.db"
			payload := []byte("a real database body")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				methods = append(methods, r.Method)
				wantPath, wantHost := "/base/backups/"+key, "s3.example"
				if virtual {
					wantPath, wantHost = "/base/"+key, "backups.s3.example"
				}
				if r.URL.Path != wantPath || r.Host != wantHost {
					t.Errorf("object address = %s %s, want %s %s", r.Host, r.URL.Path, wantHost, wantPath)
				}
				if got := r.Header.Get("X-Amz-Date"); got != "20260928T010203Z" {
					t.Errorf("x-amz-date = %q", got)
				}
				wantHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
				if r.Method == "PUT" {
					wantHash = fmt.Sprintf("%x", sha256.Sum256(payload))
				}
				if got := r.Header.Get("X-Amz-Content-Sha256"); got != wantHash {
					t.Errorf("payload hash = %q, want %q", got, wantHash)
				}
				if !regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=access/20260928/auto/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=[0-9a-f]{64}$`).MatchString(r.Header.Get("Authorization")) {
					t.Errorf("authorization structure = %q", r.Header.Get("Authorization"))
				}
				if r.Method == "PUT" {
					got, err := io.ReadAll(r.Body)
					if err != nil || !bytes.Equal(got, payload) {
						t.Errorf("uploaded body = %q, %v", got, err)
					}
				}
				if r.Method == "GET" {
					_, _ = w.Write(payload)
				} else {
					w.WriteHeader(204)
				}
			}))
			defer srv.Close()
			c := clientFor(t, "http://s3.example/base")
			c.cfg.VirtualHost = virtual
			defer dialTo(c, srv).CloseIdleConnections()
			body := bytes.NewReader(append([]byte("skip"), payload...))
			_, _ = body.Seek(4, io.SeekStart)
			if err := c.PutObject(deadline(t), key, body); err != nil {
				t.Fatal(err)
			}
			var dst bytes.Buffer
			if err := c.GetObject(deadline(t), key, &dst, int64(len(payload))); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(dst.Bytes(), payload) {
				t.Errorf("download = %q, want %q", dst.Bytes(), payload)
			}
			if err := c.DeleteObject(deadline(t), key); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(methods, ","); got != "PUT,GET,DELETE" {
				t.Fatalf("methods = %q", got)
			}
		})
	}
}

// closeRecorder 是调用方最常见的正文形态（快照临时文件）的替身：可 Seek，记录是否被关闭。
type closeRecorder struct {
	*bytes.Reader
	closed atomic.Bool
}

func (c *closeRecorder) Close() error { c.closed.Store(true); return nil }

// 第一次 503 可重试；调用方的正文必须仍然打开，Seek 回起点后用它重试成功。
func TestPutObjectLeavesBodyOpenForRetry(t *testing.T) {
	payload := []byte("sqlite snapshot")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("retried upload = %q, want %q", got, payload)
		}
	}))
	defer srv.Close()
	c := clientFor(t, srv.URL)
	body := &closeRecorder{Reader: bytes.NewReader(payload)}
	err := c.PutObject(deadline(t), "config/a.db", body)
	var e *Error
	if !errors.As(err, &e) || e.StatusCode != 503 || !e.Retryable() {
		t.Fatalf("first upload = %v, want retryable HTTP 503", err)
	}
	if body.closed.Load() {
		t.Fatal("PutObject closed the caller's body")
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := c.PutObject(deadline(t), "config/a.db", body); err != nil {
		t.Fatalf("retry with the same body: %v", err)
	}
	if body.closed.Load() || calls.Load() != 2 {
		t.Fatalf("after retry: closed=%v server calls=%d", body.closed.Load(), calls.Load())
	}
}

// countingReader 记录底层被读了多少次，用来判断 PutObject 返回之后还有没有人读它。
type countingReader struct {
	io.ReadSeeker
	reads atomic.Int64
}

func (r *countingReader) Read(p []byte) (int, error) { r.reads.Add(1); return r.ReadSeeker.Read(p) }

// lateTransport 按 RoundTripper 约定允许的方式行事：立即以 503 应答，在 RoundTrip 返回之后才在另一个协程里读正文。
type lateTransport struct {
	release chan struct{}
	done    chan struct{}
}

func (tr *lateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	go func() {
		defer close(tr.done)
		<-tr.release
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}()
	return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}, nil
}

func TestPutObjectDoesNotReadBodyAfterReturning(t *testing.T) {
	c := clientFor(t, "http://s3.example")
	tr := &lateTransport{release: make(chan struct{}), done: make(chan struct{})}
	c.http.Transport = tr
	body := &countingReader{ReadSeeker: bytes.NewReader(bytes.Repeat([]byte("x"), 1<<20))}
	if err := c.PutObject(deadline(t), "k", body); err == nil {
		t.Fatal("503 accepted")
	}
	before := body.reads.Load()
	close(tr.release)
	<-tr.done
	if after := body.reads.Load(); after != before {
		t.Fatalf("the caller's reader was read %d more times after PutObject returned", after-before)
	}
}

// 空对象走 Content-Length: 0，不走 chunked：S3 不收不带 aws-chunked 声明的 chunked 上传。
func TestPutObjectEmptyBodySendsContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			t.Errorf("empty upload: Content-Length %d, Transfer-Encoding %v", r.ContentLength, r.TransferEncoding)
		}
	}))
	defer srv.Close()
	if err := clientFor(t, srv.URL).PutObject(deadline(t), "empty", bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
}

func TestListPagination(t *testing.T) {
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("prefix") != "a +/" || q.Get("list-type") != "2" || q.Get("max-keys") != fmt.Sprint(listPageKeys) || q.Get("encoding-type") != "url" {
			t.Errorf("list query = %v", q)
		}
		token := q.Get("continuation-token")
		tokens = append(tokens, token)
		if token == "" {
			// encoding-type=url 是表单编码：+ 是空格，%2B 才是字面的 +。
			fmt.Fprint(w, `<ListBucketResult><EncodingType>url</EncodingType><IsTruncated>true</IsTruncated><NextContinuationToken>a+/=</NextContinuationToken>`+
				`<Contents><Key>a%20%2B%2Fone</Key><Size>7</Size><LastModified>2026-09-28T01:02:03Z</LastModified></Contents>`+
				`<Contents><Key>a+b%2Bc</Key><Size>1</Size></Contents></ListBucketResult>`)
		} else {
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>a +/two</Key><Size>8</Size></Contents></ListBucketResult>`)
		}
	}))
	defer srv.Close()
	objects, err := clientFor(t, srv.URL).ListObjectsV2(deadline(t), "a +/", 3)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range objects {
		keys = append(keys, o.Key)
	}
	if got := strings.Join(keys, "|"); got != "a +/one|a b+c|a +/two" || objects[0].Size != 7 || objects[0].LastModified.Format(time.RFC3339) != "2026-09-28T01:02:03Z" {
		t.Errorf("joined pages = %q %+v", got, objects)
	}
	if strings.Join(tokens, ",") != ",a+/=" {
		t.Fatalf("continuation tokens = %q", tokens)
	}
}

// 前缀下的对象多于调用方给的上界时，列举在超过上界的那一页停下并报错，不把其余的页翻完。
// 假端点 11 页、每页 2 个对象：上界 5 在第 3 页超出。
func TestListStopsAtMaxObjects(t *testing.T) {
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := pages.Add(1)
		fmt.Fprintf(w, `<ListBucketResult><IsTruncated>%v</IsTruncated><NextContinuationToken>t%d</NextContinuationToken><Contents><Key>a%d</Key></Contents><Contents><Key>b%d</Key></Contents></ListBucketResult>`, n < 11, n, n, n)
	}))
	defer srv.Close()
	c := clientFor(t, srv.URL)
	_, err := c.ListObjectsV2(deadline(t), "", 5)
	var e *Error
	if !errors.As(err, &e) || e.Kind != "response" || !strings.Contains(err.Error(), "listing exceeds 5 objects") || pages.Load() != 3 {
		t.Fatalf("endless listing: %v after %d pages", err, pages.Load())
	}
	if _, err := c.ListObjectsV2(deadline(t), "", 0); !errors.As(err, &e) || e.Kind != "request" || pages.Load() != 3 {
		t.Fatalf("maxObjects 0: %v", err)
	}
}

func TestCanonicalQueryOrdersByKeyThenValue(t *testing.T) {
	// 期望值是 botocore S3SigV4Auth._canonical_query_string_url 对 ?a-b=1&a=2&a=1 的结果（awscli 2.37.0 自带）。
	if got := canonicalQuery(url.Values{"a-b": {"1"}, "a": {"2", "1"}}); got != "a=1&a=2&a-b=1" {
		t.Fatalf("canonical query = %q", got)
	}
}

func TestHTTPFailuresAndNoRedirect(t *testing.T) {
	for _, status := range []int{301, 403, 404, 408, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			targetHits := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits++ }))
			defer target.Close()
			// 300 个字符的多字节正文：原文取前 200 个字符，按字节截断会切在字符中间、得到非法 UTF-8。
			body := strings.Repeat("错", 300)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				fmt.Fprint(w, body)
			}))
			defer srv.Close()
			err := clientFor(t, srv.URL).DeleteObject(deadline(t), "credential-in-path")
			if targetHits != 0 {
				t.Errorf("redirect leaked request: %d target hits", targetHits)
			}
			var e *Error
			if !errors.As(err, &e) || e.Kind != "http_status" || e.StatusCode != status {
				t.Fatalf("HTTP classification = %#v", err)
			}
			if !utf8.ValidString(e.Detail) || utf8.RuneCountInString(e.Detail) > outbound.SummaryChars || e.Detail != strings.Repeat("错", outbound.SummaryChars) {
				t.Fatalf("HTTP failure detail: valid UTF-8 %v, %d characters, %q", utf8.ValidString(e.Detail), utf8.RuneCountInString(e.Detail), e.Detail)
			}
			wantRetry := status == 408 || status == 429 || status >= 500
			if e.Retryable() != wantRetry {
				t.Fatalf("retryable HTTP %d = %v", status, e.Retryable())
			}
		})
	}
	// 传输失败保留"操作: 底层原因"，不含 URL（对象键在 URL 里）。
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	cancel()
	err := clientFor(t, "http://127.0.0.1:1").DeleteObject(ctx, "credential-in-path")
	var e *Error
	if !errors.As(err, &e) || e.Kind != "transport" || !e.Retryable() || err.Error() != "s3 transport: Delete: context canceled" || !errors.Is(err, context.Canceled) {
		t.Fatalf("transport classification = %v", err)
	}
	if errors.Unwrap(err) == nil || strings.Contains(errors.Unwrap(err).Error(), "credential-in-path") {
		t.Fatalf("unwrapped cause = %v", errors.Unwrap(err))
	}
}

// 没有截止时间的 context 在发出请求之前就被拒绝：客户端首字节之后不限时，这样的请求可以无限期挂住。
func TestRequestsRequireContextDeadline(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	c := clientFor(t, srv.URL)
	for name, op := range map[string]func(context.Context) error{
		"put":    func(ctx context.Context) error { return c.PutObject(ctx, "k", strings.NewReader("x")) },
		"get":    func(ctx context.Context) error { return c.GetObject(ctx, "k", io.Discard, 1) },
		"delete": func(ctx context.Context) error { return c.DeleteObject(ctx, "k") },
		"list":   func(ctx context.Context) error { _, err := c.ListObjectsV2(ctx, "", 1); return err },
	} {
		var e *Error
		if err := op(t.Context()); !errors.As(err, &e) || e.Kind != "request" || e.Detail != "context has no deadline" {
			t.Errorf("%s without deadline: %v", name, err)
		}
	}
	if hits != 0 {
		t.Fatalf("%d requests reached the server without a deadline", hits)
	}
}

func TestResponseBounds(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		list       bool
		flush      bool
	}{
		{"oversized list", strings.Repeat("x", listResponseLimit+1), true, true},
		{"invalid XML", "<not-list/>", true, true},
		{"missing token", "<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>", true, true},
		{"repeated token", "<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>same</NextContinuationToken></ListBucketResult>", true, true},
		{"oversized object without length", "too much data", false, true},
		{"oversized object with length", "too much data", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// 先 Flush 就没有 Content-Length，应答以 chunked 发出；不 Flush 时小正文带 Content-Length。
				if tc.flush {
					w.(http.Flusher).Flush()
				}
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			c := clientFor(t, srv.URL)
			var err error
			var dst bytes.Buffer
			if tc.list {
				_, err = c.ListObjectsV2(deadline(t), "", 1000)
			} else {
				err = c.GetObject(deadline(t), "object", &dst, 3)
			}
			var e *Error
			if !errors.As(err, &e) || e.Kind != "response" {
				t.Fatalf("invalid response accepted: %v", err)
			}
			// 声明的长度已超上限时一个字节都不写进 dst。
			if !tc.list && !tc.flush && dst.Len() != 0 {
				t.Fatalf("oversized object with Content-Length wrote %d bytes before failing", dst.Len())
			}
		})
	}
}

func TestConfigurationAndDisabled(t *testing.T) {
	full := Config{Endpoint: "https://s3.example", Bucket: "backups", Region: "auto", AccessKey: "access", Secret: "secret"}
	if !full.Enabled() {
		t.Fatal("complete backup disabled")
	}
	for _, field := range []string{"endpoint", "bucket", "access key", "secret"} {
		c := full
		switch field {
		case "endpoint":
			c.Endpoint = ""
		case "bucket":
			c.Bucket = ""
		case "access key":
			c.AccessKey = ""
		case "secret":
			c.Secret = ""
		}
		if c.Enabled() {
			t.Errorf("backup enabled without %s", field)
		}
		if _, err := New(c, clock.NewFake(signingTime)); err == nil {
			t.Errorf("client constructed without %s", field)
		}
	}
	for _, endpoint := range []string{"ftp://host", "https://user:pass@host", "https://host?a=b", "https://host/#secret", "not a url", "https://例子.example", "http://[fe80::1%25en0]:9000"} {
		c := full
		c.Endpoint = endpoint
		if _, err := New(c, clock.NewFake(signingTime)); err == nil {
			t.Errorf("invalid endpoint accepted: %s", endpoint)
		}
	}
}
