package updates

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/update"
)

type fakeTasks struct {
	mu     sync.Mutex
	status map[int64]*heronv1.UpdateStatus
}

func (f *fakeTasks) Snapshot(id int64) *heronv1.UpdateStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.status[id]; s != nil {
		return s
	}
	return &heronv1.UpdateStatus{}
}

func (f *fakeTasks) ActiveTasks() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for _, s := range f.status {
		if t := s.GetTask(); t != nil && update.ActiveState(t.State) {
			out[t.Id] = t.Version
		}
	}
	return out
}

func (f *fakeTasks) set(node int64, id, version, state string, expires int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[node] = &heronv1.UpdateStatus{Task: &heronv1.UpdateTask{Id: id, Version: version, State: state, ExpiresAt: expires}}
}

const (
	taskA = "aaaaaaaaaaaaaaaa"
	taskB = "bbbbbbbbbbbbbbbb"
)

var now = time.Unix(10_000, 0)

func artifacts(n int) update.Artifacts {
	return update.Artifacts{Sums: []byte("s"), Signature: []byte("g"), Archive: make([]byte, n)}
}

type harness struct {
	tasks   *fakeTasks
	fetches atomic.Int32
	relay   *Relay
}

func newHarness(t *testing.T, fetch FetchFunc, verify VerifyFunc) *harness {
	t.Helper()
	h := &harness{tasks: &fakeTasks{status: map[int64]*heronv1.UpdateStatus{}}}
	if fetch == nil {
		fetch = func(context.Context, string, string) (update.Artifacts, error) { return artifacts(10), nil }
	}
	if verify == nil {
		verify = func(string, string, update.Artifacts) error { return nil }
	}
	counted := func(ctx context.Context, v, a string) (update.Artifacts, error) {
		h.fetches.Add(1)
		return fetch(ctx, v, a)
	}
	h.relay = NewRelay(h.tasks, counted, verify, clock.NewFake(now))
	return h
}

func TestRelayServesDispatchedAndDownloadingTasks(t *testing.T) {
	for _, state := range []string{"dispatched", "downloading"} {
		h := newHarness(t, nil, nil)
		h.tasks.set(1, taskA, "v1.0.0", state, now.Unix()+60)
		a, err := h.relay.Get(context.Background(), 1, taskA, "amd64")
		if err != nil || len(a.Archive) != 10 {
			t.Fatalf("%s: a=%v err=%v", state, a, err)
		}
	}
}

func TestRelayRejectsWithoutMatchingLiveTask(t *testing.T) {
	for name, set := range map[string]func(*fakeTasks){
		"no_task":     func(*fakeTasks) {},
		"queued":      func(f *fakeTasks) { f.set(1, taskA, "v1.0.0", "queued", now.Unix()+60) },
		"succeeded":   func(f *fakeTasks) { f.set(1, taskA, "v1.0.0", "succeeded", now.Unix()+60) },
		"unconfirmed": func(f *fakeTasks) { f.set(1, taskA, "v1.0.0", "unconfirmed", now.Unix()+60) },
		"expired":     func(f *fakeTasks) { f.set(1, taskA, "v1.0.0", "dispatched", now.Unix()) },
		"other_id":    func(f *fakeTasks) { f.set(1, taskB, "v1.0.0", "dispatched", now.Unix()+60) },
	} {
		h := newHarness(t, nil, nil)
		set(h.tasks)
		if _, err := h.relay.Get(context.Background(), 1, taskA, "amd64"); !errors.Is(err, ErrNoTask) {
			t.Errorf("%s: err = %v, want ErrNoTask", name, err)
		}
		if h.fetches.Load() != 0 {
			t.Errorf("%s: fetched without a matching task", name)
		}
	}
}

func TestRelayRejectsForeignTask(t *testing.T) {
	h := newHarness(t, nil, nil)
	// 节点 1 自己持有在途任务时才构成最强的冒用形态：仅凭持有任务删不掉的 nil 分支挡不住它，
	// 必须由 ID 比较拒绝。
	h.tasks.set(1, taskA, "v1.0.0", "dispatched", now.Unix()+60)
	h.tasks.set(2, taskB, "v1.0.0", "dispatched", now.Unix()+60)
	if _, err := h.relay.Get(context.Background(), 1, taskB, "amd64"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("node 1 fetched node 2's task: %v", err)
	}
	if h.fetches.Load() != 0 {
		t.Fatal("a foreign task triggered a fetch")
	}
}

func TestRelayRejectsUnknownArch(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.tasks.set(1, taskA, "v1.0.0", "dispatched", now.Unix()+60)
	if _, err := h.relay.Get(context.Background(), 1, taskA, "mips"); !errors.Is(err, ErrArch) {
		t.Fatalf("err = %v", err)
	}
}

func TestRelayMergesConcurrentFetches(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(context.Context, string, string) (update.Artifacts, error) {
		<-release
		return artifacts(10), nil
	}, nil)
	h.tasks.set(1, taskA, "v1.0.0", "dispatched", now.Unix()+60)
	h.tasks.set(2, taskB, "v1.0.0", "dispatched", now.Unix()+60)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range []struct {
		node int64
		task string
	}{{1, taskA}, {2, taskB}} {
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = h.relay.Get(context.Background(), c.node, c.task, "amd64") }()
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.fetches.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // 给第二个请求进入等待的机会；合并与否由下面的计数判定，不靠这段时长
	close(release)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || h.fetches.Load() != 1 {
		t.Fatalf("errs=%v fetches=%d, want one shared fetch", errs, h.fetches.Load())
	}
}

func TestRelayOneInFlightPerNode(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(context.Context, string, string) (update.Artifacts, error) {
		<-release
		return artifacts(10), nil
	}, nil)
	h.tasks.set(1, taskA, "v1.0.0", "dispatched", now.Unix()+60)
	done := make(chan error, 1)
	go func() { _, err := h.relay.Get(context.Background(), 1, taskA, "amd64"); done <- err }()
	for h.fetches.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if _, err := h.relay.Get(context.Background(), 1, taskA, "amd64"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second concurrent request: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRelayLimitsAttemptsPerTask(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.tasks.set(1, taskA, "v1.0.0", "dispatched", now.Unix()+60)
	for i := 0; i < MaxAttemptsPerTask; i++ {
		if _, err := h.relay.Get(context.Background(), 1, taskA, "amd64"); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if _, err := h.relay.Get(context.Background(), 1, taskA, "amd64"); !errors.Is(err, ErrAttempts) {
		t.Fatalf("attempt %d: %v", MaxAttemptsPerTask+1, err)
	}
}

func TestRelayDoesNotCacheFailures(t *testing.T) {
	for name, h := range map[string]*harness{
		"fetch": newHarness(t, func(context.Context, string, string) (update.Artifacts, error) {
			return update.Artifacts{}, errors.New("github unreachable")
		}, nil),
		"verify": newHarness(t, nil, func(string, string, update.Artifacts) error { return errors.New("bad signature") }),
	} {
		h.tasks.set(1, taskA, "v1.0.0", "dispatched", now.Unix()+60)
		for i := 0; i < 2; i++ {
			if _, err := h.relay.Get(context.Background(), 1, taskA, "amd64"); err == nil {
				t.Fatalf("%s: failure was served", name)
			}
		}
		if h.fetches.Load() != 2 {
			t.Errorf("%s: fetches = %d, want a fresh fetch per request", name, h.fetches.Load())
		}
	}
}

func TestRelayCacheCap(t *testing.T) {
	h := newHarness(t, func(_ context.Context, _ string, arch string) (update.Artifacts, error) { return artifacts(60), nil }, nil)
	h.relay.maxBytes = 100
	h.tasks.set(1, taskA, "v1.0.0", "dispatched", now.Unix()+60)
	h.tasks.set(2, taskB, "v1.0.0", "dispatched", now.Unix()+60)
	if _, err := h.relay.Get(context.Background(), 1, taskA, "amd64"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.relay.Get(context.Background(), 2, taskB, "arm64"); !errors.Is(err, ErrCacheFull) {
		t.Fatalf("err = %v, want ErrCacheFull", err)
	}
	// 在用条目不被挤掉：同版本同架构的再次请求仍命中缓存。
	if _, err := h.relay.Get(context.Background(), 1, taskA, "amd64"); err != nil || h.fetches.Load() != 2 {
		t.Fatalf("err=%v fetches=%d", err, h.fetches.Load())
	}
}

func TestRelaySweepReleasesUnreferenced(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.tasks.set(1, taskA, "v1.0.0", "dispatched", now.Unix()+60)
	if _, err := h.relay.Get(context.Background(), 1, taskA, "amd64"); err != nil {
		t.Fatal(err)
	}
	h.relay.Sweep()
	if h.relay.cachedBytes() == 0 {
		t.Fatal("swept a version still referenced by an active task")
	}
	h.tasks.set(1, taskA, "v1.0.0", "succeeded", now.Unix()+60)
	h.relay.Sweep()
	if h.relay.cachedBytes() != 0 || h.relay.attemptCount(taskA) != 0 {
		t.Fatalf("bytes=%d attempts=%d after the task ended", h.relay.cachedBytes(), h.relay.attemptCount(taskA))
	}
}

// 中转从 GitHub 取的总上限与更新器直连同一个 update.DownloadLimit；它比节点经 hub 的期限短，节点才收得到这里的错误。
func TestRelayFetchUsesDownloadLimit(t *testing.T) {
	got := make(chan time.Duration, 1)
	h := newHarness(t, func(ctx context.Context, _, _ string) (update.Artifacts, error) {
		d, _ := ctx.Deadline()
		got <- time.Until(d)
		return artifacts(10), nil
	}, nil)
	h.tasks.set(1, taskA, "v1.0.0", "dispatched", now.Unix()+60)
	if _, err := h.relay.Get(context.Background(), 1, taskA, "amd64"); err != nil {
		t.Fatal(err)
	}
	if d := <-got; d > update.DownloadLimit || d < update.DownloadLimit-5*time.Second {
		t.Fatalf("relay fetch deadline %v, want update.DownloadLimit %v", d, update.DownloadLimit)
	}
}
