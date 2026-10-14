package api

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"google.golang.org/protobuf/proto"
)

func TestProbeTaskLifecycleThroughAdminAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	n1, _ := h.createNode(t, "a")
	n2, _ := h.createNode(t, "b")
	ctx := t.Context()
	save := func(task *heronv1.ProbeTask, nodes ...int64) (*heronv1.SaveProbeTaskResponse, error) {
		resp, err := h.admin.SaveProbeTask(ctx, connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: task, NodeIds: nodes}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	created, err := save(&heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_TCP, Target: "example.com:443", IntervalS: 30, TimeoutMs: 2000}, n2, n1, n1)
	if err != nil || created.Version < uint64(h.clk.Now().Unix()) || created.Task.Task.Id != 1 || !slices.Equal(created.Task.NodeIds, []int64{n1, n2}) {
		t.Fatalf("%+v %v", created, err)
	}
	list, err := h.admin.ListProbeTasks(ctx, connect.NewRequest(&heronv1.ListProbeTasksRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if list.Msg.Version != created.Version || len(list.Msg.Tasks) != 1 || !proto.Equal(list.Msg.Tasks[0], created.Task) {
		t.Fatalf("%+v", list.Msg)
	}
	// 整体替换：改目标、只留 n1。
	updated, err := save(&heronv1.ProbeTask{Id: 1, Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "1.1.1.1", IntervalS: 10, TimeoutMs: 1000}, n1)
	if err != nil || updated.Version <= created.Version || updated.Task.Task.Target != "1.1.1.1" || !slices.Equal(updated.Task.NodeIds, []int64{n1}) {
		t.Fatalf("%+v %v", updated, err)
	}
	list, err = h.admin.ListProbeTasks(ctx, connect.NewRequest(&heronv1.ListProbeTasksRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if list.Msg.Version != updated.Version || len(list.Msg.Tasks) != 1 || !proto.Equal(list.Msg.Tasks[0], updated.Task) {
		t.Fatalf("updated list=%v", list.Msg)
	}
	del, err := h.admin.DeleteProbeTask(ctx, connect.NewRequest(&heronv1.DeleteProbeTaskRequest{Id: 1}))
	if err != nil || del.Msg.Version <= updated.Version {
		t.Fatalf("%+v %v", del, err)
	}
	list, err = h.admin.ListProbeTasks(ctx, connect.NewRequest(&heronv1.ListProbeTasksRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if list.Msg.Version != del.Msg.Version || len(list.Msg.Tasks) != 0 {
		t.Fatalf("deleted list=%v", list.Msg)
	}
	_, err = h.admin.DeleteProbeTask(ctx, connect.NewRequest(&heronv1.DeleteProbeTaskRequest{Id: 1}))
	if codeOf(err) != connect.CodeNotFound || err.Error() != "not_found: id: probe task 1 does not exist" {
		t.Fatalf("%v", err)
	}
}

// dns_server 经管理 API 落库并原样回读，agent 取到的清单同样携带；非 DNS 任务携带它被 CheckTask 拒绝。
func TestProbeTaskDNSServerThroughAdminAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := t.Context()
	task := &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_DNS, Target: "example.com", IntervalS: 30, TimeoutMs: 2000, DnsServer: "[2001:4860:4860::8888]:53"}
	created, err := h.admin.SaveProbeTask(ctx, connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: task, AllNodes: true}))
	if err != nil || created.Msg.Task.Task.GetDnsServer() != "[2001:4860:4860::8888]:53" {
		t.Fatalf("%+v %v", created, err)
	}
	list, err := h.admin.ListProbeTasks(ctx, connect.NewRequest(&heronv1.ListProbeTasksRequest{}))
	if err != nil || len(list.Msg.Tasks) != 1 || !proto.Equal(list.Msg.Tasks[0], created.Msg.Task) {
		t.Fatalf("%+v %v", list.Msg, err)
	}
	node, _ := h.createNode(t, "agent-view")
	if got := h.reg.TasksFor(node); len(got.Tasks) != 1 || got.Tasks[0].GetDnsServer() != "[2001:4860:4860::8888]:53" {
		t.Fatalf("agent ProbeTasks=%+v", got)
	}
	stray := validProbeTask()
	stray.DnsServer = "1.1.1.1:53"
	_, err = h.admin.SaveProbeTask(ctx, connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: stray, AllNodes: true}))
	if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "dns_server only applies to a DNS task") {
		t.Fatalf("%v", err)
	}
}

// query 分支覆盖窗口校验中的节点查找失败，不覆盖 QueryProbes 自身的历史存储查询。
func TestProbeWritesAndWindowLookupFailuresStayInternal(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, saveErr := h.svc.SaveProbeTask(ctx, connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: validProbeTask()}))
	_, deleteErr := h.svc.DeleteProbeTask(ctx, connect.NewRequest(&heronv1.DeleteProbeTaskRequest{Id: 1}))
	_, queryErr := h.svc.QueryProbes(ctx, connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: 1, From: 0, To: 60}))
	for name, err := range map[string]error{"save": saveErr, "delete": deleteErr, "query": queryErr} {
		if codeOf(err) != connect.CodeInternal {
			t.Errorf("%s error=%v want Internal", name, err)
		}
	}
}

func validProbeTask() *heronv1.ProbeTask {
	return &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "127.0.0.1", IntervalS: 5, TimeoutMs: 1000}
}

func TestSaveProbeTaskErrorsNameTheFieldAndCode(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	badInterval := validProbeTask()
	badInterval.IntervalS = 1
	badTCP := validProbeTask()
	badTCP.Kind = heronv1.ProbeKind_PROBE_KIND_TCP
	unknownTask := validProbeTask()
	unknownTask.Id = 999
	for _, tc := range []struct {
		name  string
		task  *heronv1.ProbeTask
		nodes []int64
		code  connect.Code
		text  string
	}{
		{"missing", nil, nil, connect.CodeInvalidArgument, "task: required"},
		{"interval", badInterval, nil, connect.CodeInvalidArgument, "interval_s must be between 5 and 3600"},
		{"tcp", badTCP, nil, connect.CodeInvalidArgument, "host:port"},
		{"node", validProbeTask(), []int64{42}, connect.CodeNotFound, "node_ids: node 42 does not exist"},
		{"task", unknownTask, nil, connect.CodeNotFound, "task.id: probe task 999 does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: tc.task, NodeIds: tc.nodes}))
			if codeOf(err) != tc.code || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("error=%v want=%s %q", err, tc.code, tc.text)
			}
		})
	}
	for range 64 {
		if _, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: validProbeTask(), NodeIds: []int64{id}})); err != nil {
			t.Fatal(err)
		}
	}
	_, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: validProbeTask(), NodeIds: []int64{id}}))
	if codeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "node_ids: node 1 would have 65 probe tasks (maximum 64)") {
		t.Fatalf("limit error=%v", err)
	}
}

func TestQueryProbesGroupsPerTaskAndOmitsEmptyPoints(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	// 探测历史只为存在的任务落库、只对存在的任务可读：三个任务按 7、9、8 的先后建，显示顺序即建立顺序。
	var tasks []uint64
	for range 3 {
		saved, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: validProbeTask(), NodeIds: []int64{id}}))
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, saved.Msg.GetTask().GetTask().GetId())
	}
	t7, t9, t8 := tasks[0], tasks[1], tasks[2]
	base := h.clk.Now().Truncate(time.Hour).Unix()
	rows := []metric.ProbeRow{
		{NodeID: id, TS: base + 180, TaskID: t7, Bucket: &metric.ProbeBucket{Sent: 2, Errors: 2}},
		{NodeID: id, TS: base + 120, TaskID: t9, Bucket: &metric.ProbeBucket{Sent: 1, RttN: 1, RttSumUs: 900, RttMinUs: 900, RttMaxUs: 900}},
		{NodeID: id, TS: base + 120, TaskID: t7, Bucket: &metric.ProbeBucket{Sent: 1, RttN: 1}},
		{NodeID: id, TS: base + 60, TaskID: t7, Bucket: &metric.ProbeBucket{Sent: 2, Lost: 2}},
		{NodeID: id, TS: base, TaskID: t7, Bucket: &metric.ProbeBucket{Sent: 4, Lost: 1, Errors: 1, RttN: 2, RttSumUs: 600, RttMinUs: 200, RttMaxUs: 400}},
		{NodeID: id, TS: base + 60, TaskID: t9, Bucket: &metric.ProbeBucket{}},
		{NodeID: id, TS: base, TaskID: t8, Bucket: &metric.ProbeBucket{}},
	}
	if _, err := h.store.WriteMinuteBatch(t.Context(), metric.Batch{Probes: rows}); err != nil {
		t.Fatal(err)
	}
	resp, err := h.admin.QueryProbes(t.Context(), connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: id, From: base, To: base + 3600, MaxPoints: 100}))
	if err != nil {
		t.Fatal(err)
	}
	want := &heronv1.QueryProbesResponse{Level: "1m", StepS: 60, Series: []*heronv1.ProbeSeries{
		{TaskId: t7, Samples: []*heronv1.ProbeSample{
			{Ts: base, Sent: 4, Lost: 1, Errors: 1, RttMeanUs: proto.Uint32(300), RttMinUs: proto.Uint32(200), RttMaxUs: proto.Uint32(400)},
			{Ts: base + 60, Sent: 2, Lost: 2},
			{Ts: base + 120, Sent: 1, RttMeanUs: proto.Uint32(0), RttMinUs: proto.Uint32(0), RttMaxUs: proto.Uint32(0)},
			{Ts: base + 180, Sent: 2, Errors: 2},
		}},
		{TaskId: t9, Samples: []*heronv1.ProbeSample{{Ts: base + 120, Sent: 1, RttMeanUs: proto.Uint32(900), RttMinUs: proto.Uint32(900), RttMaxUs: proto.Uint32(900)}}},
	}}
	for _, series := range resp.Msg.Series {
		series.Kind, series.Target = 0, "" // 标注由 TestQueryProbesLabelsSeriesWithCurrentTaskConfig 覆盖，这里只比样本与分组
	}
	if !proto.Equal(resp.Msg, want) {
		t.Fatalf("history=%v want=%v", resp.Msg, want)
	}
}

func TestProbeAndMetricQueriesShareWindowValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	for _, tc := range []struct {
		name           string
		node, from, to int64
		max            uint32
		code           connect.Code
		text           string
	}{
		{"negative", id, -1, 60, 0, connect.CodeInvalidArgument, "from must be a nonnegative Unix timestamp; got -1"},
		{"order", id, 60, 60, 0, connect.CodeInvalidArgument, "from (60) must be earlier than to (60)"},
		{"span", id, 0, 401 * 86400, 0, connect.CodeInvalidArgument, "window spans 34646400 seconds; the maximum is 34560000 (400 days)"},
		{"points", id, 0, 3600, 2001, connect.CodeInvalidArgument, "max_points must be at most 2000; got 2001"},
		{"missing", 999, 0, 3600, 0, connect.CodeNotFound, "node 999 does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, pe := h.admin.QueryProbes(t.Context(), connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: tc.node, From: tc.from, To: tc.to, MaxPoints: tc.max}))
			_, me := h.admin.QueryMetrics(t.Context(), connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: tc.node, From: tc.from, To: tc.to, MaxPoints: tc.max}))
			for _, err := range []error{pe, me} {
				if codeOf(err) != tc.code || !strings.Contains(err.Error(), tc.text) {
					t.Errorf("error=%v want=%s %q", err, tc.code, tc.text)
				}
			}
		})
	}
	for _, points := range []uint32{0, 720, 2000} {
		resp, err := h.admin.QueryProbes(t.Context(), connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: id, From: 0, To: 7 * 86400, MaxPoints: points}))
		if err != nil {
			t.Fatal(err)
		}
		step := uint32(900)
		if points == 2000 {
			step = 600
		}
		if resp.Msg.Level != "5m" || resp.Msg.StepS != step || len(resp.Msg.Series) != 0 {
			t.Fatalf("points=%d response=%v", points, resp.Msg)
		}
	}
}

// 图例要的种类与目标随序列下发，取自查询时的任务清单：改过目标的任务按新目标标注，
// 没分配给被查节点的任务照样标注（管理端口径与分配无关）。已删除的任务不出现：它的历史在清理完成前
// 还在库里，但读侧按配置层判定任务存在，删除即对读者消失。
func TestQueryProbesLabelsSeriesWithCurrentTaskConfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	saved, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{
		Task:    &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_TCP, Target: "example.com:443", IntervalS: 30, TimeoutMs: 1000},
		NodeIds: []int64{id},
	}))
	if err != nil {
		t.Fatal(err)
	}
	task := saved.Msg.GetTask().GetTask().GetId()
	other, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{
		Task: &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_TCP, Target: "10.0.0.1:22", IntervalS: 30, TimeoutMs: 1000},
	}))
	if err != nil {
		t.Fatal(err)
	}
	unassigned := other.Msg.GetTask().GetTask().GetId()
	deleted, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{
		Task:    &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_TCP, Target: "10.0.0.2:22", IntervalS: 30, TimeoutMs: 1000},
		NodeIds: []int64{id},
	}))
	if err != nil {
		t.Fatal(err)
	}
	gone := deleted.Msg.GetTask().GetTask().GetId()
	base := h.clk.Now().Truncate(time.Hour).Unix()
	rows := []metric.ProbeRow{
		{NodeID: id, TS: base, TaskID: task, Bucket: &metric.ProbeBucket{Sent: 1, Lost: 1}},
		{NodeID: id, TS: base, TaskID: unassigned, Bucket: &metric.ProbeBucket{Sent: 1, Lost: 1}},
		{NodeID: id, TS: base, TaskID: gone, Bucket: &metric.ProbeBucket{Sent: 1, Lost: 1}},
	}
	if _, err := h.store.WriteMinuteBatch(t.Context(), metric.Batch{Probes: rows}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.DeleteProbeTask(t.Context(), connect.NewRequest(&heronv1.DeleteProbeTaskRequest{Id: gone})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{
		Task:    &heronv1.ProbeTask{Id: task, Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "192.0.2.1", IntervalS: 30, TimeoutMs: 1000},
		NodeIds: []int64{id},
	})); err != nil {
		t.Fatal(err)
	}
	resp, err := h.admin.QueryProbes(t.Context(), connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: id, From: base, To: base + 3600}))
	if err != nil {
		t.Fatal(err)
	}
	type label struct {
		kind   heronv1.ProbeKind
		target string
	}
	want := map[uint64]label{
		task:       {heronv1.ProbeKind_PROBE_KIND_ICMP, "192.0.2.1"},
		unassigned: {heronv1.ProbeKind_PROBE_KIND_TCP, "10.0.0.1:22"},
	}
	got := map[uint64]label{}
	for _, s := range resp.Msg.GetSeries() {
		got[s.GetTaskId()] = label{s.GetKind(), s.GetTarget()}
	}
	if len(got) != len(want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("labels = %v, want %v", got, want)
		}
	}
}
