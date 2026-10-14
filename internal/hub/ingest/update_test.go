package ingest

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/updates"
)

func TestReportRejectsUnboundedUpdateBeforeObservingMetrics(t *testing.T) {
	for _, field := range []string{"reason", "version", "task_error", "task_id", "task_state", "task_version"} {
		t.Run(field, func(t *testing.T) {
			h := newHub(t)
			id, tok := h.node(t)
			req := report(tok, &heronv1.Metrics{})
			req.Msg.Update = &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0", Task: &heronv1.UpdateTask{Id: "0123456789abcdef", Version: "v0.3.0", State: "downloading", ExpiresAt: 1}}
			s := req.Msg.Update
			switch field {
			case "reason":
				s.Reason = strings.Repeat("x", 2049)
			case "version":
				s.Version = strings.Repeat("x", 65)
			case "task_error":
				s.Task.Error = strings.Repeat("x", 2049)
			case "task_id":
				s.Task.Id = strings.Repeat("x", 65)
			case "task_state":
				s.Task.State = "arbitrary"
			case "task_version":
				s.Task.Version = "v0.3.0-rc1"
			}
			_, err := h.client.Report(t.Context(), req)
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("invalid update %s accepted: %v", field, err)
			}
			if _, ok := h.live.Get(id); ok {
				t.Fatal("invalid update altered live metrics")
			}
		})
	}
}

// 只带未知摘要时索要全文；全文与摘要同现或摘要已知时不索要。
func TestReportWantsUnknownUpdate(t *testing.T) {
	hash := func(v uint64) *uint64 { return &v }
	status := &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0", Source: "hub"}
	t.Run("without_updates", func(t *testing.T) {
		h := newHub(t)
		_, tok := h.node(t)
		req := report(tok, &heronv1.Metrics{})
		req.Msg.UpdateHash = hash(7)
		resp, err := h.client.Report(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Msg.WantUpdate {
			t.Fatal("a hub that does not handle node updates asked for the status")
		}
	})
	t.Run("with_updates", func(t *testing.T) {
		h := newHubConfigured(t, filepath.Join(t.TempDir(), "t.db"), func(st *store.Store, clk *clock.Fake) Config {
			return Config{TTL: 30 * time.Second, Updates: updates.New(st, clk, slog.Default(), "v0.3.0")}
		})
		_, tok := h.node(t)
		for _, tc := range []struct {
			name   string
			update *heronv1.UpdateStatus
			hash   *uint64
			want   bool
		}{
			{"digest_before_any_full", nil, hash(7), true},
			{"full_with_digest", status, hash(7), false},
			{"same_digest", nil, hash(7), false},
			{"changed_digest", nil, hash(8), true},
			{"old_agent", nil, nil, false},
		} {
			req := report(tok, &heronv1.Metrics{})
			req.Msg.Update, req.Msg.UpdateHash = tc.update, tc.hash
			// 上报按节点限速：每次前进一个上报间隔，用例之间不撞限速。
			h.clk.Advance(h.svc.Interval())
			resp, err := h.client.Report(t.Context(), req)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if resp.Msg.WantUpdate != tc.want {
				t.Fatalf("%s: want_update=%v, want %v", tc.name, resp.Msg.WantUpdate, tc.want)
			}
		}
	})
}
