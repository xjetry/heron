package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKind(t *testing.T) {
	for _, c := range []struct{ version, agent, want, errHas string }{
		{"v1.2.3", "v1.2.3", "full", ""},
		{"v1.3.0-rc.1", "v1.3.0-rc.1", "full", ""},
		{"v1.2.4", "v1.2.3", "hub-only", ""},
		{"v1.3.0-rc.1", "v1.2.3", "hub-only", ""},
		{"v2.0.0", "v1.9.9", "hub-only", ""},
		{"v1.2.3", "v1.2.4", "", "not lower than VERSION"},
		{"v1.3.0-rc.1", "v1.3.0", "", "not lower than VERSION"},
		{"v1.2.4", "v1.2.3-rc.1", "", "prerelease"},
		{"v1.2.4", "", "", "AGENT_VERSION"},
		{"v1.2.4", "dev", "", "AGENT_VERSION"},
		{"dev", "v1.2.3", "", "VERSION"},
	} {
		got, err := kind(c.version, c.agent)
		if c.errHas == "" && (err != nil || got != c.want) {
			t.Errorf("kind(%q, %q) = %q, %v; want %q", c.version, c.agent, got, err, c.want)
		}
		if c.errHas != "" && (err == nil || !strings.Contains(err.Error(), c.errHas)) {
			t.Errorf("kind(%q, %q) = %q, %v; want error containing %q", c.version, c.agent, got, err, c.errHas)
		}
	}
}

// 本地验收构建只给 VERSION 时，判定必须失败并说出该怎么给，而不是静默产出只发 hub 的包或绑错版本。
func TestKindRejectsLocalBuildWithoutAgentVersion(t *testing.T) {
	_, err := kind("v0.0.0-check", "v0.5.3")
	if err == nil || !strings.Contains(err.Error(), "AGENT_VERSION=$VERSION") {
		t.Fatalf("kind(v0.0.0-check, v0.5.3) = %v, want a hint to pass AGENT_VERSION=$VERSION", err)
	}
}

// AGENT_VERSION 文件按原始字节核对：Makefile 读它时只取第一行并去掉空白，只核对读出来的值会放过多余的行。
func TestRunCheckFileValidatesRawBytes(t *testing.T) {
	for _, c := range []struct {
		content string
		code    int
	}{
		{"v0.5.3\n", 0},
		{"v0.5.4-rc.1\n", 0},
		{"v0.5.3", 1},
		{"v0.5.3\nextra\n", 1},
		{"v0.5.3\n\n", 1},
		{" v0.5.3\n", 1},
		{"v0.5.3 \n", 1},
		{"v0.5.3\r\n", 1},
		{"\n", 1},
		{"v0.5\n", 1},
	} {
		path := filepath.Join(t.TempDir(), "AGENT_VERSION")
		if err := os.WriteFile(path, []byte(c.content), 0o644); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		if code := run([]string{"-check-file", path}, &out, &errb); code != c.code {
			t.Errorf("check-file %q: code %d (err %q), want %d", c.content, code, errb.String(), c.code)
		}
	}
	var out, errb bytes.Buffer
	if code := run([]string{"-check-file", filepath.Join(t.TempDir(), "missing")}, &out, &errb); code != 1 {
		t.Errorf("missing file: code %d, want 1", code)
	}
}

func TestRunPrintsKindAndRejectsBadFlags(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"-version", "v1.2.4", "-agent", "v1.2.3"}, &out, &errb); code != 0 || out.String() != "hub-only\n" {
		t.Fatalf("kind run: code %d out %q", code, out.String())
	}
	if code := run([]string{"-bogus"}, &out, &errb); code != 2 {
		t.Fatalf("bad flag: code %d, want 2", code)
	}
}
