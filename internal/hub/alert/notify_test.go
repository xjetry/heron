package alert

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
)

func messageForTest() Message {
	return Message{Rule: "rule", Node: "node", Kind: "offline", Transition: "firing", Value: 12.5, At: time.Unix(123, 0), Summary: "line\n\"quoted\""}
}

func wantRetry(t *testing.T, err error, want bool) {
	t.Helper()
	if err == nil {
		t.Fatalf("error=nil, want a failure with retryable=%v", want)
	}
	if _, retry := Classify(err); retry != want {
		t.Fatalf("error=%v retryable=%v, want %v", err, retry, want)
	}
}

func TestWebhookRendersTemplateAndHeaders(t *testing.T) {
	for _, custom := range []bool{false, true} {
		name := "default"
		if custom {
			name = "custom"
		}
		t.Run(name, func(t *testing.T) {
			var method, header, contentType, body string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method, header, contentType = r.Method, r.Header.Get("X-Probe"), r.Header.Get("Content-Type")
				b, _ := io.ReadAll(r.Body)
				body = string(b)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			cfg := WebhookConfig{URL: srv.URL, Method: "PATCH", Headers: map[string]string{"X-Probe": "custom"}}
			if custom {
				cfg.BodyTemplate = `{"node":{{json .Node}}}`
			}
			c, err := NewWebhook(cfg, outbound.NewClient(NotifyTimeout))
			must(t, err)
			must(t, c.Send(t.Context(), messageForTest()))
			if method != "PATCH" || header != "custom" || contentType != "application/json" {
				t.Fatalf("method=%q header=%q content-type=%q", method, header, contentType)
			}
			var got map[string]any
			must(t, json.Unmarshal([]byte(body), &got))
			if got["node"] != "node" {
				t.Fatalf("node body=%s", body)
			}
			if custom && len(got) != 1 {
				t.Fatalf("custom template ignored: %s", body)
			}
			if !custom && (got["rule"] != "rule" || got["kind"] != "offline" || got["transition"] != "firing" || got["value"] != 12.5 || got["at"] != float64(123) || got["summary"] != messageForTest().Summary) {
				t.Fatalf("default body=%s", body)
			}
		})
	}
}

func TestWebhookRefusesRedirectAndClassifies(t *testing.T) {
	for _, status := range []int{302, 400, 408, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/redirected" {
					w.WriteHeader(200)
					return
				}
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "failure body")
			}))
			defer srv.Close()
			c, err := NewWebhook(WebhookConfig{URL: srv.URL, Method: "POST"}, outbound.NewClient(NotifyTimeout))
			must(t, err)
			err = c.Send(t.Context(), messageForTest())
			wantRetry(t, err, status == 408 || status == 429 || status >= 500)
			if calls.Load() != 1 || !strings.Contains(err.Error(), "failure body") || !strings.Contains(err.Error(), http.StatusText(status)) {
				t.Fatalf("calls=%d error=%v", calls.Load(), err)
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
		defer srv.Close()
		defer close(release)
		client := outbound.NewClient(NotifyTimeout)
		if client.Timeout != 10*time.Second {
			t.Fatalf("timeout=%s", client.Timeout)
		}
		// 这是故意的短客户端超时，用来逼出超时重试，不是等一件事发生的上界。
		client.Timeout = 20 * time.Millisecond
		c, err := NewWebhook(WebhookConfig{URL: srv.URL, Method: "POST"}, client)
		must(t, err)
		wantRetry(t, c.Send(t.Context(), messageForTest()), true)
	})
}

func TestTelegramPostsSendMessage(t *testing.T) {
	var path, method, contentType string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method, contentType = r.URL.Path, r.Method, r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&body)
	}))
	defer srv.Close()
	c := NewTelegram(TelegramConfig{BotToken: "secret", ChatID: "chat"}, outbound.NewClient(NotifyTimeout), srv.URL)
	must(t, c.Send(t.Context(), messageForTest()))
	if path != "/botsecret/sendMessage" || method != "POST" || contentType != "application/json" || body["chat_id"] != "chat" || !strings.Contains(body["text"], messageForTest().Summary) {
		t.Fatalf("path=%q method=%q type=%q body=%v", path, method, contentType, body)
	}
}

func TestTelegramErrorBodyIsTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, strings.Repeat("x", 200)+"SECRET_TAIL")
	}))
	defer srv.Close()
	err := NewTelegram(TelegramConfig{BotToken: "token", ChatID: "chat"}, outbound.NewClient(NotifyTimeout), srv.URL).Send(t.Context(), messageForTest())
	wantRetry(t, err, false)
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), strings.Repeat("x", 200)) || strings.Contains(err.Error(), "SECRET_TAIL") {
		t.Fatalf("error=%v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type countedBody struct {
	io.ReadCloser
	read   *atomic.Int64
	closed *atomic.Bool
}

func (b countedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read.Add(int64(n))
	return n, err
}
func (b countedBody) Close() error { b.closed.Store(true); return b.ReadCloser.Close() }

func TestResponseBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, strings.Repeat("x", 1<<20)) }))
	defer srv.Close()
	var read atomic.Int64
	var closed atomic.Bool
	client := outbound.NewClient(NotifyTimeout)
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err == nil {
			resp.Body = countedBody{resp.Body, &read, &closed}
		}
		return resp, err
	})
	c, err := NewWebhook(WebhookConfig{URL: srv.URL, Method: "POST"}, client)
	must(t, err)
	must(t, c.Send(t.Context(), messageForTest()))
	if read.Load() != 64<<10 || !closed.Load() {
		t.Fatalf("read=%d closed=%v", read.Load(), closed.Load())
	}
}

func TestParseChannelSharesValidation(t *testing.T) {
	for _, config := range []string{`{"url":"bad"}`, `{"url":"https://example.invalid","method":"GET"}`, `{"url":"https://example.invalid","body_template":"{{.Wrong}}"}`, `{"url":"https://example.invalid","headers":{"X":"bad\r\n"}}`} {
		row := store.NotifyChannel{Name: "webhook", Kind: store.ChannelWebhook, Config: config}
		_, err := ParseChannel(row, outbound.NewClient(NotifyTimeout), "")
		validationErr := CheckChannel(row)
		if !errors.Is(err, ErrInvalid) || validationErr == nil || err.Error() != validationErr.Error() {
			t.Fatalf("validation differs: %v", err)
		}
	}
}

func TestTemplateRuntimeFailureIsNotRetryable(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer srv.Close()
	c, err := NewWebhook(WebhookConfig{URL: srv.URL, Method: "POST", BodyTemplate: `{{if eq .Node "missing"}}{{.Missing}}{{else}}{{.Node}}{{end}}`}, outbound.NewClient(NotifyTimeout))
	must(t, err)
	m := messageForTest()
	m.Node = "missing"
	wantRetry(t, c.Send(context.Background(), m), false)
	if calls.Load() != 0 {
		t.Fatalf("template error sent %d requests", calls.Load())
	}
}

func TestDeliveryConstructionRequiresNormalizedMethod(t *testing.T) {
	cfg := WebhookConfig{URL: "https://example.invalid"}
	b, err := json.Marshal(cfg)
	must(t, err)
	row := store.NotifyChannel{Name: "hook", Kind: store.ChannelWebhook, Config: string(b)}
	must(t, CheckChannel(row))
	for _, makeChannel := range []func() (Channel, error){
		func() (Channel, error) { return NewWebhook(cfg, outbound.NewClient(NotifyTimeout)) },
		func() (Channel, error) { return ParseChannel(row, outbound.NewClient(NotifyTimeout), "") },
	} {
		_, err := makeChannel()
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "method") {
			t.Fatalf("empty method accepted: %v", err)
		}
	}
}
