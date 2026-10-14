package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 在线更新取产物来源（--update-source）落在 root 拥有的独立配置里，与 agent 业务配置（/etc/heron-agent）分开：
// 来源是安装时的 root 决定，不给服务用户可写的面。这里的用例只覆盖 install.sh 对该文件的生命周期。
const sourceFile = "etc/heron-update-agent/config.json"

func (e *env) installWithSource(source string) (string, int) {
	return e.run("--hub", "http://hub.test", "--key", "k", "--base-url", "file://"+e.dist, "--update-source", source)
}

// chmodExec 把假根目录下的文件标记为可执行：INIT 判定按 -x 找 openrc-run，替身之外的标记文件只需要这一个。
func (e *env) chmodExec(rel string) {
	e.t.Helper()
	if err := os.Chmod(filepath.Join(e.root, rel), 0o755); err != nil {
		e.t.Fatal(err)
	}
}

func TestUpdateSourceWrittenOnInstall(t *testing.T) {
	t.Parallel()
	e := newLinuxHost(t)
	e.appendTo("etc/passwd", "heron-agent:x:480:480::/nonexistent:/usr/sbin/nologin\n")
	e.appendTo("etc/group", "heron-agent:x:480:\n")
	e.linuxRelease("amd64", "v1")
	out, code := e.installWithSource("hub")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := e.file(sourceFile); got != "{\"source\":\"hub\"}\n" {
		t.Fatalf("source config = %q", got)
	}
	e.mode(sourceFile, 0o644)
	e.mode("etc/heron-update-agent", 0o755)
}

func TestUpdateSourceAbsentByDefault(t *testing.T) {
	t.Parallel()
	e := newLinuxInstalled(t)
	if e.exists(sourceFile) {
		t.Fatal("an install without --update-source wrote a source config")
	}
}

// 不给 --update-source 的重跑不动已有文件：hub 来源的节点不能被例行升级悄悄切回 github，
// 否则它下一次在线更新必然失败（出站只通到 hub）。显式给值则覆盖。
func TestUpdateSourceKeptOnRerun(t *testing.T) {
	t.Parallel()
	e := newLinuxInstalled(t)
	if out, code := e.installWithSource("hub"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if out, code := e.linuxInstall(); code != 0 {
		t.Fatalf("rerun exit %d:\n%s", code, out)
	}
	if got := e.file(sourceFile); got != "{\"source\":\"hub\"}\n" {
		t.Fatalf("a rerun without --update-source changed the source: %q", got)
	}
	if out, code := e.installWithSource("github"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := e.file(sourceFile); got != "{\"source\":\"github\"}\n" {
		t.Fatalf("explicit switch not applied: %q", got)
	}
}

func TestUpdateSourceRejectsUnknownValue(t *testing.T) {
	t.Parallel()
	e := newLinuxInstalled(t)
	e.resetCalls()
	out, code := e.installWithSource("ftp")
	if code != 2 || !strings.Contains(out, "--update-source must be github or hub") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	assertNothingDownloaded(t, e, out)
}

// OpenRC 主机没有在线更新（靠重跑安装器升级），来源参数在那里没有读者；拒绝必须发生在任何下载之前，
// 否则一次注定回滚的下载已经消耗了出网机会。夹具发布 v1 是为了让脚本越过"源码脚本无清单"的更早退出，
// 缺了它拒绝位置的后移会被误当成通过。
func TestUpdateSourceRefusedOnOpenRC(t *testing.T) {
	t.Parallel()
	e := newStubEnv(t, "install.sh", linuxStubs)
	e.put("sbin/openrc-run", "#!/bin/sh\n")
	e.chmodExec("sbin/openrc-run")
	e.put("proc/self/status", "Uid:\t1000\t1000\t1000\t1000\n")
	e.linuxRelease("amd64", "v1")
	out, code := e.installWithSource("hub")
	if code != 2 || !strings.Contains(out, "--update-source applies only to systemd") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	assertNothingDownloaded(t, e, out)
}

func TestUpdateSourceWithUninstallIsUsageError(t *testing.T) {
	t.Parallel()
	e := newLinuxInstalled(t)
	if out, code := e.run("--uninstall", "--update-source", "hub"); code != 2 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// 普通卸载保留来源配置（重装/升级沿用原来源）；--purge 与业务配置一起删。
func TestUpdateSourcePurgeScope(t *testing.T) {
	t.Parallel()
	e := newLinuxInstalled(t)
	if out, code := e.installWithSource("hub"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if out, code := e.run("--uninstall"); code != 0 || !e.exists(sourceFile) {
		t.Fatalf("plain uninstall must keep the source config: exit %d\n%s", code, out)
	}
	if out, code := e.run("--uninstall", "--purge"); code != 0 || e.exists("etc/heron-update-agent") {
		t.Fatalf("purge must remove /etc/heron-update-agent: exit %d\n%s", code, out)
	}
}
