package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
)

// joinOnRead 让 q 每次读到发往 c 的批次之后、开始尝试之前，给它加入一个新节点的转换，至多 len(nodes) 次：模拟补货
// 在一次巡检中途读到尚未收齐的批次。发往其它渠道的批次读时不加。返回加入的事件。
func joinOnRead(t *testing.T, f *fixture, q *Queue, r store.AlertRule, c store.NotifyChannel, nodes []int64) *[]store.AlertEvent {
	t.Helper()
	var mu sync.Mutex
	joined := new([]store.AlertEvent)
	read := q.readBatch
	q.readBatch = func(ctx context.Context, batch int64) (store.DeliveryBatch, error) {
		b, err := read(ctx, batch)
		mu.Lock()
		defer mu.Unlock()
		if err == nil && b.Deliveries[0].ChannelID == c.ID && len(*joined) < len(nodes) {
			ev, err := f.st.RecordTransition(ctx, r.ID, nodes[len(*joined)], store.StateFiring, "", time.Time{},
				store.AlertEvent{At: f.clk.Now(), Transition: store.TransitionFiring, Summary: "节点 joined 离线"}, []store.DeliveryTarget{{ChannelID: c.ID, Batch: batch}})
			if err != nil {
				t.Error(err)
			}
			*joined = append(*joined, ev)
		}
		return b, err
	}
	return joined
}

// 读批次之后才加入的行不能被记进那次发送：开始尝试时行集合变了就重读、重拼，发出的消息覆盖被记成送达的每一行。
func TestAttemptRecordsOnlyRowsItSent(t *testing.T) {
	f := newFixture(t)
	ids := f.nodes(t, 2)
	base, tg := newTelegramServer(t, f, nil)
	c := telegramChannel(t, f, 20)
	r := offlineRuleTo(t, f, "离线", c)
	first := recordBatches(t, f, c, 1)[0]
	var sleeps []time.Duration
	q := NewQueue(QueueConfig{TelegramBase: base, Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	joined := joinOnRead(t, f, q, r, c, ids[1:2])
	q.Enqueue(first)
	stop := startQueue(t, q)
	awaitSettled(t, f)
	stop()
	got := tg.messages()
	if len(got) != 1 || !strings.Contains(got[0].text, "event 00") || !strings.Contains(got[0].text, "节点 joined 离线") {
		t.Fatalf("messages=%+v, want one message covering both rows of the batch", got)
	}
	for _, ev := range append([]store.AlertEvent{first}, *joined...) {
		saved, err := f.st.GetAlertEvent(t.Context(), ev.ID)
		must(t, err)
		if d := saved.Deliveries[0]; d.BatchID != first.Deliveries[0].BatchID || !d.OK || d.Attempts != 1 {
			t.Errorf("row %+v, want delivered once in batch %d", d, first.Deliveries[0].BatchID)
		}
	}
}

// 批次在每次读之后都在长：读到上限（maxBatchReads）是正常去向，不是存储故障。批次离开窗口、由补货装回，worker
// 不退避，也不记 ERROR；同时入队的另一个渠道的批次照常立即发出，不多等一次退避。补货之后批次用收齐的行发出一条。
func TestGrowingBatchYieldsWorkerAtReadBound(t *testing.T) {
	f := newFixture(t)
	ids := f.nodes(t, maxBatchReads+1)
	base, tg := newTelegramServer(t, f, nil)
	var mu sync.Mutex
	var hooked []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hooked = append(hooked, f.clk.Now())
		mu.Unlock()
	}))
	defer srv.Close()
	c := telegramChannel(t, f, 20)
	r := offlineRuleTo(t, f, "离线", c)
	first := recordBatches(t, f, c, 1)[0]
	hookEvent := recordBatches(t, f, queueChannel(t, f, srv.URL), 1)[0]
	var logs bytes.Buffer
	var sleeps []time.Duration
	q := NewQueue(QueueConfig{TelegramBase: base, Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: slog.New(slog.NewJSONHandler(&logs, nil))})
	joinOnRead(t, f, q, r, c, ids[1:])
	start := f.clk.Now()
	q.Enqueue(first)
	q.Enqueue(hookEvent)
	stop := startQueue(t, q)
	awaitSettled(t, f)
	stop()
	got := tg.messages()
	if len(got) != 1 || strings.Count(got[0].text, "节点 joined 离线") != maxBatchReads {
		t.Fatalf("messages=%+v, want one message with the first row and %d joined rows", got, maxBatchReads)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hooked) != 1 || !hooked[0].Equal(start) || len(sleeps) != 0 {
		t.Fatalf("webhook delivered at %v with sleeps=%v, want once at %v and no backoff: the read bound is not a storage failure", hooked, sleeps, start)
	}
	bound := false
	for _, line := range strings.Split(logs.String(), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Level, Msg string
			Reads      int
		}
		must(t, json.Unmarshal([]byte(line), &rec))
		if rec.Level == "ERROR" {
			t.Errorf("read bound logged an error: %s", line)
		}
		if rec.Level == "INFO" && rec.Msg == "delivery batch still growing after repeated reads; left for refill" && rec.Reads == maxBatchReads {
			bound = true
		}
	}
	if !bound {
		t.Errorf("missing the INFO record of the read bound: %s", logs.String())
	}
}

// 429 这次的结果没写进库时，Retry-After 算出的等待照样生效：批次留在窗口里等到那一刻，存储退避另算。
func TestRetryAfterSurvivesResultWriteFailure(t *testing.T) {
	f := newFixture(t)
	base, tg := newTelegramServer(t, f, func(n int, w http.ResponseWriter) {
		if n == 0 {
			w.Header().Set("Retry-After", "300")
			w.WriteHeader(http.StatusTooManyRequests)
		}
	})
	c := telegramChannel(t, f, 20)
	ev := recordBatches(t, f, c, 1)[0]
	db := deliveryDB(t, f)
	deliverySQL(t, db, "CREATE TRIGGER fail_429 BEFORE UPDATE ON alert_delivery WHEN NEW.failure = 'http_status' BEGIN SELECT RAISE(ABORT, 'result write blocked'); END")
	var mu sync.Mutex
	var sleeps []time.Duration
	q := NewQueue(QueueConfig{TelegramBase: base, Sleep: func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		if len(sleeps) == 0 {
			deliverySQL(t, db, "DROP TRIGGER fail_429")
		}
		sleeps = append(sleeps, d)
		f.clk.Advance(d)
		return nil
	}}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	start := f.clk.Now()
	q.Enqueue(ev)
	stop := startQueue(t, q)
	d := awaitDeliveries(t, f, ev.ID, allDone)[0]
	stop()
	got := tg.messages()
	if len(got) != 2 || !got[1].at.Equal(start.Add(300*time.Second)) || !d.OK || d.Attempts != 2 {
		t.Fatalf("requests=%+v row=%+v, want the retry 300s after the 429 despite the lost result", got, d)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sleeps) != 2 || sleeps[0] != time.Second || sleeps[1] != 299*time.Second {
		t.Errorf("sleeps=%v, want the 1s storage backoff and then the rest of the Retry-After", sleeps)
	}
}

// attempt 把结果没写进库的等待记成 waitUnrecorded，它不能被挤出：库里没有这次的 not_before，挤出后补货装回会立即
// 重发，早于接收方要求的 Retry-After。窗口只有一格，429 的结果写失败后、存储退避期间，发往另一个渠道的批次到达：
// 它留在库里，等这个批次按 Retry-After 发完、窗口有空位时才由补货装入。
func TestUnrecordedRetryWaitIsNotEvicted(t *testing.T) {
	f := newFixture(t)
	base, tg := newTelegramServer(t, f, func(n int, w http.ResponseWriter) {
		if n == 0 {
			w.Header().Set("Retry-After", "300")
			w.WriteHeader(http.StatusTooManyRequests)
		}
	})
	var hookMu sync.Mutex
	var hooked []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hookMu.Lock()
		hooked = append(hooked, f.clk.Now())
		hookMu.Unlock()
	}))
	defer srv.Close()
	ev := recordBatches(t, f, telegramChannel(t, f, 20), 1)[0]
	hookEvent := recordBatches(t, f, queueChannel(t, f, srv.URL), 1)[0]
	db := deliveryDB(t, f)
	deliverySQL(t, db, "CREATE TRIGGER fail_429 BEFORE UPDATE ON alert_delivery WHEN NEW.failure = 'http_status' BEGIN SELECT RAISE(ABORT, 'result write blocked'); END")
	var q *Queue
	var mu sync.Mutex
	var sleeps []time.Duration
	q = NewQueue(QueueConfig{TelegramBase: base, Sleep: func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		if len(sleeps) == 0 {
			// 存储退避：429 的等待已在 waiting 里，结果没写进库。
			deliverySQL(t, db, "DROP TRIGGER fail_429")
			q.Enqueue(hookEvent)
		}
		sleeps = append(sleeps, d)
		f.clk.Advance(d)
		return nil
	}}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	q.limit = 1
	start := f.clk.Now()
	q.Enqueue(ev)
	stop := startQueue(t, q)
	awaitSettled(t, f)
	stop()
	got := tg.messages()
	if len(got) != 2 || !got[1].at.Equal(start.Add(300*time.Second)) {
		t.Fatalf("telegram requests=%+v, want the retry 300s after the 429: the unrecorded wait was put back and resent early", got)
	}
	hookMu.Lock()
	defer hookMu.Unlock()
	if len(hooked) != 1 || !hooked[0].Equal(start.Add(300*time.Second)) {
		t.Fatalf("webhook delivered at %v, want once at %v after the telegram batch left the window", hooked, start.Add(300*time.Second))
	}
}

// Retry-After 写进库（not_before）：hub 在等待期间重启，新进程补回的批次仍等到那一刻才重发。
func TestRetryAfterSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	base, tg := newTelegramServer(t, f, func(n int, w http.ResponseWriter) {
		if n == 0 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		}
	})
	c := telegramChannel(t, f, 20)
	ev := recordBatches(t, f, c, 1)[0]
	start := f.clk.Now()
	parked := make(chan struct{})
	var once sync.Once
	q := NewQueue(QueueConfig{TelegramBase: base, Sleep: func(ctx context.Context, d time.Duration) error {
		once.Do(func() { close(parked) })
		<-ctx.Done()
		return ctx.Err()
	}}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	q.Enqueue(ev)
	stop := startQueue(t, q)
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never waited for the Retry-After")
	}
	stop()
	saved, err := f.st.GetAlertEvent(t.Context(), ev.ID)
	must(t, err)
	if d := saved.Deliveries[0]; !d.NotBefore.Equal(start.Add(120 * time.Second)) {
		t.Fatalf("row after the 429=%+v, want not_before 120s later", d)
	}
	f.clk.Advance(20 * time.Second)
	f.restart(t)
	var sleeps []time.Duration
	fresh := NewQueue(QueueConfig{TelegramBase: base, Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	must(t, fresh.Requeue(t.Context()))
	stop = startQueue(t, fresh)
	d := awaitDeliveries(t, f, ev.ID, allDone)[0]
	stop()
	got := tg.messages()
	if len(got) != 2 || !got[1].at.Equal(start.Add(120*time.Second)) || !d.OK || d.Attempts != 2 || !d.NotBefore.IsZero() {
		t.Fatalf("requests=%+v row=%+v, want the retry at the persisted time after the restart", got, d)
	}
}

// Retry-After 只看 429：别的可重试应答带着它也按固定退避。
func TestRetryAfterIgnoredWithout429(t *testing.T) {
	f := newFixture(t)
	base, tg := newTelegramServer(t, f, func(n int, w http.ResponseWriter) {
		if n == 0 {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	c := telegramChannel(t, f, 20)
	ev := recordBatches(t, f, c, 1)[0]
	var sleeps []time.Duration
	q := NewQueue(QueueConfig{TelegramBase: base, Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	start := f.clk.Now()
	q.Enqueue(ev)
	stop := startQueue(t, q)
	awaitDeliveries(t, f, ev.ID, allDone)
	stop()
	if got := tg.messages(); len(got) != 2 || !got[1].at.Equal(start.Add(time.Second)) {
		t.Fatalf("requests=%+v, want the retry after the fixed 1s backoff", got)
	}
}

// HTTP 日期写法的 Retry-After 同样至多计 MaxRetryAfter（秒数写法在解析时截断，日期写法由 retryWait 截断）。
func TestRetryAfterDateIsCapped(t *testing.T) {
	f := newFixture(t)
	start := f.clk.Now()
	base, tg := newTelegramServer(t, f, func(n int, w http.ResponseWriter) {
		if n == 0 {
			w.Header().Set("Retry-After", start.Add(900*time.Second).Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
		}
	})
	c := telegramChannel(t, f, 20)
	ev := recordBatches(t, f, c, 1)[0]
	var sleeps []time.Duration
	q := NewQueue(QueueConfig{TelegramBase: base, Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	q.Enqueue(ev)
	stop := startQueue(t, q)
	awaitDeliveries(t, f, ev.ID, allDone)
	stop()
	if got := tg.messages(); len(got) != 2 || !got[1].at.Equal(start.Add(MaxRetryAfter)) {
		t.Fatalf("requests=%+v, want the retry after MaxRetryAfter", got)
	}
}
