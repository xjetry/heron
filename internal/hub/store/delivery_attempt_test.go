package store

import (
	"errors"
	"sync"
	"testing"
)

func TestBeginBatchAttemptAtomicallyCapsStarts(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline})
	ev := recordEvent(t, s, r.ID, ids[0], []int64{cs[0].ID})
	batch := ev.Deliveries[0].BatchID
	type result struct {
		ds  []Delivery
		err error
	}
	results := make(chan result, 20)
	var wg sync.WaitGroup
	for range cap(results) {
		wg.Go(func() {
			ds, err := s.BeginBatchAttempt(t.Context(), batch, []int64{ev.Deliveries[0].ID})
			results <- result{ds, err}
		})
	}
	wg.Wait()
	close(results)
	starts := map[int]bool{}
	for got := range results {
		if got.err == nil {
			if len(got.ds) != 1 || starts[got.ds[0].Attempts] || got.ds[0].Attempts < 1 || got.ds[0].Attempts > MaxDeliveryAttempts {
				t.Fatalf("invalid or repeated attempt: %+v", got.ds)
			}
			starts[got.ds[0].Attempts] = true
		} else if !errors.Is(got.err, ErrDeliveryExhausted) {
			t.Fatal(got.err)
		}
	}
	if len(starts) != MaxDeliveryAttempts {
		t.Fatalf("starts=%v", starts)
	}
	if _, err := s.BeginBatchAttempt(t.Context(), batch, []int64{ev.Deliveries[0].ID}); !errors.Is(err, ErrDeliveryExhausted) {
		t.Fatalf("exhausted batch err=%v", err)
	}
	if err := s.UpdateBatch(t.Context(), batch, DeliveryResult{Done: true, Failure: FailureResultUnrecorded}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginBatchAttempt(t.Context(), batch, []int64{ev.Deliveries[0].ID}); !errors.Is(err, ErrDeliveryDone) {
		t.Fatalf("terminal batch err=%v", err)
	}
	saved, err := s.GetAlertEvent(t.Context(), ev.ID)
	if d := saved.Deliveries[0]; err != nil || !d.Done || d.Attempts != MaxDeliveryAttempts {
		t.Fatalf("terminal row=%+v err=%v", d, err)
	}
	_, err = s.BeginBatchAttempt(t.Context(), 999, []int64{999})
	assertAlertNotFound(t, err, ObjectAlertDelivery, 999)
}
