package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/agentconfig"
	"github.com/xjetry/heron/internal/shutdown"
	"github.com/xjetry/heron/internal/shutdown/shutdowntest"
	"github.com/xjetry/heron/internal/testlog"
	"github.com/xjetry/heron/internal/testwait"
)

// scripts/e2e.sh 从 agent 的启动行按整秒读出 request_timeout 与 initial_interval，推出告警恢复的等待上限。
// 这里经 runRun 同一个 newHandler 与 logStarting 产出启动行，按脚本同形的 testlog.WholeSeconds 取值再与常量
// 比较：字段缺失、改名、不再是整秒写法或不跟常量走时 make ci 先红，而不是等到 e2e 才停下。
func TestStartingLineStatesReportTiming(t *testing.T) {
	var out bytes.Buffer
	logStarting(newLogger(&out), "http://hub.example")
	for _, f := range []struct {
		key  string
		want time.Duration
	}{
		{"request_timeout", requestTimeout},
		{"initial_interval", initialInterval},
	} {
		got, ok := testlog.WholeSeconds(out.String(), "agent starting", f.key)
		if !ok || got != f.want {
			t.Errorf("starting line: %s read as %v (ok=%v), want whole seconds equal to %v; log: %s", f.key, got, ok, f.want, out.String())
		}
	}
}

// 子进程执行真实 main，使 os.Exit 与信号处理不会终止测试宿主。
//
// 不并行：子进程里它改进程级的 os.Args 再调用 main。
func TestAgentCommandChild(t *testing.T) {
	if os.Getenv("HERON_AGENT_COMMAND_CHILD") != "1" {
		return
	}
	// 信号用例要求子进程的 SIGINT 处置由用例指定、不随测试二进制的启动方式变。
	if err := shutdowntest.ExecWithInterrupt(); err != nil {
		t.Fatal(err)
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"heron-agent"}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	t.Fatal("missing child argument delimiter")
}

// agent run 的关停信号走 shutdown.Notify：启动方已忽略 SIGINT 时沿用这份忽略，SIGINT 不停止上报，SIGTERM 照常
// 正常退出。关停中 SIGINT 回到原处置那一条由 shutdown 的测试钉住；agent 关停没有可观察的持续阶段，这里只钉调用点。
func TestRunKeepsInheritedInterruptIgnore(t *testing.T) {
	t.Parallel()
	hub := &smokeHub{}
	mux := http.NewServeMux()
	mux.Handle(heronv1connect.NewAgentServiceHandler(hub))
	hubSrv := httptest.NewServer(mux)
	defer hubSrv.Close()
	reports := func() int { hub.mu.Lock(); defer hub.mu.Unlock(); return len(hub.reports) }

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := agentconfig.Save(cfgPath, agentconfig.Config{Hub: hubSrv.URL, Token: "smoke-token", Name: "signal"}); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestAgentCommandChild$", "--", "run", "--config", cfgPath)
	cmd.Env = append(os.Environ(), "HERON_AGENT_COMMAND_CHILD=1", shutdowntest.InterruptEnv+"="+shutdowntest.InterruptIgnored)
	var output safeBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	var result error
	go func() { result = cmd.Wait(); close(finished) }()
	// 失败也必须停止上报：不能让这个 agent 继续向别的用例的 hub 发包。
	defer func() { _ = cmd.Process.Kill(); <-finished }()

	// 第一次上报晚于 runRun 订阅信号，之后发的 SIGINT 一定落在订阅之后。
	testwait.Until(t, 10*time.Millisecond, func() bool {
		select {
		case <-finished:
			t.Fatalf("agent exited before reporting: %v %s", result, output.String())
		default:
		}
		return reports() > 0
	}, "agent never reported: %s", testwait.When(output.String))
	if !strings.Contains(output.String(), shutdown.IgnoredInterruptNotice) {
		t.Fatalf("agent did not report the inherited SIGINT ignore: %s", output.String())
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	// SIGINT 若被订阅，agent 取消 ctx 后正常退出。负向窗口之后还要等到一次新的上报，等待期间进程退出同样判失败，
	// 所以退出得比窗口晚的缺陷也在那里被抓住；窗口让 after 取在 SIGINT 有机会起效之后，信号发出时已在途的上报不被
	// 算作"SIGINT 之后仍在上报"。
	select {
	case <-finished:
		t.Fatalf("SIGINT ended agent run despite the inherited ignore: %v %s", result, output.String())
	case <-time.After(150 * time.Millisecond):
	}
	after := reports()
	testwait.Until(t, 10*time.Millisecond, func() bool {
		select {
		case <-finished:
			t.Fatalf("agent exited after SIGINT despite the inherited ignore: %v %s", result, output.String())
		default:
		}
		return reports() > after
	}, "agent stopped reporting after SIGINT: %s", testwait.When(output.String))
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
		if result != nil {
			t.Fatalf("agent did not exit cleanly on SIGTERM with SIGINT ignored: %v %s", result, output.String())
		}
	case <-time.After(testwait.Bound):
		t.Fatal("agent ignored SIGTERM with SIGINT ignored")
	}
}
