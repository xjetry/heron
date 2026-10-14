package ingest

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

func TestDiagnosticsAdmissionRejectsBeforeUpdatingLive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		epoch string
		d     *heronv1.AgentDiagnostics
	}{
		{"epoch", "invalid-epoch", nil},
		{"interval", "", &heronv1.AgentDiagnostics{ReportIntervalMs: 1}},
		{"component", "", &heronv1.AgentDiagnostics{FailedCollectors: []heronv1.CollectionComponent{99}}},
		{"policy", "", &heronv1.AgentDiagnostics{NetInclude: []string{"eth*"}, NetExclude: []string{"lo"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub(t)
			id, token := h.node(t)
			req := report(token, &heronv1.Metrics{NetCounterEpoch: tc.epoch})
			req.Msg.Facts = &heronv1.Facts{Diagnostics: tc.d}
			_, err := h.client.Report(t.Context(), req)
			if connect.CodeOf(err) != connect.CodeInvalidArgument || (tc.name == "epoch" && !strings.Contains(err.Error(), "net_counter_epoch")) {
				t.Fatalf("invalid %s admitted: %v", tc.name, err)
			}
			if _, ok := h.live.Get(id); ok {
				t.Fatal("rejected diagnostics changed live state")
			}
		})
	}
}
