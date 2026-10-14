package testwait

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

// PauseAtLog 返回一个 slog.Handler：消息等于 msg 的第一条记录在 Handle 里阻塞到 release，
// 阻塞开始时关闭 entered；之后同消息的记录不再阻塞。全部记录（含暂停的那条，放行后）按 next
// 自己的级别过滤后交给它。release 可重复调用，便于失败路径清理。
//
// 阻塞发生在发出这条日志的协程里、调用 Logger 的那条语句上，所以暂停点就是被测代码里这条日志
// 语句的位置，暂停期间该协程持有的正是那条语句处持有的锁、门与连接，与 context 的读取次序和
// 存储层实现无关。日志语句挪动，暂停点跟着挪：用例要断言暂停点处确实持有它要测的东西。
func PauseAtLog(next slog.Handler, msg string) (h slog.Handler, entered <-chan struct{}, release func()) {
	p := &logPause{msg: msg, entered: make(chan struct{}), release: make(chan struct{})}
	p.armed.Store(true)
	return &pauseHandler{next: next, p: p}, p.entered, sync.OnceFunc(func() { close(p.release) })
}

type logPause struct {
	msg     string
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

type pauseHandler struct {
	next slog.Handler
	p    *logPause
}

// Enabled 对任何级别都放行，暂停不受 next 的级别过滤影响；过滤在 Handle 里按 next 做。
func (h *pauseHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *pauseHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.p.msg && h.p.armed.CompareAndSwap(true, false) {
		close(h.p.entered)
		<-h.p.release
	}
	if !h.next.Enabled(ctx, r.Level) {
		return nil
	}
	return h.next.Handle(ctx, r)
}

func (h *pauseHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &pauseHandler{next: h.next.WithAttrs(attrs), p: h.p}
}

func (h *pauseHandler) WithGroup(name string) slog.Handler {
	return &pauseHandler{next: h.next.WithGroup(name), p: h.p}
}
