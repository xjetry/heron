package update

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestCommandOutputReturnsStdout(t *testing.T) {
	out, err := commandOutput(exec.CommandContext(context.Background(), "sh", "-c", "echo v1.2.3; echo noise >&2"), "heron-agent")
	if err != nil || out != "v1.2.3" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

// 失败原因经 Engine.reason 成为 UpdateStatus.reason；只剩 "exit status 3" 时面板上无从排查。
func TestCommandOutputFailureCarriesStderr(t *testing.T) {
	_, err := commandOutput(exec.CommandContext(context.Background(), "sh", "-c", "echo partial; echo 'cannot read config' >&2; exit 3"), "heron-agent")
	if err == nil {
		t.Fatal("want error")
	}
	if got, want := err.Error(), "heron-agent: exit status 3: cannot read config"; got != want {
		t.Fatalf("err = %q, want %q", got, want)
	}
}

func TestCommandOutputFailureWithoutStderr(t *testing.T) {
	_, err := commandOutput(exec.CommandContext(context.Background(), "sh", "-c", "exit 4"), "heron-agent")
	if err == nil || err.Error() != "heron-agent: exit status 4" {
		t.Fatalf("err = %v", err)
	}
}

func TestCommandOutputBoundsStderr(t *testing.T) {
	_, err := commandOutput(exec.CommandContext(context.Background(), "sh", "-c", "head -c 4096 /dev/zero | tr '\\0' Q >&2; exit 1"), "heron-agent")
	if err == nil {
		t.Fatal("want error")
	}
	if n := strings.Count(err.Error(), "Q"); n != 512 {
		t.Fatalf("stderr kept %d runes, want 512", n)
	}
}
