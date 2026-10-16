// 写协程钩子只对 store 的测试构建可见；把真实注册表的并发回归放在这里，避免向生产代码暴露钩子。
package store_test

import (
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

func registryRaceStore(t *testing.T) (*probe.Registry, *store.Store, int64) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), clock.NewFake(time.Unix(0, 0)), log, store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	id, _, err := st.CreateNode(t.Context(), store.NewNode{Name: "node"}, []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	r := probe.New(st, log)
	if err := r.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	return r, st, id
}

func waitRegistryWrite(t *testing.T, pending func() int) {
	t.Helper()
	deadline := time.Now().Add(testwait.Bound)
	for pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("registry write did not reach writer queue")
		}
		runtime.Gosched()
	}
}

func registryGoroutineID() string {
	var buf [128]byte
	n := runtime.Stack(buf[:], false)
	return strings.Fields(string(buf[:n]))[1]
}

// 固定等待窗口不能证明目标协程已获调度；观察指定协程实际阻塞或越过临界区才能区分互斥与缺锁。
// probe 的 export_test 钩子不会编入这里导入的 probe 包，因此从栈观察阻塞，不给生产接口增加测试钩子。
func waitRegistryMutex(t *testing.T, id, method string, progressed func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(testwait.Bound)
	buf := make([]byte, 64<<10)
	var target string
	for {
		if progressed() {
			return false
		}
		n := runtime.Stack(buf, true)
		if n == len(buf) {
			buf = make([]byte, 2*len(buf))
			continue
		}
		for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.HasPrefix(stack, "goroutine "+id+" ") {
				target = stack
				if strings.Contains(stack, "[sync.Mutex.Lock") && strings.Contains(stack, "probe.(*Registry)."+method+"(") {
					return true
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for goroutine %s Registry.%s to block on mutex or advance; last stack:\n%s", id, method, target)
		}
		runtime.Gosched()
	}
}

func TestRegistryForgetCannotBeRevivedByEarlierSave(t *testing.T) {
	t.Parallel()
	r, st, id := registryRaceStore(t)
	release, pending, drain := st.HoldWriterForTest(t)
	defer release()
	type result struct {
		detail probe.Detail
		err    error
	}
	saved := make(chan result, 1)
	go func() {
		d, _, err := r.Save(t.Context(), &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "localhost", IntervalS: 5, TimeoutMs: 1000}, store.NodeSelector{AllNodes: false, NodeIDs: []int64{id}})
		saved <- result{d, err}
	}()
	waitRegistryWrite(t, pending)
	forgotten := make(chan struct{})
	started := make(chan string)
	go func() {
		started <- registryGoroutineID()
		r.Forget(id)
		close(forgotten)
	}()
	if !waitRegistryMutex(t, <-started, "Forget", func() bool {
		select {
		case <-forgotten:
			return true
		default:
			return false
		}
	}) {
		t.Error("Forget returned before earlier Save published")
	}
	release()
	res := <-saved
	if res.err != nil {
		t.Fatal(res.err)
	}
	<-forgotten
	if err := drain(); err != nil {
		t.Fatal(err)
	}
	if r.Assigned(id, res.detail.Task.Id) {
		t.Fatal("earlier Save revived forgotten node assignment")
	}
}

func TestRegistryDeleteSerializesFollowingSave(t *testing.T) {
	t.Parallel()
	r, st, id := registryRaceStore(t)
	task := &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "localhost", IntervalS: 5, TimeoutMs: 1000}
	d, _, err := r.Save(t.Context(), task, store.NodeSelector{AllNodes: false, NodeIDs: []int64{id}})
	if err != nil {
		t.Fatal(err)
	}
	release, pending, drain := st.HoldWriterForTest(t)
	defer release()
	deleted := make(chan error, 1)
	go func() { _, err := r.Delete(t.Context(), d.Task.Id); deleted <- err }()
	waitRegistryWrite(t, pending)
	saved := make(chan error, 1)
	started := make(chan string)
	go func() {
		started <- registryGoroutineID()
		_, _, err := r.Save(t.Context(), task, store.NodeSelector{AllNodes: false, NodeIDs: []int64{id}})
		saved <- err
	}()
	// Delete 入队时已持 writeMu，后来的 Save 在删除发布之前不能进入写队列。
	waitRegistryMutex(t, <-started, "Save", func() bool { return pending() > 1 })
	n := pending()
	release()
	deleteErr, saveErr := <-deleted, <-saved
	if deleteErr != nil || saveErr != nil {
		t.Fatalf("delete error=%v save error=%v", deleteErr, saveErr)
	}
	if err := drain(); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pending writes while Delete blocked=%d; want 1", n)
	}
}
