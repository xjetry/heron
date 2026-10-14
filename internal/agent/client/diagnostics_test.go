package client

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"google.golang.org/protobuf/proto"
)

func TestDiagnosticsInvalidFiltersStopBeforeReporting(t *testing.T) {
	for _, tc := range []struct {
		name             string
		include, exclude []string
	}{
		{"net_include", []string{"["}, nil},
		{"net_exclude", nil, []string{"lo\nsecret"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := &fakeHub{interval: 10000}
			r, _ := newRunner(t, hub)
			r.Collector.NetInclude, r.Collector.NetExclude = tc.include, tc.exclude
			r.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
			err := r.Run(t.Context())
			if len(hub.received()) != 0 {
				t.Fatal("invalid filters reached Report before startup rejection")
			}
			if err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("invalid %s startup error = %v", tc.name, err)
			}
		})
	}
}

func TestDiagnosticsReconcileEffectiveIntervalWithoutRepeatedFacts(t *testing.T) {
	hub := &fakeHub{interval: 5000, reconcile: true}
	r, _ := newRunner(t, hub)
	r.Collector.NetInclude = []string{"private-uplink*"}
	round := 0
	r.Sleep = func(context.Context, time.Duration) error {
		round++
		if round == 4 {
			return context.Canceled
		}
		return nil
	}
	if err := r.Run(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	reports := hub.received()
	a, b := reports[0].Facts.GetDiagnostics(), reports[2].Facts.GetDiagnostics()
	if a == nil || b == nil || a.ReportIntervalMs != 10000 || b.ReportIntervalMs != 5000 {
		t.Fatalf("interval did not reconcile: %v / %v", a, b)
	}
	if err := agentwire.ValidateDiagnostics(b); err != nil {
		t.Fatal(err)
	}
	if reports[1].Facts != nil || reports[1].FactsHash == reports[0].FactsHash || reports[3].Facts != nil || reports[3].FactsHash != reports[2].FactsHash {
		t.Fatal("changed diagnostics must reconcile, stable diagnostics must not rewrite facts")
	}
	if len(b.NetInclude) != 1 || b.NetInclude[0] != "private-uplink*" {
		t.Fatalf("effective filters missing: %v", b)
	}
	if proto.Equal(a, &heronv1.AgentDiagnostics{}) {
		t.Fatal("diagnostics unexpectedly empty")
	}
}
