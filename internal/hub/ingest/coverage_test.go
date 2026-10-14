package ingest

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/live"
	"github.com/xjetry/heron/internal/hub/store"
)

func coverageBeats(h *hub, until time.Time) {
	for h.clk.Now().Before(until) {
		h.clk.Advance(min(live.ObservationInterval, until.Sub(h.clk.Now())))
		h.live.ObservationHeartbeat()
	}
}

func TestCoverageFirstBatchRetryDropAndWatermark(t *testing.T) {
	for _, mode := range []string{"retry", "drop", "watermark"} {
		t.Run(mode, func(t *testing.T) {
			h := newHub(t)
			id, token := h.node(t)
			base := h.clk.Now()
			if mode == "watermark" {
				h.clk.Advance(10 * time.Minute)
				if err := h.store.Rollup(t.Context()); err != nil {
					t.Fatal(err)
				}
				h.clk.SetWall(base)
			}
			h.live.SetReceiving(true)
			fw := &failingWriter{Store: h.store, fail: mode != "watermark"}
			h.svc.writer = fw
			h.clk.Advance(30 * time.Second)
			if _, err := h.client.Report(t.Context(), report(token, &heronv1.Metrics{})); err != nil {
				t.Fatal(err)
			}
			coverageBeats(h, base.Add(time.Minute))
			h.svc.Flush(t.Context(), false)
			if starts, err := h.store.CoverageStarts(t.Context()); err != nil || len(starts) != 0 {
				t.Fatal(starts, err)
			}
			minutes := 2
			if mode == "drop" {
				minutes = maxPendingBatches + 2
			}
			if mode == "watermark" {
				minutes = 7
			}
			for n := 2; n <= minutes; n++ {
				coverageBeats(h, base.Add(time.Duration(n)*time.Minute))
				h.svc.Flush(t.Context(), false)
			}
			fw.fail = false
			h.svc.Flush(t.Context(), false)
			starts, err := h.store.CoverageStarts(t.Context())
			if err != nil || starts[id] != base.Unix() {
				t.Fatalf("reception start lost: %v %v", starts, err)
			}
			lv, _ := store.LevelByName("1m")
			_, c, err := h.store.QueryMetricsCoverage(t.Context(), id, base.Unix(), h.clk.Now().Unix(), lv, 60)
			if err != nil || c.ObservedMinutes == 0 || c.ObservedReportedMinutes != 0 {
				t.Fatalf("offline coverage=%+v err=%v", c, err)
			}
			rows, err := h.store.ReadMinuteRows(t.Context(), id, base.Unix(), h.clk.Now().Unix())
			if err != nil {
				t.Fatal(err)
			}
			if mode != "retry" {
				for _, r := range rows {
					if *r.Coverage.Minutes != 0 {
						t.Fatalf("discarded first report survived: %+v", r)
					}
				}
			}
			n, err := h.store.GetNode(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "retry" && !n.LastSeenAt.IsZero() {
				t.Fatalf("pure observation changed last_seen: %+v", n)
			}
			if mode == "retry" && n.LastSeenAt.Unix() != base.Add(30*time.Second).Unix() {
				t.Fatal("last_seen moved", n.LastSeenAt)
			}
		})
	}
}

func TestCoverageRestartAndTenMinuteDowntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	h := newHubAt(t, path)
	id, token := h.node(t)
	base := h.clk.Now()
	h.clk.SetWall(base.Add(-time.Second))
	h.live.SetReceiving(true)
	if _, err := h.client.Report(t.Context(), report(token, &heronv1.Metrics{})); err != nil {
		t.Fatal(err)
	}
	for n := int64(0); n < 10; n++ {
		coverageBeats(h, base.Add(time.Duration(n)*time.Minute+time.Second))
		h.live.Observe(id, "source", &heronv1.Metrics{})
		coverageBeats(h, base.Add(time.Duration(n+1)*time.Minute))
		h.svc.Flush(t.Context(), false)
	}
	lv, _ := store.LevelByName("1m")
	_, before, err := h.store.QueryMetricsCoverage(t.Context(), id, base.Unix(), base.Add(10*time.Minute).Unix(), lv, 60)
	if err != nil || before.ObservedMinutes != 10 || before.ObservedReportedMinutes != 10 {
		t.Fatal(before, err)
	}
	h.live.SetReceiving(false)
	h.svc.Flush(t.Context(), true)
	h.srv.Close()
	h.store.Close()
	restarted := newHubAt(t, path)
	restarted.clk.SetWall(base.Add(20 * time.Minute))
	_, same, err := restarted.store.QueryMetricsCoverage(t.Context(), id, base.Unix(), base.Add(10*time.Minute).Unix(), lv, 60)
	if err != nil || !reflect.DeepEqual(before, same) {
		t.Fatal("restart changed historical coverage", before, same, err)
	}
	_, after, err := restarted.store.QueryMetricsCoverage(t.Context(), id, base.Unix(), base.Add(20*time.Minute).Unix(), lv, 60)
	if err != nil || after.EligibleMinutes != 20 || after.ObservedMinutes != 10 || after.ObservedReportedMinutes != 10 {
		t.Fatal("downtime changed observed ratio instead of unknown minutes", after, err)
	}
}
