package ingest

import (
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
	"google.golang.org/protobuf/proto"
)

// 墙钟快 8 小时时收到的上报，按快钟打点进了 8 小时之后的分钟桶。墙钟校回后的第一次刷出就把它交给写入口，写入口拒绝、
// 一行都不落库；那一分钟真正到来时，库里那一分钟只有那时的上报，两族都没有被快钟时的数据并进去。
func TestFastClockBucketIsRejectedNotMergedWhenItsMinuteArrives(t *testing.T) {
	h := newHub(t)
	id, tok := h.node(t)
	task := h.task(t, id)
	base := h.clk.Now()
	fastMinute := base.Add(8 * time.Hour)
	send := func(cpu float64, rttUs uint32) {
		t.Helper()
		req := report(tok, &heronv1.Metrics{CpuPct: proto.Float64(cpu)})
		req.Msg.TasksDigest, _ = agentwire.TasksDigest(h.reg.TasksFor(id).Tasks)
		req.Msg.ProbeResults = []*heronv1.ProbeResult{rtt(task, 0, rttUs)}
		if _, err := h.client.Report(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	minuteRows := func() ([]metric.Row, []metric.ProbeRow) {
		t.Helper()
		rows, err := h.store.ReadMinuteRows(t.Context(), id, fastMinute.Unix(), fastMinute.Unix()+60)
		if err != nil {
			t.Fatal(err)
		}
		lv, _ := store.LevelByName("1m")
		probes, err := h.store.QueryProbes(t.Context(), id, fastMinute.Unix(), fastMinute.Unix()+60, lv, 60, 0)
		if err != nil {
			t.Fatal(err)
		}
		return rows, probes
	}

	h.clk.SetWall(fastMinute.Add(10 * time.Second))
	send(90, 9000)
	h.clk.SetWall(base.Add(30 * time.Second))
	h.svc.Flush(t.Context(), false)
	if rows, probes := minuteRows(); len(rows) != 0 || len(probes) != 0 {
		t.Fatalf("fast-clock minute was written into the future: rows=%+v probes=%+v", rows, probes)
	}

	h.clk.SetWall(fastMinute.Add(30 * time.Second))
	send(10, 100)
	h.clk.SetWall(fastMinute.Add(60*time.Second + 500*time.Millisecond))
	h.svc.Flush(t.Context(), false)
	rows, probes := minuteRows()
	cpu := metric.Index("cpu")
	if len(rows) != 1 || rows[0].Bucket.N[cpu] != 1 || rows[0].Bucket.Sum[cpu] != 10 {
		t.Fatalf("metric minute = %+v, want only the report made when the minute arrived", rows)
	}
	if len(probes) != 1 || probes[0].Bucket.Sent != 1 || probes[0].Bucket.RttSumUs != 100 {
		t.Fatalf("probe minute = %+v, want only the result measured when the minute arrived", probes)
	}
}
