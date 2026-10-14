package alert

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
)

// 队列落库的类别与 TestNotifyChannel 的错误码都来自 Classify；每条可达的真实失败路径都必须在产生处带上类别，不能落到 unclassified。
func TestEveryChannelFailurePathIsClassified(t *testing.T) {
	respond := func(status int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "receiver said no")
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	rawStatus := func(line string) string {
		endpoint, _ := rawStatusServer(t, line)
		return endpoint
	}
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	webhookRow := func(config string) store.NotifyChannel {
		return store.NotifyChannel{Name: "hook", Kind: store.ChannelWebhook, Config: config}
	}
	send := func(c Channel, err error) error {
		if err != nil {
			return err
		}
		return c.Send(t.Context(), messageForTest())
	}
	sendTelegram := func(token, base string) func() error {
		return func() error {
			return NewTelegram(TelegramConfig{BotToken: token, ChatID: "chat"}, outbound.NewClient(NotifyTimeout), base).Send(t.Context(), messageForTest())
		}
	}
	sendWebhook := func(cfg WebhookConfig) func() error {
		return func() error { return send(NewWebhook(cfg, outbound.NewClient(NotifyTimeout))) }
	}
	parse := func(row store.NotifyChannel) func() error {
		return func() error { return send(ParseChannel(row, outbound.NewClient(NotifyTimeout), "")) }
	}
	for _, tc := range []struct {
		name    string
		run     func() error
		failure store.DeliveryFailure
		status  int
		retry   bool
	}{
		{"telegram/config", parse(store.NotifyChannel{Name: "tg", Kind: store.ChannelTelegram, Config: "not json"}), store.FailureChannelInvalid, 0, false},
		{"telegram/request_url", sendTelegram("SECRET%zz", respond(200)), store.FailureRequest, 0, false},
		{"telegram/transport", sendTelegram("token", closedURL), store.FailureTransport, 0, true},
		{"telegram/401", sendTelegram("token", respond(401)), store.FailureHTTPStatus, 401, false},
		{"telegram/502", sendTelegram("token", respond(502)), store.FailureHTTPStatus, 502, true},
		{"webhook/config", parse(webhookRow(`{"url":"bad","method":"POST"}`)), store.FailureChannelInvalid, 0, false},
		{"webhook/method_not_normalized", parse(webhookRow(`{"url":"https://example.invalid"}`)), store.FailureChannelInvalid, 0, false},
		{"webhook/new_invalid", sendWebhook(WebhookConfig{URL: "bad", Method: "POST"}), store.FailureChannelInvalid, 0, false},
		// 保存时的模板试运行走 else 分支；执行期失败只在 Node 为 missing 时出现。
		{"webhook/template", func() error {
			c, err := NewWebhook(WebhookConfig{URL: respond(200), Method: "POST", BodyTemplate: `{{if eq .Node "missing"}}{{.Missing}}{{else}}{{.Node}}{{end}}`}, outbound.NewClient(NotifyTimeout))
			if err != nil {
				return fmt.Errorf("constructing channel: %w", err)
			}
			m := messageForTest()
			m.Node = "missing"
			return c.Send(t.Context(), m)
		}, store.FailureRequest, 0, false},
		// checkWebhook 已拒绝无法解析的 URL，构造请求失败只能绕过它直接组装渠道来触发。
		{"webhook/request_url", func() error {
			tmpl, err := checkWebhook(WebhookConfig{URL: "https://example.invalid", Method: "POST"})
			if err != nil {
				return fmt.Errorf("constructing channel: %w", err)
			}
			return (&webhook{WebhookConfig{URL: "http://host/%zz", Method: "POST"}, outbound.NewClient(NotifyTimeout), tmpl}).Send(t.Context(), messageForTest())
		}, store.FailureRequest, 0, false},
		{"webhook/transport", sendWebhook(WebhookConfig{URL: closedURL, Method: "POST"}), store.FailureTransport, 0, true},
		{"webhook/302", sendWebhook(WebhookConfig{URL: respond(302), Method: "POST"}), store.FailureHTTPStatus, 302, false},
		{"webhook/400", sendWebhook(WebhookConfig{URL: respond(400), Method: "POST"}), store.FailureHTTPStatus, 400, false},
		{"webhook/408", sendWebhook(WebhookConfig{URL: respond(408), Method: "POST"}), store.FailureHTTPStatus, 408, true},
		{"webhook/429", sendWebhook(WebhookConfig{URL: respond(429), Method: "POST"}), store.FailureHTTPStatus, 429, true},
		{"webhook/500", sendWebhook(WebhookConfig{URL: respond(500), Method: "POST"}), store.FailureHTTPStatus, 500, true},
		// Go 的 HTTP/1.1 客户端接受三位数字的状态行，099 会以 StatusCode 99 交回来。
		{"webhook/status_099", sendWebhook(WebhookConfig{URL: rawStatus("HTTP/1.1 099 Odd"), Method: "POST"}), store.FailureTransport, 0, true},
		{"telegram/status_099", sendTelegram("token", rawStatus("HTTP/1.1 099 Odd")), store.FailureTransport, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatal("failure path succeeded")
			}
			r, retry := Classify(err)
			if r.Failure == store.FailureUnclassified {
				t.Fatalf("failure path left unclassified: %v", err)
			}
			if r.Failure != tc.failure || r.HTTPStatus != tc.status || retry != tc.retry {
				t.Fatalf("classified as %q status=%d retry=%v, want %q status=%d retry=%v (%v)", r.Failure, r.HTTPStatus, retry, tc.failure, tc.status, tc.retry, err)
			}
			wantRetry(t, err, tc.retry)
			if r.Error == "" {
				t.Fatalf("failure path lost its error text: %v", err)
			}
			if tc.failure == store.FailureHTTPStatus {
				want := fmt.Sprintf("HTTP %d %s: receiver said no", tc.status, http.StatusText(tc.status))
				if err.Error() != want || r.Error != "receiver said no" {
					t.Fatalf("error=%q stored=%q, want %q and the body alone", err, r.Error, want)
				}
			}
		})
	}
}

// rawStatusServer 用原始 TCP 回一行任意状态行，httptest 不允许写出 100–999 以外的状态码。
// 返回 URL 与已处理的请求数。
func rawStatusServer(t *testing.T, statusLine string) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var requests atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, req.Body)
				requests.Add(1)
				_, _ = io.WriteString(conn, statusLine+"\r\nContent-Length: 4\r\nConnection: close\r\n\r\nbody")
			}()
		}
	}()
	return "http://" + ln.Addr().String(), &requests
}

func TestMalformedStatusDetailNamesTheStatus(t *testing.T) {
	endpoint, _ := rawStatusServer(t, "HTTP/1.1 099 Odd")
	c, err := NewWebhook(WebhookConfig{URL: endpoint, Method: "POST"}, outbound.NewClient(NotifyTimeout))
	must(t, err)
	r, retry := Classify(c.Send(t.Context(), messageForTest()))
	want := store.DeliveryResult{Failure: store.FailureTransport, Error: "malformed HTTP status 99"}
	if r != want || !retry {
		t.Fatalf("classified as %+v retry=%v, want %+v with retry", r, retry, want)
	}
}

func TestUnclassifiedFailureIsVisibleAndFinal(t *testing.T) {
	r, retry := Classify(errors.New("plain failure"))
	want := store.DeliveryResult{Failure: store.FailureUnclassified, Error: "plain failure"}
	if r != want || retry {
		t.Fatalf("classified as %+v retry=%v, want %+v without retry", r, retry, want)
	}
	// 包了一层的带类别错误照样取得到类别。
	r, retry = Classify(fmt.Errorf("wrapped: %w", &sendFailure{failure: store.FailureTransport, detail: "refused"}))
	if want := (store.DeliveryResult{Failure: store.FailureTransport, Error: "refused"}); r != want || !retry {
		t.Fatalf("wrapped failure classified as %+v retry=%v, want %+v with retry", r, retry, want)
	}
}
