package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/agent/collect"
	"github.com/xjetry/heron/internal/agentconfig"
)

// 宿主根契约不成立（含被包装的）是退出码 2，其余错误是 1。
func TestExitCodeSeparatesHostRootContract(t *testing.T) {
	hre := &collect.HostRootError{Dir: "/hostfs", Err: errors.New("x"), Hint: "y"}
	for err, want := range map[error]int{
		hre:                          2,
		fmt.Errorf("start: %w", hre): 2,
		errors.New("load config"):    1,
	} {
		if got := exitCode(err); got != want {
			t.Errorf("exitCode(%v) = %d, want %d", err, got, want)
		}
	}
}

// run --host-root 指向一棵没有挂宿主根的目录：进程以退出码 2 拒绝启动，错误行点名缺的运行参数。
// 非 Linux 平台不接受 --host-root，同样退出码 2。经构建出的二进制跑，钉住 main 的退出码映射与 flag 接线。
func TestRunHostRootRefusesWithExitCode2(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	if err := agentconfig.Save(cfgPath, agentconfig.Config{Hub: "https://hub.invalid", Token: "t", Name: "n"}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tmp, "heron-agent")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, out)
	}
	root := filepath.Join(tmp, "hostfs")
	// 拒绝发生在构造采集器时，远在第一次上报之前；进程还活着说明 --host-root 没有生效，期限到即杀掉。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "run", "--config", cfgPath, "--host-root", root).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("agent was still running after 30s with --host-root %s; output:\n%s", root, out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("exit = %v, want code 2; output:\n%s", err, out)
	}
	want := "docker run -v /:" + root + ":ro"
	if runtime.GOOS != "linux" {
		want = "--host-root is only accepted on linux"
	}
	if !strings.Contains(string(out), "error: --host-root "+root+": ") || !strings.Contains(string(out), want) {
		t.Fatalf("output does not name the dir and %q:\n%s", want, out)
	}
}
