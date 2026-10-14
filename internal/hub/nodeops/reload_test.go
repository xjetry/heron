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
	"github.com/xjetry/heron/internal/testdeps"
)

// reloadFakes 是重载器的假协作者：代数、token 映射（按库的样子维护映射里的节点）与任务缓存。
// tokens 的 Reload 与 auth.Auth.Reload 同契约：返回映射里有、库里（fakes.exists）没有的节点，成功后映射与库一致。
type reloadFakes struct {
	*fakes

	rmu        sync.Mutex
	generation uint64
	cached     map[int64]bool
	tokenLoads int
	taskLoads  int
	genErr     error // 下一次读代数失败，之后自动清除
	tokensErr  error // 下一次重建映射失败，之后自动清除
	tasksErr   error // 下一次加载任务失败，之后自动清除
	onTasks    func()
}

func newReloadFakes(t *testing.T, confirmed uint64, ids ...int64) (*reloadFakes, *Reloader, *bytes.Buffer) {
	t.Helper()
	f, logs := newFakes(t, ids...)
	rf := &reloadFakes{fakes: f, generation: confirmed, cached: map[int64]bool{}}
	for _, id := range ids {
		rf.cached[id] = true
	}
	r := NewReloader(f.svc, ReloadDeps{Generation: reloadGen{rf}, Tokens: reloadTokens{rf}, Tasks: reloadTasks{rf},
		Log: slog.New(slog.NewTextHandler(logs, nil))}, confirmed, time.Hour)
	return rf, r, logs
}

func (rf *reloadFakes) advance() {
	rf.rmu.Lock()
	defer rf.rmu.Unlock()
	rf.generation++
}

// deleteOffline 模拟离线子命令删除节点：库里没了、代数推进，进程内的映射与各状态都还在。
func (rf *reloadFakes) deleteOffline(id int64) {
	rf.mu.Lock()
	delete(rf.exists, id)
	rf.mu.Unlock()
	rf.advance()
}

func (rf *reloadFakes) loads() (tokens, tasks int) {
	rf.rmu.Lock()
	defer rf.rmu.Unlock()
	return rf.tokenLoads, rf.taskLoads
}

type reloadGen struct{ rf *reloadFakes }

func (g reloadGen) OfflineGeneration(context.Context) (uint64, error) {
	g.rf.rmu.Lock()
	defer g.rf.rmu.Unlock()
	if err := g.rf.genErr; err != nil {
		g.rf.genErr = nil
		return 0, err
	}
	return g.rf.generation, nil
}

type reloadTokens struct{ rf *reloadFakes }

func (tm reloadTokens) Reload(context.Context) ([]int64, error) {
	rf := tm.rf
	rf.record("tokens.Reload", 0)
	rf.rmu.Lock()
	defer rf.rmu.Unlock()
	rf.tokenLoads++
	if err := rf.tokensErr; err != nil {
		rf.tokensErr = nil
		return nil, err
	}
	rf.mu.Lock()
	defer rf.mu.Unlock()
	var removed []int64
	for id := range rf.cached {
		if !rf.exists[id] {
			removed = append(removed, id)
		}
	}
	slices.Sort(removed)
	rf.cached = map[int64]bool{}
	for id := range rf.exists {
		rf.cached[id] = true
	}
	return removed, nil
}

type reloadTasks struct{ rf *reloadFakes }

func (tc reloadTasks) Load(context.Context) error {
	rf := tc.rf
	rf.record("tasks.Load", 0)
	if rf.onTasks != nil {
		rf.onTasks()
	}
	rf.rmu.Lock()
	defer rf.rmu.Unlock()
	rf.taskLoads++
	if err := rf.tasksErr; err != nil {
		rf.tasksErr = nil
		return err
	}
	return nil
}

func TestNewReloaderRequiresEveryDep(t *testing.T) {
	f, _ := newFakes(t)
	rf := &reloadFakes{fakes: f}
	valid := ReloadDeps{Generation: reloadGen{rf}, Tokens: reloadTokens{rf}, Tasks: reloadTasks{rf}, Log: slog.Default()}
	testdeps.RequireEveryField(t, "nodeops.ReloadDeps", valid, func(d ReloadDeps) { NewReloader(f.svc, d, 0, time.Second) })
	for name, construct := range map[string]func(){
		"svc":    func() { NewReloader(nil, valid, 0, time.Second) },
		"period": func() { NewReloader(f.svc, valid, 0, 0) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("NewReloader accepted a missing %s", name)
				}
			}()
			construct()
		})
	}
}

// 代数等于已确认值的周期只读一次代数，不做任何加载，也不记日志。
func TestReloadSkipsUnchangedGeneration(t *testing.T) {
	rf, r, logs := newReloadFakes(t, 3, 7)
	r.reload(t.Context())
	if tokens, tasks := rf.loads(); tokens != 0 || tasks != 0 {
		t.Fatalf("unchanged generation reloaded tokens %d, tasks %d times", tokens, tasks)
	}
	if logs.Len() != 0 {
		t.Fatalf("idle cycle logged: %s", logs)
	}
}

// 代数变化后重建映射、清掉库外删除的节点、重载任务，再确认；确认之后的周期不再加载。
func TestReloadForgetsRemovedNodesAndConfirms(t *testing.T) {
	rf, r, logs := newReloadFakes(t, 0, 7, 8)
	rf.resetDays[7], rf.resetDays[8] = 3, 4
	rf.deleteOffline(7)
	r.reload(t.Context())
	got := rf.log()
	requireCalls(t, got,
		call{"tokens.Reload", 0, false},
		call{"state.Forget", 7, false}, call{"alerts.Forget", 7, false},
		call{"tasks.Load", 0, false})
	if _, ok := rf.resetDay(7); ok {
		t.Fatal("offline-deleted node kept its reset day")
	}
	if _, ok := rf.resetDay(8); !ok {
		t.Fatal("surviving node lost its reset day")
	}
	if r.confirmed != 1 || !strings.Contains(logs.String(), `msg="offline generation confirmed" generation=1`) {
		t.Fatalf("confirmed %d, logs:\n%s", r.confirmed, logs)
	}
	r.reload(t.Context())
	if tokens, tasks := rf.loads(); tokens != 1 || tasks != 1 {
		t.Fatalf("confirmed generation reloaded again: tokens %d, tasks %d", tokens, tasks)
	}
}

// 重载期间又有库外提交（G' > G）：不确认 G，下一周期整轮重来并确认 G'。确认日志里从不出现中间代。
func TestReloadDoesNotConfirmGenerationThatMovedDuringReload(t *testing.T) {
	rf, r, logs := newReloadFakes(t, 0, 7)
	rf.advance()
	var once sync.Once
	rf.onTasks = func() { once.Do(rf.advance) }
	r.reload(t.Context())
	if r.confirmed != 0 {
		t.Fatalf("confirmed %d while a commit arrived during reload", r.confirmed)
	}
	if !strings.Contains(logs.String(), `msg="offline changes arrived during reload" generation=1 current=2`) {
		t.Fatalf("missing catch-up log:\n%s", logs)
	}
	r.reload(t.Context())
	if tokens, tasks := rf.loads(); tokens != 2 || tasks != 2 || r.confirmed != 2 {
		t.Fatalf("catch-up: tokens %d, tasks %d, confirmed %d; want 2, 2, 2", tokens, tasks, r.confirmed)
	}
	if strings.Contains(logs.String(), `msg="offline generation confirmed" generation=1`) {
		t.Fatalf("intermediate generation confirmed:\n%s", logs)
	}
}

// 任一步失败：Error 日志带代数与步骤，confirmed 不动，下一周期整轮重来并恢复。重建映射之前失败的，删除在下一轮从
// 未被替换的映射重新算出；任务加载失败时删除已在同一轮清掉，不会丢。
func TestReloadFailureKeepsConfirmedAndRecovers(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name   string
		inject func(*reloadFakes)
		step   string
	}{
		{"generation", func(rf *reloadFakes) { rf.genErr = boom }, "read generation"},
		{"tokens", func(rf *reloadFakes) { rf.tokensErr = boom }, "reload tokens"},
		{"tasks", func(rf *reloadFakes) { rf.tasksErr = boom }, "reload probe tasks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rf, r, logs := newReloadFakes(t, 0, 7, 8)
			rf.resetDays[7] = 3
			rf.deleteOffline(7)
			tc.inject(rf)
			r.reload(t.Context())
			if r.confirmed != 0 {
				t.Fatalf("confirmed %d after a failed %s", r.confirmed, tc.step)
			}
			if !strings.Contains(logs.String(), `level=ERROR msg="offline reload failed"`) || !strings.Contains(logs.String(), `step="`+tc.step+`"`) {
				t.Fatalf("missing failure log for %s:\n%s", tc.step, logs)
			}
			r.reload(t.Context())
			if r.confirmed != 1 {
				t.Fatalf("did not recover: confirmed %d\n%s", r.confirmed, logs)
			}
			if _, ok := rf.resetDay(7); ok {
				t.Fatal("offline-deleted node kept its reset day after recovery")
			}
			forgets := 0
			for _, c := range rf.log() {
				if c.name == "state.Forget" && c.node == 7 {
					forgets++
				}
			}
			if forgets != 1 {
				t.Fatalf("node 7 forgotten %d times, want exactly once: %v", forgets, names(rf.log()))
			}
		})
	}
}

// Update 停在库提交之后、改内存之前时发生库外删除：重载的 Forget 必须等 Update 放锁，最终内存里没有该节点的重置日。
func TestReloadForgetWaitsForUpdateHoldingTheLock(t *testing.T) {
	rf, r, _ := newReloadFakes(t, 0, 7)
	committed, release := make(chan struct{}), make(chan struct{})
	rf.onUpdateCommit = func() {
		close(committed)
		<-release
	}
	updated := make(chan error, 1)
	go func() { updated <- rf.svc.Update(context.Background(), 7, store.NodeEdit{TrafficResetDay: 20}) }()
	<-committed
	rf.deleteOffline(7)
	reloaded := make(chan struct{})
	go func() {
		r.reload(context.Background())
		close(reloaded)
	}()
	// 屏障若不等 Update，这段时间里 Forget 就会执行；等时它只能挂着，这段等待只影响探测灵敏度，不会误报。
	time.Sleep(50 * time.Millisecond)
	if got := names(rf.log()); slices.Contains(got, "state.Forget") {
		t.Fatalf("reload forgot node 7 while an update held the node edit lock: %v", got)
	}
	close(release)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	<-reloaded
	got := names(rf.log())
	if i, j := slices.Index(got, "traffic.SetResetDay"), slices.Index(got, "state.Forget"); i < 0 || j < 0 || i > j {
		t.Fatalf("forget did not follow the update's memory write: %v", got)
	}
	if day, ok := rf.resetDay(7); ok {
		t.Fatalf("offline-deleted node revived reset day %d", day)
	}
}

// Run 按周期执行，ctx 取消即退出；关停中途的失败是取消造成的，不记 Error。
func TestReloaderRunStopsOnCancelWithoutErrorLogs(t *testing.T) {
	f, logs := newFakes(t, 7)
	rf := &reloadFakes{fakes: f, cached: map[int64]bool{7: true}}
	ctx, cancel := context.WithCancel(context.Background())
	rf.onTasks = func() { cancel() }
	rf.tasksErr = context.Canceled
	r := NewReloader(f.svc, ReloadDeps{Generation: reloadGen{rf}, Tokens: reloadTokens{rf}, Tasks: reloadTasks{rf},
		Log: slog.New(slog.NewTextHandler(logs, nil))}, 0, time.Millisecond)
	rf.advance()
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("shutdown logged an error:\n%s", logs)
	}
}
