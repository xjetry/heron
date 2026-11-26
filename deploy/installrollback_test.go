package deploy

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// 退出处理的打断用例共用的替身片段，拼进服务管理替身（systemctl、launchctl）。
//
// stubSignalAfterFailure 放在替身最前面：模拟的那次失败发生之后（stubFailure 留下 $STUB_STATE/failed），每次调用都先给
// 安装脚本（父进程）发 STUB_SIGNAL_AFTER_FAILURE 指定的信号。失败之后的调用都在退出处理里（回滚的停、启与更新器的恢复），
// 信号因而落在回滚进行中：SSH 断开时的挂断、回滚中再按一次 Ctrl-C 都是这个形态。
const stubSignalAfterFailure = `if [ -n "${STUB_SIGNAL_AFTER_FAILURE-}" ] && [ -f "$STUB_STATE/failed" ]; then
  kill -"$STUB_SIGNAL_AFTER_FAILURE" "$PPID"
fi
`

// stubFailure 由模拟失败的那次调用在返回失败之前执行：留下 $STUB_STATE/failed；STUB_CLOSE_OUTPUT 设了时，在 stderr 写
// 一行 closeOutputMarker，再一直写到写不进去为止。用例看到标记就关掉管道的读端（runClosingOutput），此后安装脚本写的
// 就是一条读端已关的管道，与 tee 之类的读端先退出同形。替身自己忽略 SIGPIPE，写失败时结束循环，不被它杀掉。
const stubFailure = `  : > "$STUB_STATE/failed"
  if [ -n "${STUB_CLOSE_OUTPUT-}" ]; then
    trap '' PIPE
    echo ` + closeOutputMarker + ` >&2
    while printf . >&2 2>/dev/null; do :; done
  fi
`

const closeOutputMarker = "heron-test-close-output"

// runClosingOutput 执行脚本，stdout 与 stderr 接同一条管道；读到 closeOutputMarker 就关掉读端。返回关掉之前读到的输出与
// 退出码（被信号杀掉时为 -1）。
func (e *env) runClosingOutput(args ...string) (string, int) {
	e.t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		e.t.Fatal(err)
	}
	cmd := e.command(args...)
	defer cmd.Stdin.(*os.File).Close()
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	w.Close()
	var out []byte
	buf := make([]byte, 4096)
	for !bytes.Contains(out, []byte(closeOutputMarker)) {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			break
		}
	}
	r.Close()
	return string(out), e.exitCode(cmd.Wait())
}

// runUnwritableStderr 执行脚本，stderr 是只读打开的 /dev/null：每一次写 stderr 都失败（EBADF），与 SSH 断开后写终端报
// EIO 是同一种失败——写诊断的 echo 返回非零。返回 stdout 与退出码。
func (e *env) runUnwritableStderr(args ...string) (string, int) {
	e.t.Helper()
	ro, err := os.Open(os.DevNull)
	if err != nil {
		e.t.Fatal(err)
	}
	defer ro.Close()
	cmd := e.command(args...)
	defer cmd.Stdin.(*os.File).Close()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, ro
	return out.String(), e.exitCode(cmd.Run())
}

// availableShells 是本机能跑的 POSIX shell：用例在每一个上各跑一遍，退出处理在 dash、bash、busybox ash 上的信号与 set -e
// 语义各有不同。sh 是系统默认的那个（macOS 上是 bash，Debian 系上是 dash）。至少有 sh，缺的跳过。
func availableShells(t *testing.T) [][]string {
	t.Helper()
	var shells [][]string
	for _, sh := range [][]string{{"sh"}, {"dash"}, {"bash"}, {"busybox", "sh"}} {
		if _, err := exec.LookPath(sh[0]); err != nil {
			continue
		}
		if err := exec.Command(sh[0], append(slices.Clone(sh[1:]), "-c", "true")...).Run(); err != nil {
			continue
		}
		shells = append(shells, sh)
	}
	if len(shells) == 0 {
		t.Fatal("no POSIX shell on PATH")
	}
	return shells
}

// rollbackHost 是一个安装脚本在打断用例里的形态：failAfterStop 是让停服务之后的某一步失败的替身开关，running 判断
// 上一版是否重新在跑，updaterRestart 是恢复更新器那一条调用（没有更新器的平台为空）。
type rollbackHost struct {
	in             installer
	failAfterStop  func(e *env) string
	running        func(e *env) bool
	updaterRestart string
}

var rollbackHosts = []rollbackHost{
	{
		in:             installers[0],
		failAfterStop:  func(*env) string { return "STUB_SYSTEMCTL_FAILS=daemon-reload" },
		running:        func(e *env) bool { return e.exists("proc/4242/status") },
		updaterRestart: "systemctl restart heron-updater-agent",
	},
	{
		in: installers[1],
		failAfterStop: func(e *env) string {
			return "STUB_LAUNCHCTL_FAILS_ONCE=bootstrap system " + e.root + "/Library/LaunchDaemons/xyz.heron.agent.plist"
		},
		running: func(e *env) bool {
			procs, err := os.ReadFile(filepath.Join(e.state, "procs"))
			return err == nil && strings.Contains(string(procs), " /usr/local/bin/heron-agent")
		},
	},
	{
		in:             installers[2],
		failAfterStop:  func(*env) string { return "STUB_SYSTEMCTL_FAILS=enable heron-hub" },
		running:        func(e *env) bool { return e.exists("proc/4242/status") },
		updaterRestart: "systemctl restart heron-updater-hub",
	},
}

// 停服务之后某一步失败，退出处理在回滚途中遇到写不出诊断（stderr 写失败、读端已退出的管道）或再来一个信号（终端挂断、
// Ctrl-C、Ctrl-\、外部 TERM）：回滚都要做完——换回上一版二进制、不留 .bak、上一版重新在跑、维护握手停掉的更新器被拉起。
// 退出处理若仍让 set -e 生效，第一条写 stderr 的 echo 失败就结束脚本；若仍按 exit 1 处理信号、或对 SIGPIPE 不设防，
// 回滚同样停在半路。每个脚本在本机每个能跑的 shell 上各跑一遍。
func TestRollbackCompletesWhenTheExitHandlerIsInterrupted(t *testing.T) {
	t.Parallel()
	type scenario struct {
		name string
		vars []string
		run  func(e *env, args ...string) (string, int)
	}
	scenarios := []scenario{
		{"stderr unwritable", nil, (*env).runUnwritableStderr},
		{"output reader gone", []string{"STUB_CLOSE_OUTPUT=1"}, (*env).runClosingOutput},
	}
	for _, sig := range []string{"HUP", "INT", "QUIT", "TERM"} {
		scenarios = append(scenarios, scenario{"SIG" + sig + " during rollback", []string{"STUB_SIGNAL_AFTER_FAILURE=" + sig}, (*env).run})
	}
	for _, shell := range availableShells(t) {
		for _, h := range rollbackHosts {
			for _, sc := range scenarios {
				t.Run(strings.Join(shell, " ")+"/"+h.in.script+"/"+sc.name, func(t *testing.T) {
					t.Parallel()
					e := h.in.installed(t)
					e.shell = shell
					e.vars = append([]string{h.failAfterStop(e)}, sc.vars...)
					out, code := sc.run(e, append(slices.Clone(h.in.creds), "--base-url", "file://"+e.dist)...)
					if code == 0 {
						t.Fatalf("a failure after stopping must fail the install:\n%s", out)
					}
					if got := e.file(h.in.bin); !strings.Contains(got, "# v1 ") {
						t.Fatalf("the previous binary must be back, got %q; exit %d, calls %q\n%s", got, code, e.calls(), out)
					}
					if e.exists(h.in.bin + ".bak") {
						t.Errorf("the .bak must not survive a completed rollback; calls %q", e.calls())
					}
					if !h.running(e) {
						t.Errorf("the previous version must be running again; calls %q\n%s", e.calls(), out)
					}
					if index(e.calls(), "sleep 3") < 0 {
						t.Errorf("rollback must complete confirm_service_started (sleep 3); calls %q\n%s", e.calls(), out)
					}
					if h.updaterRestart != "" && index(e.calls(), h.updaterRestart) < 0 {
						t.Errorf("the updater stopped for maintenance must be restarted; calls %q", e.calls())
					}
				})
			}
		}
	}
}
