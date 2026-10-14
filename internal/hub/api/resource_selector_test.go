package api

import (
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/hub/store"
	"google.golang.org/protobuf/proto"
)

func TestDynamicSelectorAPIRefreshesAgentAndAlertScopes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a, token := h.createNode(t, "a")
	b, _ := h.createNode(t, "b")
	edit := func(id int64, tags ...string) {
		t.Helper()
		_, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(&heronv1.UpdateNodeRequest{Id: id, Name: "n", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(30), Tags: tags}))
		if err != nil {
			t.Fatal(err)
		}
	}
	edit(a, "db", "west")
	edit(b, "db")
	task, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: probeTask("192.0.2.1"), SelectorTags: []string{"DB", "west"}}))
	if err != nil {
		t.Fatal(err)
	}
	rule := saveRule(t, h, &heronv1.AlertRule{Name: "动态离线", Kind: heronv1.AlertKind_ALERT_KIND_OFFLINE, Enabled: true, SelectorTags: []string{"db", "west"}})
	if !slices.Equal(rule.NodeIds, []int64{a}) || !slices.Equal(rule.SelectorTags, []string{"db", "west"}) {
		t.Fatalf("rule=%v", rule)
	}
	got := h.reportTasks(t, token, nil)
	if !slices.Equal(taskIDs(got), []uint64{task.Msg.Task.Task.Id}) {
		t.Fatalf("agent tasks=%v", got)
	}
	h.clk.Advance(time.Minute)
	if err := h.alerts.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(h.alerts.States()) != 1 || h.alerts.States()[0].State != store.StateFiring {
		t.Fatalf("states=%+v", h.alerts.States())
	}
	edit(a)
	if len(h.alerts.States()) != 0 {
		t.Fatalf("removed scope retained states=%+v", h.alerts.States())
	}
	digest, err := agentwire.TasksDigest(got.Tasks)
	if err != nil {
		t.Fatal(err)
	}
	cleared := h.reportTasks(t, token, digest)
	if cleared == nil || len(cleared.Tasks) != 0 || cleared.Version <= got.Version {
		t.Fatalf("agent withdrawal=%v", cleared)
	}
	edit(b, "db", "west")
	if err := h.alerts.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	states := h.alerts.States()
	if len(states) != 1 || states[0].NodeID != b || states[0].State != store.StateFiring {
		t.Fatalf("new coverage or startup clock reset=%+v", states)
	}
	_, err = h.admin.DeleteTag(t.Context(), connect.NewRequest(&heronv1.DeleteTagRequest{Name: "db"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "动态离线") || !strings.Contains(err.Error(), "192.0.2.1") {
		t.Fatalf("referenced tag deletion=%v", err)
	}
	for _, req := range []*heronv1.SaveProbeTaskRequest{
		{Task: probeTask("192.0.2.2"), AllNodes: true, NodeIds: []int64{a}},
		{Task: probeTask("192.0.2.2"), SelectorTags: []string{"unknown"}},
	} {
		if _, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(req)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("invalid selector accepted: %v err=%v", req, err)
		}
	}
}

func TestResourceRuleAPIFromAgentSampleToRecovery(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, token := h.createNode(t, "n")
	rule := saveRule(t, h, &heronv1.AlertRule{Name: "内存持续紧张", Kind: heronv1.AlertKind_ALERT_KIND_RESOURCE, Enabled: true, AllNodes: true, ResourceMetric: heronv1.ResourceMetric_RESOURCE_METRIC_MEMORY_USED_PCT, Threshold: 90, RecoveryThreshold: 80, ForMinutes: 2})
	if rule.ResourceMetric != heronv1.ResourceMetric_RESOURCE_METRIC_MEMORY_USED_PCT || rule.RecoveryThreshold != 80 {
		t.Fatalf("round trip=%v", rule)
	}
	base := h.clk.Now().Unix()
	for i, used := range []uint64{90, 95, 80, 75} {
		if err := h.report(t, token, &heronv1.Metrics{MemUsed: proto.Uint64(used), MemTotal: proto.Uint64(100)}); err != nil {
			t.Fatal(err)
		}
		h.clk.Advance(time.Minute)
		h.ingest.Flush(t.Context(), false)
		if err := h.alerts.EvaluateResources(t.Context(), base+int64(i)*60); err != nil {
			t.Fatal(err)
		}
		resp, err := h.admin.ListAlertRules(t.Context(), connect.NewRequest(&heronv1.ListAlertRulesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"pending", "firing", "firing", "ok"}[i]
		if len(resp.Msg.States) != 1 || resp.Msg.States[0].State != want {
			t.Fatalf("minute=%d states=%v want=%s", i, resp.Msg.States, want)
		}
	}
	events, err := h.admin.ListAlertEvents(t.Context(), connect.NewRequest(&heronv1.ListAlertEventsRequest{NodeId: id}))
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Msg.Events) != 2 || events.Msg.Events[0].Transition != "recovered" || events.Msg.Events[1].Transition != "firing" {
		t.Fatalf("events=%v", events.Msg.Events)
	}
	invalidRule := proto.Clone(rule).(*heronv1.AlertRule)
	invalidRule.RecoveryThreshold = 90
	if _, err := h.admin.SaveAlertRule(t.Context(), connect.NewRequest(&heronv1.SaveAlertRuleRequest{Rule: invalidRule})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("equal thresholds accepted: %v", err)
	}
}
