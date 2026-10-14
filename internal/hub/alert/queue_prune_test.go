package alert

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
)

func TestQueuePrunedDeliveryDoesNotDelayFreshEvent(t *testing.T) {
	for _, stage := range []string{"read", "begin", "result"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t)
			var calls, sleeps atomic.Int32
			var pruneOnce sync.Once
			prune := func() {
				pruneOnce.Do(func() {
					_, err := f.st.PruneAlertEvents(t.Context(), f.clk.Now().Add(-90*24*time.Hour))
					if err != nil {
						t.Error(err)
					}
				})
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if stage == "result" {
					prune()
				}
			}))
			defer srv.Close()
			c := queueChannel(t, f, srv.URL)
			rule := f.rule(t, offline())
			old, err := f.st.RecordTransition(t.Context(), rule.ID, f.ids[0], store.StateFiring, "", time.Time{},
				store.AlertEvent{At: f.clk.Now().Add(-91 * 24 * time.Hour), Transition: store.TransitionFiring}, []store.DeliveryTarget{{ChannelID: c.ID}})
			must(t, err)
			fresh := queueEvent(t, f, c)
			channels := func() []store.NotifyChannel {
				if stage == "begin" {
					prune()
				}
				return f.e.Channels()
			}
			q := NewQueue(QueueConfig{Sleep: func(context.Context, time.Duration) error {
				sleeps.Add(1)
				return nil
			}}, QueueDeps{Store: f.st, Channels: channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
			q.Enqueue(old)
			q.Enqueue(fresh)
			if stage == "read" {
				prune()
			}
			stop := startQueue(t, q)
			rows := awaitDeliveries(t, f, fresh.ID, allDone)
			stop()
			if !rows[0].OK {
				t.Fatalf("fresh event not delivered: %+v", rows)
			}
			if sleeps.Load() != 0 {
				t.Fatalf("pruned delivery caused failure backoff: %d", sleeps.Load())
			}
			want := int32(1)
			if stage == "result" {
				want = 2
			}
			if calls.Load() != want {
				t.Fatalf("HTTP calls=%d want %d", calls.Load(), want)
			}
		})
	}
}
