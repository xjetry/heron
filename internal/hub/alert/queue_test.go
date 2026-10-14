package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testdeps"
	"github.com/xjetry/heron/internal/testwait"
)

type receivedMessage struct {
	Rule, Node, Kind, Transition, Summary string
	Value                                 float64
	At                                    int64
}

func queueChannel(t *testing.T, f *fixture, url string) store.NotifyChannel {
	t.Helper()
	cfg, err := json.Marshal(WebhookConfig{URL: url, Method: "POST"})
	must(t, err)
	c, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "hook", Kind: store.ChannelWebhook, Config: string(cfg)})
	must(t, err)
	return c
}
func queueEvent(t *testing.T, f *fixture, cs ...store.NotifyChannel) store.AlertEvent {
	t.Helper()
	r := f.rule(t, offline())
	var ids []store.DeliveryTarget
	for _, c := range cs {
		ids = append(ids, store.DeliveryTarget{ChannelID: c.ID})
	}
	ev, err := f.st.RecordTransition(t.Context(), r.ID, f.ids[0], store.StateFiring, "", time.Time{}, store.AlertEvent{At: f.clk.Now(), Summary: "persisted summary", Value: 5, Transition: store.TransitionFiring}, ids)
	must(t, err)
	return ev
}

// advancing 是记录等待并推进夹具时钟的 sleep：worker 空闲时按单调钟等到最早的重试时刻，时钟不走它就一直等不到。
// 调用方在 worker 停下之后读 sleeps。
func advancing(f *fixture, sleeps *[]time.Duration) func(context.Context, time.Duration) error {
	var mu sync.Mutex
	return func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		*sleeps = append(*sleeps, d)
		f.clk.Advance(d)
		return nil
	}
}
func startQueue(t *testing.T, q *Queue) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); q.Run(ctx) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testwait.Bound):
			t.Fatal("queue did not stop")
		}
	}
	t.Cleanup(stop)
	return stop
}
func awaitDeliveries(t *testing.T, f *fixture, id int64, ready func([]store.Delivery) bool) []store.Delivery {
	t.Helper()
	var got []store.Delivery
	testwait.Until(t, time.Millisecond, func() bool {
		ev, err := f.st.GetAlertEvent(t.Context(), id)
		must(t, err)
		got = ev.Deliveries
		return ready(got)
	}, "delivery state did not arrive: %s", testwait.When(func() string { return fmt.Sprintf("%+v", got) }))
	return got
}
func allDone(ds []store.Delivery) bool {
	for _, d := range ds {
		if !d.Done {
			return false
		}
	}
	return true
}

func TestQueueDeliversAndRecords(t *testing.T) {
	f := newFixture(t)
	var one, two atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/one" {
			one.Add(1)
			return
		}
		if two.Add(1) == 1 {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	a, b := queueChannel(t, f, srv.URL+"/one"), queueChannel(t, f, srv.URL+"/two")
	ev := queueEvent(t, f, a, b)
	var sleeps []time.Duration
	start := f.clk.Now()
	q := NewQueue(QueueConfig{Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	q.Enqueue(ev)
	stop := startQueue(t, q)
	ds := awaitDeliveries(t, f, ev.ID, allDone)
	stop()
	if one.Load() != 1 || two.Load() != 2 || ds[0].Attempts != 1 || ds[1].Attempts != 2 {
		t.Fatalf("calls=%d/%d deliveries=%+v", one.Load(), two.Load(), ds)
	}
	for i, d := range ds {
		// 第二个渠道在第一次失败后等过 1 s 才成功，送达时刻随之推后。
		at := start.Add(time.Duration(i) * time.Second)
		if !d.OK || !d.Done || d.Failure != store.FailureNone || d.HTTPStatus != 0 || d.LastError != "" || !d.DeliveredAt.Equal(at) {
			t.Fatalf("delivery=%+v, want delivered at %v", d, at)
		}
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{time.Second}) {
		t.Fatalf("backoff=%v", sleeps)
	}
}

func TestQueueGivesUpAfterMaxAttempts(t *testing.T) {
	for _, status := range []int{400, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newFixture(t)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(status) }))
			defer srv.Close()
			ev := queueEvent(t, f, queueChannel(t, f, srv.URL))
			var sleeps []time.Duration
			q := NewQueue(QueueConfig{Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
			q.Enqueue(ev)
			stop := startQueue(t, q)
			ds := awaitDeliveries(t, f, ev.ID, allDone)
			stop()
			want := 1
			var backoffs []time.Duration
			if status == 500 {
				want = store.MaxDeliveryAttempts
				backoffs = []time.Duration{time.Second, 4 * time.Second}
			}
			d := ds[0]
			if d.Attempts != want || calls.Load() != int32(want) || d.OK || !d.Done || d.Failure != store.FailureHTTPStatus || d.HTTPStatus != status || !d.DeliveredAt.IsZero() {
				t.Fatalf("calls=%d delivery=%+v", calls.Load(), d)
			}
			if !reflect.DeepEqual(sleeps, backoffs) {
				t.Fatalf("backoff=%v want %v", sleeps, backoffs)
			}
			// 启动行的 delivery_retry_wait 与 e2e 的等待上限都取 DeliveryRetryWait；这里把它钉到 worker
			// 在可重试的渠道失败下实际等过的总和上，两者分叉时在这里红，而不是让上限静默偏离真实等待。夹具墙钟停在
			// 整秒，not_before 的取整不添等待；墙钟不在整秒时每次重试多等不到 1 s（见 DeliveryRetryWait）。
			if status == 500 {
				var total time.Duration
				for _, d := range sleeps {
					total += d
				}
				if total != DeliveryRetryWait() {
					t.Fatalf("worker waited %v in total between retries, DeliveryRetryWait reports %v", total, DeliveryRetryWait())
				}
			}
			fresh := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
			must(t, fresh.Requeue(t.Context()))
			if len(fresh.ready) != 0 {
				t.Fatalf("terminal delivery requeued: %d", len(fresh.ready))
			}
		})
	}
}

func TestQueueRequeuesPendingOnLoad(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer srv.Close()
	ev := queueEvent(t, f, queueChannel(t, f, srv.URL))
	if _, err := f.st.BeginBatchAttempt(t.Context(), ev.Deliveries[0].BatchID, []int64{ev.Deliveries[0].ID}); err != nil {
		t.Fatal(err)
	}
	must(t, f.st.UpdateBatch(t.Context(), ev.Deliveries[0].BatchID, store.DeliveryResult{Failure: store.FailureTransport, Error: "prior failure"}))
	q := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	must(t, q.Requeue(t.Context()))
	stop := startQueue(t, q)
	ds := awaitDeliveries(t, f, ev.ID, allDone)
	stop()
	if calls.Load() != 1 || ds[0].Attempts != 2 || !ds[0].OK {
		t.Fatalf("calls=%d delivery=%+v", calls.Load(), ds[0])
	}
	fresh := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	must(t, fresh.Requeue(t.Context()))
	if len(fresh.ready) != 0 {
		t.Fatal("successful delivery requeued")
	}
}

func TestQueueDropsOldestWhenFull(t *testing.T) {
	f := newFixture(t)
	var logs bytes.Buffer
	q := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: slog.New(slog.NewJSONHandler(&logs, nil))})
	if q.limit != QueueCap || QueueCap != 256 {
		t.Fatalf("capacity=%d", q.limit)
	}
	q.limit = 2
	for id := int64(1); id <= 3; id++ {
		q.Enqueue(store.AlertEvent{ID: id, Deliveries: []store.Delivery{{ID: id, EventID: id, BatchID: id}}})
	}
	if !slices.Equal(q.ready, []int64{2, 3}) || !q.overflow {
		t.Fatalf("retained=%v overflow=%v", q.ready, q.overflow)
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) || !strings.Contains(logs.String(), `"batch_id":1`) {
		t.Fatalf("missing drop warning: %s", logs.String())
	}
}

func TestSendTestDoesNotRecord(t *testing.T) {
	f := newFixture(t)
	var got receivedMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewDecoder(r.Body).Decode(&got) }))
	defer srv.Close()
	c := queueChannel(t, f, srv.URL)
	q := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	must(t, q.SendTest(t.Context(), c))
	if got.Summary == "" || got.Transition != "test" {
		t.Fatalf("test message=%+v", got)
	}
	if len(f.events(t)) != 0 {
		t.Fatal("test notification persisted event")
	}
	pending, err := f.st.PendingBatches(t.Context())
	must(t, err)
	if len(pending) != 0 {
		t.Fatal("test notification persisted delivery")
	}
}

func TestQueueMissingChannelIsTerminal(t *testing.T) {
	f := newFixture(t)
	c := queueChannel(t, f, "http://127.0.0.1:1")
	ev := queueEvent(t, f, c)
	q := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: func() []store.NotifyChannel { return nil }, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	q.Enqueue(ev)
	stop := startQueue(t, q)
	ds := awaitDeliveries(t, f, ev.ID, allDone)
	stop()
	if d := ds[0]; d.Attempts != 0 || d.OK || d.Failure != store.FailureChannelDeleted || d.LastError != "" {
		t.Fatalf("missing channel delivery=%+v", d)
	}
}

func TestQueueCancellationLeavesPending(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	ev := queueEvent(t, f, queueChannel(t, f, srv.URL))
	sleeping := make(chan struct{})
	q := NewQueue(QueueConfig{Sleep: func(ctx context.Context, d time.Duration) error { close(sleeping); <-ctx.Done(); return ctx.Err() }}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	q.Enqueue(ev)
	stop := startQueue(t, q)
	select {
	case <-sleeping:
	case <-time.After(testwait.Bound):
		t.Fatal("no retry sleep")
	}
	stop()
	ds := awaitDeliveries(t, f, ev.ID, func(ds []store.Delivery) bool { return ds[0].Attempts == 1 })
	if ds[0].Done || ds[0].OK {
		t.Fatalf("cancelled delivery=%+v", ds[0])
	}
	fresh := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	must(t, fresh.Requeue(t.Context()))
	if len(fresh.ready) != 1 {
		t.Fatalf("pending not requeued: %d", len(fresh.ready))
	}
}

func TestQueueMessageUsesCurrentNamesAndHistoricalSummary(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		name := "present"
		if deleted {
			name = "deleted"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			received := make(chan receivedMessage, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var m receivedMessage
				_ = json.NewDecoder(r.Body).Decode(&m)
				received <- m
			}))
			defer srv.Close()
			ev := queueEvent(t, f, queueChannel(t, f, srv.URL))
			if deleted {
				must(t, f.e.DeleteRule(t.Context(), ev.RuleID))
				must(t, f.st.DeleteNode(t.Context(), ev.NodeID))
			}
			q := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
			q.Enqueue(ev)
			stop := startQueue(t, q)
			awaitDeliveries(t, f, ev.ID, allDone)
			stop()
			m := <-received
			rule, node, kind := "离线", "node1", "offline"
			if deleted {
				rule, node, kind = "规则 #1", "节点 #1", ""
			}
			if m.Rule != rule || m.Node != node || m.Kind != kind || m.Summary != ev.Summary || m.Value != ev.Value {
				t.Fatalf("message=%+v", m)
			}
		})
	}
}

// 系统事件的标签与种类按 transition 给出：0/0 只说明事件不属于任何规则×节点，同为 0/0 的系统事件有登录与备份
// 两种。表外的 transition 即使是 0/0 也不冒充系统事件，退回规则事件的编号标签。
func TestQueueMessageKindFollowsTransition(t *testing.T) {
	f := newFixture(t)
	q := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	for _, c := range []struct {
		transition       store.Transition
		rule, node, kind string
	}{
		{store.TransitionLoginSuccess, "系统事件", "Hub", "login"},
		{store.TransitionLoginLocked, "系统事件", "Hub", "login"},
		{store.TransitionBackupFailed, "系统事件", "Hub", "backup"},
		{store.TransitionBackupRecovered, "系统事件", "Hub", "backup"},
		{store.TransitionBackupDisabled, "系统事件", "Hub", "backup"},
		{"unlisted_system", "规则 #0", "节点 #0", ""},
	} {
		m, err := q.message(t.Context(), store.DeliveryBatch{Events: []store.AlertEvent{{Transition: c.transition, At: f.clk.Now()}}})
		if err != nil {
			t.Fatal(err)
		}
		if m.Rule != c.rule || m.Node != c.node || m.Kind != c.kind {
			t.Errorf("0/0 event with transition %q: rule=%q node=%q kind=%q, want %q %q %q", c.transition, m.Rule, m.Node, m.Kind, c.rule, c.node, c.kind)
		}
	}
}

func TestQueueRealSleepCanBeCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sleepContext(ctx, time.Hour); err != context.Canceled {
		t.Fatalf("sleep error=%v", err)
	}
}

func TestNewQueueRequiresEveryDep(t *testing.T) {
	f := newFixture(t)
	valid := QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log}
	testdeps.RequireEveryField(t, "alert.QueueDeps", valid, func(d QueueDeps) { NewQueue(QueueConfig{}, d) })
}
