package shutdown

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/shutdown/shutdowntest"
	"github.com/xjetry/heron/internal/testwait"
)

// 契约用真实的 SIGINT / SIGTERM 在子进程里钉：关停中的 SIGINT 要回到默认处置，在测试进程里收到就会终止整个
// 测试二进制；启动时的忽略只在进程启动时被运行时记下，进程内无法换成另一种启动方式。子进程的"排空"阻塞在读
// stdin 上，由父进程关闭 stdin 结束，关停期间的长短完全由用例掌握。
func TestNotifyChild(t *testing.T) {
	if os.Getenv("HERON_SHUTDOWN_CHILD") != "1" {
		return
	}
	if err := shutdowntest.ExecWithInterrupt(); err != nil {
		t.Fatal(err)
	}
	ctx, stop, ignored := Notify()
	defer stop()
	fmt.Printf("ready interrupt_ignored=%v\n", ignored)
	<-ctx.Done()
	fmt.Println("shutting down")
	_, _ = io.Copy(io.Discard, os.Stdin)
	fmt.Println("drained")
	stop()
	os.Exit(0)
}

type child struct {
	t        *testing.T
	cmd      *exec.Cmd
	drain    io.Closer
	finished chan struct{}
	result   error

	mu  sync.Mutex
	out bytes.Buffer
}

func (c *child) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.Write(p)
}

func (c *child) output() string { c.mu.Lock(); defer c.mu.Unlock(); return c.out.String() }

// startChild 启动 TestNotifyChild 并等它订阅完毕；interruptIgnored 指定子进程启动时 SIGINT 是否已被忽略。
func startChild(t *testing.T, interruptIgnored bool) *child {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := &child{t: t, cmd: exec.Command(exe, "-test.run=^TestNotifyChild$"), finished: make(chan struct{})}
	c.cmd.Env = append(os.Environ(), "HERON_SHUTDOWN_CHILD=1")
	if interruptIgnored {
		c.cmd.Env = append(c.cmd.Env, shutdowntest.InterruptEnv+"="+shutdowntest.InterruptIgnored)
	}
	c.cmd.Stdout, c.cmd.Stderr = c, c
	if c.drain, err = c.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { c.result = c.cmd.Wait(); close(c.finished) }()
	t.Cleanup(func() { _ = c.cmd.Process.Kill(); <-c.finished })
	want := fmt.Sprintf("ready interrupt_ignored=%v", interruptIgnored)
	c.waitFor("ready interrupt_ignored=")
	if !strings.Contains(c.output(), want) {
		t.Fatalf("child reported %q, want %q", c.output(), want)
	}
	return c
}

func (c *child) signal(sig syscall.Signal) {
	c.t.Helper()
	if err := c.cmd.Process.Signal(sig); err != nil {
		c.t.Fatal(err)
	}
}

func (c *child) waitFor(line string) {
	c.t.Helper()
	testwait.Until(c.t, time.Millisecond, func() bool {
		select {
		case <-c.finished:
			c.t.Fatalf("child exited before %q: %v %s", line, c.result, c.output())
		default:
		}
		return strings.Contains(c.output(), line)
	}, "child never printed %q: %s", line, testwait.When(c.output))
}

// stillRunning 是负向窗口：订阅着该信号的进程在毫秒级内反应。窗口短只会漏掉更晚才反应的缺陷，不会把正确的
// 进程判失败。
func (c *child) stillRunning(after string) {
	c.t.Helper()
	select {
	case <-c.finished:
		c.t.Fatalf("child exited after %s: %v %s", after, c.result, c.output())
	case <-time.After(150 * time.Millisecond):
	}
}

// finishDrain 结束子进程的排空，要求它正常走完关停。
func (c *child) finishDrain() {
	c.t.Helper()
	if err := c.drain.Close(); err != nil {
		c.t.Fatal(err)
	}
	select {
	case <-c.finished:
	case <-time.After(testwait.Bound):
		c.t.Fatalf("child did not exit after drain: %s", c.output())
	}
	if c.result != nil || !strings.Contains(c.output(), "drained") {
		c.t.Fatalf("child did not complete its shutdown: %v %s", c.result, c.output())
	}
}

// 关停一旦开始，无论由哪种信号触发，SIGINT 都按默认处置终止进程：Notify 在取消 ctx 之前退订了它。
func TestInterruptDuringShutdownTerminates(t *testing.T) {
	t.Parallel()
	for _, first := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(first.String(), func(t *testing.T) {
			t.Parallel()
			c := startChild(t, false)
			c.signal(first)
			c.waitFor("shutting down")
			c.signal(syscall.SIGINT)
			select {
			case <-c.finished:
			case <-time.After(testwait.Bound):
				t.Fatalf("SIGINT after %v did not terminate the shutting-down process: %s", first, c.output())
			}
			status, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
				t.Fatalf("SIGINT after %v was not default termination: %v (%v) %s", first, status, c.result, c.output())
			}
		})
	}
}

// 重复 SIGTERM 不绕过关停：SIGTERM 订阅到 stop，进程照常走完排空并正常退出。
func TestRepeatedTerminationKeepsShutdownRunning(t *testing.T) {
	t.Parallel()
	c := startChild(t, false)
	c.signal(syscall.SIGTERM)
	c.waitFor("shutting down")
	c.signal(syscall.SIGTERM)
	c.stillRunning("second SIGTERM")
	c.finishDrain()
}

// 启动时 SIGINT 已被忽略就沿用这份忽略：SIGINT 不触发关停，关停中也不终止进程；SIGTERM 照常触发关停。
func TestInheritedInterruptIgnoreIsKept(t *testing.T) {
	t.Parallel()
	c := startChild(t, true)
	c.signal(syscall.SIGINT)
	c.stillRunning("SIGINT before shutdown")
	if strings.Contains(c.output(), "shutting down") {
		t.Fatalf("SIGINT started shutdown despite the inherited ignore: %s", c.output())
	}
	c.signal(syscall.SIGTERM)
	c.waitFor("shutting down")
	c.signal(syscall.SIGINT)
	c.stillRunning("SIGINT during shutdown")
	c.finishDrain()
}

func TestStopReleasesAndCancels(t *testing.T) {
	ctx, stop, _ := Notify()
	stop()
	stop()
	if ctx.Err() == nil {
		t.Fatal("stop did not cancel ctx")
	}
}
