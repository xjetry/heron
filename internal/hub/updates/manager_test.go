package updates

import (
	"io"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
	"google.golang.org/protobuf/proto"
)

// fixtureBound 装配一个绑定指定 agent 版本的 Manager；用例里 Start 的目标是什么版本，绑定就给什么，
// 让拒绝落在被测的原因上而不是先被绑定检查挡下。
func fixtureBound(t *testing.T, bound string) (*Manager, int64, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(time.Now())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(t.TempDir(), "db"), clk, log, store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, _, err := st.CreateNode(t.Context(), store.NewNode{Name: "test"}, []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	m := New(st, clk, log, bound)
	if err := m.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	observe(m, id, &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0"})
	m.flush(t.Context())
	return m, id, clk
}

func fixture(t *testing.T) (*Manager, int64, *clock.Fake) {
	t.Helper()
	return fixtureBound(t, "v0.3.0")
}

func TestUpdateDispatchPersistsBeforeDeliveryAndCannotCancel(t *testing.T) {
	m, id, _ := fixture(t)
	task, err := m.Start(t.Context(), id, "v0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := observe(m, id, m.Snapshot(id)); got != nil {
		t.Fatalf("undurable dispatch delivered: %v", got)
	}
	m.flush(t.Context())
	got := observe(m, id, m.Snapshot(id))
	if got == nil || got.State != "dispatched" || got.Id != task.Id {
		t.Fatalf("delivery=%v", got)
	}
	rows, err := m.st.NodeUpdates(t.Context())
	if err != nil || rows[id].Task.GetState() != "dispatched" {
		t.Fatalf("dispatch not durable: %v %v", rows, err)
	}
	if err := m.Cancel(t.Context(), id, task.Id); err == nil {
		t.Fatal("cancelled a dispatched update")
	}
	m2 := New(m.st, m.clk, m.log, "v0.3.0")
	if err := m2.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := observe(m2, id, m.Snapshot(id)); got == nil || got.Id != task.Id {
		t.Fatalf("restart did not redeliver same authorization: %v", got)
	}
}

func TestUpdateResultRequiresMatchingTaskAndRunningVersion(t *testing.T) {
	for _, mutate := range []struct {
		name, expected string
		f              func(*heronv1.UpdateStatus)
	}{
		{"different_id", "", func(s *heronv1.UpdateStatus) { s.Task.Id = "fedcba9876543210" }},
		{"different_target", "dispatched", func(s *heronv1.UpdateStatus) { s.Task.Version = "v0.4.0" }},
		{"old_process", "dispatched", func(s *heronv1.UpdateStatus) { s.Version = "v0.2.0" }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			m, id, _ := fixture(t)
			task, err := m.Start(t.Context(), id, "v0.3.0")
			if err != nil {
				t.Fatal(err)
			}
			observe(m, id, m.Snapshot(id))
			m.flush(t.Context())
			reported := &heronv1.UpdateStatus{Supported: true, Version: "v0.3.0", Task: proto.Clone(task).(*heronv1.UpdateTask)}
			reported.Task.State = "succeeded"
			mutate.f(reported)
			observe(m, id, reported)
			m.flush(t.Context())
			if got := m.Snapshot(id).Task.GetState(); got != mutate.expected {
				t.Fatalf("unrelated success changed task: %q", got)
			}
			if mutate.expected == "" {
				// 运行版本已达到目标而本机没有本任务的记录：任务被清除，既不冒称成功，之后也没有可确认的记录。
				return
			}
			reported.Version = "v0.3.0"
			reported.Task = proto.Clone(task).(*heronv1.UpdateTask)
			reported.Task.State = "succeeded"
			observe(m, id, reported)
			m.flush(t.Context())
			if got := m.Snapshot(id).Task.GetState(); got != "succeeded" {
				t.Fatalf("matching success rejected: %s", got)
			}
		})
	}
}

func TestUpdateQueueExpiryCancellationAndSupport(t *testing.T) {
	// 不比当前版本新的目标被 Newer 拒绝：绑定给成它启动的那个版本，拒绝才落在原来的原因上。
	for _, v := range []string{"v0.1.0", "v0.2.0"} {
		m, id, _ := fixtureBound(t, v)
		if _, err := m.Start(t.Context(), id, v); err == nil {
			t.Fatalf("accepted target %s", v)
		}
	}
	m, id, clk := fixture(t)
	// 绑定必须是正式版（Start 先以 ValidVersion 校验绑定），预发布目标不可能等于它：被目标
	// 检查拒绝，到不了 Newer。
	if _, err := m.Start(t.Context(), id, "v0.3.0-rc1"); err == nil {
		t.Fatal("accepted target v0.3.0-rc1")
	}
	task, err := m.Start(t.Context(), id, "v0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(t.Context(), id, "v0.3.0"); err == nil {
		t.Fatal("replaced active task")
	}
	if err := m.Cancel(t.Context(), id, task.Id); err != nil {
		t.Fatal(err)
	}
	observe(m, id, m.Snapshot(id))
	m.flush(t.Context())
	if m.Snapshot(id).Task.GetState() != "cancelled" {
		t.Fatal("cancelled task restarted")
	}
	if _, err := m.Start(t.Context(), id, "v0.3.0"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(24 * time.Hour)
	if got := observe(m, id, m.Snapshot(id)); got != nil {
		t.Fatal("expired task delivered")
	}
	m.flush(t.Context())
	if m.Snapshot(id).Task.GetState() != "expired" {
		t.Fatal("expired task remains queued")
	}
	observe(m, id, nil)
	m.flush(t.Context())
	if _, err := m.Start(t.Context(), id, "v0.3.0"); err == nil {
		t.Fatal("accepted unsupported agent")
	}
}

func TestStartRejectsWithoutStableBound(t *testing.T) {
	for _, bound := range []string{"", "dev", "v0.3.0-rc.1"} {
		m, id, _ := fixtureBound(t, bound)
		_, err := m.Start(t.Context(), id, "v0.3.0")
		if err == nil || !strings.Contains(err.Error(), "no stable bound agent version") {
			t.Fatalf("bound %q accepted a node update: %v", bound, err)
		}
		if task := m.Snapshot(id).Task; task != nil {
			t.Fatalf("bound %q wrote a task", bound)
		}
	}
}

func TestStartRejectsVersionOtherThanBound(t *testing.T) {
	m, id, _ := fixture(t)
	_, err := m.Start(t.Context(), id, "v0.4.0")
	if err == nil || !strings.Contains(err.Error(), "bound agent version v0.3.0") {
		t.Fatalf("accepted a target other than the bound: %v", err)
	}
	if task := m.Snapshot(id).Task; task != nil {
		t.Fatal("rejected target wrote a task")
	}
	if _, err := m.Start(t.Context(), id, "v0.3.0"); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateDeletedNodeNeverReappears(t *testing.T) {
	m, id, _ := fixture(t)
	status := m.Snapshot(id)
	observe(m, id, status)
	if err := m.st.DeleteNode(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	m.Forget(id)
	observe(m, id, status)
	m.flush(t.Context())
	if m.Snapshot(id).Supported {
		t.Fatal("late report recreated deleted memory state")
	}
	rows, err := m.st.NodeUpdates(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatalf("deleted state persisted: %v %v", rows, err)
	}
}

func TestUpdateObserveDoesNotWaitForWriter(t *testing.T) {
	m, id, _ := fixture(t)
	m.op.Lock()
	done := make(chan struct{})
	go func() { observe(m, id, m.Snapshot(id)); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		m.op.Unlock()
		t.Fatal("report waited for background writer")
	}
	m.op.Unlock()
}

func TestUpdateOldProgressCannotRegressAndDuplicatesDoNotChangeTime(t *testing.T) {
	m, id, clk := fixture(t)
	if _, err := m.Start(t.Context(), id, "v0.3.0"); err != nil {
		t.Fatal(err)
	}
	observe(m, id, m.Snapshot(id))
	m.flush(t.Context())
	report := m.Snapshot(id)
	report.Task.State = "verifying"
	observe(m, id, report)
	m.flush(t.Context())
	at := m.Snapshot(id).Task.UpdatedAt
	clk.Advance(time.Minute)
	observe(m, id, report)
	m.flush(t.Context())
	if m.Snapshot(id).Task.UpdatedAt != at {
		t.Fatal("duplicate progress changed transition timestamp")
	}
	report.Task.State = "downloading"
	observe(m, id, report)
	m.flush(t.Context())
	if got := m.Snapshot(id).Task.GetState(); got != "verifying" {
		t.Fatalf("old progress regressed task: %s", got)
	}
}

func TestLostResultExpiresWithoutClaimingFailureAndAcceptsLateResult(t *testing.T) {
	for _, state := range []string{"dispatched", "downloading", "stopping", "installing", "verifying"} {
		t.Run(state, func(t *testing.T) {
			m, id, clk := fixture(t)
			if _, err := m.Start(t.Context(), id, "v0.3.0"); err != nil {
				t.Fatal(err)
			}
			observe(m, id, m.Snapshot(id))
			m.flush(t.Context())
			report := m.Snapshot(id)
			report.Task.State = state
			observe(m, id, report)
			m.flush(t.Context())
			clk.Advance(24 * time.Hour)
			observe(m, id, &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0"})
			m.flush(t.Context())
			if got := m.Snapshot(id).Task.GetState(); got != "unconfirmed" {
				t.Fatalf("lost outcome should be unconfirmed, got %s", got)
			}
			report.Task.State = "succeeded"
			observe(m, id, report)
			m.flush(t.Context())
			if m.Snapshot(id).Task.GetState() != "unconfirmed" {
				t.Fatal("old process confirmed success")
			}
			report.Version = "v0.3.0"
			observe(m, id, report)
			m.flush(t.Context())
			if m.Snapshot(id).Task.GetState() != "succeeded" {
				t.Fatal("matching late success was lost")
			}
		})
	}
}

func TestManualUpgradeReconcilesUnexecutedAuthorization(t *testing.T) {
	for _, version := range []string{"v0.3.0", "v0.4.0"} {
		t.Run(version, func(t *testing.T) {
			m, id, _ := fixture(t)
			if _, err := m.Start(t.Context(), id, "v0.3.0"); err != nil {
				t.Fatal(err)
			}
			observe(m, id, m.Snapshot(id))
			m.flush(t.Context())
			observe(m, id, &heronv1.UpdateStatus{Supported: true, Version: version})
			m.flush(t.Context())
			if task := m.Snapshot(id).Task; task != nil {
				t.Fatalf("manual install must not claim task success or leave a stale task: %v", task)
			}
			// 未确认的任务不得挡住后续更新：hub 升级到绑定更高 agent 版本的 release 后（同一库上的
			// 新进程、绑定 v0.5.0），节点必须还能再更新（目标随 hub 的绑定走）。
			upgraded := New(m.st, m.clk, m.log, "v0.5.0")
			if err := upgraded.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := upgraded.Start(t.Context(), id, "v0.5.0"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestActiveTasksListsOnlyActiveStates(t *testing.T) {
	m := New(nil, clock.NewFake(time.Unix(1000, 0)), slog.Default(), "")
	m.states = map[int64]*heronv1.UpdateStatus{
		1: {Task: &heronv1.UpdateTask{Id: "aaaaaaaaaaaaaaaa", Version: "v1.0.0", State: "dispatched"}},
		2: {Task: &heronv1.UpdateTask{Id: "bbbbbbbbbbbbbbbb", Version: "v1.0.0", State: "succeeded"}},
		3: {Task: &heronv1.UpdateTask{Id: "cccccccccccccccc", Version: "v1.1.0", State: "downloading"}},
		4: {},
	}
	got := m.ActiveTasks()
	want := map[string]string{"aaaaaaaaaaaaaaaa": "v1.0.0", "cccccccccccccccc": "v1.1.0"}
	if !maps.Equal(got, want) {
		t.Fatalf("ActiveTasks = %v, want %v", got, want)
	}
}

// 终态任务失败后，节点经别的途径（重装、手动升级）运行到目标版本：失败记录不再描述节点的更新状态，
// 必须清除；运行版本未到目标时失败记录保留。本机更新器仍保留这条失败记录时同样清除。
func TestTerminalTaskClearedOnceRunningVersionMeetsTarget(t *testing.T) {
	for _, c := range []struct {
		name  string
		state string
		local bool
	}{
		{"failed, local record kept", "failed", true},
		{"failed, local record gone", "failed", false},
		{"rolled_back", "rolled_back", true},
		{"cancelled", "cancelled", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, id, _ := fixture(t)
			task, err := m.Start(t.Context(), id, "v0.3.0")
			if err != nil {
				t.Fatal(err)
			}
			observe(m, id, m.Snapshot(id))
			m.flush(t.Context())
			failed := proto.Clone(task).(*heronv1.UpdateTask)
			failed.State, failed.Error = c.state, "download official release: dial tcp: i/o timeout"
			observe(m, id, &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0", Task: failed})
			m.flush(t.Context())
			if got := m.Snapshot(id).Task.GetState(); got != c.state {
				// cancelled 只能由 hub 自己产生；本机上报的 cancelled 不前进，直接在 hub 侧取消。
				if c.state != "cancelled" {
					t.Fatalf("terminal result not recorded: %q", got)
				}
				m.mu.Lock()
				m.states[id].Task.State = "cancelled"
				m.mu.Unlock()
			}
			observe(m, id, &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0", Task: failed})
			m.flush(t.Context())
			if m.Snapshot(id).Task == nil {
				t.Fatal("terminal task cleared while running version is still below its target")
			}
			reinstalled := &heronv1.UpdateStatus{Supported: true, Version: "v0.3.0"}
			if c.local {
				reinstalled.Task = failed
			}
			observe(m, id, reinstalled)
			m.flush(t.Context())
			if got := m.Snapshot(id).Task; got != nil {
				t.Fatalf("stale %s task kept after the node reached its target: %v", c.state, got)
			}
			reloaded := New(m.st, m.clk, m.log, "v0.3.0")
			if err := reloaded.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := reloaded.Snapshot(id).Task; got != nil {
				t.Fatalf("cleared task came back after reload: %v", got)
			}
		})
	}
}

// 新进程已运行目标版本、本机更新器仍在验证这条任务：任务在执行中，不能因运行版本已达到目标而被清除。
func TestTaskStillExecutedLocallySurvivesTargetVersion(t *testing.T) {
	m, id, _ := fixture(t)
	task, err := m.Start(t.Context(), id, "v0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	observe(m, id, m.Snapshot(id))
	m.flush(t.Context())
	verifying := proto.Clone(task).(*heronv1.UpdateTask)
	verifying.State = "verifying"
	observe(m, id, &heronv1.UpdateStatus{Supported: true, Version: "v0.3.0", Task: verifying})
	m.flush(t.Context())
	if got := m.Snapshot(id).Task.GetState(); got != "verifying" {
		t.Fatalf("task executed by the local updater was cleared or changed: %q", got)
	}
}

// states 里 Task 之外的字段跟随 agent 原报，双向都跟：来源由空变为 hub、再变为 github、再被清空，每一步落库值与
// Snapshot 都等于这次原报；反过来，原报里与 hub 任务无关的任务记录不进 states——Task 归 hub。
func TestReportedFieldsOtherThanTaskFollowAgent(t *testing.T) {
	m, id, _ := fixture(t)
	for _, source := range []string{"hub", "github", ""} {
		observe(m, id, &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0", Source: source})
		m.flush(t.Context())
		rows, err := m.st.NodeUpdates(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if got, snap := rows[id].GetSource(), m.Snapshot(id).GetSource(); got != source || snap != source {
			t.Fatalf("source reported %q: persisted %q, snapshot %q", source, got, snap)
		}
	}
	// 目标高于运行版本，Superseded 不会清它：留下来的只能是被当成 hub 的任务。
	foreign := &heronv1.UpdateTask{Id: "0123456789abcdef", Version: "v0.4.0", State: "failed", Error: "local leftover"}
	observe(m, id, &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0", Source: "hub", Task: foreign})
	m.flush(t.Context())
	if s := m.Snapshot(id); s.Task != nil || s.Source != "hub" {
		t.Fatalf("the agent's own task record leaked into the hub-owned task: %v", s)
	}
}

// observe 以全文加摘要喂给 Manager；全文缺席时两者都缺席。
func observe(m *Manager, id int64, s *heronv1.UpdateStatus) *heronv1.UpdateTask {
	var hash *uint64
	if s != nil {
		hash = proto.Uint64(7)
	}
	task, _ := m.Observe(id, s, hash)
	return task
}
