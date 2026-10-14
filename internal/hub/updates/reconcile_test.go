package updates

import (
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
	"google.golang.org/protobuf/proto"
)

// 摘要的取值对 hub 没有含义，只比较相等；用例直接给常量。
func digest(v uint64) *uint64 { return &v }

type reconcileFixture struct {
	m    *Manager
	id   int64
	path string
}

// newReconcile 装配一个已收到过一次全文（摘要 7）并落库的 Manager。
func newReconcile(t *testing.T, s *heronv1.UpdateStatus) reconcileFixture {
	t.Helper()
	clk := clock.NewFake(time.Now())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "db")
	st, err := store.Open(path, clk, log, store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, _, err := st.CreateNode(t.Context(), store.NewNode{Name: "test"}, []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	m := New(st, clk, log, "v0.3.0")
	if err := m.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if task, want := m.Observe(id, s, digest(7)); task != nil || want {
		t.Fatalf("first full report: task=%v want=%v", task, want)
	}
	m.flush(t.Context())
	return reconcileFixture{m: m, id: id, path: path}
}

func supported() *heronv1.UpdateStatus {
	return &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0", Source: "hub"}
}

func (f reconcileFixture) persisted(t *testing.T) *heronv1.UpdateStatus {
	t.Helper()
	rows, err := f.m.st.NodeUpdates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return rows[f.id]
}

// 稳态只带摘要：摘要等于已存原报即确认触达，排队的任务照常经后台落盘为 dispatched，此后只带摘要的应答照常下发。
func TestDigestOnlyReportConfirmsTouchAndDispatches(t *testing.T) {
	f := newReconcile(t, supported())
	if task, want := f.m.Observe(f.id, nil, digest(7)); task != nil || want {
		t.Fatalf("steady digest-only report: task=%v want=%v", task, want)
	}
	queued, err := f.m.Start(t.Context(), f.id, "v0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if task, want := f.m.Observe(f.id, nil, digest(7)); task != nil || want {
		t.Fatalf("queued task delivered before it was persisted as dispatched: task=%v want=%v", task, want)
	}
	f.m.flush(t.Context())
	if got := f.persisted(t).GetTask(); got.GetState() != "dispatched" || got.GetId() != queued.Id {
		t.Fatalf("digest-only touch did not dispatch the queued task: %v", got)
	}
	task, want := f.m.Observe(f.id, nil, digest(7))
	if want || task == nil || task.Id != queued.Id || task.State != "dispatched" {
		t.Fatalf("digest-only report after dispatch: task=%v want=%v", task, want)
	}
}

// hub 重启丢掉内存里的原报：持久化的 states 仍在、Start 照旧读它；第一次只带摘要的上报得到 want_update，不算触达，
// 排队的任务不下发；下一次带全文之后恢复。已下发的任务与 want 无关，照常下发。
func TestRestartWantsFullStatusAndKeepsPersistedState(t *testing.T) {
	f := newReconcile(t, supported())
	restarted := New(f.m.st, f.m.clk, f.m.log, "v0.3.0")
	if err := restarted.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s := restarted.Snapshot(f.id); !s.Supported || s.Source != "hub" {
		t.Fatalf("persisted state lost across restart: %v", s)
	}
	queued, err := restarted.Start(t.Context(), f.id, "v0.3.0")
	if err != nil {
		t.Fatalf("Start must read the persisted last known state: %v", err)
	}
	if task, want := restarted.Observe(f.id, nil, digest(7)); task != nil || !want {
		t.Fatalf("first digest-only report after restart: task=%v want=%v, want no task and want_update", task, want)
	}
	restarted.flush(t.Context())
	if s := f.persisted(t); s.GetTask().GetState() != "queued" || !s.Supported {
		t.Fatalf("an unconfirmed report touched the node: %v", s)
	}
	if task, want := restarted.Observe(f.id, supported(), digest(7)); task != nil || want {
		t.Fatalf("full report after restart: task=%v want=%v", task, want)
	}
	restarted.flush(t.Context())
	task, want := restarted.Observe(f.id, nil, digest(7))
	if want || task.GetId() != queued.Id {
		t.Fatalf("digest-only report after the full one: task=%v want=%v", task, want)
	}
	again := New(f.m.st, f.m.clk, f.m.log, "v0.3.0")
	if err := again.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if task, want := again.Observe(f.id, nil, digest(7)); !want || task.GetId() != queued.Id {
		t.Fatalf("persisted dispatch must be delivered regardless of want_update: task=%v want=%v", task, want)
	}
}

// 摘要不同（含 0：它是合法摘要，不是缺席）：want_update，本次不算触达，已持久化的状态不动。
func TestDigestMismatchWantsFullWithoutTouching(t *testing.T) {
	f := newReconcile(t, supported())
	if _, err := f.m.Start(t.Context(), f.id, "v0.3.0"); err != nil {
		t.Fatal(err)
	}
	for _, h := range []uint64{8, 0} {
		if task, want := f.m.Observe(f.id, nil, digest(h)); task != nil || !want {
			t.Fatalf("digest %d against stored 7: task=%v want=%v", h, task, want)
		}
	}
	f.m.flush(t.Context())
	if s := f.persisted(t); s.GetTask().GetState() != "queued" || !s.Supported || s.Version != "v0.2.0" {
		t.Fatalf("mismatched digest changed persisted state: %v", s)
	}
	if _, want := f.m.Observe(f.id, supported(), digest(0)); want {
		t.Fatal("full report wanted again")
	}
	if _, want := f.m.Observe(f.id, nil, digest(0)); want {
		t.Fatal("digest 0 is a valid digest; equal to the stored one it must confirm the touch")
	}
	f.m.flush(t.Context())
	if got := f.persisted(t).GetTask().GetState(); got != "dispatched" {
		t.Fatalf("touch confirmed by digest 0 did not dispatch: %q", got)
	}
}

// 状态正常变为不支持：新摘要先被索要，全文到达后 states 跟着变，Start 拒绝。
func TestStatusBecomesUnsupportedThroughDigest(t *testing.T) {
	f := newReconcile(t, supported())
	if _, want := f.m.Observe(f.id, nil, digest(9)); !want {
		t.Fatal("changed digest not wanted")
	}
	f.m.flush(t.Context())
	if !f.m.Snapshot(f.id).Supported {
		t.Fatal("support withdrawn before the full status arrived")
	}
	unsupported := &heronv1.UpdateStatus{Reason: "local updater unavailable", Version: "v0.2.0"}
	if _, want := f.m.Observe(f.id, unsupported, digest(9)); want {
		t.Fatal("full report wanted again")
	}
	f.m.flush(t.Context())
	if s := f.persisted(t); s.Supported || s.Reason != unsupported.Reason {
		t.Fatalf("persisted state %v, want unsupported with the reported reason", s)
	}
	if _, err := f.m.Start(t.Context(), f.id, "v0.3.0"); err == nil {
		t.Fatal("Start accepted a node that reported no support")
	}
}

// 版本、任务进度与来源的改变都经"新摘要 → 索要 → 全文"进入 states；来源双向都跟随原报（含清空）。
func TestReportedChangesReachStatesThroughDigest(t *testing.T) {
	f := newReconcile(t, supported())
	queued, err := f.m.Start(t.Context(), f.id, "v0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	f.m.Observe(f.id, nil, digest(7))
	f.m.flush(t.Context())
	downloading := proto.Clone(queued).(*heronv1.UpdateTask)
	downloading.State = "downloading"
	for i, tc := range []struct {
		name   string
		status *heronv1.UpdateStatus
		check  func(*heronv1.UpdateStatus) bool
	}{
		{"source_to_github", &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0", Source: "github"}, func(s *heronv1.UpdateStatus) bool { return s.Source == "github" }},
		{"source_cleared", &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0"}, func(s *heronv1.UpdateStatus) bool { return s.Source == "" }},
		{"task_progress", &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0", Source: "hub", Task: downloading}, func(s *heronv1.UpdateStatus) bool {
			return s.Source == "hub" && s.GetTask().GetState() == "downloading"
		}},
		{"version", &heronv1.UpdateStatus{Supported: true, Version: "v0.3.0", Source: "hub"}, func(s *heronv1.UpdateStatus) bool { return s.Version == "v0.3.0" }},
	} {
		h := uint64(100 + i)
		if _, want := f.m.Observe(f.id, nil, digest(h)); !want {
			t.Fatalf("%s: new digest not wanted", tc.name)
		}
		f.m.flush(t.Context())
		if tc.check(f.persisted(t)) {
			t.Fatalf("%s: state changed before the full status arrived: %v", tc.name, f.persisted(t))
		}
		f.m.Observe(f.id, tc.status, digest(h))
		f.m.flush(t.Context())
		if s := f.persisted(t); !tc.check(s) || !tc.check(f.m.Snapshot(f.id)) {
			t.Fatalf("%s: persisted %v snapshot %v", tc.name, s, f.m.Snapshot(f.id))
		}
	}
}

// Forget 丢掉原报：同一摘要此后不再确认触达。
func TestForgetDropsStoredOriginal(t *testing.T) {
	f := newReconcile(t, supported())
	f.m.Forget(f.id)
	if _, want := f.m.Observe(f.id, nil, digest(7)); !want {
		t.Fatal("digest matched an original stored before Forget")
	}
}

// 节点删除后迟到的带全文上报（在 Forget 之后到达）不留下原报：后台写库发现节点已不存在时一并丢掉。
func TestLateReportAfterDeleteLeavesNoOriginal(t *testing.T) {
	f := newReconcile(t, supported())
	if err := f.m.st.DeleteNode(t.Context(), f.id); err != nil {
		t.Fatal(err)
	}
	f.m.Forget(f.id)
	f.m.Observe(f.id, supported(), digest(7))
	f.m.flush(t.Context())
	if _, want := f.m.Observe(f.id, nil, digest(7)); !want {
		t.Fatal("a late report kept the deleted node's original")
	}
}

// 缺摘要的全文与完全缺席走同一路径，不留下可供以后匹配的原报。
func TestObserveWithoutHashIsAbsent(t *testing.T) {
	for _, text := range []*heronv1.UpdateStatus{supported(), nil} {
		f := newReconcile(t, supported())
		if task, want := f.m.Observe(f.id, text, nil); task != nil || want {
			t.Fatalf("absent digest: task=%v want=%v", task, want)
		}
		f.m.flush(t.Context())
		if s := f.persisted(t); s.Supported || s.Reason != "agent has not reported online update support" {
			t.Fatalf("missing hash contributed status: %v", s)
		}
		if _, want := f.m.Observe(f.id, nil, digest(7)); !want {
			t.Fatal("original survived absent hash")
		}
	}
}

func TestObserveFullTextWithHash(t *testing.T) {
	f := newReconcile(t, supported())
	if s := f.persisted(t); !proto.Equal(s, supported()) {
		t.Fatalf("full status not stored: %v", s)
	}
	if _, want := f.m.Observe(f.id, nil, digest(7)); want {
		t.Fatal("matching hash requested full text")
	}
}

// 写库失败：确认过的触达留待重试，失败期间排队的任务不下发；库恢复后后台补上，摘要照常确认。
func TestDigestTouchSurvivesWriteFailure(t *testing.T) {
	f := newReconcile(t, supported())
	queued, err := f.m.Start(t.Context(), f.id, "v0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	u := url.URL{Path: f.path}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)", u.EscapedPath(), testwait.Bound.Milliseconds()))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("CREATE TRIGGER fail_update BEFORE UPDATE ON node_update BEGIN SELECT RAISE(ABORT, 'write blocked'); END"); err != nil {
		t.Fatal(err)
	}
	if _, want := f.m.Observe(f.id, nil, digest(7)); want {
		t.Fatal("matching digest wanted")
	}
	f.m.flush(t.Context())
	if s := f.m.Snapshot(f.id); s.GetTask().GetState() != "queued" {
		t.Fatalf("in-memory state advanced although the write failed: %v", s.GetTask())
	}
	if got := f.persisted(t).GetTask().GetState(); got != "queued" {
		t.Fatalf("persisted state %q despite the blocked write", got)
	}
	if _, err := db.Exec("DROP TRIGGER fail_update"); err != nil {
		t.Fatal(err)
	}
	// 之后不再有上报也要补上：写失败时这次触达回到 observed，下一轮后台合并据它推进。
	f.m.flush(t.Context())
	if got := f.persisted(t).GetTask(); got.GetState() != "dispatched" || got.GetId() != queued.Id {
		t.Fatalf("retry after the write recovered: %v", got)
	}
	if task, want := f.m.Observe(f.id, nil, digest(7)); want || task.GetId() != queued.Id {
		t.Fatalf("digest-only report after the recovered dispatch: task=%v want=%v", task, want)
	}
}
