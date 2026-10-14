package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/updates"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func grantedClient(t *testing.T, h *harness, grant *heronv1.TokenGrant) (heronv1connect.AdminServiceClient, int64, string) {
	t.Helper()
	r, err := h.admin.CreateApiToken(t.Context(), connect.NewRequest(&heronv1.CreateApiTokenRequest{Name: "automation", Grant: grant}))
	if err != nil {
		t.Fatal(err)
	}
	token := r.Msg.Token
	client := heronv1connect.NewAdminServiceClient(h.srv.Client(), h.srv.URL, connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	})))
	return client, r.Msg.ApiToken.Id, token
}

func previewChange(t *testing.T, client heronv1connect.AdminServiceClient, m *heronv1.ExecuteChangeRequest) *heronv1.ExecuteChangeResponse {
	t.Helper()
	p := proto.Clone(m).(*heronv1.ExecuteChangeRequest)
	p.Preview = true
	r, err := client.ExecuteChange(t.Context(), connect.NewRequest(p))
	if err != nil {
		t.Fatal(err)
	}
	if r.Msg.ExpectedVersion == "" || r.Msg.Operation.CommittedAt != 0 || r.Msg.Operation.Id != "" || r.Msg.Result != nil {
		t.Fatalf("invalid preview: %v", r.Msg)
	}
	m.ExpectedVersion = r.Msg.ExpectedVersion
	return r.Msg
}

func TestAgenticPreviewCommitReplayAndConflict(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a, _ := h.createNode(t, "A")
	b, _ := h.createNode(t, "B")
	client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{a}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	m := &heronv1.ExecuteChangeRequest{RequestId: "edit-a", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"note"}}, Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: a, Note: "changed"}}}
	p := previewChange(t, client, m)
	if !strings.Contains(p.Operation.AfterJson, "changed") {
		t.Fatal("preview did not describe edit")
	}
	n, err := h.store.GetNode(t.Context(), a)
	if err != nil || n.Note != "" {
		t.Fatalf("preview changed database: %+v %v", n, err)
	}
	result, err := client.ExecuteChange(t.Context(), connect.NewRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	if result.Msg.Operation.CommittedAt == 0 || result.Msg.Result == nil || result.Msg.Replayed {
		t.Fatalf("missing committed result: %v", result.Msg)
	}
	n, err = h.store.GetNode(t.Context(), a)
	if err != nil || n.Note != "changed" || n.Name != "A" || n.TrafficResetDay != 1 {
		t.Fatalf("field mask lost fields: %+v %v", n, err)
	}
	retry, err := client.ExecuteChange(t.Context(), connect.NewRequest(m))
	if err != nil || !retry.Msg.Replayed || retry.Msg.Result != nil {
		t.Fatalf("unsafe replay: %v %v", retry, err)
	}
	if result.Msg.Operation.Id == "" || retry.Msg.Operation.Id != result.Msg.Operation.Id {
		t.Fatalf("receipt ID changed on replay: first=%v retry=%v", result.Msg.Operation, retry.Msg.Operation)
	}
	changed := proto.Clone(m).(*heronv1.ExecuteChangeRequest)
	changed.GetUpdateNode().Note = "other"
	if _, err = client.ExecuteChange(t.Context(), connect.NewRequest(changed)); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("request collision: %v", err)
	}
	changed.RequestId = "stale"
	if _, err = client.ExecuteChange(t.Context(), connect.NewRequest(changed)); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("stale version: %v", err)
	}
	changed.RequestId = "outside"
	changed.Preview = true
	changed.GetUpdateNode().Id = b
	if _, err = client.ExecuteChange(t.Context(), connect.NewRequest(changed)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("out-of-scope edit: %v", err)
	}
	list, err := client.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil || len(list.Msg.Nodes) != 1 || list.Msg.Nodes[0].Id != a {
		t.Fatalf("scope leak: %v %v", list, err)
	}
	snap, err := client.GetSnapshot(t.Context(), connect.NewRequest(&heronv1.GetSnapshotRequest{}))
	if err != nil || len(snap.Msg.Nodes) != 1 || snap.Msg.Nodes[0].Id != a {
		t.Fatalf("snapshot scope leak: %v %v", snap, err)
	}
	traffic, err := client.GetTraffic(t.Context(), connect.NewRequest(&heronv1.GetTrafficRequest{}))
	if err != nil || len(traffic.Msg.Nodes) != 1 {
		t.Fatalf("traffic scope leak: %v %v", traffic, err)
	}
	ops, err := client.ListOperations(t.Context(), connect.NewRequest(&heronv1.ListOperationsRequest{}))
	if err != nil || len(ops.Msg.Operations) != 1 {
		t.Fatalf("receipt: %v %v", ops, err)
	}
	if ops.Msg.Operations[0].Id != result.Msg.Operation.Id {
		t.Fatalf("listed receipt ID differs from execution: %v", ops.Msg)
	}
}

func TestAgenticCreateRotateDeleteAndIsolatedWindows(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	grant := &heronv1.TokenGrant{Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CREATE, heronv1.TokenPermission_TOKEN_PERMISSION_REGISTER, heronv1.TokenPermission_TOKEN_PERMISSION_ROTATE, heronv1.TokenPermission_TOKEN_PERMISSION_DELETE}}
	client, id, _ := grantedClient(t, h, grant)
	other, _, _ := grantedClient(t, h, grant)
	m := &heronv1.ExecuteChangeRequest{RequestId: "create", Change: &heronv1.ExecuteChangeRequest_CreateNode{CreateNode: &heronv1.CreateNodeRequest{Name: "owned"}}}
	previewChange(t, client, m)
	if nodes, err := h.store.ListNodes(t.Context()); err != nil || len(nodes) != 0 {
		t.Fatalf("preview created node: %v %v", nodes, err)
	}
	r, err := client.ExecuteChange(t.Context(), connect.NewRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	created := new(heronv1.CreateNodeResponse)
	if err := r.Msg.Result.UnmarshalTo(created); err != nil {
		t.Fatal(err)
	}
	if created.Node == nil || created.Token == "" {
		t.Fatalf("missing created node or secret: %v", created)
	}
	list, err := client.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil || len(list.Msg.Nodes) != 1 {
		t.Fatalf("new node not granted: %v %v", list, err)
	}
	foreign, err := other.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil || len(foreign.Msg.Nodes) != 0 {
		t.Fatalf("new node leaked: %v %v", foreign, err)
	}
	rotate := &heronv1.ExecuteChangeRequest{RequestId: "rotate", Change: &heronv1.ExecuteChangeRequest_RotateNodeToken{RotateNodeToken: &heronv1.RotateNodeTokenRequest{Id: created.Node.Id}}}
	previewChange(t, client, rotate)
	r, err = client.ExecuteChange(t.Context(), connect.NewRequest(rotate))
	if err != nil {
		t.Fatal(err)
	}
	rotated := new(heronv1.RotateNodeTokenResponse)
	if err := r.Msg.Result.UnmarshalTo(rotated); err != nil || rotated.Token == "" || rotated.Token == created.Token {
		t.Fatalf("rotation: %v %v", rotated, err)
	}
	replay, err := client.ExecuteChange(t.Context(), connect.NewRequest(rotate))
	if err != nil || !replay.Msg.Replayed || replay.Msg.Result != nil {
		t.Fatalf("rotation replay: %v %v", replay, err)
	}
	for i, c := range []heronv1connect.AdminServiceClient{client, other} {
		window := &heronv1.ExecuteChangeRequest{RequestId: "window", Change: &heronv1.ExecuteChangeRequest_OpenRegisterWindow{OpenRegisterWindow: &heronv1.OpenRegisterWindowRequest{TtlS: 600, MaxNodes: uint32(i + 1)}}}
		previewChange(t, c, window)
		if _, err := c.ExecuteChange(t.Context(), connect.NewRequest(window)); err != nil {
			t.Fatal(err)
		}
	}
	w, err := client.GetRegisterWindow(t.Context(), connect.NewRequest(&heronv1.GetRegisterWindowRequest{}))
	if err != nil || !w.Msg.Open || w.Msg.Remaining != 1 {
		t.Fatalf("window overwritten: %v %v", w, err)
	}
	close := &heronv1.ExecuteChangeRequest{RequestId: "close", Change: &heronv1.ExecuteChangeRequest_CloseRegisterWindow{CloseRegisterWindow: &heronv1.CloseRegisterWindowRequest{}}}
	previewChange(t, other, close)
	if _, err := other.ExecuteChange(t.Context(), connect.NewRequest(close)); err != nil {
		t.Fatal(err)
	}
	w, err = client.GetRegisterWindow(t.Context(), connect.NewRequest(&heronv1.GetRegisterWindowRequest{}))
	if err != nil || !w.Msg.Open {
		t.Fatalf("other token closed window: %v %v", w, err)
	}
	del := &heronv1.ExecuteChangeRequest{RequestId: "delete", Change: &heronv1.ExecuteChangeRequest_DeleteNode{DeleteNode: &heronv1.DeleteNodeRequest{Id: created.Node.Id}}}
	previewChange(t, client, del)
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(del)); err != nil {
		t.Fatal(err)
	}
	if r, err := client.ExecuteChange(t.Context(), connect.NewRequest(del)); err != nil || !r.Msg.Replayed {
		t.Fatalf("delete retry: %v %v", r, err)
	}
	ops, err := client.ListOperations(t.Context(), connect.NewRequest(&heronv1.ListOperationsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range ops.Msg.Operations {
		if strings.Contains(o.BeforeJson+o.AfterJson, created.Token) || strings.Contains(o.BeforeJson+o.AfterJson, rotated.Token) {
			t.Fatal("secret persisted in audit")
		}
	}
	if _, err = h.admin.DeleteApiToken(t.Context(), connect.NewRequest(&heronv1.DeleteApiTokenRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if _, err = client.ExecuteChange(t.Context(), connect.NewRequest(m)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked token accepted: %v", err)
	}
}

func TestAgenticCannotEscalateOrTouchHub(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	client, _, token := grantedClient(t, h, &heronv1.TokenGrant{AllNodes: true, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE, heronv1.TokenPermission_TOKEN_PERMISSION_UPDATE}})
	for _, method := range []string{"CreateApiToken", "DeleteApiToken", "ListApiTokens", "SaveNotifyChannel", "ListNotifyChannels", "SecurityAction", "StartUpdate", "CreateNode"} {
		if r := rawCall(t, h, method, "{}", bearer(token)); r.status != http.StatusForbidden {
			t.Errorf("%s escaped preauthorization: %+v", method, r)
		}
	}
	for _, m := range []*heronv1.ExecuteChangeRequest{
		{Preview: true, Change: &heronv1.ExecuteChangeRequest_StartUpdate{StartUpdate: &heronv1.StartUpdateRequest{NodeId: 0, Version: "v9.0.0"}}},
		{Preview: true, Change: &heronv1.ExecuteChangeRequest_CreateNode{CreateNode: &heronv1.CreateNodeRequest{Name: "unauthorized"}}},
	} {
		if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(m)); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("unauthorized change: %v", err)
		}
	}
}

func TestAgenticSharedRulesAndReadSurfaces(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a, _ := h.createNode(t, "a")
	b, _ := h.createNode(t, "b")
	for _, n := range []struct {
		id   int64
		name string
		tags []string
	}{{a, "a", []string{"shared", "mine"}}, {b, "b", []string{"shared", "outside"}}} {
		if _, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(&heronv1.UpdateNodeRequest{Id: n.id, Name: n.name, TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0), Tags: n.tags})); err != nil {
			t.Fatal(err)
		}
	}
	client, _, token := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{a}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	for _, scope := range []struct {
		name string
		all  bool
		ids  []int64
		tags []string
	}{{"cross", false, []int64{a, b}, nil}, {"all", true, nil, nil}, {"dynamic", false, nil, []string{"shared"}}} {
		t.Run(scope.name, func(t *testing.T) {
			probe := &heronv1.ExecuteChangeRequest{Preview: true, Change: &heronv1.ExecuteChangeRequest_SaveProbeTask{SaveProbeTask: &heronv1.SaveProbeTaskRequest{Task: validProbeTask(), AllNodes: scope.all, NodeIds: scope.ids, SelectorTags: scope.tags}}}
			if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(probe)); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("out-of-scope probe preview: %v", err)
			}
			rule := offlineRule()
			rule.AllNodes = scope.all
			rule.NodeIds = scope.ids
			rule.SelectorTags = scope.tags
			change := &heronv1.ExecuteChangeRequest{Preview: true, Change: &heronv1.ExecuteChangeRequest_SaveAlertRule{SaveAlertRule: &heronv1.SaveAlertRuleRequest{Rule: rule}}}
			if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(change)); connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("out-of-scope alert preview: %v", err)
			}
		})
	}
	if _, tasks := h.reg.List(); len(tasks) != 0 {
		t.Fatal("rejected preview published probe cache")
	}
	if len(h.alerts.Rules()) != 0 {
		t.Fatal("rejected preview published alert cache")
	}
	shared, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: validProbeTask(), NodeIds: []int64{a, b}}))
	if err != nil {
		t.Fatal(err)
	}
	sharedRule := offlineRule()
	sharedRule.AllNodes = false
	sharedRule.NodeIds = []int64{a, b}
	sharedSaved := saveRule(t, h, sharedRule)
	for _, change := range []*heronv1.ExecuteChangeRequest{
		{Preview: true, Change: &heronv1.ExecuteChangeRequest_DeleteProbeTask{DeleteProbeTask: &heronv1.DeleteProbeTaskRequest{Id: shared.Msg.Task.Task.Id}}},
		{Preview: true, Change: &heronv1.ExecuteChangeRequest_DeleteAlertRule{DeleteAlertRule: &heronv1.DeleteAlertRuleRequest{Id: sharedSaved.Id}}},
	} {
		if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(change)); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("shared object deletion allowed: %v", err)
		}
	}
	owned := &heronv1.ExecuteChangeRequest{RequestId: "probe", Change: &heronv1.ExecuteChangeRequest_SaveProbeTask{SaveProbeTask: &heronv1.SaveProbeTaskRequest{Task: validProbeTask(), NodeIds: []int64{a}}}}
	previewChange(t, client, owned)
	result, err := client.ExecuteChange(t.Context(), connect.NewRequest(owned))
	if err != nil {
		t.Fatal(err)
	}
	created := new(heronv1.SaveProbeTaskResponse)
	if err := result.Msg.Result.UnmarshalTo(created); err != nil {
		t.Fatal(err)
	}
	edit := &heronv1.ExecuteChangeRequest{RequestId: "probe-edit", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"task.target"}}, Change: &heronv1.ExecuteChangeRequest_SaveProbeTask{SaveProbeTask: &heronv1.SaveProbeTaskRequest{Task: &heronv1.ProbeTask{Id: created.Task.Task.Id, Target: "127.0.0.2"}}}}
	previewChange(t, client, edit)
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(edit)); err != nil {
		t.Fatal(err)
	}
	pl, err := client.ListProbeTasks(t.Context(), connect.NewRequest(&heronv1.ListProbeTasksRequest{}))
	if err != nil || len(pl.Msg.Tasks) != 1 || pl.Msg.Tasks[0].Task.Target != "127.0.0.2" {
		t.Fatalf("probe projection: %v %v", pl, err)
	}
	channel := saveChannel(t, h, webhook("http://127.0.0.1/SECRET"))
	refs, err := client.ListNotifyChannelRefs(t.Context(), connect.NewRequest(&heronv1.ListNotifyChannelRefsRequest{}))
	if err != nil || len(refs.Msg.Channels) != 1 || refs.Msg.Channels[0].Id != channel.Id {
		t.Fatalf("channel references: %v %v", refs, err)
	}
	rule := offlineRule()
	rule.AllNodes = false
	rule.NodeIds = []int64{a}
	rule.ChannelIds = []int64{channel.Id}
	rc := &heronv1.ExecuteChangeRequest{RequestId: "rule", Change: &heronv1.ExecuteChangeRequest_SaveAlertRule{SaveAlertRule: &heronv1.SaveAlertRuleRequest{Rule: rule}}}
	previewChange(t, client, rc)
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(rc)); err != nil {
		t.Fatal(err)
	}
	rl, err := client.ListAlertRules(t.Context(), connect.NewRequest(&heronv1.ListAlertRulesRequest{}))
	if err != nil || len(rl.Msg.Rules) != 1 {
		t.Fatalf("alert scope: %v %v", rl, err)
	}
	tags, err := client.ListTags(t.Context(), connect.NewRequest(&heronv1.ListTagsRequest{}))
	if err != nil || len(tags.Msg.Tags) != 2 {
		t.Fatalf("tag scope: %v %v", tags, err)
	}
	for _, tag := range tags.Msg.Tags {
		if tag.Name == "outside" || tag.NodeCount != 1 {
			t.Fatalf("tag leaked membership: %v", tag)
		}
	}
	h.clk.Advance(31 * time.Second)
	if err := h.alerts.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	events, err := client.ListAlertEvents(t.Context(), connect.NewRequest(&heronv1.ListAlertEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Msg.Events) == 0 {
		t.Fatal("no scoped events returned")
	}
	for _, ev := range events.Msg.Events {
		if ev.NodeId != a {
			t.Fatalf("event leaked: %v", ev)
		}
	}
	for _, method := range []string{"QueryMetrics", "QueryProbes"} {
		body := fmt.Sprintf(`{"nodeId":"%d","from":"%d","to":"%d"}`, b, h.clk.Now().Add(-time.Hour).Unix(), h.clk.Now().Unix())
		if r := rawCall(t, h, method, body, bearer(token)); r.status != http.StatusNotFound {
			t.Fatalf("%s leaked foreign node: %+v", method, r)
		}
	}
	updates, err := client.GetUpdates(t.Context(), connect.NewRequest(&heronv1.GetUpdatesRequest{}))
	if err != nil || len(updates.Msg.Targets) != 1 || updates.Msg.Targets[0].NodeId != a {
		t.Fatalf("update target leaked: %v %v", updates, err)
	}
}

func TestAgenticPreviewKeepsCredentialsAndConcurrentRetry(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, token := h.createNode(t, "stable")
	client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{node}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_ROTATE}})
	m := &heronv1.ExecuteChangeRequest{RequestId: "concurrent-rotate", Change: &heronv1.ExecuteChangeRequest_RotateNodeToken{RotateNodeToken: &heronv1.RotateNodeTokenRequest{Id: node}}}
	previewChange(t, client, m)
	if id, ok := h.auth.Authenticate(token); !ok || id != node {
		t.Fatal("preview revoked the live credential")
	}
	type outcome struct {
		response *connect.Response[heronv1.ExecuteChangeResponse]
		err      error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			r, err := client.ExecuteChange(t.Context(), connect.NewRequest(proto.Clone(m).(*heronv1.ExecuteChangeRequest)))
			results <- outcome{r, err}
		}()
	}
	close(start)
	committed, replayed := 0, 0
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.response.Msg.Replayed {
			replayed++
			if r.response.Msg.Result != nil {
				t.Fatal("retry returned a one-time secret")
			}
		} else {
			committed++
			rotated := new(heronv1.RotateNodeTokenResponse)
			if err := r.response.Msg.Result.UnmarshalTo(rotated); err != nil {
				t.Fatal(err)
			}
			if id, ok := h.auth.Authenticate(h.claimNode(t, rotated.Token)); !ok || id != node {
				t.Fatal("committed credential was not published")
			}
		}
	}
	if committed != 1 || replayed != 1 {
		t.Fatalf("duplicate mutation: committed=%d replayed=%d", committed, replayed)
	}
	if _, ok := h.auth.Authenticate(token); ok {
		t.Fatal("rotation retained the old credential")
	}
}

func TestAgenticRulePatchUsesDatabaseAndPreservesSelectors(t *testing.T) {
	t.Parallel()
	for _, dynamic := range []bool{false, true} {
		t.Run(fmt.Sprint(dynamic), func(t *testing.T) {
			h := newHarness(t, "")
			h.login(t)
			node, _ := h.createNode(t, "node")
			if _, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(&heronv1.UpdateNodeRequest{Id: node, Name: "node", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0), Tags: []string{"group"}})); err != nil {
				t.Fatal(err)
			}
			client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{AllNodes: true, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
			selector := store.NodeSelector{AllNodes: !dynamic}
			if dynamic {
				selector.Tags = []string{"group"}
			}
			saved, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: validProbeTask(), AllNodes: selector.AllNodes, SelectorTags: selector.Tags}))
			if err != nil {
				t.Fatal(err)
			}
			latest := proto.Clone(saved.Msg.Task.Task).(*heronv1.ProbeTask)
			latest.IntervalS = 120
			// 直接提交存储构造“事务已提交，缓存尚未发布”的确定性窗口。
			if _, _, err := h.store.SaveProbeTask(t.Context(), latest, selector); err != nil {
				t.Fatal(err)
			}
			m := &heronv1.ExecuteChangeRequest{RequestId: "patch-probe", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"task.target"}}, Change: &heronv1.ExecuteChangeRequest_SaveProbeTask{SaveProbeTask: &heronv1.SaveProbeTaskRequest{Task: &heronv1.ProbeTask{Id: latest.Id, Target: "127.0.0.2"}}}}
			previewChange(t, client, m)
			_, cached := h.reg.List()
			if len(cached) != 1 || cached[0].Task.Target != latest.Target {
				t.Fatal("preview published probe changes")
			}
			if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(m)); err != nil {
				t.Fatal(err)
			}
			_, tasks, err := h.store.LoadProbeTasks(t.Context())
			if err != nil || len(tasks) != 1 || tasks[0].Task.IntervalS != 120 || tasks[0].Task.Target != "127.0.0.2" || tasks[0].AllNodes != !dynamic || (len(tasks[0].SelectorTags) != 0) != dynamic {
				t.Fatalf("patch lost durable fields or selector: %+v %v", tasks, err)
			}
			rule := offlineRule()
			rule.AllNodes, rule.SelectorTags = selector.AllNodes, selector.Tags
			r := saveRule(t, h, rule)
			rows, err := h.store.ListAlertRules(t.Context())
			if err != nil || len(rows) != 1 {
				t.Fatalf("load rule: %+v %v", rows, err)
			}
			durable := rows[0]
			durable.Name, durable.NodeIDs = "durable", nil
			if _, err := h.store.SaveAlertRule(t.Context(), durable); err != nil {
				t.Fatal(err)
			}
			m = &heronv1.ExecuteChangeRequest{RequestId: "patch-alert", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rule.enabled"}}, Change: &heronv1.ExecuteChangeRequest_SaveAlertRule{SaveAlertRule: &heronv1.SaveAlertRuleRequest{Rule: &heronv1.AlertRule{Id: r.Id, Enabled: false}}}}
			previewChange(t, client, m)
			if !h.alerts.Rules()[0].Enabled {
				t.Fatal("preview published alert changes")
			}
			if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(m)); err != nil {
				t.Fatal(err)
			}
			rows, err = h.store.ListAlertRules(t.Context())
			if err != nil || len(rows) != 1 || rows[0].Name != "durable" || rows[0].Enabled || rows[0].AllNodes != !dynamic || (len(rows[0].SelectorTags) != 0) != dynamic {
				t.Fatalf("alert patch lost durable fields or selector: %+v %v", rows, err)
			}
		})
	}
}

func TestAgenticHistoryHidesReassignedTaskMetadata(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a, _ := h.createNode(t, "a")
	b, _ := h.createNode(t, "b")
	client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{a}})
	saved, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: validProbeTask(), NodeIds: []int64{a}}))
	if err != nil {
		t.Fatal(err)
	}
	task := saved.Msg.Task.Task
	base := h.clk.Now().Truncate(time.Hour).Unix()
	if _, err := h.store.WriteMinuteBatch(t.Context(), metric.Batch{Probes: []metric.ProbeRow{{NodeID: a, TS: base, TaskID: task.Id, Bucket: &metric.ProbeBucket{Sent: 1, Lost: 1}}}}); err != nil {
		t.Fatal(err)
	}
	query := func(c heronv1connect.AdminServiceClient) *heronv1.QueryProbesResponse {
		t.Helper()
		r, err := c.QueryProbes(t.Context(), connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: a, From: base, To: base + 3600}))
		if err != nil || len(r.Msg.Series) != 1 {
			t.Fatalf("history: %v %v", r, err)
		}
		return r.Msg
	}
	if query(client).Series[0].Target != task.Target {
		t.Fatal("authorized task metadata was hidden")
	}
	task.Target = "192.0.2.25"
	if _, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: task, NodeIds: []int64{b}})); err != nil {
		t.Fatal(err)
	}
	if series := query(client).Series[0]; series.Target != "" || series.Kind != heronv1.ProbeKind_PROBE_KIND_UNSPECIFIED {
		t.Fatalf("foreign task metadata leaked through history: %v", series)
	}
	if query(h.admin).Series[0].Target != task.Target {
		t.Fatal("session lost global metadata visibility")
	}
}

func TestAgenticScopedWritesPreserveGlobalExpiryState(t *testing.T) {
	t.Parallel()
	for _, save := range []bool{false, true} {
		t.Run(fmt.Sprint(save), func(t *testing.T) {
			h := newHarness(t, "")
			h.login(t)
			a, _ := h.createNode(t, "a")
			b, _ := h.createNode(t, "b")
			h.update(t, billed(b, &heronv1.Billing{ExpiresOn: "2026-01-04"}))
			saveRule(t, h, expiryRuleProto())
			if len(h.alerts.States()) != 1 {
				t.Fatal("expiry fixture did not fire")
			}
			client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{a}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
			m := &heronv1.ExecuteChangeRequest{RequestId: "billing", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"billing.expires_on"}}, Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: a, Billing: &heronv1.Billing{ExpiresOn: "2030-01-01"}}}}
			if save {
				r := expiryRuleProto()
				r.AllNodes, r.NodeIds = false, []int64{a}
				m = &heronv1.ExecuteChangeRequest{RequestId: "expiry", Change: &heronv1.ExecuteChangeRequest_SaveAlertRule{SaveAlertRule: &heronv1.SaveAlertRuleRequest{Rule: r}}}
			}
			previewChange(t, client, m)
			if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(m)); err != nil {
				t.Fatal(err)
			}
			if states := h.alerts.States(); len(states) != 1 || states[0].NodeID != b {
				t.Fatalf("scoped write pruned foreign alert state: %v", states)
			}
			if err := h.alerts.SweepExpiry(t.Context()); err != nil {
				t.Fatal(err)
			}
			events, err := h.admin.ListAlertEvents(t.Context(), connect.NewRequest(&heronv1.ListAlertEventsRequest{NodeId: b}))
			if err != nil || len(events.Msg.Events) != 1 {
				t.Fatalf("scoped sweep repeated foreign notification: %v %v", events, err)
			}
			list, err := client.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
			if err != nil || len(list.Msg.Nodes) != 1 || list.Msg.Nodes[0].Id != a {
				t.Fatalf("system scan widened request visibility: %v %v", list, err)
			}
		})
	}
}

func TestAgenticReferenceErrorsHideForeignRules(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, _ := h.createNode(t, "a")
	client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{node}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	saved, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: validProbeTask(), NodeIds: []int64{node}}))
	if err != nil {
		t.Fatal(err)
	}
	task := saved.Msg.Task.Task.Id
	saveRule(t, h, &heronv1.AlertRule{Name: "hidden-rule-name", Kind: heronv1.AlertKind_ALERT_KIND_PROBE, AllNodes: true, TaskId: task, Metric: heronv1.ProbeMetric_PROBE_METRIC_LOSS_PCT, Threshold: 100, ForMinutes: 1})
	visible := saveRule(t, h, &heronv1.AlertRule{Name: "visible-rule-name", Kind: heronv1.AlertKind_ALERT_KIND_PROBE, NodeIds: []int64{node}, TaskId: task, Metric: heronv1.ProbeMetric_PROBE_METRIC_LOSS_PCT, Threshold: 100, ForMinutes: 1})
	check := func(wantVisible bool) {
		t.Helper()
		_, err := client.ExecuteChange(t.Context(), connect.NewRequest(&heronv1.ExecuteChangeRequest{Preview: true, Change: &heronv1.ExecuteChangeRequest_DeleteProbeTask{DeleteProbeTask: &heronv1.DeleteProbeTaskRequest{Id: task}}}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || strings.Contains(err.Error(), "hidden-rule-name") || strings.Contains(err.Error(), "visible-rule-name") != wantVisible {
			t.Fatalf("reference error did not respect visibility: %v", err)
		}
	}
	check(true)
	if _, err := h.admin.DeleteAlertRule(t.Context(), connect.NewRequest(&heronv1.DeleteAlertRuleRequest{Id: visible.Id})); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := h.admin.DeleteProbeTask(t.Context(), connect.NewRequest(&heronv1.DeleteProbeTaskRequest{Id: task})); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "hidden-rule-name") {
		t.Fatalf("session lost reference details: %v", err)
	}
}

func rawJSONChange(t *testing.T, h *harness, token string) func(map[string]any) map[string]any {
	t.Helper()
	return func(body map[string]any) map[string]any {
		t.Helper()
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+"/heron.v1.AdminService/ExecuteChange", strings.NewReader(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("raw change failed: status=%d body=%s err=%v", resp.StatusCode, data, err)
		}
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
}

func TestAgenticRawJSONFieldMaskAndAny(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, _ := h.createNode(t, "original")
	_, _, token := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{node}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	call := rawJSONChange(t, h, token)
	body := map[string]any{"preview": true, "updateMask": "note,trafficResetDay", "updateNode": map[string]any{"id": fmt.Sprint(node), "note": "raw-json", "trafficResetDay": 12}}
	p := call(body)
	if _, ok := p["expectedVersion"].(string); !ok || p["result"] != nil {
		t.Fatalf("preview JSON contract: %v", p)
	}
	body["preview"], body["requestId"], body["expectedVersion"] = false, "raw-json", p["expectedVersion"]
	result := call(body)
	any, ok := result["result"].(map[string]any)
	if !ok || any["@type"] != "type.googleapis.com/heron.v1.UpdateNodeResponse" {
		t.Fatalf("Any JSON contract: %v", result)
	}
	n, err := h.store.GetNode(t.Context(), node)
	if err != nil || n.Note != "raw-json" || n.Name != "original" || n.TrafficResetDay != 12 {
		t.Fatalf("JSON field mask lost fields: %+v %v", n, err)
	}
	if retry := call(body); retry["replayed"] != true || retry["result"] != nil {
		t.Fatalf("raw retry contract: %v", retry)
	}
}

func TestAgenticRawJSONOneTimeSecrets(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, _ := h.createNode(t, "rotatable")
	client, _, token := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{node}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CREATE, heronv1.TokenPermission_TOKEN_PERMISSION_ROTATE, heronv1.TokenPermission_TOKEN_PERMISSION_REGISTER}})
	call := rawJSONChange(t, h, token)
	for _, tc := range []struct {
		name, response, field string
		request               map[string]any
	}{
		{"createNode", "CreateNodeResponse", "token", map[string]any{"name": "json-created"}},
		{"rotateNodeToken", "RotateNodeTokenResponse", "token", map[string]any{"id": fmt.Sprint(node)}},
		{"openRegisterWindow", "OpenRegisterWindowResponse", "key", map[string]any{"ttlS": 600, "maxNodes": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"preview": true, tc.name: tc.request}
			p := call(body)
			if p["result"] != nil {
				t.Fatalf("secret preview exposed result: %v", p)
			}
			body["preview"], body["requestId"], body["expectedVersion"] = false, tc.name, p["expectedVersion"]
			first := call(body)
			result, ok := first["result"].(map[string]any)
			if !ok || result["@type"] != "type.googleapis.com/heron.v1."+tc.response {
				t.Fatalf("secret Any JSON type: %v", first)
			}
			secret, ok := result[tc.field].(string)
			if !ok || secret == "" {
				t.Fatalf("first JSON response lacks %s", tc.field)
			}
			retry := call(body)
			encoded, err := json.Marshal(retry)
			if err != nil || retry["replayed"] != true || retry["result"] != nil || strings.Contains(string(encoded), secret) {
				t.Fatalf("JSON retry exposed secret: %v %v", retry, err)
			}
			ops, err := client.ListOperations(t.Context(), connect.NewRequest(&heronv1.ListOperationsRequest{RequestId: tc.name}))
			if err != nil || len(ops.Msg.Operations) != 1 || strings.Contains(ops.Msg.String(), secret) {
				t.Fatalf("receipt exposed secret or missing: %v %v", ops, err)
			}
		})
	}
}

func TestAgenticFieldMaskClearsAndRejects(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, _ := h.createNode(t, "original")
	h.update(t, &heronv1.UpdateNodeRequest{Id: node, Name: "original", Note: "kept", TrafficResetDay: 12, OfflineGraceS: proto.Uint32(60), Tags: []string{"group"}, Billing: &heronv1.Billing{Price: "10", Currency: "USD", ExpiresOn: "2030-01-01"}})
	client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{node}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	m := &heronv1.ExecuteChangeRequest{RequestId: "clear", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"tags", "billing", "offline_grace_s"}}, Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: node, OfflineGraceS: proto.Uint32(0)}}}
	previewChange(t, client, m)
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(m)); err != nil {
		t.Fatal(err)
	}
	n, err := h.store.GetNode(t.Context(), node)
	if err != nil || len(n.Tags) != 0 || n.Billing != (store.Billing{}) || n.OfflineGraceS != 0 || n.Name != "original" || n.Note != "kept" || n.TrafficResetDay != 12 {
		t.Fatalf("field mask clear lost semantics: %+v %v", n, err)
	}
	snapshot := func() string {
		t.Helper()
		n, err := h.store.GetNode(t.Context(), node)
		if err != nil {
			t.Fatal(err)
		}
		version, tasks := h.reg.List()
		b, err := json.Marshal([]any{n, version, tasks, h.alerts.Rules()})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for i, path := range []string{"id", "billing.days_left", "not_a_field", "note.child"} {
		t.Run(path, func(t *testing.T) {
			before := snapshot()
			bad := &heronv1.ExecuteChangeRequest{RequestId: fmt.Sprintf("invalid-%d", i), Preview: true, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"note", path}}, Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: node, Note: fmt.Sprintf("must-not-apply-%d", i)}}}
			valid := proto.Clone(bad).(*heronv1.ExecuteChangeRequest)
			valid.UpdateMask.Paths = []string{"note"}
			bad.ExpectedVersion = previewChange(t, client, valid).ExpectedVersion
			for _, preview := range []bool{true, false} {
				bad.Preview = preview
				if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(bad)); connect.CodeOf(err) != connect.CodeInvalidArgument {
					t.Errorf("invalid mask accepted (preview=%t): %v", preview, err)
				}
				if snapshot() != before {
					t.Error("rejected mask changed resource or cache")
				}
			}
			ops, err := client.ListOperations(t.Context(), connect.NewRequest(&heronv1.ListOperationsRequest{RequestId: bad.RequestId}))
			if err != nil || len(ops.Msg.Operations) != 0 {
				t.Fatalf("rejected mask left receipt: %v %v", ops, err)
			}
		})
	}
}

func TestAgenticAlertScopeIncludesReferencedProbe(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a, _ := h.createNode(t, "a")
	b, _ := h.createNode(t, "b")
	client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{a}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	all, _, _ := grantedClient(t, h, &heronv1.TokenGrant{AllNodes: true, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	for _, outside := range []bool{false, true} {
		t.Run(fmt.Sprint(outside), func(t *testing.T) {
			ids := []int64{a}
			if outside {
				ids = append(ids, b)
			}
			saved, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: validProbeTask(), NodeIds: ids}))
			if err != nil {
				t.Fatal(err)
			}
			rule := saveRule(t, h, &heronv1.AlertRule{Name: fmt.Sprintf("rule-%t", outside), Kind: heronv1.AlertKind_ALERT_KIND_PROBE, NodeIds: []int64{a}, TaskId: saved.Msg.Task.Task.Id, Metric: heronv1.ProbeMetric_PROBE_METRIC_LOSS_PCT, Threshold: 100, ForMinutes: 1})
			for _, reader := range []struct {
				name    string
				client  heronv1connect.AdminServiceClient
				visible bool
			}{{"scoped", client, !outside}, {"all", all, true}, {"session", h.admin, true}} {
				t.Run(reader.name, func(t *testing.T) {
					list, err := reader.client.ListAlertRules(t.Context(), connect.NewRequest(&heronv1.ListAlertRulesRequest{}))
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, r := range list.Msg.Rules {
						found = found || r.Id == rule.Id
					}
					if found != reader.visible {
						t.Errorf("alert read scope disagrees with referenced probe: found=%t want=%t", found, reader.visible)
					}
					_, err = reader.client.ExecuteChange(t.Context(), connect.NewRequest(&heronv1.ExecuteChangeRequest{Preview: true, Change: &heronv1.ExecuteChangeRequest_DeleteAlertRule{DeleteAlertRule: &heronv1.DeleteAlertRuleRequest{Id: rule.Id}}}))
					if reader.visible && err != nil || !reader.visible && connect.CodeOf(err) != connect.CodePermissionDenied {
						t.Errorf("alert write scope disagrees with read scope: %v", err)
					}
				})
			}
		})
	}
}

func TestAgenticNodeUpdatePreviewCommitReplay(t *testing.T) {
	t.Parallel()
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancel), func(t *testing.T) {
			h := newHarness(t, "")
			h.login(t)
			node, _ := h.createNode(t, "updatable")
			initial := &heronv1.UpdateStatus{Supported: true, Version: "v0.1.0"}
			want := "queued"
			m := &heronv1.ExecuteChangeRequest{RequestId: "update", Change: &heronv1.ExecuteChangeRequest_StartUpdate{StartUpdate: &heronv1.StartUpdateRequest{NodeId: node, Version: "v0.2.0"}}}
			if cancel {
				initial.Task = &heronv1.UpdateTask{Id: "queued-task", Version: "v0.2.0", State: "queued", ExpiresAt: h.clk.Now().Add(time.Hour).Unix()}
				want = "cancelled"
				m.Change = &heronv1.ExecuteChangeRequest_CancelUpdate{CancelUpdate: &heronv1.CancelUpdateRequest{NodeId: node, Id: initial.Task.Id}}
			}
			if err := h.store.SaveNodeUpdate(t.Context(), node, initial); err != nil {
				t.Fatal(err)
			}
			manager := updates.New(h.store, h.clk, slog.Default(), "v0.2.0")
			if err := manager.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			h.svc.cfg.Updates = manager
			client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{node}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_UPDATE}})
			previewChange(t, client, m)
			states, err := h.store.NodeUpdates(t.Context())
			if err != nil || !proto.Equal(states[node], initial) || !proto.Equal(manager.Snapshot(node), initial) {
				t.Fatalf("update preview published state: durable=%v memory=%v err=%v", states[node], manager.Snapshot(node), err)
			}
			r, err := client.ExecuteChange(t.Context(), connect.NewRequest(m))
			if err != nil {
				t.Fatal(err)
			}
			states, err = h.store.NodeUpdates(t.Context())
			if err != nil || states[node].GetTask().GetState() != want || !proto.Equal(states[node], manager.Snapshot(node)) {
				t.Fatalf("update commit diverged: durable=%v memory=%v err=%v", states[node], manager.Snapshot(node), err)
			}
			if r.Msg.Operation.GetCommittedAt() == 0 || r.Msg.Result == nil {
				t.Fatalf("update commit lacks receipt or result: %v", r.Msg)
			}
			retry, err := client.ExecuteChange(t.Context(), connect.NewRequest(m))
			if err != nil || !retry.Msg.GetReplayed() || retry.Msg.Result != nil {
				t.Fatalf("update replay: %v %v", retry, err)
			}
		})
	}
}

func TestAgenticReceiptFailureRollsBackBeforePublication(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"node", "rotate", "probe", "alert", "update"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, "")
			h.login(t)
			node, token := h.createNode(t, "unchanged")
			if err := h.store.SaveNodeUpdate(t.Context(), node, &heronv1.UpdateStatus{Supported: true, Version: "v0.1.0"}); err != nil {
				t.Fatal(err)
			}
			manager := updates.New(h.store, h.clk, slog.Default(), "v0.2.0")
			if err := manager.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			h.svc.cfg.Updates = manager
			client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{node}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE, heronv1.TokenPermission_TOKEN_PERMISSION_ROTATE, heronv1.TokenPermission_TOKEN_PERMISSION_UPDATE}})
			m := &heronv1.ExecuteChangeRequest{RequestId: "recoverable"}
			switch action {
			case "node":
				m.Change = &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: node, Note: "changed"}}
				m.UpdateMask = &fieldmaskpb.FieldMask{Paths: []string{"note"}}
			case "rotate":
				m.Change = &heronv1.ExecuteChangeRequest_RotateNodeToken{RotateNodeToken: &heronv1.RotateNodeTokenRequest{Id: node}}
			case "probe":
				m.Change = &heronv1.ExecuteChangeRequest_SaveProbeTask{SaveProbeTask: &heronv1.SaveProbeTaskRequest{Task: validProbeTask(), NodeIds: []int64{node}}}
			case "alert":
				m.Change = &heronv1.ExecuteChangeRequest_SaveAlertRule{SaveAlertRule: &heronv1.SaveAlertRuleRequest{Rule: &heronv1.AlertRule{Name: "new-rule", Kind: heronv1.AlertKind_ALERT_KIND_OFFLINE, NodeIds: []int64{node}}}}
			case "update":
				m.Change = &heronv1.ExecuteChangeRequest_StartUpdate{StartUpdate: &heronv1.StartUpdateRequest{NodeId: node, Version: "v0.2.0"}}
			}
			cache := func() string {
				t.Helper()
				version, tasks := h.reg.List()
				b, err := json.Marshal([]any{version, tasks, h.alerts.Rules(), manager.Snapshot(node)})
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			before := previewChange(t, client, m)
			cached := cache()
			db, err := sql.Open("sqlite", h.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`CREATE TRIGGER reject_operation BEFORE INSERT ON operation BEGIN SELECT RAISE(ABORT, 'receipt unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			var triggers int
			if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE type='trigger' AND name='reject_operation'").Scan(&triggers); err != nil || triggers != 1 {
				t.Fatalf("failure injection not installed: %d %v", triggers, err)
			}
			if r, err := client.ExecuteChange(t.Context(), connect.NewRequest(m)); err == nil {
				t.Errorf("receipt failure reported success: %v", r)
			}
			after := previewChange(t, client, m)
			if after.ExpectedVersion != before.ExpectedVersion || after.Operation.BeforeJson != before.Operation.BeforeJson {
				t.Error("receipt failure changed durable resource")
			}
			if got := cache(); got != cached {
				t.Errorf("receipt failure published cache: before=%s after=%s", cached, got)
			}
			if id, ok := h.auth.Authenticate(token); !ok || id != node {
				t.Error("receipt failure revoked live credential")
			}
			ops, err := client.ListOperations(t.Context(), connect.NewRequest(&heronv1.ListOperationsRequest{}))
			if err != nil || len(ops.Msg.Operations) != 0 {
				t.Fatalf("failed transaction left receipt: %v %v", ops, err)
			}
			if _, err := db.Exec("DROP TRIGGER reject_operation"); err != nil {
				t.Fatal(err)
			}
			r, err := client.ExecuteChange(t.Context(), connect.NewRequest(m))
			if err != nil || r.Msg.GetReplayed() || r.Msg.Operation.GetCommittedAt() == 0 || r.Msg.Result == nil {
				t.Fatalf("retry after receipt recovery did not commit: %v %v", r, err)
			}
			r, err = client.ExecuteChange(t.Context(), connect.NewRequest(m))
			if err != nil || !r.Msg.GetReplayed() || r.Msg.Result != nil {
				t.Fatalf("committed retry executed again: %v %v", r, err)
			}
			ops, err = client.ListOperations(t.Context(), connect.NewRequest(&heronv1.ListOperationsRequest{}))
			if err != nil || len(ops.Msg.Operations) != 1 {
				t.Fatalf("recovery did not leave exactly one receipt: %v %v", ops, err)
			}
		})
	}
}

// 节点可编辑字段在 ExecuteChange 里有两张手写登记表：合并基底（prepareChange 从库里的节点构造的 UpdateNodeRequest）
// 与变更快照的列白名单（store 的 changeKinds）。漏登一个字段有两种后果：只改别的字段时它被整体替换成零值；
// 只改它自己时预期版本不变、审计前后相同。本用例按 UpdateNodeRequest 的描述符枚举每个字段，新增字段没有在这里
// 给出取值就直接失败，迫使两张登记表与这里一起更新。
func TestUpdateNodeChangeCoversEveryEditableField(t *testing.T) {
	t.Parallel()
	type pair struct {
		first, second func(*heronv1.UpdateNodeRequest)
	}
	values := map[string]pair{
		"traffic_quota_bytes": {func(r *heronv1.UpdateNodeRequest) { r.TrafficQuotaBytes = 1000 }, func(r *heronv1.UpdateNodeRequest) { r.TrafficQuotaBytes = 2000 }},
		"traffic_quota_mode": {func(r *heronv1.UpdateNodeRequest) {
			r.TrafficQuotaMode = heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_RX
		}, func(r *heronv1.UpdateNodeRequest) {
			r.TrafficQuotaMode = heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_TX
		}},
		"name":              {func(r *heronv1.UpdateNodeRequest) { r.Name = "first" }, func(r *heronv1.UpdateNodeRequest) { r.Name = "second" }},
		"public":            {func(r *heronv1.UpdateNodeRequest) { r.Public = true }, func(r *heronv1.UpdateNodeRequest) { r.Public = false }},
		"note":              {func(r *heronv1.UpdateNodeRequest) { r.Note = "first note" }, func(r *heronv1.UpdateNodeRequest) { r.Note = "second note" }},
		"public_remark":     {func(r *heronv1.UpdateNodeRequest) { r.PublicRemark = "first remark" }, func(r *heronv1.UpdateNodeRequest) { r.PublicRemark = "second remark" }},
		"traffic_reset_day": {func(r *heronv1.UpdateNodeRequest) { r.TrafficResetDay = 12 }, func(r *heronv1.UpdateNodeRequest) { r.TrafficResetDay = 13 }},
		"offline_grace_s":   {func(r *heronv1.UpdateNodeRequest) { r.OfflineGraceS = proto.Uint32(60) }, func(r *heronv1.UpdateNodeRequest) { r.OfflineGraceS = proto.Uint32(120) }},
		"billing": {func(r *heronv1.UpdateNodeRequest) {
			r.Billing = &heronv1.Billing{Price: "10", Currency: "USD", ExpiresOn: "2030-01-01"}
		}, func(r *heronv1.UpdateNodeRequest) {
			r.Billing = &heronv1.Billing{Price: "20", Currency: "USD", ExpiresOn: "2031-01-01"}
		}},
		"country_pin": {func(r *heronv1.UpdateNodeRequest) { r.CountryPin = "JP" }, func(r *heronv1.UpdateNodeRequest) { r.CountryPin = "US" }},
		"tags":        {func(r *heronv1.UpdateNodeRequest) { r.Tags = []string{"first"} }, func(r *heronv1.UpdateNodeRequest) { r.Tags = []string{"second"} }},
		// 两个开关互斥：first 都为 false，初始写入不冲突，逐字段翻转时另一个保持 false。
		"maintenance": {func(r *heronv1.UpdateNodeRequest) { r.Maintenance = false }, func(r *heronv1.UpdateNodeRequest) { r.Maintenance = true }},
		"rented":      {func(r *heronv1.UpdateNodeRequest) { r.Rented = false }, func(r *heronv1.UpdateNodeRequest) { r.Rented = true }},
		"ipv4_pin":    {func(r *heronv1.UpdateNodeRequest) { r.Ipv4Pin = "8.8.8.8" }, func(r *heronv1.UpdateNodeRequest) { r.Ipv4Pin = "1.1.1.1" }},
		"ipv6_pin":    {func(r *heronv1.UpdateNodeRequest) { r.Ipv6Pin = "2606:4700::1111" }, func(r *heronv1.UpdateNodeRequest) { r.Ipv6Pin = "2001:4860::8888" }},
	}
	fields := (&heronv1.UpdateNodeRequest{}).ProtoReflect().Descriptor().Fields()
	var names []string
	for i := 0; i < fields.Len(); i++ {
		name := string(fields.Get(i).Name())
		if name == "id" {
			continue
		}
		if _, ok := values[name]; !ok {
			t.Fatalf("UpdateNodeRequest.%s has no values here: register it in prepareChange's merge base, the store changeKinds snapshot columns and this test", name)
		}
		names = append(names, name)
	}
	nodeFields := (&heronv1.Node{}).ProtoReflect().Descriptor().Fields()
	read := func(n *heronv1.Node, name string) protoreflect.Value {
		t.Helper()
		f := nodeFields.ByName(protoreflect.Name(name))
		if f == nil {
			t.Fatalf("Node has no field %s to read the edited value back", name)
		}
		if name == "billing" {
			b := proto.Clone(n.GetBilling()).(*heronv1.Billing)
			b.DaysLeft = nil
			return protoreflect.ValueOfMessage(b.ProtoReflect())
		}
		return n.ProtoReflect().Get(f)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, "")
			h.login(t)
			id, _ := h.createNode(t, "seed")
			initial := &heronv1.UpdateNodeRequest{Id: id}
			for _, n := range names {
				values[n].first(initial)
			}
			before := h.update(t, initial)
			client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{id}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
			list := func() *heronv1.Node {
				t.Helper()
				r, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
				if err != nil {
					t.Fatal(err)
				}
				for _, n := range r.Msg.Nodes {
					if n.Id == id {
						return n
					}
				}
				t.Fatal("node vanished")
				return nil
			}
			// 只改另一个字段：本字段的值必须保持。
			other := "note"
			if name == "note" {
				other = "name"
			}
			edit := &heronv1.UpdateNodeRequest{Id: id}
			values[other].second(edit)
			m := &heronv1.ExecuteChangeRequest{RequestId: "other-" + name, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{other}}, Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: edit}}
			previewChange(t, client, m)
			if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(m)); err != nil {
				t.Fatal(err)
			}
			if after := list(); !read(after, name).Equal(read(before, name)) {
				t.Fatalf("changing %s replaced %s:\nbefore: %v\nafter:  %v", other, name, prototext.Format(before), prototext.Format(after))
			}
			// 只改本字段：预期版本必须变化，审计前后必须不同。
			edit = &heronv1.UpdateNodeRequest{Id: id}
			values[name].second(edit)
			m = &heronv1.ExecuteChangeRequest{RequestId: "self-" + name, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{name}}, Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: edit}}
			p := previewChange(t, client, m)
			if p.Operation.BeforeJson == p.Operation.AfterJson {
				t.Fatalf("changing only %s leaves no audit difference: %s", name, p.Operation.AfterJson)
			}
			if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(m)); err != nil {
				t.Fatal(err)
			}
			// 只为读出当前预期版本：再提交一次与现值相同的另一字段，不改变任何状态。
			same := &heronv1.UpdateNodeRequest{Id: id}
			values[other].second(same)
			probe := &heronv1.ExecuteChangeRequest{RequestId: "probe-" + name, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{other}}, Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: same}}
			if previewChange(t, client, probe).ExpectedVersion == m.ExpectedVersion {
				t.Fatalf("changing only %s did not change the expected version", name)
			}
			m.RequestId = "stale-" + name
			if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(m)); connect.CodeOf(err) != connect.CodeAborted {
				t.Fatalf("stale version after changing %s: %v, want Aborted", name, err)
			}
		})
	}
}

// changeExpectations 是 ExecuteChangeRequest.change 每个 oneof 分支期望映射到的操作，以及一份能被预览执行的请求。
// 新增分支时这里没有条目，TestExecuteChangeCoversEveryOneofField 直接失败。
type changeFixture struct {
	node, other, probe, rule int64
}

var changeExpectations = map[string]struct {
	action store.ChangeAction
	build  func(f changeFixture) *heronv1.ExecuteChangeRequest
}{
	"create_node": {store.ActionCreateNode, func(changeFixture) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{Change: &heronv1.ExecuteChangeRequest_CreateNode{CreateNode: &heronv1.CreateNodeRequest{Name: "new"}}}
	}},
	"update_node": {store.ActionUpdateNode, func(f changeFixture) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"note"}}, Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: f.node, Note: "edited"}}}
	}},
	"delete_node": {store.ActionDeleteNode, func(f changeFixture) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{Change: &heronv1.ExecuteChangeRequest_DeleteNode{DeleteNode: &heronv1.DeleteNodeRequest{Id: f.node}}}
	}},
	"rotate_node_token": {store.ActionRotateNodeToken, func(f changeFixture) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{Change: &heronv1.ExecuteChangeRequest_RotateNodeToken{RotateNodeToken: &heronv1.RotateNodeTokenRequest{Id: f.node}}}
	}},
	"open_register_window": {store.ActionOpenRegisterWindow, func(changeFixture) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{Change: &heronv1.ExecuteChangeRequest_OpenRegisterWindow{OpenRegisterWindow: &heronv1.OpenRegisterWindowRequest{TtlS: 600, MaxNodes: 1}}}
	}},
	"close_register_window": {store.ActionCloseRegisterWindow, func(changeFixture) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{Change: &heronv1.ExecuteChangeRequest_CloseRegisterWindow{CloseRegisterWindow: &heronv1.CloseRegisterWindowRequest{}}}
	}},
	"save_probe_task": {store.ActionSaveProbeTask, func(f changeFixture) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{Change: &heronv1.ExecuteChangeRequest_SaveProbeTask{SaveProbeTask: &heronv1.SaveProbeTaskRequest{Task: validProbeTask(), NodeIds: []int64{f.node}}}}
	}},
	"delete_probe_task": {store.ActionDeleteProbeTask, func(f changeFixture) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{Change: &heronv1.ExecuteChangeRequest_DeleteProbeTask{DeleteProbeTask: &heronv1.DeleteProbeTaskRequest{Id: uint64(f.probe)}}}
	}},
	"save_alert_rule": {store.ActionSaveAlertRule, func(f changeFixture) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{Change: &heronv1.ExecuteChangeRequest_SaveAlertRule{SaveAlertRule: &heronv1.SaveAlertRuleRequest{Rule: &heronv1.AlertRule{Name: "new-rule", Kind: heronv1.AlertKind_ALERT_KIND_OFFLINE, NodeIds: []int64{f.node}}}}}
	}},
	"delete_alert_rule": {store.ActionDeleteAlertRule, func(f changeFixture) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{Change: &heronv1.ExecuteChangeRequest_DeleteAlertRule{DeleteAlertRule: &heronv1.DeleteAlertRuleRequest{Id: f.rule}}}
	}},
	"start_update": {store.ActionStartUpdate, func(f changeFixture) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{Change: &heronv1.ExecuteChangeRequest_StartUpdate{StartUpdate: &heronv1.StartUpdateRequest{NodeId: f.node, Version: "v0.2.0"}}}
	}},
	"cancel_update": {store.ActionCancelUpdate, func(f changeFixture) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{Change: &heronv1.ExecuteChangeRequest_CancelUpdate{CancelUpdate: &heronv1.CancelUpdateRequest{NodeId: f.other, Id: "queued-task"}}}
	}},
	"renew_node_billing": {store.ActionRenewNodeBilling, func(f changeFixture) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{Change: &heronv1.ExecuteChangeRequest_RenewNodeBilling{RenewNodeBilling: &heronv1.RenewNodeBillingRequest{NodeId: f.node}}}
	}},
	"delete_tag": {store.ActionDeleteTag, func(changeFixture) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{Change: &heronv1.ExecuteChangeRequest_DeleteTag{DeleteTag: &heronv1.DeleteTagRequest{Name: "fixture-tag"}}}
	}},
}

// TestExecuteChangeCoversEveryOneofField 按描述符枚举 change 的每个分支：prepareChange 必须把它映射到期望的操作，
// 操作必须在 store 的登记表里（动作名与分支名相同、有种类），预览执行的回执以种类名为快照主键、资源身份正确，
// 且不写回执。
func TestExecuteChangeCoversEveryOneofField(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	var f changeFixture
	f.node, _ = h.createNode(t, "target")
	f.other, _ = h.createNode(t, "queued")
	// 带周期与到期日：renew_node_billing 的预览要走到写边界，没有这两项的节点在 handler 里就被拒绝。
	h.update(t, &heronv1.UpdateNodeRequest{Id: f.node, Name: "target", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0), Tags: []string{"fixture-tag"},
		Billing: &heronv1.Billing{BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_MONTHLY, ExpiresOn: "2026-03-01"}})
	saved, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: validProbeTask(), NodeIds: []int64{f.node}}))
	if err != nil {
		t.Fatal(err)
	}
	f.probe = int64(saved.Msg.Task.Task.Id)
	f.rule = saveRule(t, h, &heronv1.AlertRule{Name: "rule", Kind: heronv1.AlertKind_ALERT_KIND_OFFLINE, NodeIds: []int64{f.node}}).Id
	if err := h.store.SaveNodeUpdate(t.Context(), f.node, &heronv1.UpdateStatus{Supported: true, Version: "v0.1.0"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SaveNodeUpdate(t.Context(), f.other, &heronv1.UpdateStatus{Supported: true, Version: "v0.1.0", Task: &heronv1.UpdateTask{Id: "queued-task", Version: "v0.2.0", State: "queued", ExpiresAt: h.clk.Now().Add(time.Hour).Unix()}}); err != nil {
		t.Fatal(err)
	}
	manager := updates.New(h.store, h.clk, slog.Default(), "v0.2.0")
	if err := manager.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.svc.cfg.Updates = manager
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	receipts := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM operation").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	fields := (&heronv1.ExecuteChangeRequest{}).ProtoReflect().Descriptor().Oneofs().ByName("change").Fields()
	if fields.Len() != len(changeExpectations) {
		t.Errorf("change oneof has %d fields, expectations list %d", fields.Len(), len(changeExpectations))
	}
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		name := string(field.Name())
		t.Run(name, func(t *testing.T) {
			want, ok := changeExpectations[name]
			if !ok {
				t.Fatalf("ExecuteChangeRequest.%s has no expectation: register it in store's changeActions, prepareChange and changeExpectations", name)
			}
			m := want.build(f)
			if got := m.ProtoReflect().WhichOneof(field.ContainingOneof()); got == nil || got.Name() != field.Name() {
				t.Fatalf("expectation for %s builds a different branch: %v", name, got)
			}
			c, _, _, err := h.svc.prepareChange(t.Context(), proto.Clone(m).(*heronv1.ExecuteChangeRequest))
			if err != nil {
				t.Fatalf("prepareChange: %v", err)
			}
			if c.Action != want.action {
				t.Fatalf("prepareChange mapped %s to %s, want %s", name, c.Action, want.action)
			}
			if c.Action.String() != name || c.Action.Kind() == 0 || c.Action.Permission() == "" {
				t.Fatalf("action %d is not registered for %s: name=%q kind=%d permission=%q", c.Action, name, c.Action.String(), c.Action.Kind(), c.Action.Permission())
			}
			before := receipts()
			m.Preview = true
			r, err := h.admin.ExecuteChange(t.Context(), connect.NewRequest(m))
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			op := r.Msg.Operation
			if r.Msg.ExpectedVersion == "" || op.CommittedAt != 0 || op.Action != name {
				t.Fatalf("not a preview receipt: %v", r.Msg)
			}
			wantResource := c.ResourceID
			if wantResource == 0 && (name == "create_node" || name == "save_probe_task" || name == "save_alert_rule") {
				if op.ResourceId == 0 {
					t.Fatalf("preview of %s did not allocate an identity", name)
				}
				wantResource = op.ResourceId
			}
			if op.ResourceId != wantResource {
				t.Fatalf("receipt resource %d, want %d", op.ResourceId, wantResource)
			}
			for side, raw := range map[string]string{"before": op.BeforeJson, "after": op.AfterJson} {
				var snapshot map[string]json.RawMessage
				if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
					t.Fatalf("%s snapshot: %v", side, err)
				}
				if _, ok := snapshot[c.Action.Kind().String()]; !ok {
					t.Fatalf("%s snapshot lacks the %s row: %s", side, c.Action.Kind(), raw)
				}
			}
			if after := receipts(); after != before {
				t.Fatalf("preview wrote %d receipts", after-before)
			}
		})
	}
}

// stagedChange 按 ExecuteChange 的步骤装好一次变更（映射、字段补全、版本），把执行留给用例：用例用替身 handler
// 驱动 runChange，覆盖真实 handler 走不到的接线错误。
func stagedChange(t *testing.T, h *harness, m *heronv1.ExecuteChangeRequest, preview bool) (*store.Change, changeCall) {
	t.Helper()
	c, call, patch, err := h.svc.prepareChange(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	if patch != nil {
		if err := patch(); err != nil {
			t.Fatal(err)
		}
	}
	c.RequestID, c.RequestHash, c.Preview = "staged", "staged", preview
	if c.ExpectedVersion, err = h.store.ChangeVersion(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	return c, call
}

type nodeTrace struct {
	name      string
	tokenHash []byte
	receipts  int
}

func traceNode(t *testing.T, h *harness, id int64) nodeTrace {
	t.Helper()
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out nodeTrace
	if err := db.QueryRow("SELECT name, token_hash FROM node WHERE id=?", id).Scan(&out.name, &out.tokenHash); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM operation").Scan(&out.receipts); err != nil {
		t.Fatal(err)
	}
	return out
}

// 轮换 token 的 handler 若在主写之前接错线、先做一次同一节点上改节点目标的写，整次变更必须失败且不留任何痕迹：
// 名字、凭据哈希、回执与内存里的凭据映射都不变。预览与正式各一遍。
func TestChangeSiblingFirstWriteLeavesNoTrace(t *testing.T) {
	t.Parallel()
	for _, preview := range []bool{false, true} {
		t.Run(fmt.Sprintf("preview=%t", preview), func(t *testing.T) {
			h := newHarness(t, "")
			h.login(t)
			node, token := h.createNode(t, "original")
			before := traceNode(t, h, node)
			c, rotate := stagedChange(t, h, &heronv1.ExecuteChangeRequest{Change: &heronv1.ExecuteChangeRequest_RotateNodeToken{RotateNodeToken: &heronv1.RotateNodeTokenRequest{Id: node}}}, preview)
			miswired := func(ctx context.Context) (proto.Message, error) {
				if _, err := h.store.UpdateNode(ctx, node, store.NodeEdit{Name: "renamed", TrafficResetDay: 1}); !errors.Is(err, store.ErrChangeTarget) {
					t.Errorf("sibling write: %v, want ErrChangeTarget", err)
				}
				return rotate(ctx)
			}
			if _, err := h.svc.runChange(t.Context(), c, miswired); connect.CodeOf(err) != connect.CodeInternal {
				t.Fatalf("miswired rotate: %v, want Internal", err)
			}
			if after := traceNode(t, h, node); after.name != before.name || !bytes.Equal(after.tokenHash, before.tokenHash) || after.receipts != 0 {
				t.Fatalf("refused change left a trace: before=%+v after=%+v", before, after)
			}
			if id, ok := h.auth.Authenticate(token); !ok || id != node {
				t.Fatal("refused rotate revoked the live credential")
			}
		})
	}
}

func TestRunChangeTerminalStates(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, _ := h.createNode(t, "original")
	var logs bytes.Buffer
	h.svc.log = slog.New(slog.NewTextHandler(&logs, nil))
	update := func(note string) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"note"}}, Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: node, Note: note}}}
	}
	note := func() string {
		t.Helper()
		n, err := h.store.GetNode(t.Context(), node)
		if err != nil {
			t.Fatal(err)
		}
		return n.Note
	}

	t.Run("zero consumption", func(t *testing.T) {
		logs.Reset()
		c, _ := stagedChange(t, h, update("never"), false)
		idle := func(ctx context.Context) (proto.Message, error) {
			// 一次普通写不消费变更；handler 随后成功返回却没有主写。
			return nil, h.store.ReorderNodes(ctx, []int64{node})
		}
		if _, err := h.svc.runChange(t.Context(), c, idle); connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("handler without primary write: %v, want Internal", err)
		}
		if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "without committing its primary write") || !strings.Contains(logs.String(), "action=update_node") {
			t.Fatalf("zero consumption not logged with the action: %s", logs.String())
		}
		if traceNode(t, h, node).receipts != 0 || note() != "" {
			t.Fatal("zero consumption changed state")
		}
	})

	t.Run("business rejection", func(t *testing.T) {
		c, _ := stagedChange(t, h, update("rejected"), false)
		rejected := func(context.Context) (proto.Message, error) { return nil, invalid("note: rejected") }
		if _, err := h.svc.runChange(t.Context(), c, rejected); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("business error was not passed through: %v", err)
		}
		if c.Claimed() || traceNode(t, h, node).receipts != 0 {
			t.Fatal("business rejection used the change or wrote a receipt")
		}
	})

	t.Run("preview", func(t *testing.T) {
		c, call := stagedChange(t, h, update("previewed"), true)
		r, err := h.svc.runChange(t.Context(), c, call)
		if err != nil || r.Msg.ExpectedVersion == "" || !strings.Contains(r.Msg.Operation.AfterJson, "previewed") || r.Msg.Operation.CommittedAt != 0 {
			t.Fatalf("preview: %v %v", r, err)
		}
		if traceNode(t, h, node).receipts != 0 || note() != "" {
			t.Fatal("preview changed state")
		}
	})

	t.Run("second primary write", func(t *testing.T) {
		c, call := stagedChange(t, h, update("first"), false)
		twice := func(ctx context.Context) (proto.Message, error) {
			if _, err := call(ctx); err != nil {
				return nil, err
			}
			_, err := h.store.UpdateNode(ctx, node, store.NodeEdit{Name: "original", Note: "second", TrafficResetDay: 1})
			return nil, err
		}
		r, err := h.svc.runChange(t.Context(), c, twice)
		if err != nil || r.Msg.Operation.CommittedAt == 0 {
			t.Fatalf("committed receipt lost: %v %v", r, err)
		}
		if got := note(); got != "first" || traceNode(t, h, node).receipts != 1 {
			t.Fatalf("second primary write applied: note=%q", got)
		}
	})

	t.Run("success", func(t *testing.T) {
		c, call := stagedChange(t, h, update("done"), false)
		c.RequestID = "success"
		r, err := h.svc.runChange(t.Context(), c, call)
		if err != nil || r.Msg.Operation.CommittedAt == 0 || r.Msg.Result == nil || note() != "done" {
			t.Fatalf("success: %v %v", r, err)
		}
	})
}

// 预览与重放不是失败：主写按变更的要求回滚（ErrPreview）或跳过（ErrReplay），handler 经 operationError 退出时不记 Error 日志。
// 记成 Error 会让每次预览都在日志里留下一条"updating node failed"。
func TestPreviewAndReplayDoNotLogErrors(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	h := newHarness(t, "", withLog(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	h.login(t)
	a, _ := h.createNode(t, "a")
	edit := func(requestID string, preview bool, expected string) string {
		t.Helper()
		m := &heronv1.ExecuteChangeRequest{RequestId: requestID, Preview: preview, ExpectedVersion: expected, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"note"}},
			Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: a, Note: "changed"}}}
		resp, err := h.admin.ExecuteChange(t.Context(), connect.NewRequest(m))
		if err != nil {
			t.Fatalf("ExecuteChange(preview=%v): %v", preview, err)
		}
		return resp.Msg.GetExpectedVersion()
	}
	version := edit("edit", true, "")
	edit("edit", false, version)
	edit("edit", false, version) // 同一 request_id 重放：返回原回执，不再写。
	if out := logs.String(); strings.Contains(out, "level=ERROR") {
		t.Fatalf("preview or replay of a change logged an error:\n%s", out)
	}
}

// syncBuffer 是 handler 协程写、用例读的日志缓冲：写发生在请求处理协程里，读在应答返回之后，锁让两者有明确的先后。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }
