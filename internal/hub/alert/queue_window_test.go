package alert

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

// gatedSleep 在 gate 关闭之前让每次等待都挂起到被唤醒（ctx 取消，不推进时钟）；关闭之后推进夹具时钟并立即返回。
func gatedSleep(f *fixture, gate <-chan struct{}) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
		f.clk.Advance(d)
		return nil
	}
}

func awaitWindow(t *testing.T, q *Queue, ready, waiting int) {
	t.Helper()
	testwait.Until(t, time.Millisecond, func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return len(q.ready) == ready && len(q.waiting) == waiting && q.current == 0
	}, "window never reached ready=%d waiting=%d", ready, waiting)
}

// 窗口被等待中的批次占满时，发往另一个渠道的新批次挤掉一个等待时刻能复原的批次，立即发出，不等那个渠道腾出位置；
// 被挤掉的批次随后由补货装回，照原来的时刻发出。两种可复原的等待各测一次：节奏空位与已写进库的 Retry-After。
func TestFullWaitingWindowDoesNotHoldOtherChannels(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rate  int
		reply func(n int, w http.ResponseWriter)
	}{
		{"rate", 1, nil},
		{"retry_after", 20, func(n int, w http.ResponseWriter) {
			if n < 2 {
				w.Header().Set("Retry-After", "300")
				w.WriteHeader(http.StatusTooManyRequests)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			base, tg := newTelegramServer(t, f, tc.reply)
			var mu sync.Mutex
			var hooked []time.Time
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				hooked = append(hooked, f.clk.Now())
				mu.Unlock()
			}))
			defer srv.Close()
			c := telegramChannel(t, f, tc.rate)
			tgEvents := recordBatches(t, f, c, 3)
			hookEvent := recordBatches(t, f, queueChannel(t, f, srv.URL), 1)[0]
			gate := make(chan struct{})
			q := NewQueue(QueueConfig{TelegramBase: base, Sleep: gatedSleep(f, gate)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
			q.limit = 2
			start := f.clk.Now()
			waitingNow := 1
			if tc.name == "retry_after" {
				waitingNow = 2
			}
			q.Enqueue(tgEvents[0])
			q.Enqueue(tgEvents[1])
			stop := startQueue(t, q)
			awaitWindow(t, q, 0, waitingNow)
			if waitingNow == 1 {
				q.Enqueue(tgEvents[2])
				awaitWindow(t, q, 0, 2)
			}
			q.Enqueue(hookEvent)
			testwait.Until(t, time.Millisecond, func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(hooked) == 1
			}, "webhook batch was not sent while the window held waiting telegram batches")
			close(gate)
			awaitSettled(t, f)
			stop()
			mu.Lock()
			defer mu.Unlock()
			if !hooked[0].Equal(start) {
				t.Fatalf("webhook delivered at %v, want immediately at %v", hooked[0], start)
			}
			if tc.name == "rate" {
				for i, m := range tg.messages() {
					if want := start.Add(time.Duration(i) * time.Minute); !m.at.Equal(want) {
						t.Errorf("telegram message %d at %v, want %v: an evicted batch must keep its rate slot", i, m.at, want)
					}
				}
			} else {
				for _, m := range tg.messages()[2:] {
					if m.at.Before(start.Add(300 * time.Second)) {
						t.Errorf("telegram retry at %v, before its Retry-After: an evicted batch must keep its persisted time", m.at)
					}
				}
			}
		})
	}
}

// 满窗口的挤出顺序：先就绪批次，再节奏等待，再已写进库的重试等待，同类里先挤最晚到期的；结果没写进库的重试等待
// （时刻只在内存里）不挤，这时新批次留在库里并记 Warn。
func TestEnqueueEvictionOrder(t *testing.T) {
	f := newFixture(t)
	var logs bytes.Buffer
	q := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: slog.New(slog.NewJSONHandler(&logs, nil))})
	q.limit = 5
	put := func(b int64, w waitEntry) { q.waiting[b], q.active[b] = w, 1 }
	put(1, waitEntry{at: time.Hour, kind: waitUnrecorded})
	put(2, waitEntry{at: time.Minute, kind: waitRetry})
	put(3, waitEntry{at: 2 * time.Minute, kind: waitRetry})
	put(4, waitEntry{at: time.Second, kind: waitRate})
	put(5, waitEntry{at: 2 * time.Second, kind: waitRate})
	for _, want := range []int64{5, 4, 3, 2} {
		if !q.enqueue(100+want, 1, true) {
			t.Fatalf("enqueue refused while batch %d could be put back", want)
		}
		q.mu.Lock()
		_, still := q.waiting[want]
		// 新批次改记成不可挤出的等待，窗口保持满，只看下一次挤谁。
		q.ready = nil
		q.waiting[100+want] = waitEntry{kind: waitUnrecorded}
		q.mu.Unlock()
		if still {
			t.Fatalf("batch %d not evicted; waiting=%v", want, q.waiting)
		}
	}
	q.overflow = false
	if q.enqueue(200, 1, true) {
		t.Fatalf("enqueue evicted an unrecorded retry; waiting=%v ready=%v", q.waiting, q.ready)
	}
	if !q.overflow || len(q.waiting) != 5 || !strings.Contains(logs.String(), "delivery batch left for refill") {
		t.Fatalf("overflow=%v waiting=%v logs=%s", q.overflow, q.waiting, logs.String())
	}
}

// 窗口已满、ready 为空时不补货：装不进任何一项，只会空转读库。补货的信号留着，窗口腾出位置后再补。
func TestNextDoesNotRefillFullWindow(t *testing.T) {
	f := newFixture(t)
	q := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	q.limit = 2
	later := f.clk.Mono() + time.Minute
	q.waiting[1], q.active[1] = waitEntry{at: later, kind: waitRate}, 1
	q.waiting[2], q.active[2] = waitEntry{at: later, kind: waitRate}, 1
	q.overflow = true
	if _, refill, until, timed := q.next(); refill || !timed || until != later || !q.overflow {
		t.Fatalf("next on a full window: refill=%v until=%v timed=%v overflow=%v", refill, until, timed, q.overflow)
	}
	delete(q.waiting, 2)
	delete(q.active, 2)
	if _, refill, _, _ := q.next(); !refill || q.overflow {
		t.Fatalf("next with room: refill=%v overflow=%v", refill, q.overflow)
	}
}

// 同一渠道的多个批次等同一个节奏空位时，空位打开前谁也不读库：读批次的次数等于发送次数。
func TestRateLimitedBatchesAreNotReadBeforeTheirSlot(t *testing.T) {
	f := newFixture(t)
	base, tg := newTelegramServer(t, f, nil)
	c := telegramChannel(t, f, 1)
	events := recordBatches(t, f, c, 5)
	var sleeps []time.Duration
	q := NewQueue(QueueConfig{TelegramBase: base, Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	var reads atomic.Int32
	read := q.readBatch
	q.readBatch = func(ctx context.Context, batch int64) (store.DeliveryBatch, error) {
		reads.Add(1)
		return read(ctx, batch)
	}
	for _, ev := range events {
		q.Enqueue(ev)
	}
	stop := startQueue(t, q)
	awaitSettled(t, f)
	stop()
	if n := len(tg.messages()); n != 5 || reads.Load() != 5 {
		t.Fatalf("sent %d messages with %d batch reads, want 5 and 5", n, reads.Load())
	}
}

// 空闲的 worker 被入队唤醒，包括紧接在一次不挂起就返回的等待之后：唤醒不经过会被后一次空闲吞掉的中间协程。
func TestEnqueueWakesIdleWorkerAfterImmediateSleep(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1)%2 == 1 {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c := queueChannel(t, f, srv.URL)
	var sleeps []time.Duration
	q := NewQueue(QueueConfig{Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	stop := startQueue(t, q)
	defer stop()
	for i := range 50 {
		// 每个批次先失败一次：worker 为它的重试做一次立即返回的等待，送达后进入无限期的空闲，等下一次入队。
		ev := recordBatches(t, f, c, 1)[0]
		q.Enqueue(ev)
		deadline := time.Now().Add(5 * time.Second)
		for {
			saved, err := f.st.GetAlertEvent(t.Context(), ev.ID)
			must(t, err)
			if saved.Deliveries[0].Done {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("iteration %d: enqueued batch never delivered; the idle worker missed the wake-up", i)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// 入队落在 next 之后、idle 登记空闲之前：这时没有可取消的空闲，入队只记下 woken，idle 看到它就不睡。否则 worker
// 要睡到最早的到期时刻（没有等待批次时是下一次入队）才看到窗口里已有的批次。
func TestIdleReturnsAtOnceAfterEnqueueSinceNext(t *testing.T) {
	f := newFixture(t)
	var slept []time.Duration
	q := NewQueue(QueueConfig{Sleep: func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	// 一个一小时后才到期的等待批次让 next 给出有期限的空闲：idle 若不看 woken，就会经 q.sleep 睡向那一刻。批次号取
	// 库里这个用例不会分配到的值，入队的新批次才不会被当成它去重。
	q.waiting[999], q.active[999] = waitEntry{at: f.clk.Mono() + time.Hour, kind: waitRate}, 1
	batch, refill, until, timed := q.next()
	if batch != 0 || refill || !timed {
		t.Fatalf("next=%d refill=%v timed=%v, want a timed idle", batch, refill, timed)
	}
	ev := queueEvent(t, f, queueChannel(t, f, "http://127.0.0.1:1"))
	q.Enqueue(ev)
	q.idle(t.Context(), until, timed)
	if len(slept) != 0 {
		t.Fatalf("idle slept %v although a batch was enqueued after next", slept)
	}
	if b, _, _, _ := q.next(); b != ev.Deliveries[0].BatchID {
		t.Fatalf("next after idle took batch %d, want the enqueued %d", b, ev.Deliveries[0].BatchID)
	}
}

// 读到渠道已删除时，它在 waiting 里的其余批次随即离开窗口，不再占着格子等到原定时刻；它的节奏记录一并丢掉。
func TestDeletedChannelReleasesWaitingBatches(t *testing.T) {
	f := newFixture(t)
	base, _ := newTelegramServer(t, f, func(n int, w http.ResponseWriter) {
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c := telegramChannel(t, f, 1)
	events := recordBatches(t, f, c, 2)
	var mu sync.Mutex
	var sleeps []time.Duration
	q := NewQueue(QueueConfig{TelegramBase: base, Sleep: func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		if len(sleeps) == 0 {
			// 第一个批次正按 Retry-After 等 300 s，第二个等节奏空位 60 s：这时删掉渠道。
			must(t, f.e.DeleteChannel(t.Context(), c.ID))
		}
		sleeps = append(sleeps, d)
		f.clk.Advance(d)
		return nil
	}}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	q.Enqueue(events[0])
	q.Enqueue(events[1])
	stop := startQueue(t, q)
	awaitSettled(t, f)
	awaitWindow(t, q, 0, 0)
	stop()
	mu.Lock()
	defer mu.Unlock()
	if len(sleeps) != 1 || sleeps[0] != time.Minute {
		t.Fatalf("sleeps=%v, want only the 60s rate wait: the Retry-After batch of the deleted channel must leave with it", sleeps)
	}
	if _, kept := q.sent[c.ID]; kept {
		t.Errorf("rate log of deleted channel %d kept: %v", c.ID, q.sent)
	}
	for _, ev := range events {
		saved, err := f.st.GetAlertEvent(t.Context(), ev.ID)
		must(t, err)
		if d := saved.Deliveries[0]; !d.Done || d.Failure != store.FailureChannelDeleted {
			t.Errorf("row of deleted channel=%+v", d)
		}
	}
}

// 窗口容量把在途的批次算在内：它失败后回到 waiting 只是换个位置，ready、waiting 与在途合计始终不超过 limit。
// limit 为 1 时，在途批次之外再入队的批次留在库里，由补货在它离开后装入。
func TestWindowCountsBatchInFlight(t *testing.T) {
	f := newFixture(t)
	entered, gate := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-gate
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c := queueChannel(t, f, srv.URL)
	a, b := queueEvent(t, f, c), queueEvent(t, f, c)
	var sleeps []time.Duration
	q := NewQueue(QueueConfig{Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	q.limit = 1
	q.Enqueue(a)
	stop := startQueue(t, q)
	<-entered
	q.Enqueue(b)
	q.mu.Lock()
	size, overflow := q.size(), q.overflow
	q.mu.Unlock()
	close(gate)
	if size != 1 || !overflow {
		t.Fatalf("window with one batch in flight took another: size=%d overflow=%v", size, overflow)
	}
	awaitDeliveries(t, f, a.ID, allDone)
	awaitDeliveries(t, f, b.ID, allDone)
	stop()
}
