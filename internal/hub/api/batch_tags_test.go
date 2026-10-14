package api

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/probelimit"
	"google.golang.org/protobuf/proto"
)

func TestBatchNodeTagsRefreshesDynamicScopesAndRollsBackTaskLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a, tokenA := h.createNode(t, "a")
	b, tokenB := h.createNode(t, "b")
	c, tokenC := h.createNode(t, "c")
	mustUpdateTags(t, h, a, "a", "group")
	mustUpdateTags(t, h, c, "c", "group")
	dynamic, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: probeTask("192.0.2.1"), SelectorTags: []string{"group"}}))
	if err != nil {
		t.Fatal(err)
	}
	saveRule(t, h, &heronv1.AlertRule{Name: "group offline", Kind: heronv1.AlertKind_ALERT_KIND_OFFLINE, Enabled: true, SelectorTags: []string{"group"}})
	batch := func(add, remove []string) error {
		_, err := h.admin.BatchUpdateNodeTags(t.Context(), connect.NewRequest(&heronv1.BatchUpdateNodeTagsRequest{NodeIds: []int64{a, b}, AddTags: add, RemoveTags: remove}))
		return err
	}
	if err := batch([]string{"group"}, nil); err != nil {
		t.Fatal(err)
	}
	_, assignments := h.reg.List()
	if len(assignments) != 1 || !slices.Equal(assignments[0].NodeIDs, []int64{a, b, c}) {
		t.Fatalf("task list coverage=%v", assignments)
	}
	for _, token := range []string{tokenA, tokenB, tokenC} {
		if got := h.reportTasks(t, token, nil); !slices.Equal(taskIDs(got), []uint64{dynamic.Msg.Task.Task.Id}) {
			t.Fatalf("agent task coverage=%v", got)
		}
	}
	h.clk.Advance(time.Minute)
	if err := h.alerts.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	if states := h.alerts.States(); len(states) != 3 {
		t.Fatalf("alert coverage=%v", states)
	}
	if err := batch(nil, []string{"group"}); err != nil {
		t.Fatal(err)
	}
	_, assignments = h.reg.List()
	if len(assignments) != 1 || !slices.Equal(assignments[0].NodeIDs, []int64{c}) {
		t.Fatalf("task list withdrawal=%v", assignments)
	}
	if got := h.reportTasks(t, tokenC, nil); !slices.Equal(taskIDs(got), []uint64{dynamic.Msg.Task.Task.Id}) {
		t.Fatalf("unselected agent coverage=%v", got)
	}
	for _, token := range []string{tokenA, tokenB} {
		if got := h.reportTasks(t, token, nil); len(got.Tasks) != 0 {
			t.Fatalf("withdrawal=%v", got)
		}
	}
	if states := h.alerts.States(); len(states) != 1 || states[0].NodeID != c {
		t.Fatalf("retained alert state=%v", states)
	}
	rules, err := h.store.ListAlertRules(t.Context())
	if err != nil || len(rules) != 1 || !slices.Equal(rules[0].NodeIDs, []int64{c}) {
		t.Fatalf("stored rules=%v err=%v", rules, err)
	}
	for range probelimit.MaxTasksPerNode {
		if _, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: probeTask("192.0.2.2"), NodeIds: []int64{b}})); err != nil {
			t.Fatal(err)
		}
	}
	version := h.reg.Version()
	if err := batch([]string{"group"}, nil); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("task limit=%v", err)
	}
	if h.reg.Version() != version {
		t.Fatal("rejected batch advanced task version")
	}
	if got, err := listByTags(t, h, "group"); err != nil || !slices.Equal(got, []string{"c"}) {
		t.Fatalf("rejected batch wrote tags=%v err=%v", got, err)
	}
	for _, id := range []int64{a, b} {
		if h.reg.Assigned(id, dynamic.Msg.Task.Task.Id) {
			t.Fatalf("rejected batch published node %d", id)
		}
	}
	if err := h.alerts.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, state := range h.alerts.States() {
		if state.State == store.StateFiring && state.NodeID != c {
			t.Fatalf("rejected batch published alert state=%v", state)
		}
	}
}

func TestBatchNodeTagsPreservesOtherDataAndIsIdempotent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	var ids []int64
	for i := range 6 {
		id, _ := h.createNode(t, fmt.Sprintf("n%d", i))
		tags := []string{"kept", "DB"}
		if i < 3 {
			tags = append(tags, "家宽")
		}
		mustUpdateTags(t, h, id, fmt.Sprintf("n%d", i), tags...)
		ids = append(ids, id)
	}
	before, err := h.store.ListNodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []*heronv1.BatchUpdateNodeTagsRequest{
		{NodeIds: append(slices.Clone(ids[:5]), ids[0]), AddTags: []string{" 家宽 ", "家宽", "new"}, RemoveTags: []string{" db "}},
		{NodeIds: ids[:5], AddTags: []string{"家宽", "NEW"}, RemoveTags: []string{"DB"}},
	} {
		if _, err := h.admin.BatchUpdateNodeTags(t.Context(), connect.NewRequest(req)); err != nil {
			t.Fatal(err)
		}
	}
	after, err := h.store.ListNodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range after {
		want := []string{"kept", "new", "家宽"}
		if i == 5 {
			want = []string{"DB", "kept"}
		}
		if !slices.Equal(n.Tags, want) {
			t.Fatalf("node %d tags=%v want=%v", n.ID, n.Tags, want)
		}
		a, b := nodeProto(n, h.svc.today()), nodeProto(before[i], h.svc.today())
		a.Tags, b.Tags = nil, nil
		if !proto.Equal(a, b) {
			t.Fatalf("non-tag data changed: %v -> %v", b, a)
		}
	}
	if _, err := h.admin.BatchUpdateNodeTags(t.Context(), connect.NewRequest(&heronv1.BatchUpdateNodeTagsRequest{NodeIds: ids[:5], RemoveTags: []string{"家宽"}})); err != nil {
		t.Fatal(err)
	}
	if got, err := listByTags(t, h, "家宽"); err != nil || len(got) != 0 {
		t.Fatalf("removed tags: %v %v", got, err)
	}
	if !slices.Contains(listTags(t, h), "家宽:0") {
		t.Fatal("batch removal deleted the tag itself")
	}
}

func TestBatchNodeTagsValidationAndRollback(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a, _ := h.createNode(t, "a")
	b, _ := h.createNode(t, "b")
	var full []string
	for i := range 16 {
		full = append(full, fmt.Sprintf("t%02d", i))
	}
	mustUpdateTags(t, h, a, "a", "kept")
	mustUpdateTags(t, h, b, "b", full...)
	before, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		req  *heronv1.BatchUpdateNodeTagsRequest
		code connect.Code
	}{
		{&heronv1.BatchUpdateNodeTagsRequest{AddTags: []string{"new"}}, connect.CodeInvalidArgument},
		{&heronv1.BatchUpdateNodeTagsRequest{NodeIds: []int64{a, 0}}, connect.CodeInvalidArgument},
		{&heronv1.BatchUpdateNodeTagsRequest{NodeIds: []int64{a}, AddTags: []string{"DB"}, RemoveTags: []string{" db "}}, connect.CodeInvalidArgument},
		{&heronv1.BatchUpdateNodeTagsRequest{NodeIds: []int64{a}, AddTags: []string{"bad\x00"}}, connect.CodeInvalidArgument},
		{&heronv1.BatchUpdateNodeTagsRequest{NodeIds: []int64{a}, RemoveTags: []string{""}}, connect.CodeInvalidArgument},
		{&heronv1.BatchUpdateNodeTagsRequest{NodeIds: []int64{a, 999999}, AddTags: []string{"new"}}, connect.CodeNotFound},
		{&heronv1.BatchUpdateNodeTagsRequest{NodeIds: []int64{a, b}, AddTags: []string{"new"}}, connect.CodeInvalidArgument},
	} {
		_, err := h.admin.BatchUpdateNodeTags(t.Context(), connect.NewRequest(tc.req))
		if connect.CodeOf(err) != tc.code {
			t.Errorf("request=%v error=%v want=%v", tc.req, err, tc.code)
		}
		after, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
		if err != nil || !proto.Equal(before.Msg, after.Msg) {
			t.Fatalf("failed batch changed nodes: %v %v", after, err)
		}
	}
	if slices.Contains(listTags(t, h), "new:0") {
		t.Fatal("failed batch left a new tag")
	}
	if _, err := h.admin.BatchUpdateNodeTags(t.Context(), connect.NewRequest(&heronv1.BatchUpdateNodeTagsRequest{NodeIds: []int64{a, b}, AddTags: []string{"new"}, RemoveTags: append(full, "kept")})); err != nil {
		t.Fatal(err)
	}
	if got, err := listByTags(t, h, "new"); err != nil || !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("remove before add: %v %v", got, err)
	}
}
