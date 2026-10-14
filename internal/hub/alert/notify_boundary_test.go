package alert

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

func TestOutboundErrorsDoNotExposeCredentials(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cause         error
		token, reason string
	}{
		{"refused", syscall.ECONNREFUSED, "123456:SECRET-TOKEN", "connection refused"},
		{"timeout", context.DeadlineExceeded, "123456:SECRET-TOKEN", "deadline exceeded"},
		{"malformed", syscall.ECONNREFUSED, "SECRET%zz", "invalid URL escape"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			const base = "http://127.0.0.1/private-key"
			client := outbound.NewClient(NotifyTimeout)
			client.Transport = &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return nil, tc.cause }}
			defer client.CloseIdleConnections()
			config, err := json.Marshal(TelegramConfig{BotToken: tc.token, ChatID: "chat"})
			must(t, err)
			c, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "telegram", Kind: store.ChannelTelegram, Config: string(config)})
			must(t, err)
			var sleeps []time.Duration
			q := NewQueue(QueueConfig{TelegramBase: base, Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: client, Clock: f.clk, Log: f.log})
			ev := queueEvent(t, f, c)
			q.Enqueue(ev)
			stop := startQueue(t, q)
			ds := awaitDeliveries(t, f, ev.ID, allDone)
			stop()
			testErr := q.SendTest(t.Context(), c)
			directErr := NewTelegram(TelegramConfig{BotToken: tc.token, ChatID: "chat"}, client, base).Send(t.Context(), messageForTest())
			if testErr == nil || directErr == nil {
				t.Fatal("unreachable target succeeded")
			}
			for source, text := range map[string]string{"last_error": ds[0].LastError, "SendTest": testErr.Error(), "Send": directErr.Error()} {
				if strings.Contains(text, tc.token) || strings.Contains(text, base) || strings.Contains(text, "/sendMessage") {
					t.Errorf("%s credential or URL leaked: %q", source, text)
				}
				if !strings.Contains(text, tc.reason) {
					t.Errorf("%s cause lost: %q", source, text)
				}
			}
		})
	}
}

func TestResponseSummaryIsValidUTF8AndRuneBounded(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"multibyte", "x" + strings.Repeat("网", 230), "x" + strings.Repeat("网", 199)},
		{"invalid", "x\xffy", "x�y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400); _, _ = io.WriteString(w, tc.body) }))
			defer srv.Close()
			c := queueChannel(t, f, srv.URL)
			ev := queueEvent(t, f, c)
			q := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
			q.Enqueue(ev)
			stop := startQueue(t, q)
			ds := awaitDeliveries(t, f, ev.ID, allDone)
			stop()
			if !utf8.ValidString(ds[0].LastError) {
				t.Fatalf("last_error has invalid UTF-8: %q", ds[0].LastError)
			}
			ch, err := ParseChannel(c, outbound.NewClient(NotifyTimeout), "")
			must(t, err)
			err = ch.Send(t.Context(), messageForTest())
			var response *sendFailure
			if !errors.As(err, &response) || response.failure != store.FailureHTTPStatus {
				t.Fatalf("HTTP error=%v", err)
			}
			if response.detail != tc.want || utf8.RuneCountInString(response.detail) > 200 || ds[0].LastError != tc.want {
				t.Fatalf("summary=%q runes=%d stored=%q", response.detail, utf8.RuneCountInString(response.detail), ds[0].LastError)
			}
		})
	}
}

func TestQueueAccepts2xxWithInterruptedBodyOnce(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "short")
	}))
	defer srv.Close()
	ev := queueEvent(t, f, queueChannel(t, f, srv.URL))
	var sleeps []time.Duration
	q := NewQueue(QueueConfig{Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	q.Enqueue(ev)
	stop := startQueue(t, q)
	ds := awaitDeliveries(t, f, ev.ID, allDone)
	stop()
	if calls.Load() != 1 || ds[0].Attempts != 1 || !ds[0].OK || ds[0].LastError != "" {
		t.Fatalf("calls=%d delivery=%+v", calls.Load(), ds[0])
	}
}

func TestQueueDoesNotSendQueuedTerminalDelivery(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer srv.Close()
	c := queueChannel(t, f, srv.URL)
	first, last := queueEvent(t, f, c), queueEvent(t, f, c)
	q := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	q.Enqueue(first)
	if _, err := f.st.BeginBatchAttempt(t.Context(), first.Deliveries[0].BatchID, []int64{first.Deliveries[0].ID}); err != nil {
		t.Fatal(err)
	}
	must(t, f.st.UpdateBatch(t.Context(), first.Deliveries[0].BatchID, store.DeliveryResult{Done: true, Failure: store.FailureHTTPStatus, HTTPStatus: 400, Error: "permanent failure"}))
	q.Enqueue(last)
	stop := startQueue(t, q)
	awaitDeliveries(t, f, last.ID, allDone)
	stop()
	if calls.Load() != 1 {
		t.Fatalf("queued terminal delivery sent: total requests=%d want 1", calls.Load())
	}
}

func TestEnqueueCompletesWithBusyWorkerAndConcurrentProducers(t *testing.T) {
	f := newFixture(t)
	entered := make(chan struct{})
	client := outbound.NewClient(NotifyTimeout)
	// 客户端超时必须长于测试的正向等待，否则 worker 会先被客户端掐断，测不到忙着也不堵生产者。
	client.Timeout = 2 * testwait.Bound
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	c := queueChannel(t, f, "http://127.0.0.1/busy")
	ev := queueEvent(t, f, c)
	q := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: client, Clock: f.clk, Log: f.log})
	q.limit = 2
	q.Enqueue(ev)
	stopWorker := startQueue(t, q)
	select {
	case <-entered:
	case <-time.After(testwait.Bound):
		t.Fatal("worker did not become busy")
	}
	for range q.limit {
		q.Enqueue(ev)
	}
	start, abort, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			<-start
			for range 5000 {
				select {
				case <-abort:
					return
				default:
				}
				q.Enqueue(ev)
			}
		})
	}
	go func() { wg.Wait(); close(done) }()
	// 失败时放走阻塞的生产者；不能让故意破坏互斥的验证把协程遗留给后续测试。
	defer func() {
		close(abort)
		stopWorker()
		<-done
	}()
	close(start)
	select {
	case <-done:
	case <-time.After(testwait.Bound):
		t.Fatal("Enqueue blocked with busy worker and 8 producers")
	}
}
