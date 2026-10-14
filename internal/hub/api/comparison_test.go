package api

import (
	"database/sql"
	"strings"
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
)

// 两个公开节点、一个私有节点，一个显式分配到前两者的任务，窗口内逐分钟样本。
// 两个公开节点的取值不同，供比对响应内容；私有节点在公开端不可见。
func comparisonHarness(t *testing.T) (h *harness, task uint64, pub, pub2, priv, other, base int64) {
	t.Helper()
	h = newHarness(t, "")
	h.login(t)
	ctx := t.Context()
	pub, _ = h.createNode(t, "pub")
	pub2, _ = h.createNode(t, "pub2")
	priv, _ = h.createNode(t, "priv")
	h.setPublic(t, priv, "priv", false)
	other, _ = h.createNode(t, "other")
	saved, err := h.admin.SaveProbeTask(ctx, connect.NewRequest(&heronv1.SaveProbeTaskRequest{
		Task:    &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "192.0.2.1", IntervalS: 60, TimeoutMs: 1000},
		NodeIds: []int64{pub, pub2, priv},
	}))
	if err != nil {
		t.Fatal(err)
	}
	task = saved.Msg.Task.Task.Id
	base = h.clk.Now().Unix() - 3600
	h.store.WriteMinuteBatch(ctx, metric.Batch{Probes: []metric.ProbeRow{
		{NodeID: pub, TS: base, TaskID: task, Bucket: &metric.ProbeBucket{Sent: 2, RttN: 2, RttSumUs: 1000, RttMinUs: 400, RttMaxUs: 600}},
		{NodeID: pub2, TS: base, TaskID: task, Bucket: &metric.ProbeBucket{Sent: 2, Lost: 1, RttN: 1, RttSumUs: 500, RttMinUs: 500, RttMaxUs: 500}},
		{NodeID: priv, TS: base, TaskID: task, Bucket: &metric.ProbeBucket{Sent: 1, RttN: 1, RttSumUs: 700, RttMinUs: 700, RttMaxUs: 700}},
	}})
	return h, task, pub, pub2, priv, other, base
}

// 四种调用方（会话、全站 token、范围内 token、范围外 token）经真实 Connect 入口读到同一份候选，
// 标注按任务是否整体落入凭据范围决定；没有可见候选节点时与任务不存在同一回答。
func TestListProbeComparisonNodesThroughAdminAPI(t *testing.T) {
	t.Parallel()
	h, task, pub, pub2, priv, other, _ := comparisonHarness(t)
	ctx := t.Context()
	resp, err := h.admin.ListProbeComparisonNodes(ctx, connect.NewRequest(&heronv1.ListProbeComparisonNodesRequest{TaskId: task}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Kind != heronv1.ProbeKind_PROBE_KIND_ICMP || resp.Msg.Target != "192.0.2.1" {
		t.Fatalf("session label = %s %s", resp.Msg.Kind, resp.Msg.Target)
	}
	if resp.Msg.MaxNodesPerQuery != uint32(store.MaxComparisonNodes) {
		t.Fatalf("max_nodes_per_query = %d, want %d", resp.Msg.MaxNodesPerQuery, store.MaxComparisonNodes)
	}
	if len(resp.Msg.NodeIds) != 3 {
		t.Fatalf("session candidates = %v", resp.Msg.NodeIds)
	}
	// 全站 token：标注照给。
	full, _, _ := grantedClient(t, h, &heronv1.TokenGrant{AllNodes: true, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	resp, err = full.ListProbeComparisonNodes(ctx, connect.NewRequest(&heronv1.ListProbeComparisonNodesRequest{TaskId: task}))
	if err != nil || resp.Msg.Target != "192.0.2.1" {
		t.Fatalf("site-wide token: %+v %v", resp.Msg, err)
	}
	// 范围内 token（覆盖任务的全部显式节点）：候选照给（范围含全部分配节点），标注照给。
	scoped, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{pub, pub2, priv}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	resp, err = scoped.ListProbeComparisonNodes(ctx, connect.NewRequest(&heronv1.ListProbeComparisonNodesRequest{TaskId: task}))
	if err != nil || resp.Msg.Target != "192.0.2.1" || len(resp.Msg.NodeIds) != 3 {
		t.Fatalf("scoped-in token: %+v %v", resp.Msg, err)
	}
	// 部分范围 token（只覆盖三个显式节点之一）：可见候选照给，任务标注不给——任务的显式分配
	// 有一部分在凭据范围之外，泄露种类与目标等于泄露它探测什么。
	partial, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{pub}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	resp, err = partial.ListProbeComparisonNodes(ctx, connect.NewRequest(&heronv1.ListProbeComparisonNodesRequest{TaskId: task}))
	if err != nil || resp.Msg.Target != "" || resp.Msg.Kind != heronv1.ProbeKind_PROBE_KIND_UNSPECIFIED {
		t.Fatalf("scoped-partial token must lose the label: %+v %v", resp.Msg, err)
	}
	if len(resp.Msg.NodeIds) != 1 || resp.Msg.NodeIds[0] != pub {
		t.Fatalf("scoped-partial candidates = %v", resp.Msg.NodeIds)
	}
	// 一个可见候选都没有：与任务不存在同一回答，不区分"存在但不可见"。
	blind, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{other}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	_, err = blind.ListProbeComparisonNodes(ctx, connect.NewRequest(&heronv1.ListProbeComparisonNodesRequest{TaskId: task}))
	if codeOf(err) != connect.CodeNotFound {
		t.Fatalf("blind token err = %v, want NotFound", err)
	}
	_, err = h.admin.ListProbeComparisonNodes(ctx, connect.NewRequest(&heronv1.ListProbeComparisonNodesRequest{TaskId: task + 1000}))
	if codeOf(err) != connect.CodeNotFound || err.Error() != "not_found: task_id: no probe task has this id" {
		t.Fatalf("missing task err = %v", err)
	}
	_ = priv
	_ = other
}

func TestQueryProbeComparisonThroughAdminAPI(t *testing.T) {
	t.Parallel()
	h, task, pub, pub2, priv, other, base := comparisonHarness(t)
	ctx := t.Context()
	resp, err := h.admin.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{
		TaskId: task, NodeIds: []int64{pub2, pub}, From: base - 60, To: base + 60, MaxPoints: 100,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Level != "1m" || resp.Msg.StepS != 60 || len(resp.Msg.UnavailableNodeIds) != 0 {
		t.Fatalf("meta = %+v", resp.Msg)
	}
	// 序列与请求同序；窗口内没有样本的节点也出现，samples 为空。
	if len(resp.Msg.Series) != 2 || resp.Msg.Series[0].NodeId != pub2 || resp.Msg.Series[1].NodeId != pub {
		t.Fatalf("series order = %+v", resp.Msg.Series)
	}
	s2 := resp.Msg.Series[0].Samples
	if len(s2) != 1 || s2[0].Sent != 2 || s2[0].Lost != 1 || s2[0].GetRttMeanUs() != 500 {
		t.Fatalf("pub2 samples = %+v", s2)
	}
	s1 := resp.Msg.Series[1].Samples
	if len(s1) != 1 || s1[0].Sent != 2 || s1[0].Lost != 0 || s1[0].GetRttMeanUs() != 500 {
		t.Fatalf("pub samples = %+v", s1)
	}
	// 与单节点 QueryProbes 同窗同结果（已由 store 层双向对照测试覆盖；这里核对经真实入口的同一形状）。
	single, err := h.admin.QueryProbes(ctx, connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: pub, From: base - 60, To: base + 60, MaxPoints: 100}))
	if err != nil {
		t.Fatal(err)
	}
	if len(single.Msg.Series) != 1 || len(single.Msg.Series[0].Samples) != 1 || single.Msg.Series[0].Samples[0].GetRttMeanUs() != 500 {
		t.Fatalf("single-node series = %+v", single.Msg.Series)
	}
	// 会话可见私有节点；节点清单顺序保留，unavailable 为空。
	resp, err = h.admin.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{
		TaskId: task, NodeIds: []int64{priv, pub}, From: base, To: base + 60, MaxPoints: 10,
	}))
	if err != nil || len(resp.Msg.Series) != 2 || len(resp.Msg.UnavailableNodeIds) != 0 {
		t.Fatalf("private node via session: %+v %v", resp.Msg, err)
	}
	// 分块不核对分配：可见但未分配该任务的节点照常出现在序列里、samples 为空（该节点
	// 上这个任务的历史本就按同一授权可查，没有可泄露的新信息）。
	resp, err = h.admin.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{
		TaskId: task, NodeIds: []int64{other, pub}, From: base, To: base + 60, MaxPoints: 10,
	}))
	if err != nil || len(resp.Msg.Series) != 2 || len(resp.Msg.UnavailableNodeIds) != 0 {
		t.Fatalf("unassigned visible node: %+v %v", resp.Msg, err)
	}
	for _, series := range resp.Msg.Series {
		if series.NodeId == other && len(series.Samples) != 0 {
			t.Fatalf("unassigned node must have empty samples: %+v", series)
		}
	}
	// 范围外节点进 unavailable_node_ids，顺序同请求。
	scoped, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{pub}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	resp, err = scoped.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{
		TaskId: task, NodeIds: []int64{other, pub2, pub}, From: base, To: base + 60, MaxPoints: 10,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Series) != 1 || resp.Msg.Series[0].NodeId != pub || !equalInt64(resp.Msg.UnavailableNodeIds, []int64{other, pub2}) {
		t.Fatalf("scoped query = %+v", resp.Msg)
	}
	// 校验：空清单、重复节点、超出 max_nodes_per_query、窗口颠倒。
	_, err = scoped.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{TaskId: task, From: base, To: base + 60, MaxPoints: 10}))
	if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "node_ids") {
		t.Fatalf("empty node_ids err = %v", err)
	}
	_, err = scoped.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{TaskId: task, NodeIds: []int64{pub, pub}, From: base, To: base + 60, MaxPoints: 10}))
	if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "must not repeat") {
		t.Fatalf("repeated node_ids err = %v", err)
	}
	over := make([]int64, store.MaxComparisonNodes+1)
	for i := range over {
		over[i] = int64(i + 1)
	}
	_, err = scoped.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{TaskId: task, NodeIds: over, From: base, To: base + 60, MaxPoints: 10}))
	if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("oversized chunk err = %v", err)
	}
	_, err = scoped.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{TaskId: task, NodeIds: []int64{pub}, From: base + 60, To: base, MaxPoints: 10}))
	if codeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("inverted window err = %v", err)
	}
	// 全部节点都不可见：空序列、unavailable 全量，仍为 2xx。
	resp, err = scoped.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{
		TaskId: task, NodeIds: []int64{priv, other}, From: base, To: base + 60, MaxPoints: 10,
	}))
	if err != nil || len(resp.Msg.Series) != 0 || !equalInt64(resp.Msg.UnavailableNodeIds, []int64{priv, other}) {
		t.Fatalf("all unavailable: %+v %v", resp.Msg, err)
	}
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 公开端：候选按公开节点过滤、非空即标注；私有节点的对比对公开调用方不存在。
func TestPublicProbeComparison(t *testing.T) {
	t.Parallel()
	h, task, pub, pub2, priv, _, base := comparisonHarness(t)
	ctx := t.Context()
	resp, err := h.publicClient().ListProbeComparisonNodes(ctx, connect.NewRequest(&heronv1.ListProbeComparisonNodesRequest{TaskId: task}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Target != "192.0.2.1" || resp.Msg.MaxNodesPerQuery != uint32(store.MaxComparisonNodes) || !equalInt64(resp.Msg.NodeIds, []int64{pub, pub2}) {
		t.Fatalf("public list = %+v", resp.Msg)
	}
	q, err := h.publicClient().QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{
		TaskId: task, NodeIds: []int64{priv, pub2}, From: base - 60, To: base + 60, MaxPoints: 100,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Msg.Series) != 1 || q.Msg.Series[0].NodeId != pub2 || !equalInt64(q.Msg.UnavailableNodeIds, []int64{priv}) {
		t.Fatalf("public query = %+v", q.Msg)
	}
	// 与管理端同一节点同一窗口的样本一致。
	admin, err := h.admin.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{
		TaskId: task, NodeIds: []int64{pub2}, From: base - 60, To: base + 60, MaxPoints: 100,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(admin.Msg.Series[0].Samples) != len(q.Msg.Series[0].Samples) {
		t.Fatalf("admin vs public samples differ: %+v vs %+v", admin.Msg, q.Msg)
	}
	// 公开端只分配到私有节点的任务：与不存在同一回答。保存一个只给 priv 的任务。
	private, err := h.admin.SaveProbeTask(ctx, connect.NewRequest(&heronv1.SaveProbeTaskRequest{
		Task:    &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "192.0.2.9", IntervalS: 60, TimeoutMs: 1000},
		NodeIds: []int64{priv},
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.publicClient().ListProbeComparisonNodes(ctx, connect.NewRequest(&heronv1.ListProbeComparisonNodesRequest{TaskId: private.Msg.Task.Task.Id}))
	if codeOf(err) != connect.CodeNotFound || err.Error() != "not_found: task_id: no probe task has this id" {
		t.Fatalf("private-only task err = %v", err)
	}
	// 私有节点在公开端的对比查询里不可见：unavailable + 空序列，仍为 2xx。
	privQ, err := h.publicClient().QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{
		TaskId: private.Msg.Task.Task.Id, NodeIds: []int64{priv}, From: base, To: base + 60, MaxPoints: 10,
	}))
	if err != nil || len(privQ.Msg.Series) != 0 || !equalInt64(privQ.Msg.UnavailableNodeIds, []int64{priv}) {
		t.Fatalf("private node query via public entry: %+v %v", privQ.Msg, err)
	}
}

// 读量超额从两端真实入口返回 FailedPrecondition，错误文本带额度事实与建议；映射共用
// history.queryError，这里经指标族（权重 1，额度 12000）用真实数据触发同一条路径。
func TestReadQuotaThroughRealEntries(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "a")
	ctx := t.Context()
	// 维护从未推进（水位为 0）：分钟行全部留在 metric_1m，长窗口的 1h 级查询要读
	// 一万两千多行源数据，超过指标族 12000 的额度。种子数据经一条递归 CTE 直连写入
	//（与负载台同一条路，绕过测试套件的慢速插入——race 下逐行写是分钟级）。
	base := h.clk.Now().Unix() - 12050*60
	db, err := sql.Open("sqlite", "file:"+h.dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i < 12050)
		INSERT INTO metric_1m (node_id, ts, cpu_sum, cpu_n, cpu_max, reported, observed)
		SELECT ?, (? - (12050 - i) * 60), 1, 1, 1, 1, NULL FROM seq`, id, h.clk.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	req := &heronv1.QueryMetricsRequest{NodeId: id, From: base, To: h.clk.Now().Unix(), MaxPoints: 10}
	_, err = h.admin.QueryMetrics(ctx, connect.NewRequest(req))
	if codeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("admin entry err = %v (%+v), want FailedPrecondition", err, err)
	}
	if !strings.Contains(err.Error(), "12000") || !strings.Contains(err.Error(), "narrow the window") {
		t.Fatalf("quota error must carry the budget and a remedy: %v", err)
	}
	_, err = h.publicClient().QueryMetrics(ctx, connect.NewRequest(req))
	if codeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("public entry err = %v, want the same FailedPrecondition", err)
	}
	// 缩短窗口后同一节点照常服务（额度不按跨度推算，只按实际行数）。
	ok, err := h.admin.QueryMetrics(ctx, connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: id, From: h.clk.Now().Unix() - 3600, To: h.clk.Now().Unix(), MaxPoints: 100}))
	if err != nil || len(ok.Msg.Ts) == 0 || len(ok.Msg.Series) == 0 {
		t.Fatalf("short window after rejection: %+v %v", ok.Msg, err)
	}
}

// 范围内只读 token 能读对比两个入口（scopedReadAllowed）；它们不携带 CONFIGURE，
// 只有在 token 读方法表里才能通过。公开端不适用（无鉴权）。
func TestScopedReadOnlyTokenReadsComparison(t *testing.T) {
	t.Parallel()
	h, task, pub, pub2, priv, _, base := comparisonHarness(t)
	ctx := t.Context()
	ro, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{pub, pub2, priv}})
	if _, err := ro.ListProbeComparisonNodes(ctx, connect.NewRequest(&heronv1.ListProbeComparisonNodesRequest{TaskId: task})); err != nil {
		t.Fatalf("scoped read-only token list err = %v", err)
	}
	if _, err := ro.QueryProbeComparison(ctx, connect.NewRequest(&heronv1.QueryProbeComparisonRequest{TaskId: task, NodeIds: []int64{pub2}, From: base - 60, To: base + 60, MaxPoints: 100})); err != nil {
		t.Fatalf("scoped read-only token query err = %v", err)
	}
}
