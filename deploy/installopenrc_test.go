package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newOpenRCHost 是一台 OpenRC 主机：有 /sbin/openrc-run，没有 /run/systemd/system；
// running 为 true 时放上 /run/openrc/softlevel（OpenRC 正在运行），为 false 时模拟容器里装了 OpenRC 却没启动。
func newOpenRCHost(t *testing.T, running bool) *env {
	t.Helper()
	e := newStubEnv(t, "install.sh", linuxStubs)
	for rel, body := range map[string]string{
		"sbin/openrc-run":                   "#!/bin/sh\n",
		"usr/local/bin/.keep":               "",
		"etc/init.d/.keep":                  "",
		"etc/ssl/certs/ca-certificates.crt": "",
		"proc/self/status":                  "Uid:\t1000\t1000\t1000\t1000\n",
		"etc/passwd":                        "root:x:0:0:root:/root:/bin/sh\n",
		"etc/group":                         "root:x:0:\n",
	} {
		e.put(rel, body)
	}
	e.chmodExec("sbin/openrc-run")
	if running {
		e.put("run/openrc/softlevel", "default\n")
	}
	return e
}

// OpenRC 正在运行（/run/openrc/softlevel 存在）：安装脚本正常建账户、注册、装入 OpenRC 并启动服务。
func TestOpenRCRunningInstallsAndStarts(t *testing.T) {
	t.Parallel()
	e := newOpenRCHost(t, true)
	e.linuxRelease("amd64", "v1")
	out, code := e.linuxInstall()
	if code != 0 || !strings.Contains(out, "heron-agent installed and started (openrc, amd64, heron-agent_linux_amd64.tar.gz)") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	c := e.calls()
	if index(c, "rc-update add heron-agent default") < 0 || index(c, "rc-service heron-agent start") < 0 {
		t.Fatalf("expected rc-update add and rc-service start, calls %q", c)
	}
	if !e.exists("proc/4242/status") {
		t.Fatal("agent process must be running after OpenRC install")
	}
}

// OpenRC 已安装但未运行（无 /run/openrc/softlevel）：首次安装与升级重跑都在建账户、下载、注册与停服务之前拒绝，
// 提示指向 agent 的 Docker 镜像，不改动宿主文件也不消费注册凭据。
func TestOpenRCNotRunningRefusesInstallBeforeAccountAndRegistration(t *testing.T) {
	t.Parallel()
	t.Run("first install", func(t *testing.T) {
		t.Parallel()
		e := newOpenRCHost(t, false)
		e.linuxRelease("amd64", "v1")
		out, code := e.linuxInstall()
		if code != 1 {
			t.Fatalf("expected exit 1, got %d:\n%s", code, out)
		}
		for _, want := range []string{"/run/openrc/softlevel", "ghcr.io/xjetry/heron-agent", "docs/install.md#docker"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
		c := e.calls()
		for _, forbidden := range []string{"groupadd", "useradd", "curl ", "heron-agent register", "rc-service"} {
			if index(c, forbidden) >= 0 {
				t.Errorf("must refuse before %q, calls %q", forbidden, c)
			}
		}
		if strings.Contains(e.file("etc/passwd"), "heron-agent:") || e.exists("etc/heron-agent/config.json") {
			t.Fatal("nothing should be created when OpenRC is not running")
		}
	})

	t.Run("rerun upgrade", func(t *testing.T) {
		t.Parallel()
		e := newOpenRCHost(t, true)
		e.linuxRelease("amd64", "v1")
		if out, code := e.linuxInstall(); code != 0 {
			t.Fatalf("setup exit %d:\n%s", code, out)
		}
		os.Remove(filepath.Join(e.root, "run/openrc/softlevel"))
		e.linuxRelease("amd64", "v2")
		e.resetCalls()
		out, code := e.linuxInstall()
		if code != 1 || !strings.Contains(out, "/run/openrc/softlevel") {
			t.Fatalf("expected refusal, exit %d:\n%s", code, out)
		}
		assertNothingDownloaded(t, e, out)
		if !strings.Contains(e.file("usr/local/bin/heron-agent"), "# v1 amd64") {
			t.Fatal("existing binary must not be touched")
		}
	})
}

// OpenRC 未运行时卸载照常可用：跳过必然失败的 rc-service stop，仍通过 /proc 扫描确认进程已退出。
// 没有残留进程时清掉 runlevel 链接、init 脚本、二进制与（--purge 时）账户和配置；仍有进程在跑时拒绝卸载。
func TestOpenRCNotRunningUninstallSkipsRCServiceAndChecksProc(t *testing.T) {
	t.Parallel()
	t.Run("no running process uninstalls cleanly", func(t *testing.T) {
		t.Parallel()
		e := newOpenRCHost(t, true)
		e.linuxRelease("amd64", "v1")
		if out, code := e.linuxInstall(); code != 0 {
			t.Fatalf("setup exit %d:\n%s", code, out)
		}
		// 模拟容器环境：OpenRC 未启动，也没有在跑的 agent 进程。
		os.Remove(filepath.Join(e.root, "run/openrc/softlevel"))
		os.RemoveAll(filepath.Join(e.root, "proc/4242"))
		e.resetCalls()
		out, code := e.run("--uninstall", "--purge")
		if code != 0 || !strings.Contains(out, "heron-agent uninstalled") {
			t.Fatalf("uninstall exit %d:\n%s", code, out)
		}
		if index(e.calls(), "rc-service ") >= 0 {
			t.Errorf("must skip rc-service stop when OpenRC is not running, calls %q", e.calls())
		}
		if index(e.calls(), "rc-update del heron-agent default") < 0 {
			t.Errorf("must still remove runlevel link via rc-update del, calls %q", e.calls())
		}
		for _, rel := range []string{"usr/local/bin/heron-agent", "etc/init.d/heron-agent", "etc/runlevels/default/heron-agent", "etc/heron-agent"} {
			if _, err := os.Lstat(filepath.Join(e.root, rel)); err == nil {
				t.Errorf("%s left behind after uninstall", rel)
			}
		}
		if strings.Contains(e.file("etc/passwd"), "heron-agent:") {
			t.Fatal("account left behind after --purge")
		}
	})

	t.Run("stray running process blocks uninstall", func(t *testing.T) {
		t.Parallel()
		e := newOpenRCHost(t, true)
		e.linuxRelease("amd64", "v1")
		if out, code := e.linuxInstall(); code != 0 {
			t.Fatalf("setup exit %d:\n%s", code, out)
		}
		// OpenRC 未启动但 proc/4242 仍在运行：必须经 /proc 查到并拒绝删文件。
		os.Remove(filepath.Join(e.root, "run/openrc/softlevel"))
		e.resetCalls()
		out, code := e.run("--uninstall")
		if code == 0 || !strings.Contains(out, "heron-agent is still running") {
			t.Fatalf("expected uninstall to refuse while process is running, exit %d:\n%s", code, out)
		}
		if index(e.calls(), "rc-service ") >= 0 {
			t.Errorf("must not call rc-service when OpenRC is not running, calls %q", e.calls())
		}
		if !e.exists("usr/local/bin/heron-agent") {
			t.Fatal("binary must not be removed while agent is still running")
		}
	})
}
