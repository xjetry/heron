package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

type historyClient interface {
	QueryMetrics(context.Context, *connect.Request[heronv1.QueryMetricsRequest]) (*connect.Response[heronv1.QueryMetricsResponse], error)
	QueryProbes(context.Context, *connect.Request[heronv1.QueryProbesRequest]) (*connect.Response[heronv1.QueryProbesResponse], error)
}

func TestHistoryTailFromReportToBothAPIs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, token := h.createNode(t, "history")
	h.setPublic(t, id, "history", true)
	apiToken, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{id}})
	task, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: validProbeTask(), AllNodes: true}))
	if err != nil {
		t.Fatal(err)
	}
	base := h.clk.Now().Unix()
	report := func(value int) {
		t.Helper()
		req := connect.NewRequest(&heronv1.ReportRequest{
			Metrics:      &heronv1.Metrics{CpuPct: proto.Float64(float64(value))},
			ProbeResults: []*heronv1.ProbeResult{{TaskId: task.Msg.Task.Task.Id, Outcome: &heronv1.ProbeResult_RttUs{RttUs: uint32(value * 100)}}},
		})
		req.Header().Set("Authorization", "Bearer "+token)
		if _, err := h.agent.Report(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 197 {
		for j := 0; j <= i%3; j++ {
			report((i + j) % 90)
		}
		h.clk.Advance(time.Minute)
		h.ingest.Flush(t.Context(), false)
	}
	if err := h.store.Rollup(t.Context()); err != nil {
		t.Fatal(err)
	}
	// 当前分钟仍在 live：历史末桶只能包含前 197 个已刷出分钟。
	report(99)
	for _, client := range []struct {
		name   string
		client historyClient
	}{{"admin_token", apiToken}, {"public", h.publicClient()}} {
		for _, window := range []int64{86400, 10 * 86400} {
			t.Run(fmt.Sprintf("%s/%d", client.name, window), func(t *testing.T) {
				to := base + 198*60
				q := &heronv1.QueryMetricsRequest{NodeId: id, From: to - window, To: to, MaxPoints: 200}
				m, err := client.client.QueryMetrics(t.Context(), connect.NewRequest(q))
				if err != nil {
					t.Fatal(err)
				}
				p, err := client.client.QueryProbes(t.Context(), connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: id, From: q.From, To: q.To, MaxPoints: q.MaxPoints}))
				if err != nil {
					t.Fatal(err)
				}
				level := "5m"
				if window > 7*86400 {
					level = "1h"
				}
				if m.Msg.Level != level || p.Msg.Level != level || m.Msg.StepS != p.Msg.StepS || len(m.Msg.Ts) > int(q.MaxPoints) {
					t.Fatalf("query budget/level: metrics=%v probes=%v", m.Msg, p.Msg)
				}
				type expected struct {
					n        uint32
					sum, max float64
					min      float64
				}
				want := map[int64]expected{}
				step := int64(m.Msg.StepS)
				for i := range 197 {
					ts := (base + int64(i)*60) / step * step
					b := want[ts]
					for j := 0; j <= i%3; j++ {
						v := float64((i + j) % 90)
						if b.n == 0 || v < b.min {
							b.min = v
						}
						b.n++
						b.sum += v
						b.max = max(b.max, v)
					}
					want[ts] = b
				}
				var cpu *heronv1.MetricSeries
				for _, series := range m.Msg.Series {
					if series.Name == "cpu" {
						cpu = series
					}
				}
				if cpu == nil || len(m.Msg.Ts) != len(want) || len(cpu.Samples) != len(want) || len(p.Msg.Series) != 1 || len(p.Msg.Series[0].Samples) != len(want) {
					t.Fatalf("tail continuity: metrics=%v probes=%v want points=%d", m.Msg, p.Msg, len(want))
				}
				for i, ts := range m.Msg.Ts {
					b, ok := want[ts]
					ms, ps := cpu.Samples[i], p.Msg.Series[0].Samples[i]
					if !ok || ms.N != b.n || ms.GetMean() != b.sum/float64(b.n) || ms.GetMax() != b.max || ps.Ts != ts || ps.Sent != b.n || ps.Lost != 0 || ps.Errors != 0 || ps.GetRttMeanUs() != uint32(b.sum*100/float64(b.n)) || ps.GetRttMinUs() != uint32(b.min*100) || ps.GetRttMaxUs() != uint32(b.max*100) {
						t.Fatalf("flushed minute truth at %d: metric=%v probe=%v want=%+v", ts, ms, ps, b)
					}
					delete(want, ts)
				}
			})
		}
	}
}
