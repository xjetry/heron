package nodeops

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
	"github.com/xjetry/heron/internal/testdeps"
)

// call 是假协作者收到的一次调用；held 是调用发生时 Service.mu 是否被持有（TryLock 失败即被持有）。
type call struct {
	name string
	node int64
	held bool
}

// fakes 是一套共用调用日志的假协作者，并按库的样子维护"存在的节点"与流量账本里的重置日，
// 用例据此判断内存是否与库分叉。
type fakes struct {
	svc *Service

	mu        sync.Mutex
	calls     []call
	exists    map[int64]bool
	resetDays map[int64]int
	pins      map[int64][2]string

	// 下面几项在用例开始前设好，之后只读。
	updateErr      error
	updateResult   store.NodeUpdateResult
	batchErr       error
	deleteErr      error
	createErr      error
	commitErr      error
	sweepErr       error
	evaluateErr    error
	onUpdateCommit func() // UpdateNode 提交成功之后、返回之前调用，用例借它让 Update 停在锁内
}

func newFakes(t *testing.T, ids ...int64) (*fakes, *bytes.Buffer) {
	t.Helper()
	f := &fakes{exists: map[int64]bool{}, resetDays: map[int64]int{}, pins: map[int64][2]string{}}
	for _, id := range ids {
		f.exists[id] = true
	}
	var logs bytes.Buffer
	f.svc = New(f.deps(slog.New(slog.NewTextHandler(&logs, nil))))
	return f, &logs
}

func (f *fakes) deps(log *slog.Logger) Deps {
	return Deps{Credentials: fakeCreds{f}, Nodes: fakeNodes{f}, Alerts: fakeAlerts{f}, Traffic: fakeTraffic{f}, State: fakeState{f}, Log: log}
}

func (f *fakes) record(name string, node int64) {
	held := !f.svc.mu.TryLock()
	if !held {
		f.svc.mu.Unlock()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{name: name, node: node, held: held})
}

func (f *fakes) log() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakes) resetDay(id int64) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.resetDays[id]
	return d, ok
}

type fakeCreds struct{ f *fakes }

func (c fakeCreds) CreateNode(_ context.Context, _ store.NewNode) (int64, string, error) {
	c.f.record("creds.CreateNode", 0)
	if c.f.createErr != nil {
		return 0, "", c.f.createErr
	}
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	id := int64(len(c.f.exists) + 1)
	c.f.exists[id] = true
	return id, "token", nil
}

func (c fakeCreds) RotateToken(_ context.Context, id int64) (string, error) {
	c.f.record("creds.RotateToken", id)
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	if !c.f.exists[id] {
		return "", store.ErrNotFound
	}
	return "rotated", nil
}

func (c fakeCreds) DeleteNode(_ context.Context, id int64) error {
	c.f.record("creds.DeleteNode", id)
	if c.f.deleteErr != nil {
		return c.f.deleteErr
	}
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	if !c.f.exists[id] {
		return store.ErrNotFound
	}
	delete(c.f.exists, id)
	return nil
}

type fakeNodes struct{ f *fakes }

func (n fakeNodes) UpdateNode(_ context.Context, id int64, _ store.NodeEdit) (store.NodeUpdateResult, error) {
	n.f.record("nodes.UpdateNode", id)
	if n.f.updateErr != nil {
		return store.NodeUpdateResult{}, n.f.updateErr
	}
	n.f.mu.Lock()
	ok := n.f.exists[id]
	n.f.mu.Unlock()
	if !ok {
		return store.NodeUpdateResult{}, store.ErrNotFound
	}
	if n.f.onUpdateCommit != nil {
		n.f.onUpdateCommit()
	}
	return n.f.updateResult, nil
}

func (n fakeNodes) BatchUpdateNodeTags(_ context.Context, ids []int64, _, _ []string) (store.NodeUpdateResult, error) {
	n.f.record("nodes.BatchUpdateNodeTags", ids[0])
	return store.NodeUpdateResult{}, n.f.batchErr
}

// fakeAlerts.UpdateScope 与 alert.Engine.UpdateScope 同契约：执行 mutate，失败原样返回且不发布，成功返回计费是否变化。
type fakeAlerts struct{ f *fakes }

func (a fakeAlerts) UpdateScope(mutate func() (store.NodeUpdateResult, error)) (bool, error) {
	a.f.record("alerts.UpdateScope", 0)
	result, err := mutate()
	if err != nil {
		return false, err
	}
	a.f.record("alerts.publishScope", 0)
	return result.BillingChanged, nil
}

func (a fakeAlerts) SweepExpiry(context.Context) error {
	a.f.record("alerts.SweepExpiry", 0)
	return a.f.sweepErr
}

func (a fakeAlerts) EvaluateTrafficNode(_ context.Context, id int64) error {
	a.f.record("alerts.EvaluateTrafficNode", id)
	return a.f.evaluateErr
}

func (a fakeAlerts) Forget(id int64) { a.f.record("alerts.Forget", id) }

type fakeTraffic struct{ f *fakes }

func (tr fakeTraffic) SetResetDay(id int64, day int) {
	tr.f.record("traffic.SetResetDay", id)
	tr.f.mu.Lock()
	defer tr.f.mu.Unlock()
	tr.f.resetDays[id] = day
}

func (tr fakeTraffic) Commit(_ context.Context, id int64) (traffic.Entry, error) {
	tr.f.record("traffic.Commit", id)
	return traffic.Entry{}, tr.f.commitErr
}

// fakeState.Forget 与 ingest.Service.Forget 一样清掉流量账本里该节点的状态与逐族探测开关。
type fakeState struct{ f *fakes }

func (s fakeState) SetAddressPins(id int64, ipv4, ipv6 string) {
	s.f.record("state.SetAddressPins", id)
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.pins[id] = [2]string{ipv4, ipv6}
}

func (s fakeState) Forget(id int64) {
	s.f.record("state.Forget", id)
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	delete(s.f.resetDays, id)
	delete(s.f.pins, id)
}

func (f *fakes) pinsOf(id int64) ([2]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pins[id]
	return p, ok
}

func names(calls []call) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.name
	}
	return out
}

// requireCalls 逐个核对调用名、节点与调用时是否持锁。
func requireCalls(t *testing.T, got []call, want ...call) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("calls:\n got  %v\n want %v", got, want)
	}
}

func TestNewRequiresEveryDep(t *testing.T) {
	f, _ := newFakes(t)
	testdeps.RequireEveryField(t, "nodeops.Deps", f.deps(slog.Default()), func(d Deps) { New(d) })
}

// 库提交成功之后才改内存，且库写入与内存更新同在锁内；扫描与评估在锁外、排在内存更新之后。
func TestUpdateCommitsMemoryOnlyAfterStoreAndScansOutsideLock(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		billing, traffc, rented bool
		want                    []call
	}{
		{"neither", false, false, false, []call{
			{"alerts.UpdateScope", 0, true}, {"nodes.UpdateNode", 7, true}, {"alerts.publishScope", 0, true},
			{"traffic.SetResetDay", 7, true}, {"state.SetAddressPins", 7, true},
		}},
		{"billing", true, false, false, []call{
			{"alerts.UpdateScope", 0, true}, {"nodes.UpdateNode", 7, true}, {"alerts.publishScope", 0, true},
			{"traffic.SetResetDay", 7, true}, {"state.SetAddressPins", 7, true},
			{"alerts.SweepExpiry", 0, false},
		}},
		{"rented", false, false, true, []call{
			{"alerts.UpdateScope", 0, true}, {"nodes.UpdateNode", 7, true}, {"alerts.publishScope", 0, true},
			{"traffic.SetResetDay", 7, true}, {"state.SetAddressPins", 7, true},
			{"alerts.SweepExpiry", 0, false},
		}},
		{"traffic", false, true, false, []call{
			{"alerts.UpdateScope", 0, true}, {"nodes.UpdateNode", 7, true}, {"alerts.publishScope", 0, true},
			{"traffic.SetResetDay", 7, true}, {"state.SetAddressPins", 7, true}, {"traffic.Commit", 7, true},
			{"alerts.EvaluateTrafficNode", 7, false},
		}},
		{"both", true, true, false, []call{
			{"alerts.UpdateScope", 0, true}, {"nodes.UpdateNode", 7, true}, {"alerts.publishScope", 0, true},
			{"traffic.SetResetDay", 7, true}, {"state.SetAddressPins", 7, true}, {"traffic.Commit", 7, true},
			{"alerts.SweepExpiry", 0, false}, {"alerts.EvaluateTrafficNode", 7, false},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newFakes(t, 7)
			f.updateResult = store.NodeUpdateResult{BillingChanged: tc.billing, TrafficChanged: tc.traffc, RentedChanged: tc.rented}
			if err := f.svc.Update(t.Context(), 7, store.NodeEdit{TrafficResetDay: 15, IPv4Pin: "8.8.8.8"}); err != nil {
				t.Fatal(err)
			}
			requireCalls(t, f.log(), tc.want...)
			if day, _ := f.resetDay(7); day != 15 {
				t.Fatalf("reset day = %d, want 15", day)
			}
			if pins, _ := f.pinsOf(7); pins != [2]string{"8.8.8.8", ""} {
				t.Fatalf("published pins = %q, want the committed edit's", pins)
			}
		})
	}
}

// 库写入失败时内存不动、作用域不发布、不扫描，错误原样返回给调用方映射。
func TestUpdateStoreFailureLeavesMemoryUntouched(t *testing.T) {
	boom := errors.New("disk full")
	for _, tc := range []struct {
		name   string
		exists bool
		err    error
		want   error
	}{
		{"not found", false, nil, store.ErrNotFound},
		{"store error", true, boom, boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ids []int64
			if tc.exists {
				ids = append(ids, 7)
			}
			f, _ := newFakes(t, ids...)
			f.updateErr = tc.err
			f.updateResult = store.NodeUpdateResult{BillingChanged: true, TrafficChanged: true}
			if err := f.svc.Update(t.Context(), 7, store.NodeEdit{TrafficResetDay: 15}); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			requireCalls(t, f.log(), call{"alerts.UpdateScope", 0, true}, call{"nodes.UpdateNode", 7, true})
			if day, ok := f.resetDay(7); ok {
				t.Fatalf("reset day %d written after a failed store write", day)
			}
			if pins, ok := f.pinsOf(7); ok {
				t.Fatalf("pins %q published after a failed store write", pins)
			}
		})
	}
}

// 流量 Commit 失败只记日志：修改已提交，Update 成功；没有落库的流量不评估。
func TestUpdateTrafficCommitFailureSkipsEvaluation(t *testing.T) {
	f, logs := newFakes(t, 7)
	f.updateResult = store.NodeUpdateResult{TrafficChanged: true}
	f.commitErr = errors.New("commit failed")
	if err := f.svc.Update(t.Context(), 7, store.NodeEdit{TrafficResetDay: 15}); err != nil {
		t.Fatal(err)
	}
	if got := names(f.log()); slices.Contains(got, "alerts.EvaluateTrafficNode") {
		t.Fatalf("evaluated traffic after a failed commit: %v", got)
	}
	if !strings.Contains(logs.String(), "traffic commit after node update failed") {
		t.Fatalf("commit failure not logged: %s", logs)
	}
}

// 扫描与评估失败只记日志：修改已提交，Update 成功。
func TestUpdateScanFailuresAreLoggedNotReturned(t *testing.T) {
	f, logs := newFakes(t, 7)
	f.updateResult = store.NodeUpdateResult{BillingChanged: true, TrafficChanged: true}
	f.sweepErr, f.evaluateErr = errors.New("sweep failed"), errors.New("evaluate failed")
	if err := f.svc.Update(t.Context(), 7, store.NodeEdit{TrafficResetDay: 15}); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []string{"expiry sweep after node update failed", "traffic evaluation after node update failed"} {
		if !strings.Contains(logs.String(), msg) {
			t.Fatalf("%q not logged: %s", msg, logs)
		}
	}
}

// Delete 在锁内提交，提交之后在锁外依次清上报侧与告警；Forget 是同一条链。
func TestDeleteRunsForgetChainAfterCommitOutsideLock(t *testing.T) {
	f, _ := newFakes(t, 7)
	f.resetDays[7] = 15
	if err := f.svc.Delete(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, f.log(), call{"creds.DeleteNode", 7, true}, call{"state.Forget", 7, false}, call{"alerts.Forget", 7, false})
	if day, ok := f.resetDay(7); ok {
		t.Fatalf("deleted node kept reset day %d", day)
	}
}

func TestForgetClearsEveryHolderInOrder(t *testing.T) {
	f, _ := newFakes(t)
	f.svc.Forget(9)
	requireCalls(t, f.log(), call{"state.Forget", 9, false}, call{"alerts.Forget", 9, false})
}

// 库删除失败时什么都不清：节点仍在，它的进程内状态必须留着。
func TestDeleteFailureForgetsNothing(t *testing.T) {
	boom := errors.New("disk full")
	for _, tc := range []struct {
		name   string
		exists bool
		err    error
		want   error
	}{
		{"not found", false, nil, store.ErrNotFound},
		{"store error", true, boom, boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ids []int64
			if tc.exists {
				ids = append(ids, 7)
			}
			f, _ := newFakes(t, ids...)
			f.deleteErr = tc.err
			if err := f.svc.Delete(t.Context(), 7); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			requireCalls(t, f.log(), call{"creds.DeleteNode", 7, true})
		})
	}
}

// Update 停在库提交之后、改内存之前时，并发的 Delete 必须等它放锁：两者串行，Delete 的 Forget 排在
// SetResetDay 之后，已删节点的重置日不会被复活。
func TestConcurrentUpdateAndDeleteSerialize(t *testing.T) {
	f, _ := newFakes(t, 7)
	committed, release := make(chan struct{}), make(chan struct{})
	f.onUpdateCommit = func() {
		close(committed)
		<-release
	}
	updated := make(chan error, 1)
	go func() { updated <- f.svc.Update(context.Background(), 7, store.NodeEdit{TrafficResetDay: 20}) }()
	<-committed
	deleted := make(chan error, 1)
	go func() { deleted <- f.svc.Delete(context.Background(), 7) }()
	// Delete 若不与 Update 互斥，这段时间里它就会提交；互斥时它只能等着，这段等待只影响探测灵敏度，不会误报。
	time.Sleep(50 * time.Millisecond)
	if got := names(f.log()); slices.Contains(got, "creds.DeleteNode") {
		t.Fatalf("delete committed while update held the node edit lock: %v", got)
	}
	close(release)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	got := names(f.log())
	if i, j := slices.Index(got, "traffic.SetResetDay"), slices.Index(got, "state.Forget"); i < 0 || j < 0 || i > j {
		t.Fatalf("forget did not follow the update's memory write: %v", got)
	}
	if day, ok := f.resetDay(7); ok {
		t.Fatalf("deleted node revived reset day %d", day)
	}
	if pins, ok := f.pinsOf(7); ok {
		t.Fatalf("deleted node revived pins %q", pins)
	}
}

// Delete 提交之后到达的 Update 在库层得到不存在，不改内存、不扫描，不重建已删节点的流量状态。
func TestUpdateAfterDeleteIsNotFoundAndRebuildsNothing(t *testing.T) {
	f, _ := newFakes(t, 7)
	f.updateResult = store.NodeUpdateResult{BillingChanged: true, TrafficChanged: true}
	if err := f.svc.Delete(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	before := len(f.log())
	if err := f.svc.Update(t.Context(), 7, store.NodeEdit{TrafficResetDay: 20}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update after delete: err = %v, want ErrNotFound", err)
	}
	requireCalls(t, f.log()[before:], call{"alerts.UpdateScope", 0, true}, call{"nodes.UpdateNode", 7, true})
	if day, ok := f.resetDay(7); ok {
		t.Fatalf("update after delete rebuilt reset day %d", day)
	}
}

// 建节点不取节点编辑锁；带计费才在建成之后扫描一次，建节点失败不扫描。
func TestCreateSweepsOnlyWithBillingAfterCommit(t *testing.T) {
	billing := store.Billing{ExpiresOn: "2026-01-02"}
	for _, tc := range []struct {
		name    string
		billing store.Billing
		err     error
		want    []call
	}{
		{"no billing", store.Billing{}, nil, []call{{"creds.CreateNode", 0, false}}},
		{"billing", billing, nil, []call{{"creds.CreateNode", 0, false}, {"alerts.SweepExpiry", 0, false}}},
		{"limit", billing, store.ErrNodeLimit, []call{{"creds.CreateNode", 0, false}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newFakes(t)
			f.createErr = tc.err
			id, tok, err := f.svc.Create(t.Context(), store.NewNode{Name: "n", Billing: tc.billing})
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if tc.err == nil && (id != 1 || tok != "token") {
				t.Fatalf("created (%d, %q), want (1, token)", id, tok)
			}
			requireCalls(t, f.log(), tc.want...)
		})
	}
}

// 建节点后的扫描失败只记日志：节点已提交，Create 成功。
func TestCreateSweepFailureIsLoggedNotReturned(t *testing.T) {
	f, logs := newFakes(t)
	f.sweepErr = errors.New("sweep failed")
	if _, _, err := f.svc.Create(t.Context(), store.NewNode{Name: "n", Billing: store.Billing{ExpiresOn: "2026-01-02"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "expiry sweep after node create failed") {
		t.Fatalf("sweep failure not logged: %s", logs)
	}
}

// 轮换 token 不改节点身份：不取节点编辑锁、不清进程内状态。
func TestRotateTokenForgetsNothing(t *testing.T) {
	f, _ := newFakes(t, 7)
	tok, err := f.svc.RotateToken(t.Context(), 7)
	if err != nil || tok != "rotated" {
		t.Fatalf("rotate = (%q, %v)", tok, err)
	}
	if _, err := f.svc.RotateToken(t.Context(), 8); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rotate missing node: err = %v, want ErrNotFound", err)
	}
	requireCalls(t, f.log(), call{"creds.RotateToken", 7, false}, call{"creds.RotateToken", 8, false})
}

// 批量改标签与 Update、Delete 共用节点编辑锁，在 UpdateScope 里提交；它不碰流量账本。
func TestBatchUpdateTagsHoldsLockAndPassesErrors(t *testing.T) {
	f, _ := newFakes(t, 7)
	if err := f.svc.BatchUpdateTags(t.Context(), []int64{7}, []string{"a"}, nil); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, f.log(), call{"alerts.UpdateScope", 0, true}, call{"nodes.BatchUpdateNodeTags", 7, true}, call{"alerts.publishScope", 0, true})

	f, _ = newFakes(t, 7)
	f.batchErr = store.ErrTagLimit
	if err := f.svc.BatchUpdateTags(t.Context(), []int64{7}, []string{"a"}, nil); !errors.Is(err, store.ErrTagLimit) {
		t.Fatalf("err = %v, want ErrTagLimit", err)
	}
	requireCalls(t, f.log(), call{"alerts.UpdateScope", 0, true}, call{"nodes.BatchUpdateNodeTags", 7, true})
}
