package outbound

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestResponseSummaryIsValidAndAtMostSummaryChars(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"multi-byte", []byte(strings.Repeat("错", 300)), strings.Repeat("错", SummaryChars)},
		{"ascii", []byte(strings.Repeat("a", 250)), strings.Repeat("a", SummaryChars)},
		{"short", []byte("forbidden"), "forbidden"},
		// 连续的非法字节并成一个 U+FFFD，截在第 200 个字符处。
		{"invalid bytes", append([]byte(strings.Repeat("错", 199)), 0xff, 0xfe, 'x'), strings.Repeat("错", 199) + "�"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ResponseSummary(tc.body)
			if !utf8.ValidString(got) || utf8.RuneCountInString(got) > SummaryChars || got != tc.want {
				t.Fatalf("summary: valid %v, %d characters, %q; want %q", utf8.ValidString(got), utf8.RuneCountInString(got), got, tc.want)
			}
		})
	}
}

func TestWithoutURLKeepsOperationAndCause(t *testing.T) {
	err := WithoutURL(&url.Error{Op: "Post", URL: "https://api.telegram.org/bot123:secret/sendMessage", Err: context.Canceled})
	if err.Error() != "Post: context canceled" || !errors.Is(err, context.Canceled) {
		t.Fatalf("stripped error = %q, is canceled %v", err, errors.Is(err, context.Canceled))
	}
	plain := errors.New("not a url error")
	if WithoutURL(plain) != plain {
		t.Fatal("non-URL error was rewritten")
	}
}

// 两个构造函数的客户端各有自己的连接池（为什么见 client.go 里 NewClient 上方的说明）：Transport 不是共享的
// http.DefaultTransport，两次调用各得一份，设置取自 DefaultTransport 的克隆（代理与空闲连接设置相同）。
func TestClientsOwnTheirTransport(t *testing.T) {
	shared := http.DefaultTransport.(*http.Transport)
	for _, c := range []struct {
		name string
		new  func() *http.Client
	}{
		{"NewClient", func() *http.Client { return NewClient(time.Second) }},
		{"NewTransferClient", func() *http.Client { return NewTransferClient(time.Second, time.Second) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			first, second := c.new(), c.new()
			tr, ok := first.Transport.(*http.Transport)
			if !ok || tr == nil {
				t.Fatalf("Transport = %T %v, want a *http.Transport of its own", first.Transport, first.Transport)
			}
			if first.Transport == http.DefaultTransport {
				t.Fatal("Transport is the shared http.DefaultTransport")
			}
			if first.Transport == second.Transport {
				t.Fatal("two clients share one Transport")
			}
			if tr.Proxy == nil || tr.MaxIdleConns != shared.MaxIdleConns || tr.IdleConnTimeout != shared.IdleConnTimeout || tr.ForceAttemptHTTP2 != shared.ForceAttemptHTTP2 {
				t.Errorf("Transport settings: proxy set %v, MaxIdleConns %d, IdleConnTimeout %v, ForceAttemptHTTP2 %v; want those of http.DefaultTransport (%d, %v, %v)",
					tr.Proxy != nil, tr.MaxIdleConns, tr.IdleConnTimeout, tr.ForceAttemptHTTP2, shared.MaxIdleConns, shared.IdleConnTimeout, shared.ForceAttemptHTTP2)
			}
		})
	}
}

func TestZeroTimeoutsAreRejected(t *testing.T) {
	for name, build := range map[string]func(){
		"NewClient":              func() { NewClient(0) },
		"NewTransferClient/conn": func() { NewTransferClient(0, time.Second) },
		"NewTransferClient/ttfb": func() { NewTransferClient(time.Second, 0) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s accepted a zero timeout, which net/http reads as no limit", name)
				}
			}()
			build()
		}()
	}
}

func TestClientsDoNotFollowRedirects(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer srv.Close()
	for name, c := range map[string]*http.Client{"NewClient": NewClient(time.Minute), "NewTransferClient": NewTransferClient(time.Minute, time.Minute)} {
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Errorf("%s: status %d, want the 302 itself", name, resp.StatusCode)
		}
	}
	if hits != 0 {
		t.Fatalf("redirect target was hit %d times", hits)
	}
}

// 传输客户端只限首字节：响应头迟于 firstByte 即失败；响应头按时到达之后，应答体用多久都不受客户端限制。
func TestTransferClientLimitsFirstByteOnly(t *testing.T) {
	const firstByte = 500 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow-headers" {
			time.Sleep(4 * firstByte)
			return
		}
		for range 3 {
			_, _ = io.WriteString(w, "chunk")
			w.(http.Flusher).Flush()
			time.Sleep(firstByte)
		}
	}))
	defer srv.Close()
	c := NewTransferClient(time.Minute, firstByte)
	if resp, err := c.Get(srv.URL + "/slow-headers"); err == nil {
		resp.Body.Close()
		t.Fatal("headers later than firstByte were accepted")
	}
	start := time.Now()
	resp, err := c.Get(srv.URL + "/slow-body")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "chunkchunkchunk" {
		t.Fatalf("slow body = %q, %v", body, err)
	}
	if took := time.Since(start); took < 2*firstByte {
		t.Fatalf("slow body arrived in %v; the case did not exceed firstByte", took)
	}
}
