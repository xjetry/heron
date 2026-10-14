package api

import (
	"math"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"google.golang.org/protobuf/proto"
)

func TestProbeDisplayOrderAcrossAPIStorageAndAgent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := t.Context()
	node, _ := h.createNode(t, "public")
	h.setPublic(t, node, "public", true)
	createTask := func() uint64 {
		t.Helper()
		r, err := h.admin.SaveProbeTask(ctx, connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: validProbeTask(), AllNodes: true}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.Task.Task.Id
	}
	a, b, c := createTask(), createTask(), createTask()
	before := h.reg.TasksFor(node)
	check := func(want []uint64) {
		t.Helper()
		list, err := h.admin.ListProbeTasks(ctx, connect.NewRequest(&heronv1.ListProbeTasksRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		var ids []uint64
		for _, d := range list.Msg.Tasks {
			ids = append(ids, d.Task.Id)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("display order=%v want=%v", ids, want)
		}
		_, records, err := h.store.LoadProbeTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ids = nil
		for _, d := range records {
			ids = append(ids, d.Task.Id)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("stored order=%v want=%v", ids, want)
		}
	}
	reorder := func(ids []uint64) error {
		_, err := h.admin.ReorderProbeTasks(ctx, connect.NewRequest(&heronv1.ReorderProbeTasksRequest{Ids: ids}))
		return err
	}
	if err := reorder([]uint64{c, a, b}); err != nil {
		t.Fatal(err)
	}
	check([]uint64{c, a, b})
	if !proto.Equal(before, h.reg.TasksFor(node)) {
		t.Fatal("display reorder changed agent tasks or version")
	}
	for _, bad := range [][]uint64{nil, {c, a}, {c, c, b}, {c, a, 999}, {c, a, 0}, {c, a, math.MaxUint64}} {
		if err := reorder(bad); codeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("bad order %v: %v", bad, err)
		}
		check([]uint64{c, a, b})
		if !proto.Equal(before, h.reg.TasksFor(node)) {
			t.Fatal("rejected reorder changed agent tasks or version")
		}
	}
	if err := h.reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	check([]uint64{c, a, b})
	if !proto.Equal(before, h.reg.TasksFor(node)) {
		t.Fatal("reload changed agent tasks or version")
	}
	edit := validProbeTask()
	edit.Id, edit.Target = a, "192.0.2.1"
	if _, err := h.admin.SaveProbeTask(ctx, connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: edit, AllNodes: true})); err != nil {
		t.Fatal(err)
	}
	d := createTask()
	check([]uint64{c, a, b, d})
	base := h.clk.Now().Truncate(time.Hour).Unix()
	var rows []metric.ProbeRow
	for _, id := range []uint64{a, b, c, d} {
		rows = append(rows, metric.ProbeRow{NodeID: node, TS: base, TaskID: id, Bucket: &metric.ProbeBucket{Sent: 1, Lost: 1}})
	}
	if _, err := h.store.WriteMinuteBatch(ctx, metric.Batch{Probes: rows}); err != nil {
		t.Fatal(err)
	}
	query := &heronv1.QueryProbesRequest{NodeId: node, From: base, To: base + 3600}
	checkHistory := func(want []uint64) {
		t.Helper()
		admin, err := h.admin.QueryProbes(ctx, connect.NewRequest(query))
		if err != nil {
			t.Fatal(err)
		}
		public, err := h.publicClient().QueryProbes(ctx, connect.NewRequest(query))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range []*heronv1.QueryProbesResponse{admin.Msg, public.Msg} {
			var ids []uint64
			for _, s := range r.Series {
				ids = append(ids, s.TaskId)
			}
			if !slices.Equal(ids, want) {
				t.Fatalf("history order=%v want=%v", ids, want)
			}
		}
	}
	checkHistory([]uint64{c, a, b, d})
	if _, err := h.admin.DeleteProbeTask(ctx, connect.NewRequest(&heronv1.DeleteProbeTaskRequest{Id: a})); err != nil {
		t.Fatal(err)
	}
	check([]uint64{c, b, d})
	// 删除即对读者消失：a 的历史在清理完成前还在库里，两端的历史都不再有它。
	checkHistory([]uint64{c, b, d})
}
