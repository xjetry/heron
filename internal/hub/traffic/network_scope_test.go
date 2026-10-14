package traffic

import (
	"strings"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

func scopeCounters(epoch string, rx uint64) *heronv1.Metrics {
	m := counters("same-boot", rx, rx*2)
	m.NetCounterEpoch = epoch
	return m
}

func TestNetworkScopeBaselineSurvivesFlushAndReload(t *testing.T) {
	st := newMem()
	b, _ := newBook(t, st)
	a, z := strings.Repeat("a", 64), strings.Repeat("b", 64)
	b.Account(1, scopeCounters(a, 100), nil)
	if d, ok := b.Account(1, scopeCounters(a, 110), nil); !ok || d != (Delta{10, 20}) {
		t.Fatalf("same scope delta=%v %v", d, ok)
	}
	if d, ok := b.Account(1, scopeCounters(z, 10000), nil); ok || d != (Delta{}) {
		t.Fatalf("scope switch counted old interface counters: %v %v", d, ok)
	}
	if err := b.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	b, _ = newBook(t, st)
	if d, ok := b.Account(1, scopeCounters(z, 10020), nil); !ok || d != (Delta{20, 40}) {
		t.Fatalf("reload lost scope baseline: %v %v", d, ok)
	}
	for _, epoch := range []string{"", a, ""} {
		if d, ok := b.Account(1, scopeCounters(epoch, 20000), nil); ok || d != (Delta{}) {
			t.Fatalf("legacy/new transition counted delta: %v %v", d, ok)
		}
		if d, ok := b.Account(1, scopeCounters(epoch, 20001), nil); !ok || d != (Delta{1, 2}) {
			t.Fatalf("stable legacy/new scope failed to count: %v %v", d, ok)
		}
	}
	got, _ := b.Get(1)
	if got.TotalRx != 33 || got.TotalTx != 66 {
		t.Fatalf("total includes scope jumps: %+v", got)
	}
}
