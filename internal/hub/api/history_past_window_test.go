package api

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"google.golang.org/protobuf/proto"
)

// 十天前的两小时：只看跨度会选 1m，而 1m 已按默认保留期（7 天）清掉。经两端真实入口，三个历史查询都要选到
// 仍保有窗口起点的 5m 并返回全部分钟的数据；管理端与公开端选级、步长与数据相同（选级的保留期只来自 Store）。
func TestPastWindowHistoryReadsTheRetainedLevelThroughBothServices(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := t.Context()
	a, _ := h.createNode(t, "a")
	b, _ := h.createNode(t, "b")
	saved, err := h.admin.SaveProbeTask(ctx, connect.NewRequest(&heronv1.SaveProbeTaskRequest{
		Task:    &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "192.0.2.1", IntervalS: 60, TimeoutMs: 1000},
		NodeIds: []int64{a, b},
	}))
	if err != nil {
		t.Fatal(err)
	}
	task := saved.Msg.Task.Task.Id
	from := h.clk.Now().Add(-10 * 24 * time.Hour).Truncate(time.Hour).Unix()
	to := from + 2*3600
	minutes := uint32((to - from) / 60)
	var batch metric.Batch
	for ts := from; ts < to; ts += 60 {
		for _, id := range []int64{a, b} {
			bucket := metric.NewBucket()
			bucket.Add(&heronv1.Metrics{CpuPct: proto.Float64(1)})
			batch.Rows = append(batch.Rows, metric.Row{NodeID: id, TS: ts, CoverageStart: from, Bucket: bucket})
			batch.Probes = append(batch.Probes, metric.ProbeRow{NodeID: id, TS: ts, TaskID: task, Bucket: &metric.ProbeBucket{Sent: 1, RttN: 1, RttSumUs: 500, RttMinUs: 500, RttMaxUs: 500}})
		}
	}
	if _, err := h.store.WriteMinuteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := h.store.Prune(ctx); err != nil || n == 0 {
		t.Fatalf("prune removed %d rows, err %v; the 1m rows ten days back must be gone", n, err)
	}

	type answer struct {
		level string
		step  uint32
		n     uint32
	}
	sent := func(samples []*heronv1.ProbeSample) uint32 {
		var n uint32
		for _, s := range samples {
			n += s.Sent
		}
		return n
	}
	pub := h.publicClient()
	var answers [2][3]answer
	for i, c := range []historyClient{h.admin, pub} {
		m, err := c.QueryMetrics(ctx, connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: a, From: from, To: to}))
		if err != nil {
			t.Fatal(err)
		}
		var cpu uint32
		for _, s := range m.Msg.Series {
			if s.Name == "cpu" {
				for _, sample := range s.Samples {
					cpu += sample.N
				}
			}
		}
		answers[i][0] = answer{m.Msg.Level, m.Msg.StepS, cpu}
		p, err := c.QueryProbes(ctx, connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: a, From: from, To: to}))
		if err != nil {
			t.Fatal(err)
		}
		var probes uint32
		for _, s := range p.Msg.Series {
			probes += sent(s.Samples)
		}
		answers[i][1] = answer{p.Msg.Level, p.Msg.StepS, probes}
	}
	adminCmp, err := h.admin.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{TaskId: task, NodeIds: []int64{a, b}, From: from, To: to}))
	if err != nil {
		t.Fatal(err)
	}
	pubCmp, err := pub.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{TaskId: task, NodeIds: []int64{a, b}, From: from, To: to}))
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range []*heronv1.QueryProbeComparisonResponse{adminCmp.Msg, pubCmp.Msg} {
		if len(r.Series) != 2 || sent(r.Series[0].Samples) != sent(r.Series[1].Samples) {
			t.Fatalf("comparison series = %+v", r.Series)
		}
		answers[i][2] = answer{r.Level, r.StepS, sent(r.Series[0].Samples)}
	}
	for i, side := range []string{"admin", "public"} {
		for j, query := range []string{"QueryMetrics", "QueryProbes", "QueryProbeComparison"} {
			if got := answers[i][j]; got.level != "5m" || got.n != minutes {
				t.Errorf("%s %s = level %s step %d samples %d, want 5m with %d samples", side, query, got.level, got.step, got.n, minutes)
			}
		}
	}
	if answers[0] != answers[1] {
		t.Errorf("admin and public answered differently: %+v vs %+v", answers[0], answers[1])
	}
}
