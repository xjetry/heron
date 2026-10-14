package alert

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/testwait"
)

const rejectResult = "CREATE TRIGGER reject_result BEFORE UPDATE ON alert_delivery WHEN NEW.done = 1 OR NEW.ok = 1 BEGIN SELECT RAISE(ABORT, 'result write blocked'); END"
const rejectAttempt = "CREATE TRIGGER reject_attempt BEFORE UPDATE ON alert_delivery BEGIN SELECT RAISE(ABORT, 'attempt write blocked'); END"

func TestQueueRetriesNonterminalExitWithoutExternalOverflow(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer srv.Close()
	ev := queueEvent(t, f, queueChannel(t, f, srv.URL))
	db := deliveryDB(t, f)
	deliverySQL(t, db, rejectResult)
	var sleeps []time.Duration
	q := NewQueue(QueueConfig{Sleep: func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		_, err := db.Exec("DROP TRIGGER reject_result")
		return err
	}}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	q.Enqueue(ev)
	stop := startQueue(t, q)
	ds := awaitDeliveries(t, f, ev.ID, allDone)
	stop()
	if !ds[0].OK || ds[0].Attempts != 2 || calls.Load() != 2 || !slices.Equal(sleeps, []time.Duration{time.Second}) {
		t.Fatalf("nonterminal retry row=%+v HTTP=%d sleeps=%v", ds[0], calls.Load(), sleeps)
	}
}

func TestQueueRefillReadFailureRetainsSignal(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer srv.Close()
	c := queueChannel(t, f, srv.URL)
	a, b := queueEvent(t, f, c), queueEvent(t, f, c)
	db := deliveryDB(t, f)
	deliverySQL(t, db, "ALTER TABLE alert_delivery RENAME TO held_deliveries")
	slept := make(chan struct{})
	var sleeps []time.Duration
	q := NewQueue(QueueConfig{Sleep: func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		_, err := db.Exec("ALTER TABLE held_deliveries RENAME TO alert_delivery")
		if len(sleeps) == 1 {
			close(slept)
		}
		return err
	}}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	// 溢出窗口已取空，库中两条待投递仍是真源；首次补货读失败不能消耗续投信号。
	q.overflow = true
	stop := startQueue(t, q)
	select {
	case <-slept:
	case <-time.After(testwait.Bound):
		t.Fatal("refill failure did not sleep")
	}
	awaitDeliveries(t, f, a.ID, allDone)
	awaitDeliveries(t, f, b.ID, allDone)
	stop()
	if calls.Load() != 2 || !slices.Equal(sleeps, []time.Duration{time.Second}) {
		t.Fatalf("refill did not recover once: HTTP=%d sleeps=%v", calls.Load(), sleeps)
	}
}

func TestQueueFailureBackoffCapsAndResetsAfterCompletion(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := queueChannel(t, f, srv.URL)
	first := queueEvent(t, f, c)
	db := deliveryDB(t, f)
	deliverySQL(t, db, rejectAttempt)
	ready := make(chan struct{})
	var sleeps []time.Duration
	var mu sync.Mutex
	q := NewQueue(QueueConfig{Sleep: func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		sleeps = append(sleeps, d)
		n := len(sleeps)
		mu.Unlock()
		switch n {
		case 1:
			if _, err := db.Exec("DROP TRIGGER reject_attempt"); err != nil {
				return err
			}
			_, err := db.Exec("ALTER TABLE alert_delivery RENAME TO held_deliveries")
			return err
		case 2:
			if _, err := db.Exec("ALTER TABLE held_deliveries RENAME TO alert_delivery"); err != nil {
				return err
			}
			_, err := db.Exec(rejectAttempt)
			return err
		case 8, 9:
			_, err := db.Exec("DROP TRIGGER reject_attempt")
			if n == 8 {
				close(ready)
			}
			return err
		}
		return nil
	}}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	q.Enqueue(first)
	stop := startQueue(t, q)
	select {
	case <-ready:
	case <-time.After(testwait.Bound):
		t.Fatal("failure backoff did not reach cap")
	}
	awaitDeliveries(t, f, first.ID, allDone)
	second := queueEvent(t, f, c)
	deliverySQL(t, db, rejectAttempt)
	q.Enqueue(second)
	awaitDeliveries(t, f, second.ID, allDone)
	stop()
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 60 * time.Second, 60 * time.Second, time.Second}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(sleeps, want) {
		t.Fatalf("failure backoff=%v want %v", sleeps, want)
	}
}
