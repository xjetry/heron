package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/updates"
	"github.com/xjetry/heron/internal/update"
	"google.golang.org/protobuf/proto"
)

type updateAPIFake struct {
	submitted update.Request
	checks    int
	// latestErr 让 Latest 失败；bound / boundErr 是 BoundAgentVersion 的应答，boundTags 记下它被问过哪些 tag。
	latestErr error
	bound     string
	boundErr  error
	boundTags []string
}

func (f *updateAPIFake) Status(context.Context) update.Status {
	return update.Status{Supported: true, Version: "v0.2.0"}
}
func (f *updateAPIFake) Submit(_ context.Context, r update.Request) (update.Job, error) {
	f.submitted = r
	return update.Job{Request: r, State: "queued"}, nil
}
func (f *updateAPIFake) Latest(context.Context) (string, error) {
	f.checks++
	if f.latestErr != nil {
		return "", f.latestErr
	}
	return "v0.3.0", nil
}
func (f *updateAPIFake) BoundAgentVersion(_ context.Context, tag string) (string, error) {
	f.boundTags = append(f.boundTags, tag)
	return f.bound, f.boundErr
}

func TestUpdateAPIUsesExplicitReleaseChecksAndRunningHubVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "", withConfig(func(c *Config) { c.HubVersion = "v0.2.0" }))
	h.login(t)
	f := &updateAPIFake{}
	h.svc.updateLocal = f
	h.svc.updateSource = f
	got, err := h.admin.GetUpdates(t.Context(), connect.NewRequest(&heronv1.GetUpdatesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if f.checks != 0 || len(got.Msg.Targets) != 1 || got.Msg.Targets[0].Status.Version != "v0.2.0" || !got.Msg.Targets[0].Status.Supported {
		t.Fatalf("local status=%v checks=%d", got.Msg, f.checks)
	}
	got, err = h.admin.GetUpdates(t.Context(), connect.NewRequest(&heronv1.GetUpdatesRequest{CheckLatest: true}))
	if err != nil || got.Msg.LatestVersion != "v0.3.0" || f.checks != 1 {
		t.Fatalf("explicit check=%v err=%v checks=%d", got, err, f.checks)
	}
	if _, err := h.admin.StartUpdate(t.Context(), connect.NewRequest(&heronv1.StartUpdateRequest{Version: "v0.2.0"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("same-version install=%v", err)
	}
	if f.submitted.ID != "" {
		t.Fatal("same-version request reached updater")
	}
	started, err := h.admin.StartUpdate(t.Context(), connect.NewRequest(&heronv1.StartUpdateRequest{Version: "v0.3.0"}))
	if err != nil {
		t.Fatal(err)
	}
	if started.Msg.Task.Id != f.submitted.ID || f.submitted.Version != "v0.3.0" {
		t.Fatalf("submission=%+v response=%v", f.submitted, started.Msg)
	}
	if err := f.submitted.Validate(h.clk.Now()); err != nil {
		t.Fatalf("generated authorization=%v", err)
	}
}

func TestUpdateAPIReadTokenCannotAuthorizeUpdates(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	_, tok := createToken(t, h, "read")
	if got := rawCall(t, h, "GetUpdates", "{}", bearer(tok)); got.status != 200 {
		t.Fatalf("read status: %+v", got)
	}
	for _, method := range []string{"StartUpdate", "CancelUpdate"} {
		if got := rawCall(t, h, method, `{"node_id":"1","version":"v0.3.0"}`, bearer(tok)); got.status != 403 {
			t.Fatalf("read token %s: %+v", method, got)
		}
	}
}

func TestUpdateAPIRejectsInvalidTargetsAndBounds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	for _, v := range []string{"https://example.test/program", "v0.3.0-rc1", strings.Repeat("x", 35)} {
		_, err := h.admin.StartUpdate(t.Context(), connect.NewRequest(&heronv1.StartUpdateRequest{Version: v}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("version %q error=%v", v, err)
		}
	}
	_, err := h.admin.CancelUpdate(t.Context(), connect.NewRequest(&heronv1.CancelUpdateRequest{NodeId: 1, Id: strings.Repeat("x", 65)}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("oversized cancel ID=%v", err)
	}
}

// boundManager 装配一个绑定指定 agent 版本的 Manager 并挂进 harness 的服务。
func boundManager(t *testing.T, h *harness, bound string) *updates.Manager {
	t.Helper()
	m := updates.New(h.store, h.clk, slog.Default(), bound)
	if err := m.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.svc.cfg.Updates = m
	return m
}

func TestSnapshotAndUpdatesCarryBoundAgentVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	boundManager(t, h, "v1.2.3")
	f := &updateAPIFake{}
	h.svc.updateSource = f
	snap, err := h.admin.GetSnapshot(t.Context(), connect.NewRequest(&heronv1.GetSnapshotRequest{}))
	if err != nil || snap.Msg.BoundAgentVersion != "v1.2.3" {
		t.Fatalf("snapshot bound=%q err=%v", snap.Msg.BoundAgentVersion, err)
	}
	got, err := h.admin.GetUpdates(t.Context(), connect.NewRequest(&heronv1.GetUpdatesRequest{}))
	if err != nil || got.Msg.BoundAgentVersion != "v1.2.3" {
		t.Fatalf("updates bound=%q err=%v", got.Msg.BoundAgentVersion, err)
	}
	got, err = h.admin.GetUpdates(t.Context(), connect.NewRequest(&heronv1.GetUpdatesRequest{CheckLatest: true}))
	if err != nil || got.Msg.BoundAgentVersion != "v1.2.3" || f.checks != 1 {
		t.Fatalf("check_latest bound=%q err=%v checks=%d", got.Msg.BoundAgentVersion, err, f.checks)
	}
}

func TestSnapshotWithoutUpdatesHasNoBoundAgentVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	snap, err := h.admin.GetSnapshot(t.Context(), connect.NewRequest(&heronv1.GetSnapshotRequest{}))
	if err != nil || snap.Msg.BoundAgentVersion != "" {
		t.Fatalf("snapshot bound=%q err=%v", snap.Msg.BoundAgentVersion, err)
	}
	got, err := h.admin.GetUpdates(t.Context(), connect.NewRequest(&heronv1.GetUpdatesRequest{}))
	if err != nil || got.Msg.BoundAgentVersion != "" {
		t.Fatalf("updates bound=%q err=%v", got.Msg.BoundAgentVersion, err)
	}
}

func TestStartUpdateRejectsNonBoundVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, _ := h.createNode(t, "bound")
	if err := h.store.SaveNodeUpdate(t.Context(), node, &heronv1.UpdateStatus{Supported: true, Version: "v1.2.0"}); err != nil {
		t.Fatal(err)
	}
	m := boundManager(t, h, "v1.2.3")
	_, err := h.admin.StartUpdate(t.Context(), connect.NewRequest(&heronv1.StartUpdateRequest{NodeId: node, Version: "v1.2.4"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "bound agent version v1.2.3") {
		t.Fatalf("non-bound target error=%v", err)
	}
	if task := m.Snapshot(node).Task; task != nil {
		t.Fatalf("rejected target queued a task: %v", task)
	}
}

func TestExecuteChangeStartUpdateRejectsNonBoundVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, _ := h.createNode(t, "bound")
	if err := h.store.SaveNodeUpdate(t.Context(), node, &heronv1.UpdateStatus{Supported: true, Version: "v1.2.0"}); err != nil {
		t.Fatal(err)
	}
	boundManager(t, h, "v1.2.3")
	client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{node}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_UPDATE}})
	bad := &heronv1.ExecuteChangeRequest{RequestId: "update-non-bound", Change: &heronv1.ExecuteChangeRequest_StartUpdate{StartUpdate: &heronv1.StartUpdateRequest{NodeId: node, Version: "v1.2.4"}}}
	// 预览先被业务校验拒绝。
	p := proto.Clone(bad).(*heronv1.ExecuteChangeRequest)
	p.Preview = true
	_, err := client.ExecuteChange(t.Context(), connect.NewRequest(p))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "bound agent version v1.2.3") {
		t.Fatalf("preview error=%v", err)
	}
	// 执行也过不去：先拿合法目标（绑定版本）的预览换 expected_version，再提交非法目标。
	good := &heronv1.ExecuteChangeRequest{RequestId: "update-bound", Change: &heronv1.ExecuteChangeRequest_StartUpdate{StartUpdate: &heronv1.StartUpdateRequest{NodeId: node, Version: "v1.2.3"}}}
	previewChange(t, client, good)
	bad.ExpectedVersion = good.ExpectedVersion
	_, err = client.ExecuteChange(t.Context(), connect.NewRequest(bad))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "bound agent version v1.2.3") {
		t.Fatalf("commit error=%v", err)
	}
	ops, err := client.ListOperations(t.Context(), connect.NewRequest(&heronv1.ListOperationsRequest{}))
	if err != nil || len(ops.Msg.Operations) != 0 {
		t.Fatalf("rejected change left receipts: %v err=%v", ops.Msg, err)
	}
	if task := h.svc.cfg.Updates.Snapshot(node).Task; task != nil {
		t.Fatalf("rejected change queued a task: %v", task)
	}
}

// 已出租节点没有 agent，不是更新目标：StartUpdate 拒绝并点名节点，GetUpdates 不列它；普通节点照常列出。
func TestStartUpdateRejectsRentedNode(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	rented, _, err := h.store.CreateNode(t.Context(), store.NewNode{Name: "leased", Rented: true}, []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := h.createNode(t, "plain")
	// 已出租的机器不会有上报；直接落一份支持状态，让请求穿过 Manager 的版本与能力检查、到达 store 的守卫。
	if err := h.store.SaveNodeUpdate(t.Context(), rented, &heronv1.UpdateStatus{Supported: true, Version: "v1.2.0"}); err != nil {
		t.Fatal(err)
	}
	m := boundManager(t, h, "v1.2.3")
	_, err = h.admin.StartUpdate(t.Context(), connect.NewRequest(&heronv1.StartUpdateRequest{NodeId: rented, Version: "v1.2.3"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "leased") || !strings.Contains(err.Error(), "rented") {
		t.Fatalf("rented start error=%v, want FailedPrecondition naming the node and rented", err)
	}
	if task := m.Snapshot(rented).Task; task != nil {
		t.Fatalf("rejected start queued a task: %v", task)
	}
	got, err := h.admin.GetUpdates(t.Context(), connect.NewRequest(&heronv1.GetUpdatesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	listed := map[int64]bool{}
	for _, tg := range got.Msg.GetTargets() {
		listed[tg.GetNodeId()] = true
	}
	if listed[rented] {
		t.Fatalf("GetUpdates lists rented node %d", rented)
	}
	if !listed[plain] {
		t.Fatalf("GetUpdates omits plain node %d", plain)
	}
}

// ExecuteChange 的 start_update 走同一个守卫：预览与提交都被拒，且不留回执。
func TestExecuteChangeStartUpdateRejectsRentedNode(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, _, err := h.store.CreateNode(t.Context(), store.NewNode{Name: "leased"}, []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SaveNodeUpdate(t.Context(), node, &heronv1.UpdateStatus{Supported: true, Version: "v1.2.0"}); err != nil {
		t.Fatal(err)
	}
	boundManager(t, h, "v1.2.3")
	client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{node}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_UPDATE}})
	req := &heronv1.ExecuteChangeRequest{RequestId: "update-rented", Change: &heronv1.ExecuteChangeRequest_StartUpdate{StartUpdate: &heronv1.StartUpdateRequest{NodeId: node, Version: "v1.2.3"}}}
	// 标为已出租之前预览通过，换来 expected_version：start_update 的版本只覆盖 node_update 行，改开关不动它。
	previewChange(t, client, req)
	if _, err := h.store.UpdateNodeTasks(t.Context(), node, store.NodeEdit{Name: "leased", Rented: true}); err != nil {
		t.Fatal(err)
	}
	p := proto.Clone(req).(*heronv1.ExecuteChangeRequest)
	p.Preview = true
	p.ExpectedVersion = ""
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(p)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("preview error=%v, want FailedPrecondition", err)
	}
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(req)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("commit error=%v, want FailedPrecondition", err)
	}
	ops, err := client.ListOperations(t.Context(), connect.NewRequest(&heronv1.ListOperationsRequest{}))
	if err != nil || len(ops.Msg.Operations) != 0 {
		t.Fatalf("rejected change left receipts: %v err=%v", ops.Msg, err)
	}
	if task := h.svc.cfg.Updates.Snapshot(node).Task; task != nil {
		t.Fatalf("rejected change queued a task: %v", task)
	}
}

type localJobFake struct {
	updateAPIFake
	job *update.Job
}

func (f *localJobFake) Status(context.Context) update.Status {
	return update.Status{Supported: true, Version: "v0.3.0", Job: f.job}
}

// hub 自身的更新状态与节点同一判定：hub 经安装脚本等途径已运行到目标版本后，本机更新器留下的失败记录不再显示；
// 本机更新器仍在执行的任务（新进程已运行目标版本、处于验证阶段）与成功记录照常显示。
func TestUpdateAPIHidesSupersededLocalHubTask(t *testing.T) {
	t.Parallel()
	job := func(state, version string) *update.Job {
		return &update.Job{Request: update.Request{ID: "0123456789abcdef0123456789abcdef", Version: version}, State: state, Error: "download official release: i/o timeout"}
	}
	for _, c := range []struct {
		name string
		job  *update.Job
		kept bool
	}{
		{"failed, running hub already at target", job("failed", "v0.3.0"), false},
		{"failed, target still ahead", job("failed", "v0.4.0"), true},
		{"verifying by the local updater", job("verifying", "v0.3.0"), true},
		{"succeeded", job("succeeded", "v0.3.0"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, "", withConfig(func(cfg *Config) { cfg.HubVersion = "v0.3.0" }))
			h.login(t)
			h.svc.updateLocal = &localJobFake{job: c.job}
			got, err := h.admin.GetUpdates(t.Context(), connect.NewRequest(&heronv1.GetUpdatesRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			if task := got.Msg.Targets[0].Status.Task; (task != nil) != c.kept {
				t.Fatalf("hub task kept=%v, want %v: %v", task != nil, c.kept, task)
			}
		})
	}
}

// latest_bound_agent_version 是 check_latest 时取到的最新版的绑定值，与当前绑定无论相同还是不同都原样下发，比较由
// 面板做；取不到（资产缺失、不合契约、读取失败）一律是空串，三类在日志里分开，且都不写 check_error——最新版本
// 本身已经取到，hub 更新照常可以发起。
func TestUpdatesCarryLatestBoundAgentVersion(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		bound    string
		boundErr error
		want     string
		log      string
	}{
		{"same as current binding", "v1.2.3", nil, "v1.2.3", ""},
		{"different binding", "v1.3.0", nil, "v1.3.0", ""},
		{"unknown: asset missing", "", update.ErrBoundAgentMissing, "", "level=INFO msg=\"latest release publishes no AGENT_VERSION asset"},
		{"unknown: asset malformed", "", fmt.Errorf("%w: got \"x\"", update.ErrBoundAgentInvalid), "", "level=WARN msg=\"latest release AGENT_VERSION asset is malformed"},
		{"unknown: network", "", errors.New("connection reset by peer"), "", "level=WARN msg=\"could not read the latest release AGENT_VERSION asset"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logs bytes.Buffer
			h := newHarness(t, "", withLog(slog.New(slog.NewTextHandler(&logs, nil))))
			h.login(t)
			boundManager(t, h, "v1.2.3")
			f := &updateAPIFake{bound: c.bound, boundErr: c.boundErr}
			h.svc.updateSource = f
			got, err := h.admin.GetUpdates(t.Context(), connect.NewRequest(&heronv1.GetUpdatesRequest{CheckLatest: true}))
			if err != nil {
				t.Fatal(err)
			}
			m := got.Msg
			if m.LatestVersion != "v0.3.0" || m.CheckError != "" || m.BoundAgentVersion != "v1.2.3" || m.LatestBoundAgentVersion != c.want {
				t.Fatalf("latest=%q check_error=%q bound=%q latest_bound=%q, want latest_bound %q", m.LatestVersion, m.CheckError, m.BoundAgentVersion, m.LatestBoundAgentVersion, c.want)
			}
			if len(f.boundTags) != 1 || f.boundTags[0] != "v0.3.0" {
				t.Fatalf("bound agent version read for %q, want exactly the latest tag", f.boundTags)
			}
			if c.log == "" {
				if logs.Len() != 0 {
					t.Fatalf("known binding logged %q", logs.String())
				}
			} else if !strings.Contains(logs.String(), c.log) || strings.Count(logs.String(), "\n") != 1 {
				t.Fatalf("logs = %q, want one line containing %q", logs.String(), c.log)
			}
		})
	}
}

// 绑定值只跟着一次成功的最新版查询走：不带 check_latest 时不发起读取，最新版查询失败时没有 tag 可读。
func TestUpdatesReadLatestBindingOnlyAfterLatestVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	f := &updateAPIFake{bound: "v0.3.0"}
	h.svc.updateSource = f
	got, err := h.admin.GetUpdates(t.Context(), connect.NewRequest(&heronv1.GetUpdatesRequest{}))
	if err != nil || got.Msg.LatestBoundAgentVersion != "" || len(f.boundTags) != 0 {
		t.Fatalf("without check_latest: latest_bound=%q reads=%q err=%v", got.Msg.GetLatestBoundAgentVersion(), f.boundTags, err)
	}
	f.latestErr = errors.New("rate limited")
	got, err = h.admin.GetUpdates(t.Context(), connect.NewRequest(&heronv1.GetUpdatesRequest{CheckLatest: true}))
	if err != nil || got.Msg.CheckError != "rate limited" || got.Msg.LatestBoundAgentVersion != "" || len(f.boundTags) != 0 {
		t.Fatalf("failed latest check: %v reads=%q err=%v", got.Msg, f.boundTags, err)
	}
}
