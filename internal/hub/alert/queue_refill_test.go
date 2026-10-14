package alert

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

// 窗口取 8 而不是 QueueCap：性质是"满窗口溢出之后会补货"，与窗口多大无关；QueueCap+1 次真实 HTTP 投递在高负载的
// 全量 -race 下跑不进等待上界。
const refillWindow = 8

func TestQueueRefillsOverflowWithinProcess(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprint(running), func(t *testing.T) {
			f := newFixture(t)
			gate, entered := make(chan struct{}), make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(gate) })
			var mu sync.Mutex
			received := map[string]int{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var msg receivedMessage
				if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				mu.Lock()
				first := len(received) == 0
				received[msg.Summary]++
				mu.Unlock()
				if running && first {
					close(entered)
					select {
					case <-gate:
					case <-r.Context().Done():
					}
				}
			}))
			defer func() { release.Do(func() { close(gate) }); srv.Close() }()
			c := queueChannel(t, f, srv.URL)
			rule := f.rule(t, offline())
			count := refillWindow + 1
			if running {
				count++
			}
			events := make([]store.AlertEvent, count)
			for i := range events {
				ev, err := f.st.RecordTransition(t.Context(), rule.ID, f.ids[0], store.StateFiring, "", time.Time{},
					store.AlertEvent{At: f.clk.Now(), Transition: store.TransitionFiring, Summary: fmt.Sprint(i)}, []store.DeliveryTarget{{ChannelID: c.ID}})
				must(t, err)
				events[i] = ev
			}
			q := NewQueue(QueueConfig{Limit: refillWindow}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
			var stop func()
			if running {
				q.Enqueue(events[0])
				stop = startQueue(t, q)
				defer stop()
				select {
				case <-entered:
				case <-time.After(testwait.Bound):
					t.Fatal("worker did not enter HTTP")
				}
				// 多个生产者可以在 worker 等待网络时填满窗口；每个事件仍只产生一次请求。
				var producers sync.WaitGroup
				for p := 0; p < 8; p++ {
					producers.Go(func() {
						for i := p + 1; i < len(events); i += 8 {
							q.Enqueue(events[i])
						}
					})
				}
				producers.Wait()
				release.Do(func() { close(gate) })
			} else {
				must(t, q.Requeue(t.Context()))
				stop = startQueue(t, q)
				defer stop()
			}
			// 全部送达即结束；上界只作为失败界，不参与“会不会补货”这个性质。
			testwait.Until(t, 10*time.Millisecond, func() bool {
				pending, err := f.st.PendingBatches(t.Context())
				must(t, err)
				return len(pending) == 0
			}, "overflow deliveries not refilled: pending=%s", testwait.When(func() string {
				pending, err := f.st.PendingBatches(t.Context())
				if err != nil {
					return err.Error()
				}
				return fmt.Sprint(len(pending))
			}))
			stop()
			mu.Lock()
			defer mu.Unlock()
			if len(received) != count {
				t.Fatalf("HTTP unique deliveries=%d want %d", len(received), count)
			}
			for _, ev := range events {
				if received[ev.Summary] != 1 {
					t.Errorf("delivery %s sent %d times", ev.Summary, received[ev.Summary])
				}
				saved, err := f.st.GetAlertEvent(t.Context(), ev.ID)
				must(t, err)
				if len(saved.Deliveries) != 1 || !saved.Deliveries[0].OK || !saved.Deliveries[0].Done || saved.Deliveries[0].Attempts != 1 {
					t.Errorf("delivery did not succeed exactly once: %+v", saved.Deliveries)
				}
			}
		})
	}
}

func TestQueueDeduplicatesQueuedAndInflightIDs(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprint(running), func(t *testing.T) {
			f := newFixture(t)
			entered, gate := make(chan struct{}), make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(gate) })
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-gate:
				case <-r.Context().Done():
				}
			}))
			defer func() { release.Do(func() { close(gate) }); srv.Close() }()
			c := queueChannel(t, f, srv.URL)
			ev := queueEvent(t, f, c)
			q := NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
			q.Enqueue(ev)
			if !running {
				q.Enqueue(ev)
				must(t, q.Requeue(t.Context()))
				assertQueueIDs(t, q, []int64{ev.Deliveries[0].BatchID}, 0)
				return
			}
			stop := startQueue(t, q)
			defer stop()
			select {
			case <-entered:
			case <-time.After(testwait.Bound):
				t.Fatal("worker did not enter HTTP")
			}
			q.Enqueue(ev)
			must(t, q.Requeue(t.Context()))
			assertQueueIDs(t, q, nil, ev.Deliveries[0].BatchID)
			release.Do(func() { close(gate) })
			awaitDeliveries(t, f, ev.ID, allDone)
			stop()
			assertQueueIDs(t, q, nil, 0)
		})
	}
}

// worker 被 HTTP 门挡住或尚未启动时检查窗口，不靠时间窗口推测是否已经出队。
func assertQueueIDs(t *testing.T, q *Queue, want []int64, inflight int64) {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	seen := make(map[int64]bool)
	if inflight != 0 {
		seen[inflight] = true
	}
	ids := slices.Clone(q.ready)
	for _, id := range append(slices.Clone(q.ready), slices.Collect(maps.Keys(q.waiting))...) {
		if seen[id] {
			t.Fatalf("duplicate queued/inflight id %d", id)
		}
		seen[id] = true
	}
	if !slices.Equal(ids, want) {
		t.Errorf("queued IDs=%v want %v", ids, want)
	}
	if len(q.active) != len(seen) {
		t.Errorf("active IDs=%v want %v", q.active, seen)
	}
	for id := range seen {
		if _, ok := q.active[id]; !ok {
			t.Errorf("active ID %d missing", id)
		}
	}
}
