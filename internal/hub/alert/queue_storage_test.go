package alert

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

// 外部连接只在故障用例中安装/移除 SQLite 故障；不向 Store 或 Queue 暴露生产钩子。
func deliveryDB(t *testing.T, f *fixture) *sql.DB {
	t.Helper()
	u := url.URL{Path: f.path}
	// DDL 等产品写锁。窗口只为锁久占时能结束，不参与被测性质，所以用正向等待上界，不必与 store 的 busy_timeout 取同一值。
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)", u.EscapedPath(), testwait.Bound.Milliseconds()))
	must(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { must(t, db.Close()) })
	return db
}
func deliverySQL(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	_, err := db.Exec(query)
	must(t, err)
}
func TestAttemptWriteFailureSendsNothing(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer srv.Close()
	ev := queueEvent(t, f, queueChannel(t, f, srv.URL))
	db := deliveryDB(t, f)
	deliverySQL(t, db, "CREATE TRIGGER fail_attempt BEFORE UPDATE ON alert_delivery BEGIN SELECT RAISE(ABORT, 'attempt write blocked'); END")
	q := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	q.Enqueue(ev) // attempt 从窗口取批次所属的渠道。
	_, err := q.attempt(t.Context(), ev.Deliveries[0].BatchID)
	if err == nil || !strings.Contains(err.Error(), "attempt write blocked") {
		t.Fatalf("write failure=%v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("HTTP sent despite failed attempt persistence: %d", calls.Load())
	}
	saved, err := f.st.GetAlertEvent(t.Context(), ev.ID)
	must(t, err)
	if saved.Deliveries[0].Attempts != 0 {
		t.Fatalf("failed transaction consumed attempt: %+v", saved.Deliveries)
	}
}

// 最后一次尝试已发出而结果未知：不沿用更早一次的失败（接收方可能已经收到），也不留下它的原文与状态码。
func TestExhaustedUnrecordedDeliveryBecomesTerminal(t *testing.T) {
	prior := store.DeliveryResult{Failure: store.FailureHTTPStatus, HTTPStatus: 503, Error: "prior error"}
	for _, last := range []*store.DeliveryResult{nil, &prior} {
		name := "unrecorded"
		if last != nil {
			name = "prior_failure"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
			defer srv.Close()
			ev := queueEvent(t, f, queueChannel(t, f, srv.URL))
			for range store.MaxDeliveryAttempts {
				_, err := f.st.BeginBatchAttempt(t.Context(), ev.Deliveries[0].BatchID, []int64{ev.Deliveries[0].ID})
				must(t, err)
			}
			if last != nil {
				must(t, f.st.UpdateBatch(t.Context(), ev.Deliveries[0].BatchID, *last))
			}
			q := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
			q.Enqueue(ev) // attempt 从窗口取批次所属的渠道。
			out, err := q.attempt(t.Context(), ev.Deliveries[0].BatchID)
			must(t, err)
			if out.fate != fateSettled {
				t.Fatalf("exhausted batch not settled: %+v", out)
			}
			saved, err := f.st.GetAlertEvent(t.Context(), ev.ID)
			must(t, err)
			d := saved.Deliveries[0]
			got := store.DeliveryResult{OK: d.OK, Done: d.Done, Failure: d.Failure, HTTPStatus: d.HTTPStatus, Error: d.LastError}
			want := store.DeliveryResult{Done: true, Failure: store.FailureResultUnrecorded}
			if calls.Load() != 0 || d.Attempts != store.MaxDeliveryAttempts || got != want {
				t.Fatalf("exhausted delivery sent=%d row=%+v want %+v", calls.Load(), d, want)
			}
			raw := deliveryDB(t, f)
			var status sql.NullInt64
			var text string
			must(t, raw.QueryRow("SELECT http_status, last_error FROM alert_delivery WHERE id = ?", d.ID).Scan(&status, &text))
			if status.Valid || text != "" {
				t.Fatalf("earlier failure kept: http_status=%v last_error=%q, want NULL and empty", status, text)
			}
		})
	}
}

// 越界状态码不是合法应答：按 transport 重试到次数上限，每次结果都写得进库，不落进存储故障的退避。
func TestMalformedStatusIsRetriedAsTransport(t *testing.T) {
	f := newFixture(t)
	endpoint, requests := rawStatusServer(t, "HTTP/1.1 099 Odd")
	ev := queueEvent(t, f, queueChannel(t, f, endpoint))
	var sleeps []time.Duration
	q := NewQueue(QueueConfig{Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	q.Enqueue(ev)
	stop := startQueue(t, q)
	d := awaitDeliveries(t, f, ev.ID, allDone)[0]
	stop()
	if requests.Load() != store.MaxDeliveryAttempts || d.Attempts != store.MaxDeliveryAttempts || !d.Done || d.OK ||
		d.Failure != store.FailureTransport || d.HTTPStatus != 0 || d.LastError != "malformed HTTP status 99" {
		t.Fatalf("requests=%d delivery=%+v", requests.Load(), d)
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{time.Second, 4 * time.Second}) {
		t.Fatalf("backoff=%v, want the per-attempt backoff only", sleeps)
	}
}

func TestResultWriteFailuresCannotExceedSendBudget(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	counts := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg receivedMessage
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		counts[msg.Summary]++
		excessive := counts[msg.Summary] > store.MaxDeliveryAttempts
		mu.Unlock()
		if excessive {
			cancel()
		}
	}))
	defer srv.Close()
	c := queueChannel(t, f, srv.URL)
	rule := f.rule(t, offline())
	for i := range QueueCap + 1 {
		_, err := f.st.RecordTransition(t.Context(), rule.ID, f.ids[0], store.StateFiring, "", time.Time{}, store.AlertEvent{
			At: f.clk.Now(), Transition: store.TransitionFiring, Summary: fmt.Sprint(i),
		}, []store.DeliveryTarget{{ChannelID: c.ID}})
		must(t, err)
	}
	db := deliveryDB(t, f)
	deliverySQL(t, db, "CREATE TRIGGER fail_result BEFORE UPDATE ON alert_delivery WHEN NEW.done = 1 OR NEW.ok = 1 BEGIN SELECT RAISE(ABORT, 'result write blocked'); END")
	sleeps := 0
	q := NewQueue(QueueConfig{Sleep: func(ctx context.Context, _ time.Duration) error {
		sleeps++
		if sleeps > (QueueCap+1)*(store.MaxDeliveryAttempts+1) {
			cancel()
			return ctx.Err()
		}
		return nil
	}}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	must(t, q.Requeue(t.Context()))
	timer := time.AfterFunc(testwait.Bound, cancel)
	defer timer.Stop()
	q.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	total := 0
	for id, n := range counts {
		total += n
		if n > store.MaxDeliveryAttempts {
			t.Errorf("delivery %s HTTP=%d exceeds maximum %d", id, n, store.MaxDeliveryAttempts)
		}
	}
	if total == 0 || total > (QueueCap+1)*store.MaxDeliveryAttempts {
		t.Fatalf("total HTTP=%d outside send budget %d", total, (QueueCap+1)*store.MaxDeliveryAttempts)
	}
}
