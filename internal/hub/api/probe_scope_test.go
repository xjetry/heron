package api

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/probelimit"
)

func probeTask(target string) *heronv1.ProbeTask {
	return &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: target, IntervalS: 30, TimeoutMs: 1000}
}

func (h *harness) saveAllNodesTask(t *testing.T, target string) *heronv1.ProbeTaskDetail {
	t.Helper()
	resp, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: probeTask(target), AllNodes: true}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetTask()
}

func (h *harness) listProbeTasks(t *testing.T) *heronv1.ListProbeTasksResponse {
	t.Helper()
	resp, err := h.admin.ListProbeTasks(t.Context(), connect.NewRequest(&heronv1.ListProbeTasksRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg
}

// reportTasks 以给定摘要上报，内容一致时不重发清单。
func (h *harness) reportTasks(t *testing.T, tok string, digest []byte) *heronv1.ProbeTasks {
	t.Helper()
	req := connect.NewRequest(&heronv1.ReportRequest{Metrics: &heronv1.Metrics{}, TasksDigest: digest})
	req.Header().Set("Authorization", "Bearer "+tok)
	resp, err := h.agent.Report(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	// 上报限速按节点计：同一节点连续上报之间推进时钟，不撞上两倍间隔的限速。
	h.clk.Advance(10 * time.Second)
	return resp.Msg.GetTasks()
}

func taskIDs(tasks *heronv1.ProbeTasks) []uint64 {
	var ids []uint64
	for _, task := range tasks.GetTasks() {
		ids = append(ids, task.GetId())
	}
	return ids
}

// ListProbeTasks 对 all_nodes 任务回显当前展开的节点：保存请求不混入显式节点，之后建的节点纳入、删的节点
// 掉出。建节点推进版本，删节点不推。
func TestListProbeTasksExpandsAllNodesAcrossNodeCreationAndDeletion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a, _ := h.createNode(t, "a")
	saved, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: probeTask("192.0.2.1"), AllNodes: true}))
	if err != nil {
		t.Fatal(err)
	}
	if d := saved.Msg.GetTask(); !d.GetAllNodes() || !slices.Equal(d.GetNodeIds(), []int64{a}) {
		t.Fatalf("saved = %v, want all_nodes over [%d]", d, a)
	}
	expanded := func(want ...int64) uint64 {
		t.Helper()
		list := h.listProbeTasks(t)
		if len(list.GetTasks()) != 1 || !list.GetTasks()[0].GetAllNodes() || !slices.Equal(list.GetTasks()[0].GetNodeIds(), want) {
			t.Fatalf("listed = %v, want all_nodes over %v", list.GetTasks(), want)
		}
		return list.GetVersion()
	}
	saveVersion := expanded(a)
	b, _ := h.createNode(t, "b")
	created := expanded(a, b)
	if created <= saveVersion {
		t.Fatalf("creating a node left the version at %d (was %d)", created, saveVersion)
	}
	if _, err := h.admin.DeleteNode(t.Context(), connect.NewRequest(&heronv1.DeleteNodeRequest{Id: a})); err != nil {
		t.Fatal(err)
	}
	if deleted := expanded(b); deleted != created {
		t.Fatalf("deleting a node changed the version %d -> %d", created, deleted)
	}
}

// 建节点不会改变已有节点的清单内容；新节点未持有清单时下发，删除其他节点也不重发。
func TestAgentsReconcileAllNodesTasksAfterNodeCreation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	_, tokA := h.createNode(t, "a")
	all := h.saveAllNodesTask(t, "192.0.2.1")
	if _, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: probeTask("192.0.2.2")})); err != nil {
		t.Fatal(err)
	}
	first := h.reportTasks(t, tokA, nil)
	digest, err := agentwire.TasksDigest(first.Tasks)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.reportTasks(t, tokA, digest); got != nil {
		t.Fatalf("equal digest resent: %v", got)
	}
	b, tokB := h.createNode(t, "b")
	if got := h.reportTasks(t, tokB, nil); !slices.Equal(taskIDs(got), []uint64{all.GetTask().GetId()}) {
		t.Fatalf("new node tasks = %v", got)
	}
	if got := h.reportTasks(t, tokA, digest); got != nil {
		t.Fatalf("new node changed existing list: %v", got)
	}
	if _, err := h.admin.DeleteNode(t.Context(), connect.NewRequest(&heronv1.DeleteNodeRequest{Id: b})); err != nil {
		t.Fatal(err)
	}
	if got := h.reportTasks(t, tokA, digest); got != nil {
		t.Fatalf("deleted node changed existing list: %v", got)
	}
}

// all_nodes 任务计入每个节点的上限；超限指向 all_nodes 字段并回滚。
func TestSaveAllNodesProbeTaskPastTheLimitIsRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "a")
	for range probelimit.MaxTasksPerNode {
		h.saveAllNodesTask(t, "192.0.2.1")
	}
	_, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: probeTask("192.0.2.1"), AllNodes: true}))
	want := fmt.Sprintf("resource_exhausted: all_nodes: node %d would have 65 probe tasks (maximum 64)", id)
	if codeOf(err) != connect.CodeResourceExhausted || err.Error() != want {
		t.Fatalf("65th all_nodes task error = %v, want %q", err, want)
	}
	if n := len(h.listProbeTasks(t).GetTasks()); n != probelimit.MaxTasksPerNode {
		t.Fatalf("tasks after the rejected save = %d", n)
	}
}

// 没有节点时 all_nodes 任务可以多于 64 个；此时建节点与自助注册都以 ResourceExhausted 失败并说明原因，节点不建。
func TestCreatingNodeInheritingTooManyTasksIsRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	var tasks []*heronv1.ProbeTaskDetail
	for range probelimit.MaxTasksPerNode + 1 {
		tasks = append(tasks, h.saveAllNodesTask(t, "192.0.2.1"))
	}
	want := "resource_exhausted: a new node would inherit 65 all-nodes probe tasks (maximum 64 per node); assign some of them to explicit nodes or delete them first"
	if _, err := h.admin.CreateNode(t.Context(), connect.NewRequest(&heronv1.CreateNodeRequest{Name: "n"})); codeOf(err) != connect.CodeResourceExhausted || err.Error() != want {
		t.Fatalf("CreateNode error = %v, want %q", err, want)
	}
	opened, err := h.admin.OpenRegisterWindow(t.Context(), connect.NewRequest(&heronv1.OpenRegisterWindowRequest{TtlS: 600, MaxNodes: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.agent.Register(t.Context(), connect.NewRequest(&heronv1.RegisterRequest{Key: opened.Msg.GetKey(), Name: "r"})); codeOf(err) != connect.CodeResourceExhausted || err.Error() != want {
		t.Fatalf("Register error = %v, want %q", err, want)
	}
	nodes, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil || len(nodes.Msg.GetNodes()) != 0 {
		t.Fatalf("rejected creation left nodes: %v %v", nodes, err)
	}
	if _, err := h.admin.DeleteProbeTask(t.Context(), connect.NewRequest(&heronv1.DeleteProbeTaskRequest{Id: tasks[0].GetTask().GetId()})); err != nil {
		t.Fatal(err)
	}
	h.createNode(t, "n")
}

// 公开端的任务标签规则对 all_nodes 任务同样成立：它对每个公开节点都算当前分配，包括任务保存之后才建的节点。
func TestPublicProbeLabelsIncludeAllNodesTasks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	all := h.saveAllNodesTask(t, "192.0.2.1").GetTask().GetId()
	pub, _ := h.createNode(t, "pub")
	h.setPublic(t, pub, "pub", true)
	base := h.clk.Now().Truncate(time.Hour).Unix()
	rows := []metric.ProbeRow{{NodeID: pub, TS: base, TaskID: all, Bucket: &metric.ProbeBucket{Sent: 1, Lost: 1}}}
	if _, err := h.store.WriteMinuteBatch(t.Context(), metric.Batch{Probes: rows}); err != nil {
		t.Fatal(err)
	}
	resp, err := h.publicClient().QueryProbes(t.Context(), connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: pub, From: base, To: base + 3600}))
	if err != nil {
		t.Fatal(err)
	}
	got := map[uint64]string{}
	for _, s := range resp.Msg.GetSeries() {
		got[s.GetTaskId()] = fmt.Sprintf("%v %q", s.GetKind(), s.GetTarget())
	}
	if want := map[uint64]string{all: `PROBE_KIND_ICMP "192.0.2.1"`}; !maps.Equal(got, want) {
		t.Fatalf("public labels = %v, want %v", got, want)
	}
}
