package traffic

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
	"google.golang.org/protobuf/proto"
)

func TestQuotaModes(t *testing.T) {
	for _, tc := range []struct {
		mode        string
		rx, tx      int64
		quota, used uint64
		pct         float64
		has         bool
	}{
		{"sum", 60, 30, 100, 90, 90, true},
		{"rx", 60, 30, 100, 60, 60, true},
		{"tx", 60, 30, 100, 30, 30, true},
		{"max", 60, 30, 100, 60, 60, true},
		{"sum", math.MaxInt64, math.MaxInt64, 0, math.MaxUint64 - 1, 0, false},
		{"sum", 60, 40, 100, 100, 100, true},
	} {
		used, pct, has := Quota(Entry{State: State{PeriodRx: tc.rx, PeriodTx: tc.tx}}, tc.quota, tc.mode)
		if used != tc.used || pct != tc.pct || has != tc.has {
			t.Fatalf("%+v: got %d/%g/%t", tc, used, pct, has)
		}
	}
}

type partialTrafficStore struct{ *memStore }

func (m partialTrafficStore) WriteTraffic(ctx context.Context, recs []store.TrafficRecord) ([]int64, error) {
	var kept []store.TrafficRecord
	for _, r := range recs {
		if r.NodeID == 1 {
			kept = append(kept, r)
		}
	}
	return m.memStore.WriteTraffic(ctx, kept)
}

func TestCommittedExcludesSkippedWrites(t *testing.T) {
	m := newMem()
	b, _ := newBook(t, m)
	b.st = partialTrafficStore{m}
	for _, id := range []int64{1, 2} {
		b.Account(id, &heronv1.Metrics{BootId: "b", NetRxTotal: proto.Uint64(10), NetTxTotal: proto.Uint64(20)}, nil)
	}
	if err := b.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := b.Committed(); len(got) != 1 || base(got[1]).Rx != 10 {
		t.Fatalf("committed includes skipped write: %+v", got)
	}
	if _, err := b.Commit(t.Context(), 2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Commit skipped: %v", err)
	}
}

func TestCommittedPublishesOnlySuccessfulWrites(t *testing.T) {
	m := newMem()
	b, clk := newBook(t, m)
	ctx := t.Context()
	if _, err := b.Adjust(ctx, 1, 90, 10); err != nil {
		t.Fatal(err)
	}
	want := b.View(1).State
	if got := b.Committed()[1]; !reflect.DeepEqual(got, want) {
		t.Fatalf("Adjust committed=%+v want=%+v", got, want)
	}
	b.Account(1, &heronv1.Metrics{BootId: "b", NetRxTotal: proto.Uint64(100), NetTxTotal: proto.Uint64(100)}, nil)
	b.Account(1, &heronv1.Metrics{BootId: "b", NetRxTotal: proto.Uint64(150), NetTxTotal: proto.Uint64(120)}, nil)
	m.fail = errors.New("write failed")
	if err := b.Flush(ctx); err == nil {
		t.Fatal("Flush accepted failed write")
	}
	if got := b.Committed()[1]; !reflect.DeepEqual(got, want) {
		t.Fatalf("failed Flush published %+v, want %+v", got, want)
	}
	if _, err := b.Commit(ctx, 1); err == nil {
		t.Fatal("Commit accepted failed write")
	}
	if got := b.Committed()[1]; !reflect.DeepEqual(got, want) {
		t.Fatalf("failed Commit published %+v, want %+v", got, want)
	}
	m.fail = nil
	if err := b.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	want = b.View(1).State
	if got := b.Committed()[1]; !reflect.DeepEqual(got, want) {
		t.Fatalf("Flush committed=%+v want=%+v", got, want)
	}
	reloaded, _ := newBook(t, m)
	if got := reloaded.Committed()[1]; !reflect.DeepEqual(got, want) {
		t.Fatalf("Load committed=%+v want=%+v", got, want)
	}
	clk.Advance(40 * 24 * time.Hour)
	b.SetResetDay(1, 5)
	b.View(1)
	if got := b.Committed()[1]; !reflect.DeepEqual(got, want) {
		t.Fatalf("read rolled committed=%+v want raw=%+v", got, want)
	}
	if _, err := b.Commit(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := b.Committed()[1]; got.PeriodRx != 0 || got.PeriodTx != 0 || !got.PeriodStart.After(want.PeriodStart) {
		t.Fatalf("Commit failed to publish rollover: %+v", got)
	}
}
