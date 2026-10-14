package updates

import (
	"context"
	"errors"
	"sync"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/update"
)

const (
	// MaxAttemptsPerTask 大于更新器每个任务的 1 次请求；它把持 token 者能消耗的 hub 出口带宽绑定到
	// 管理员创建的任务数上（任务只有管理员会话能建）。
	MaxAttemptsPerTask = 3
	// MaxCacheBytes 是缓存总字节上限，为单个归档上限（128 MiB）的两倍。
	MaxCacheBytes = 256 << 20
)

var (
	ErrArch      = errors.New("architecture is not in the agent artifact matrix")
	ErrNoTask    = errors.New("no matching update task in progress for this node")
	ErrBusy      = errors.New("this node already has a release download in progress")
	ErrAttempts  = errors.New("release download attempts for this task are exhausted")
	ErrCacheFull = errors.New("release cache is full")
)

type RelayTasks interface {
	Snapshot(int64) *heronv1.UpdateStatus
	ActiveTasks() map[string]string
}

type FetchFunc func(ctx context.Context, version, arch string) (update.Artifacts, error)
type VerifyFunc func(version, arch string, a update.Artifacts) error

// Relay 为 hub 来源的节点中转官方产物（规则见 update.Accept）。它只转发：取回后预验签，让坏产物在 hub 处就报错、
// 不必每个节点各下一遍才失败；接受与否仍由节点上的更新器验签决定——失守的 hub 可以跳过这里，所以这里不是防线。
// 缓存只在内存，按引用释放（Sweep）；不落盘，没有崩溃残留要清理。
type Relay struct {
	tasks    RelayTasks
	fetch    FetchFunc
	verify   VerifyFunc
	clk      clock.Clock
	maxBytes int64

	mu       sync.Mutex
	inflight map[int64]bool
	attempts map[string]int
	cache    map[relayKey]*relayEntry
	bytes    int64
}

type relayKey struct{ version, arch string }

// relayEntry 在取回完成前 done 未关闭；完成后 a 与 err 只读。失败的条目在关闭 done 之前已从 cache 删除。
type relayEntry struct {
	done chan struct{}
	a    update.Artifacts
	err  error
	size int64
}

func NewRelay(tasks RelayTasks, fetch FetchFunc, verify VerifyFunc, clk clock.Clock) *Relay {
	return &Relay{tasks: tasks, fetch: fetch, verify: verify, clk: clk, maxBytes: MaxCacheBytes,
		inflight: map[int64]bool{}, attempts: map[string]int{}, cache: map[relayKey]*relayEntry{}}
}

// Get 只服务该节点当前进行中的那个任务：ID 相同、状态 dispatched 或 downloading、未过期，版本取自任务本身。
// hub 侧任务在更新器开始下载前是 dispatched，更新器报出 downloading 后推进（Manager.flush）；更新器本地的
// queued 不推进 hub 状态，所以这两个状态覆盖了下载期间 hub 能看到的全部取值。
func (r *Relay) Get(ctx context.Context, node int64, taskID, arch string) (update.Artifacts, error) {
	if !update.AgentArch(arch) {
		return update.Artifacts{}, ErrArch
	}
	t := r.tasks.Snapshot(node).GetTask()
	if t == nil || t.Id != taskID || (t.State != "dispatched" && t.State != "downloading") || t.ExpiresAt <= r.clk.Now().Unix() {
		return update.Artifacts{}, ErrNoTask
	}
	r.mu.Lock()
	if r.inflight[node] {
		r.mu.Unlock()
		return update.Artifacts{}, ErrBusy
	}
	if r.attempts[taskID] >= MaxAttemptsPerTask {
		r.mu.Unlock()
		return update.Artifacts{}, ErrAttempts
	}
	r.inflight[node] = true
	r.attempts[taskID]++
	k := relayKey{t.Version, arch}
	e := r.cache[k]
	if e == nil {
		e = &relayEntry{done: make(chan struct{})}
		r.cache[k] = e
		go r.load(k, e)
	}
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.inflight, node); r.mu.Unlock() }()
	select {
	case <-e.done:
		return e.a, e.err
	case <-ctx.Done():
		return update.Artifacts{}, ctx.Err()
	}
}

// load 用独立的上下文取回：发起请求的节点断开不能让同键上等待的其他节点一起失败。总上限与更新器直连 GitHub
// 是同一个 update.DownloadLimit，停滞判定在 fetch（正式装配为 update.OfficialSource.Fetch）读正文时做；节点侧
// 经 hub 的期限比它长（见 update 包 hubFetchLimit），这里先到期，节点才收得到这里的错误文案。
func (r *Relay) load(k relayKey, e *relayEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), update.DownloadLimit)
	defer cancel()
	a, err := r.fetch(ctx, k.version, k.arch)
	if err == nil {
		err = r.verify(k.version, k.arch, a)
	}
	size := int64(len(a.Sums) + len(a.Signature) + len(a.Archive))
	r.mu.Lock()
	if err == nil && r.bytes+size > r.maxBytes {
		err = ErrCacheFull
	}
	if err != nil {
		delete(r.cache, k)
		e.err = err
	} else {
		e.a, e.size = a, size
		r.bytes += size
	}
	r.mu.Unlock()
	close(e.done)
}

// Sweep 释放不再被进行中任务引用的版本与已结束任务的取用计数。
func (r *Relay) Sweep() {
	active := r.tasks.ActiveTasks()
	versions := make(map[string]bool, len(active))
	for _, v := range active {
		versions[v] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.attempts {
		if _, ok := active[id]; !ok {
			delete(r.attempts, id)
		}
	}
	for k, e := range r.cache {
		select {
		case <-e.done:
		default:
			continue
		}
		if !versions[k.version] {
			delete(r.cache, k)
			r.bytes -= e.size
		}
	}
}

func (r *Relay) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Sweep()
		}
	}
}

func (r *Relay) cachedBytes() int64 { r.mu.Lock(); defer r.mu.Unlock(); return r.bytes }

func (r *Relay) attemptCount(id string) int { r.mu.Lock(); defer r.mu.Unlock(); return r.attempts[id] }
