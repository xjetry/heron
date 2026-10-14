package api

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
)

// 删除节点只删配置层、历史行留给清理作业：删除一提交，被删节点对两端的历史查询立即是 NotFound，对比候选不再含它，
// 对比分块把它列为不可用；其余节点的查询结果在删除前、清理前、清理后逐字相同。待清理作业数经 GetStorageStats 下发。
func TestDeletedNodeVanishesFromHistoryBeforeCleanup(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := t.Context()
	gone, _ := h.createNode(t, "gone")
	kept, _ := h.createNode(t, "kept")
	h.setPublic(t, gone, "gone", true)
	h.setPublic(t, kept, "kept", true)
	saved, err := h.admin.SaveProbeTask(ctx, connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: validProbeTask(), AllNodes: true}))
	if err != nil {
		t.Fatal(err)
	}
	task := saved.Msg.GetTask().GetTask().GetId()
	base := h.clk.Now().Truncate(time.Hour).Add(-2 * time.Hour).Unix()
	var batch metric.Batch
	for m := int64(0); m < 30; m++ {
		for _, node := range []int64{gone, kept} {
			b := metric.NewBucket()
			b.Add(&heronv1.Metrics{CpuPct: proto.Float64(float64(node*10 + m))})
			batch.Rows = append(batch.Rows, metric.Row{NodeID: node, TS: base + m*60, CoverageStart: base, Bucket: b})
			batch.Probes = append(batch.Probes, metric.ProbeRow{NodeID: node, TS: base + m*60, TaskID: task,
				Bucket: &metric.ProbeBucket{Sent: 2, Lost: uint32(m % 2), RttN: 1, RttSumUs: 100, RttMinUs: 100, RttMaxUs: 100}})
		}
	}
	if _, err := h.store.WriteMinuteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	from, to := base, base+3600
	type views struct{ metrics, probes, publicMetrics, publicProbes, comparison proto.Message }
	read := func(node int64) views {
		t.Helper()
		var v views
		m, err := h.admin.QueryMetrics(ctx, connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: node, From: from, To: to}))
		if err != nil {
			t.Fatal(err)
		}
		p, err := h.admin.QueryProbes(ctx, connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: node, From: from, To: to}))
		if err != nil {
			t.Fatal(err)
		}
		pm, err := h.publicClient().QueryMetrics(ctx, connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: node, From: from, To: to}))
		if err != nil {
			t.Fatal(err)
		}
		pp, err := h.publicClient().QueryProbes(ctx, connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: node, From: from, To: to}))
		if err != nil {
			t.Fatal(err)
		}
		c, err := h.admin.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{TaskId: task, NodeIds: []int64{node}, From: from, To: to}))
		if err != nil {
			t.Fatal(err)
		}
		v.metrics, v.probes, v.publicMetrics, v.publicProbes, v.comparison = m.Msg, p.Msg, pm.Msg, pp.Msg, c.Msg
		return v
	}
	same := func(a, b views) bool {
		return proto.Equal(a.metrics, b.metrics) && proto.Equal(a.probes, b.probes) && proto.Equal(a.publicMetrics, b.publicMetrics) &&
			proto.Equal(a.publicProbes, b.publicProbes) && proto.Equal(a.comparison, b.comparison)
	}
	before := read(kept)
	if len(before.probes.(*heronv1.QueryProbesResponse).GetSeries()) != 1 {
		t.Fatalf("fixture must be visible before delete: %v", before.probes)
	}

	if _, err := h.admin.DeleteNode(ctx, connect.NewRequest(&heronv1.DeleteNodeRequest{Id: gone})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.QueryMetrics(ctx, connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: gone, From: from, To: to})); codeOf(err) != connect.CodeNotFound {
		t.Fatalf("admin QueryMetrics of the deleted node: %v, want NotFound", err)
	}
	if _, err := h.admin.QueryProbes(ctx, connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: gone, From: from, To: to})); codeOf(err) != connect.CodeNotFound {
		t.Fatalf("admin QueryProbes of the deleted node: %v, want NotFound", err)
	}
	if _, err := h.publicClient().QueryProbes(ctx, connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: gone, From: from, To: to})); codeOf(err) != connect.CodeNotFound {
		t.Fatalf("public QueryProbes of the deleted node: %v, want NotFound", err)
	}
	for name, list := range map[string]func() ([]int64, error){
		"admin": func() ([]int64, error) {
			r, err := h.admin.ListProbeComparisonNodes(ctx, connect.NewRequest(&heronv1.ListProbeComparisonNodesRequest{TaskId: task}))
			return r.Msg.GetNodeIds(), err
		},
		"public": func() ([]int64, error) {
			r, err := h.publicClient().ListProbeComparisonNodes(ctx, connect.NewRequest(&heronv1.ListProbeComparisonNodesRequest{TaskId: task}))
			return r.Msg.GetNodeIds(), err
		},
	} {
		if nodes, err := list(); err != nil || len(nodes) != 1 || nodes[0] != kept {
			t.Fatalf("%s comparison candidates after delete = %v %v, want only %d", name, nodes, err, kept)
		}
	}
	both, err := h.admin.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{TaskId: task, NodeIds: []int64{gone, kept}, From: from, To: to}))
	if err != nil || len(both.Msg.GetUnavailableNodeIds()) != 1 || both.Msg.GetUnavailableNodeIds()[0] != gone || len(both.Msg.GetSeries()) != 1 {
		t.Fatalf("comparison over the deleted and the kept node = %v %v", both, err)
	}
	if !same(read(kept), before) {
		t.Fatal("the kept node's history changed when another node was deleted")
	}
	stats, err := h.admin.GetStorageStats(ctx, connect.NewRequest(&heronv1.GetStorageStatsRequest{}))
	if err != nil || stats.Msg.CleanupPending == nil || stats.Msg.GetCleanupPending() != 1 {
		t.Fatalf("cleanup_pending after deleting a node = %v %v, want 1", stats.Msg.CleanupPending, err)
	}

	if _, err := h.store.CleanupDeleted(ctx); err != nil {
		t.Fatal(err)
	}
	if !same(read(kept), before) {
		t.Fatal("the kept node's history changed across cleanup")
	}
}
