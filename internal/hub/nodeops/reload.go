package nodeops

import (
	"context"
	"log/slog"
	"time"
)

// Generation 是库的离线变更代数，实现是 store.Store。库外写者（带 store.ExternalWriter 打开的离线子命令）每提交一个
// 写事务就把它加一，hub 自己的写不推进；读它只是一次单行点查。
type Generation interface {
	OfflineGeneration(ctx context.Context) (uint64, error)
}

// TokenMap 是节点 token 映射，实现是 auth.Auth。Reload 自库整体重建映射，返回重建前在映射里、重建后不在的节点，
// 即库外删除的节点；返回错误时映射未变。
type TokenMap interface {
	Reload(ctx context.Context) ([]int64, error)
}

// TaskCache 是探测任务缓存，实现是 probe.Registry。
type TaskCache interface {
	Load(ctx context.Context) error
}

// ReloadDeps 是 Reloader 的协作者，全部必需。
type ReloadDeps struct {
	Generation Generation
	Tokens     TokenMap
	Tasks      TaskCache
	Log        *slog.Logger
}

// Reloader 让运行中的 hub 发现库外写入（离线子命令）并重载由库派生、只在启动时建立的缓存：token 映射与探测任务
// 缓存，并对库外删除的节点走与在线删除同一条清理链（Service.Forget）。
//
// confirmed 是已确认的代数：代数不超过它的库外写入都已反映在缓存里。它只由 Run 所在的协程读写。启动时调用方必须先读代数、再做各缓存的首次加载，再以读到的值构造 Reloader（见 cmd/hub 的 newHub）：
// 此后任何库外提交都让代数不等于 confirmed，首次加载与读代数之间不存在漏看的窗口；反过来先加载后读代数，夹在
// 两步之间的库外提交已计入 confirmed，却不在缓存里，永远不会被重载。
//
// 每个周期：
//  1. 读代数 G。G == confirmed 即本周期结束，不做任何加载——无变更的周期只有这一次点查。G 与 confirmed 不等就重载：
//     代数只增不减，变小只会来自 hub 运行中对它的库做了恢复（restore 重新种子为 0，它要求先停 hub）或手工改库，
//     这时库与缓存同样可能分叉，重载仍是正确的反应。
//  2. 重建 token 映射，得到库外删除的节点；随即对它们走 Service.forgetRemoved。删除集合只能从重建前的映射算出，
//     映射一旦被替换就再也算不到，所以清理紧跟在重建之后、不夹任何会失败的步骤：之后的任何失败都不会丢掉一次删除。
//  3. 重载探测任务缓存。
//  4. 全部成功后再读一次代数 G'。G' == G 才确认 G：两次读之间没有库外提交，各缓存看到的是同一组库外写入。G' 不等于
//     G 说明重载期间又有库外提交，可能一个缓存在它之前加载、另一个在它之后，缓存之间看到的库外写入不是同一组；这时
//     不确认，下一周期整轮重来。于是确认日志里的代数可以读作"到这一代为止的离线变更已在全部缓存里生效，且没有更晚的
//     离线变更只生效了一半"，运维据此判断离线命令是否已经生效。
//
// 任一步失败记 Error 日志（代数、步骤、错误），confirmed 不动，下一周期整轮重试。
//
// 各缓存按各自的锁独立发布，重载是最终一致的：重载进行中，读侧可以看到已换新的 token 映射与尚未换新的任务清单，
// 不宣称跨缓存原子。
type Reloader struct {
	svc       *Service
	gen       Generation
	tokens    TokenMap
	tasks     TaskCache
	log       *slog.Logger
	every     time.Duration
	confirmed uint64
}

// NewReloader 对缺失的依赖或非正的周期 panic，口径与 New 相同。confirmed 是启动时在首次加载之前读到的代数。
func NewReloader(svc *Service, deps ReloadDeps, confirmed uint64, every time.Duration) *Reloader {
	if svc == nil {
		panic("nodeops.NewReloader: svc must be set")
	}
	if deps.Generation == nil {
		panic("nodeops.ReloadDeps.Generation must be set")
	}
	if deps.Tokens == nil {
		panic("nodeops.ReloadDeps.Tokens must be set")
	}
	if deps.Tasks == nil {
		panic("nodeops.ReloadDeps.Tasks must be set")
	}
	if deps.Log == nil {
		panic("nodeops.ReloadDeps.Log must be set")
	}
	if every <= 0 {
		panic("nodeops.NewReloader: every must be positive")
	}
	return &Reloader{svc: svc, gen: deps.Generation, tokens: deps.Tokens, tasks: deps.Tasks, log: deps.Log, every: every, confirmed: confirmed}
}

// Run 每个周期执行一次 reload，直到 ctx 取消。
func (r *Reloader) Run(ctx context.Context) {
	t := time.NewTicker(r.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.reload(ctx)
		}
	}
}

func (r *Reloader) reload(ctx context.Context) {
	g, err := r.gen.OfflineGeneration(ctx)
	if err != nil {
		r.fail(ctx, r.confirmed, "read generation", err)
		return
	}
	if g == r.confirmed {
		return
	}
	r.log.Info("offline changes detected", "generation", g, "confirmed", r.confirmed)
	removed, err := r.tokens.Reload(ctx)
	if err != nil {
		r.fail(ctx, g, "reload tokens", err)
		return
	}
	r.svc.forgetRemoved(removed)
	if err := r.tasks.Load(ctx); err != nil {
		r.fail(ctx, g, "reload probe tasks", err)
		return
	}
	r.log.Info("offline caches reloaded", "generation", g, "removed_nodes", removed)
	now, err := r.gen.OfflineGeneration(ctx)
	if err != nil {
		r.fail(ctx, g, "confirm generation", err)
		return
	}
	if now != g {
		r.log.Info("offline changes arrived during reload", "generation", g, "current", now)
		return
	}
	r.confirmed = g
	r.log.Info("offline generation confirmed", "generation", g)
}

// fail 记下失败的一步。ctx 已取消说明进程在关停，失败是取消造成的，不记。
func (r *Reloader) fail(ctx context.Context, generation uint64, step string, err error) {
	if ctx.Err() != nil {
		return
	}
	r.log.Error("offline reload failed", "generation", generation, "confirmed", r.confirmed, "step", step, "err", err)
}
