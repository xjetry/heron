package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// linuxBrokenRelease 发布一个"起不来"的 agent 版本：包里的 heron-agent 带 heron-test-broken 标记，
// linuxStubs 的 systemctl start 见到它就返回 0 却不建进程，回滚用例因此走到启动确认失败的分支。
// 发布形状与 linuxRelease 相同，只换掉二进制内容，其余（单元、OpenRC 脚本、更新器包）保持一致。
func (e *env) linuxBrokenRelease(arch, version string) {
	e.t.Helper()
	e.updaterPackage(e.dist, arch, version)
	unit, err := os.ReadFile("systemd/heron-agent.service")
	if err != nil {
		e.t.Fatal(err)
	}
	openrc, err := os.ReadFile("openrc/heron-agent")
	if err != nil {
		e.t.Fatal(err)
	}
	e.pack(e.dist, "heron-agent_linux_"+arch+".tar.gz", []packFile{
		{"heron-agent", fakeAgent + "# " + version + " " + arch + "\n# heron-test-broken\n", 0o755},
		{"heron-agent.service", string(unit), 0o644},
		{"heron-agent.openrc", string(openrc), 0o755},
	})
	e.publish(version)
}

// hubBrokenRelease 与 hubRelease 同形，发布一个带 heron-test-broken 标记的 hub；hubStubs 的 start
// 在退出前把三个库文件改写成新内容（模拟候选 hub 打开库时的 schema 迁移），再返回 0 却不建进程。
func (e *env) hubBrokenRelease(version string) {
	e.t.Helper()
	e.updaterPackage(e.dist, "amd64", version)
	unit, err := os.ReadFile("systemd/heron-hub.service")
	if err != nil {
		e.t.Fatal(err)
	}
	e.pack(e.dist, "heron-hub_linux_amd64.tar.gz", []packFile{
		{"heron-hub", "#!/bin/sh\n# " + version + " amd64\n# heron-test-broken\n", 0o755},
		{"heron-hub.service", string(unit), 0o644},
	})
	e.publish(version)
}

// 升级到起不来的新 agent：安装器把上一版换回来并按 systemd 重启，stderr 说明已回滚，服务重新在跑，
// .bak 随成功回滚消失（首次安装没有旧版本，另有用例）。
func TestLinuxRollsBackWhenTheNewAgentDoesNotStart(t *testing.T) {
	t.Parallel()
	e := newLinuxInstalled(t)
	e.linuxBrokenRelease("amd64", "v2")
	out, code := e.linuxInstall()
	if code == 0 || !strings.Contains(out, "heron-agent did not start") || !strings.Contains(out, "rolling back to the previous version") ||
		!strings.Contains(out, "restored the previous binary") || !strings.Contains(out, "the previous heron-agent is running again") {
		t.Fatalf("upgrade to a binary that does not start must roll back: exit %d:\n%s", code, out)
	}
	if got := e.file("usr/local/bin/heron-agent"); !strings.Contains(got, "# v1 amd64") {
		t.Fatalf("the previous binary must be back, got %q", got)
	}
	if e.exists("usr/local/bin/heron-agent.bak") {
		t.Fatal("the .bak must not survive a successful rollback")
	}
	if !e.exists("proc/4242/status") {
		t.Fatal("the previous agent must be running again")
	}
}

// 停服务之后不只是新二进制起不来：之后任何一步失败（daemon-reload、enable），或安装器被 SIGTERM 打断（SSH 断开），
// 都要把上一版换回来并重新跑起来，.bak 随成功回滚消失。
func TestLinuxRollsBackWhenAnyStepAfterStoppingFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, stub string }{
		{"daemon-reload fails", "STUB_SYSTEMCTL_FAILS=daemon-reload"},
		{"enable fails", "STUB_SYSTEMCTL_FAILS=enable heron-agent"},
		{"terminated", "STUB_SYSTEMCTL_TERM=daemon-reload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newLinuxInstalled(t)
			e.linuxRelease("amd64", "v2")
			e.vars = []string{tc.stub}
			e.resetCalls()
			out, code := e.linuxInstall()
			if code == 0 || !strings.Contains(out, "rolling back to the previous version") || !strings.Contains(out, "the previous heron-agent is running again") {
				t.Fatalf("a failure after stopping must roll back: exit %d:\n%s", code, out)
			}
			if index(e.calls(), "systemctl stop heron-agent") < 0 {
				t.Fatalf("the failure must come after the service was stopped, calls %q", e.calls())
			}
			if got := e.file("usr/local/bin/heron-agent"); !strings.Contains(got, "# v1 amd64") {
				t.Fatalf("the previous binary must be back, got %q", got)
			}
			if e.exists("usr/local/bin/heron-agent.bak") || !e.exists("proc/4242/status") {
				t.Fatalf("the previous agent must be running again without a leftover .bak:\n%s", out)
			}
		})
	}
}

// 更新器在 agent 确认运行之后才启动：它起不来时安装失败，但已经确认在跑的新 agent 不回滚。
func TestLinuxUpdaterStartFailureKeepsTheNewAgent(t *testing.T) {
	t.Parallel()
	e := newLinuxInstalled(t)
	e.linuxRelease("amd64", "v2")
	e.vars = []string{"STUB_SYSTEMCTL_FAILS=start heron-updater-agent"}
	out, code := e.linuxInstall()
	if code == 0 || strings.Contains(out, "rolling back") {
		t.Fatalf("an updater that does not start must fail without rolling back the agent: exit %d:\n%s", code, out)
	}
	if got := e.file("usr/local/bin/heron-agent"); !strings.Contains(got, "# v2 amd64") || e.exists("usr/local/bin/heron-agent.bak") || !e.exists("proc/4242/status") {
		t.Fatalf("the new agent must stay installed and running, binary %q:\n%s", got, out)
	}
}

// 首次安装没有旧二进制：新二进制起不来、或停服务之后某一步失败时，没有可回滚的版本，安装器只报失败，
// 不伪造一条回滚消息、也不留下 .bak。
func TestLinuxFirstInstallHasNothingToRollBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		release func(e *env)
		vars    []string
	}{
		{"broken binary", func(e *env) { e.linuxBrokenRelease("amd64", "v1") }, nil},
		{"daemon-reload fails", func(e *env) { e.linuxRelease("amd64", "v1") }, []string{"STUB_SYSTEMCTL_FAILS=daemon-reload"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newLinuxHost(t)
			e.appendTo("etc/passwd", "heron-agent:x:480:480::/nonexistent:/usr/sbin/nologin\n")
			e.appendTo("etc/group", "heron-agent:x:480:\n")
			tc.release(e)
			e.vars = tc.vars
			out, code := e.linuxInstall()
			if code == 0 {
				t.Fatalf("the first install must fail:\n%s", out)
			}
			if strings.Contains(out, "rolling back") || strings.Contains(out, "restored the previous binary") || e.exists("usr/local/bin/heron-agent.bak") {
				t.Fatalf("a first install has nothing to roll back:\n%s", out)
			}
		})
	}
}

// 升级到起不来的新 hub：旧二进制与三个库文件一起换回来（候选 hub 启动时已迁移 schema，只换二进制会把 hub
// 留在旧程序打不开新库的状态），并按 systemd 重启；服务重新在跑，.bak 随成功回滚消失。
func TestHubRollsBackBinaryAndDatabaseWhenTheNewHubDoesNotStart(t *testing.T) {
	t.Parallel()
	e := newHubInstalled(t)
	for rel, body := range map[string]string{
		"heron.db":     "old-schema\n",
		"heron.db-wal": "old-wal\n",
		"heron.db-shm": "old-shm\n",
	} {
		e.put(hubData+"/"+rel, body)
	}
	e.hubBrokenRelease("v2")
	out, code := e.hubInstall()
	if code == 0 || !strings.Contains(out, "heron-hub did not start") || !strings.Contains(out, "restored the previous binary") ||
		!strings.Contains(out, "restored the previous database") || !strings.Contains(out, "the previous heron-hub is running again") {
		t.Fatalf("upgrade to a binary that does not start must roll back: exit %d:\n%s", code, out)
	}
	if got := e.file("usr/local/bin/heron-hub"); !strings.Contains(got, "# v1 amd64") {
		t.Fatalf("the previous binary must be back, got %q", got)
	}
	for rel, want := range map[string]string{
		"heron.db":     "old-schema\n",
		"heron.db-wal": "old-wal\n",
		"heron.db-shm": "old-shm\n",
	} {
		if got := e.file(hubData + "/" + rel); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	for _, rel := range []string{"usr/local/bin/heron-hub.bak", hubData + "/heron.db.bak", hubData + "/heron.db-wal.bak", hubData + "/heron.db-shm.bak"} {
		if e.exists(rel) {
			t.Errorf("%s must not survive a successful rollback", rel)
		}
	}
	if !e.exists("proc/4242/status") {
		t.Fatal("the previous hub must be running again")
	}
}

// 候选 hub 新建的 WAL/SHM 在备份时并不存在：回滚删掉它们，别把新 schema 的页留给旧库；旧库本身照旧换回。
func TestHubRollbackRemovesWalFilesTheCandidateCreated(t *testing.T) {
	t.Parallel()
	e := newHubInstalled(t)
	e.put(hubData+"/heron.db", "old-schema\n")
	e.hubBrokenRelease("v2")
	out, code := e.hubInstall()
	if code == 0 || !strings.Contains(out, "restored the previous database") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := e.file(hubData + "/heron.db"); got != "old-schema\n" {
		t.Fatalf("heron.db = %q, want the old schema", got)
	}
	for _, rel := range []string{"heron.db-wal", "heron.db-shm"} {
		if e.exists(hubData + "/" + rel) {
			t.Errorf("%s was created by the candidate hub and must not outlive the rollback", rel)
		}
	}
}

// 停服务之后的机械性失败（enable 失败、备份库文件时磁盘写满、安装器被 SIGTERM 打断）都要换回旧二进制并重新启动旧
// hub，不补"hub 已停"那句。备份没做完时候选 hub 从没启动过、库原样未动：不能拿只复制了一半的 .bak 覆盖它，也不能把
// 还没来得及备份的 -shm 当成候选 hub 新建的删掉；停服务后收紧成 0750 的数据目录要放回 0770，旧 hub 才建得了 WAL/SHM。
func TestHubRollsBackWhenAStepAfterStoppingFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, stub string }{
		{"enable fails", "STUB_SYSTEMCTL_FAILS=enable heron-hub"},
		{"backup runs out of space", "STUB_CP_FAILS=heron.db-wal.bak"},
		{"terminated", "STUB_SYSTEMCTL_TERM=enable heron-hub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newHubInstalled(t)
			db := map[string]string{"heron.db": "old-schema\n", "heron.db-wal": "old-wal\n", "heron.db-shm": "old-shm\n"}
			for rel, body := range db {
				e.put(hubData+"/"+rel, body)
			}
			e.vars = []string{tc.stub}
			out, code := e.hubInstall()
			if code == 0 || !strings.Contains(out, "rolling back to the previous version") || !strings.Contains(out, "the previous heron-hub is running again") ||
				strings.Contains(out, "heron-hub is stopped") {
				t.Fatalf("a failure after stopping must roll back and restart the previous hub: exit %d:\n%s", code, out)
			}
			if got := e.file("usr/local/bin/heron-hub"); !strings.Contains(got, "# v1 amd64") {
				t.Fatalf("the previous binary must be back, got %q", got)
			}
			for rel, want := range db {
				if got := e.file(hubData + "/" + rel); got != want {
					t.Errorf("%s = %q, want %q", rel, got, want)
				}
			}
			for _, rel := range []string{"usr/local/bin/heron-hub.bak", hubData + "/heron.db.bak", hubData + "/heron.db-wal.bak", hubData + "/heron.db-shm.bak"} {
				if e.exists(rel) {
					t.Errorf("%s must not survive a successful rollback", rel)
				}
			}
			e.mode(hubData, 0o770)
			if !e.exists("proc/4242/status") {
				t.Fatal("the previous hub must be running again")
			}
		})
	}
}

// darwinBrokenRelease 与 release 同形，发布一个带 heron-test-broken 标记的 macOS agent：launchctl 替身的 bootstrap
// 见到它就返回 0 却不起进程。
func (e *env) darwinBrokenRelease(arch, version string) {
	e.t.Helper()
	plist, err := os.ReadFile("launchd/xyz.heron.agent.plist")
	if err != nil {
		e.t.Fatal(err)
	}
	e.pack(e.dist, "heron-agent_darwin_"+arch+".tar.gz", []packFile{
		{"heron-agent", fakeAgent + "# " + version + " " + arch + "\n# heron-test-broken\n", 0o755},
		{"xyz.heron.agent.plist", string(plist), 0o644},
	})
	e.publish(version)
}

// macOS 与 Linux 同一套回滚：新二进制起不来、停服务之后某一步失败（bootstrap 一次性报 EIO）、安装器被 SIGTERM 打断，
// 都换回上一版并重新载入，.bak 随成功回滚消失。
func TestMacOSRollsBackWhenTheUpgradeFailsAfterStopping(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		broken bool
		vars   func(e *env) []string
	}{
		{"new binary does not start", true, func(*env) []string { return nil }},
		{"bootstrap fails once", false, func(e *env) []string {
			return []string{"STUB_LAUNCHCTL_FAILS_ONCE=bootstrap system " + e.root + "/Library/LaunchDaemons/xyz.heron.agent.plist"}
		}},
		{"terminated", false, func(*env) []string { return []string{"STUB_LAUNCHCTL_TERM_ONCE=enable system/xyz.heron.agent"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			if out, code := e.install(); code != 0 {
				t.Fatalf("first install exit %d:\n%s", code, out)
			}
			if tc.broken {
				e.darwinBrokenRelease("arm64", "v2")
			} else {
				e.release("arm64", "v2")
			}
			e.vars = tc.vars(e)
			out, code := e.install()
			if code == 0 || !strings.Contains(out, "rolling back to the previous version") || !strings.Contains(out, "restored the previous binary") ||
				!strings.Contains(out, "the previous heron-agent is running again") {
				t.Fatalf("a failed upgrade must roll back: exit %d:\n%s", code, out)
			}
			if got := e.file("usr/local/bin/heron-agent"); !strings.Contains(got, "# v1 arm64") || e.exists("usr/local/bin/heron-agent.bak") {
				t.Fatalf("the previous binary must be back without a .bak, got %q", got)
			}
			if procs, err := os.ReadFile(filepath.Join(e.state, "procs")); err != nil || !strings.Contains(string(procs), " /usr/local/bin/heron-agent") {
				t.Fatalf("the previous agent must be running again: %q %v", procs, err)
			}
		})
	}
}

// macOS 首次安装没有旧二进制：起不来时没有可回滚的版本，不伪造回滚消息、不留下 .bak。
func TestMacOSFirstInstallHasNothingToRollBack(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.darwinBrokenRelease("arm64", "v1")
	out, code := e.install()
	if code == 0 || strings.Contains(out, "rolling back") || e.exists("usr/local/bin/heron-agent.bak") {
		t.Fatalf("a first install has nothing to roll back: exit %d:\n%s", code, out)
	}
}
