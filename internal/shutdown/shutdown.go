// Package shutdown 是 hub、agent、updater 三个长驻入口共用的关停信号订阅。
//
// 三条不变式由 Notify 一处保证，入口只调它、不各自订阅信号：
//
//   - 关停一旦开始（第一个 SIGINT 或 SIGTERM），SIGINT 立即回到进程启动时的原处置（交互启动时为默认终止）：
//     关停中的 Ctrl-C 是人工强退入口。Notify 的协程在 cancel 之前 signal.Stop SIGINT 的通道，而 ctx 只由这个协程
//     或 stop 取消，所以调用方看到 ctx.Done 时 SIGINT 已不再被订阅。
//   - SIGTERM 订阅到调用方 stop：监管方重复 SIGTERM 只落进无人再读的缓冲（满了 signal.Notify 不阻塞、直接丢弃），
//     不会以默认处置杀掉仍在排空、落盘的进程。
//   - 启动时 SIGINT 已被忽略（如无作业控制的 shell 以 & 启动的后台作业）就不订阅它，沿用这份忽略：订阅会覆盖它，
//     让第一次 SIGINT 触发关停、Stop 后又恢复为忽略，同一个信号前后语义不一。这个事实回给调用方，由它按自己的
//     日志出口记 IgnoredInterruptNotice。
//
// signal.Stop 按通道恢复原处置，所以两类信号必须订阅在不同通道上。
package shutdown

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// IgnoredInterruptNotice 是 Notify 报告 SIGINT 沿用启动方的忽略时，调用方记的那一行日志。
const IgnoredInterruptNotice = "SIGINT ignored by the starting process; only SIGTERM shuts down"

// Notify 订阅关停信号。ctx 在第一个 SIGINT 或 SIGTERM 到来时取消；stop 释放全部订阅并取消 ctx，可重复调用，
// 调用方在退出前 defer 它。interruptIgnored 为真表示 SIGINT 在启动时已被忽略、没有订阅，只有 SIGTERM 能触发关停。
func Notify() (ctx context.Context, stop func(), interruptIgnored bool) {
	terminate := make(chan os.Signal, 1)
	signal.Notify(terminate, syscall.SIGTERM)
	// interrupt 为 nil 时下面的 select 永不选中它。
	var interrupt chan os.Signal
	interruptIgnored = signal.Ignored(syscall.SIGINT)
	if !interruptIgnored {
		interrupt = make(chan os.Signal, 1)
		signal.Notify(interrupt, syscall.SIGINT)
	}
	ctx, cancel := context.WithCancel(context.Background())
	released := make(chan struct{})
	go func() {
		select {
		case <-interrupt:
		case <-terminate:
		case <-released:
			return
		}
		if interrupt != nil {
			signal.Stop(interrupt)
		}
		cancel()
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			close(released)
			if interrupt != nil {
				signal.Stop(interrupt)
			}
			signal.Stop(terminate)
			cancel()
		})
	}
	return ctx, stop, interruptIgnored
}
