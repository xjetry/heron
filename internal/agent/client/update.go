package client

import (
	"context"
	"log/slog"
	"sync"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/update"
	"google.golang.org/protobuf/proto"
)

type localUpdater interface {
	Status(context.Context) update.Status
	Submit(context.Context, update.Request) (update.Job, error)
	Ready(context.Context, string) error
}

// UpdateCoordinator 将本机 socket 往返移到后台，Report 只交换不可变快照。
type UpdateCoordinator struct {
	client  localUpdater
	version string
	log     *slog.Logger
	mu      sync.Mutex
	// status 在第一次 refresh（本机更新器查询）完成前为 nil：在此之前没有真实状态可报，
	// 占位状态会让 hub 按摘要对账时先记下一份假状态、多走一轮 want_update。Ready 之前的
	// 读者由 Runner.Run 在构造第一份上报前的等待排除，这是它保证的，不是本类型。
	status *heronv1.UpdateStatus
	// ready 在第一次 refresh 完成后由 Run 关闭（Run 恰好被调用一次，调用点只有 main 的
	// go updater.Run 与测试）。关闭后 status 永远非 nil：refresh 总是整体写入一个状态。
	ready   chan struct{}
	success chan *heronv1.UpdateTask
}

func NewUpdateCoordinator(version string, log *slog.Logger) *UpdateCoordinator {
	return &UpdateCoordinator{client: update.NewClient("agent"), version: version, log: log, ready: make(chan struct{}), success: make(chan *heronv1.UpdateTask, 1)}
}

// Ready 关闭即第一次本机状态查询已完成，此后的 Snapshot 非 nil；关闭前为 nil。等待它的一方
// 只能是构造第一份上报之前的 Runner.Run，别的读者不得早于它。
func (c *UpdateCoordinator) Ready() <-chan struct{} { return c.ready }
func (c *UpdateCoordinator) Snapshot() *heronv1.UpdateStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status == nil {
		return nil
	}
	return proto.Clone(c.status).(*heronv1.UpdateStatus)
}
func (c *UpdateCoordinator) Reported(task *heronv1.UpdateTask) {
	if task != nil {
		task = proto.Clone(task).(*heronv1.UpdateTask)
	}
	select {
	case c.success <- task:
	default:
	}
}
func (c *UpdateCoordinator) refresh(ctx context.Context) update.Status {
	s := c.client.Status(ctx)
	c.mu.Lock()
	c.status = update.StatusProto(s, c.version)
	c.mu.Unlock()
	return s
}
func (c *UpdateCoordinator) Run(ctx context.Context) {
	c.refresh(ctx)
	close(c.ready)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.refresh(ctx)
		case task := <-c.success:
			s := c.refresh(ctx)
			if s.Job != nil && s.Job.State == "verifying" && s.Job.Version == c.version {
				if err := c.client.Ready(ctx, c.version); err != nil {
					c.log.Warn("confirm agent update", "err", err)
				}
				c.refresh(ctx)
			}
			if task == nil || task.State != "dispatched" {
				continue
			}
			r := update.Request{ID: task.Id, Version: task.Version, ExpiresAt: task.ExpiresAt}
			if err := r.Validate(time.Now()); err != nil {
				c.log.Warn("reject agent update", "err", err)
				continue
			}
			if !update.Newer(task.Version, c.version) {
				continue
			}
			if _, err := c.client.Submit(ctx, r); err != nil {
				c.log.Warn("submit agent update", "err", err)
			}
			c.refresh(ctx)
		}
	}
}
