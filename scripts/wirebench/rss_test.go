package main

import (
	"bytes"
	"errors"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer 让测试在 rss.sh 还在写的时候读它的输出。
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// rss.sh 只采调用方按身份给出的 pid：宿主的 /proc 也列出各容器里的进程，measure-agent.sh 让各组同时运行，
// 与被测进程同名的别组进程不能出现在输出里。给出的进程消失（已回收）或成了僵尸（已退出、未回收）都算退出，
// 各写一行注释；全部退出后 rss.sh 自己结束。
func TestRSSSamplesOnlyGivenPIDs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rss.sh reads /proc")
	}
	start := func() *exec.Cmd {
		c := exec.Command("sleep", "120")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
		return c
	}
	// 三个进程同名（comm 都是 sleep），只把 reaped 与 zombie 交给 rss.sh。
	reaped, zombie, other := start(), start(), start()
	pid := func(c *exec.Cmd) string { return strconv.Itoa(c.Process.Pid) }

	var out lockedBuffer
	rss := exec.Command("sh", "rss.sh", "1", pid(reaped), pid(zombie))
	rss.Stdout, rss.Stderr = &out, &out
	if err := rss.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- rss.Wait() }()
	t.Cleanup(func() { _ = rss.Process.Kill() })

	waitOutput := func(what string, ok func(string) bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok(out.String()) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: rss.sh output so far:\n%s", what, out.String())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	sampled := func(s, p string) bool {
		for _, line := range strings.Split(s, "\n") {
			if f := strings.Fields(line); len(f) == 10 && f[2] == p {
				return true
			}
		}
		return false
	}
	exited := func(s, p string) bool { return strings.Contains(s, " pid "+p+" exited\n") }

	waitOutput("both given pids sampled", func(s string) bool { return sampled(s, pid(reaped)) && sampled(s, pid(zombie)) })
	// 同一轮采样里同名进程若被选中，此刻已经在输出里；结束前再查一次。
	if s := out.String(); sampled(s, pid(other)) {
		t.Fatalf("rss.sh sampled pid %s, which it was not given:\n%s", pid(other), s)
	}
	// 杀掉并回收：/proc/<pid> 消失。
	_ = reaped.Process.Kill()
	_ = reaped.Wait()
	waitOutput("reaped pid reported exited", func(s string) bool { return exited(s, pid(reaped)) })
	// 杀掉但不回收：进程成了僵尸，/proc/<pid>/status 还在，只是没有内存行。
	_ = zombie.Process.Kill()
	waitOutput("zombie pid reported exited", func(s string) bool { return exited(s, pid(zombie)) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("rss.sh exited with %v:\n%s", err, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("rss.sh kept running after every given pid exited:\n%s", out.String())
	}

	s := out.String()
	if sampled(s, pid(other)) {
		t.Fatalf("rss.sh sampled pid %s, which it was not given:\n%s", pid(other), s)
	}
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 10 || (f[2] != pid(reaped) && f[2] != pid(zombie)) || f[1] != "sleep" {
			t.Fatalf("unexpected sample line %q:\n%s", line, s)
		}
		for _, v := range f[3:] {
			if _, err := strconv.Atoi(v); err != nil {
				t.Fatalf("sample line %q has a non-numeric field %q", line, v)
			}
		}
	}
}

// 给不出进程号、或给出的进程不存在时，rss.sh 在采样之前失败，不留下一份空的 rss.txt 冒充"采过了"。
func TestRSSRejectsBadPIDs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rss.sh reads /proc")
	}
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		pid  string
		code int
	}{{"", 2}, {"0", 2}, {"12a", 2}, {strconv.Itoa(gone.Process.Pid), 1}} {
		out, err := exec.Command("sh", "rss.sh", "1", tc.pid).CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != tc.code {
			t.Errorf("rss.sh 1 %q: err=%v, want exit %d; output %q", tc.pid, err, tc.code, out)
		}
		if strings.Contains(string(out), "# time_utc") {
			t.Errorf("rss.sh 1 %q printed the sample header before failing: %q", tc.pid, out)
		}
	}
}
