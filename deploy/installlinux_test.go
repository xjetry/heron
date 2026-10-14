package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// install.sh 的替身。本地账户记录在 $HERON_INSTALL_ROOT/etc/passwd 与 etc/group 里；
// state/remote-passwd 里的账户由 NSS 的非本地源解析得到：id 查得到，本地文件里没有。
// userdel、groupdel 只改本地文件，找不到记录时照 shadow 4.13 实测的报错与退出码（6）失败。
// systemctl 记下参数；start 在假 /proc 里放一个以服务用户运行的进程（uid 与 exe 齐备：脚本按两者认本服务的
// 进程），stop 把它拿走，供脚本的起停确认读取。
// curl 是三个脚本共用的 fileCurl。服务的真实起停由 scripts/install-accept.sh 在容器里验证。
var linuxStubs = map[string]string{
	"flock": `#!/bin/sh
cat > /dev/null
[ -z "${STUB_INSTALLER_BUSY-}" ]
`,
	"id": `#!/bin/sh
S=$STUB_STATE
if [ "$#" = 1 ] && [ "$1" = -u ]; then echo "${STUB_UID:-0}"; exit 0; fi
flag=""; [ "$#" = 2 ] && { flag=$1; shift; }
line=$(grep "^$1:" "$HERON_INSTALL_ROOT/etc/passwd") || line=$(grep "^$1:" "$S/remote-passwd" 2>/dev/null) || {
  echo "id: '$1': no such user" >&2; exit 1; }
uid=$(echo "$line" | cut -d: -f3); gid=$(echo "$line" | cut -d: -f4)
case "$flag" in
  "") echo "uid=$uid($1) gid=$gid";;
  -u) echo "$uid";;
  -g) echo "$gid";;
  -gn) awk -F: -v g="$gid" '$3 == g { print $1; found = 1; exit } END { exit !found }' "$HERON_INSTALL_ROOT/etc/group";;
esac
`,
	// STUB_SYSTEMCTL_FAILS 与 STUB_SYSTEMCTL_TERM 各是一条完整的参数串：参数与前者相同时这次调用失败；与后者相同时
	// 给安装脚本（父进程）发 SIGTERM，模拟 SSH 断开，调用本身照常完成。
	"systemctl": `#!/bin/sh
cat > /dev/null
echo "systemctl $*" >> "$STUB_STATE/calls"
[ "$*" != "${STUB_SYSTEMCTL_FAILS-}" ] || { echo "systemctl $*: failed" >&2; exit 1; }
[ "$*" != "${STUB_SYSTEMCTL_TERM-}" ] || kill -TERM "$PPID"
case "$*" in
  "show heron-updater-agent -p ActiveState --value") echo "${STUB_UPDATER_STATE:-active}"; exit 0;;
  *heron-updater-agent*) exit 0;;
esac
P=$HERON_INSTALL_ROOT/proc
case "$1" in
  start)
    # 包里带这个标记的 heron-agent 模拟"起不来的新二进制"：start 返回 0 但进程不在，
    # 供回滚用例走 confirm_service_started 的失败分支。
    if grep -q 'heron-test-broken' "$HERON_INSTALL_ROOT/usr/local/bin/heron-agent" 2>/dev/null; then exit 0; fi
    uid=$(grep '^heron-agent:' "$HERON_INSTALL_ROOT/etc/passwd" | cut -d: -f3)
    mkdir -p "$P/4242"; printf 'Uid:\t%s\t%s\t%s\t%s\n' "$uid" "$uid" "$uid" "$uid" > "$P/4242/status"
    rm -f "$P/4242/exe"; ln -s "$HERON_INSTALL_ROOT/usr/local/bin/heron-agent" "$P/4242/exe";;
  stop) rm -rf "$P/4242";;
esac
`,
	"uname": `#!/bin/sh
[ "$*" = -m ] || { echo "unexpected uname $*" >&2; exit 64; }
echo x86_64
`,
	// STUB_CHOWN_FAILS 是一个路径：对它 chown 时失败。
	"chown": `#!/bin/sh
echo "chown $*" >> "$STUB_STATE/calls"
if [ -n "${STUB_CHOWN_FAILS-}" ] && [ "$2" = "$STUB_CHOWN_FAILS" ]; then
  echo "chown: $2: Operation not permitted" >&2; exit 1
fi
`,
	"userdel": `#!/bin/sh
cat > /dev/null
echo "userdel $*" >> "$STUB_STATE/calls"
f=$HERON_INSTALL_ROOT/etc/passwd
grep -q "^$1:" "$f" || { echo "userdel: user '$1' does not exist" >&2; exit 6; }
grep -v "^$1:" "$f" > "$f.new"; mv "$f.new" "$f"
`,
	"groupdel": `#!/bin/sh
cat > /dev/null
echo "groupdel $*" >> "$STUB_STATE/calls"
f=$HERON_INSTALL_ROOT/etc/group
grep -q "^$1:" "$f" || { echo "groupdel: group '$1' does not exist" >&2; exit 6; }
grep -v "^$1:" "$f" > "$f.new"; mv "$f.new" "$f"
`,
	// useradd/groupadd 记下参数后把记录追加进账户文件，像 useradd 那样按 -g 解析主组的 gid。
	"useradd": `#!/bin/sh
cat > /dev/null
echo "useradd $*" >> "$STUB_STATE/calls"
uid=""; grp=""; prev=""
for a in "$@"; do
  [ "$prev" = "-u" ] && uid=$a
  [ "$prev" = "-g" ] && grp=$a
  prev=$a
done
for a in "$@"; do last=$a; done
gid=$(awk -F: -v g="$grp" '$1 == g { print $3 }' "$HERON_INSTALL_ROOT/etc/group")
printf '%s:x:%s:%s::/nonexistent:/usr/sbin/nologin\n' "$last" "$uid" "$gid" >> "$HERON_INSTALL_ROOT/etc/passwd"
`,
	"groupadd": `#!/bin/sh
cat > /dev/null
echo "groupadd $*" >> "$STUB_STATE/calls"
gid=""; prev=""
for a in "$@"; do
  [ "$prev" = "-g" ] && gid=$a
  prev=$a
done
for a in "$@"; do last=$a; done
printf '%s:x:%s:\n' "$last" "$gid" >> "$HERON_INSTALL_ROOT/etc/group"
`,
	"sleep": `#!/bin/sh
echo "sleep $*" >> "$STUB_STATE/calls"
`,
	"curl": fileCurl,
}

// newLinuxHost 是一台 systemd 主机：服务用户不在本地账户文件里，由各用例按需加上；
// /proc 下只有 self，没有以任何 uid 运行的进程目录。
func newLinuxHost(t *testing.T) *env {
	t.Helper()
	e := newStubEnv(t, "install.sh", linuxStubs)
	for rel, body := range map[string]string{
		"run/systemd/system/.keep": "",
		// 真实主机上这两个目录总在（FHS 与 systemd 自带），install.sh 不建它们。
		"usr/local/bin/.keep":               "",
		"etc/systemd/system/.keep":          "",
		"etc/ssl/certs/ca-certificates.crt": "",
		"proc/self/status":                  "Uid:\t1000\t1000\t1000\t1000\n",
		"etc/passwd":                        "root:x:0:0:root:/root:/bin/sh\n",
		"etc/group":                         "root:x:0:\n",
	} {
		e.put(rel, body)
	}
	return e
}

// newLinuxEnv 在 newLinuxHost 上放好一次安装留下的文件，服务没有在运行。
func newLinuxEnv(t *testing.T) *env {
	t.Helper()
	e := newLinuxHost(t)
	for rel, body := range map[string]string{
		"etc/systemd/system/heron-agent.service": "[Service]\n",
		"etc/heron-agent/config.json":            "{}\n",
		"var/log/heron-agent/.keep":              "",
		"usr/local/bin/heron-agent":              "#!/bin/sh\n",
	} {
		e.put(rel, body)
	}
	return e
}

// put 在假根目录下写文件，缺的父目录一并建出。
func (e *env) put(rel, body string) {
	e.t.Helper()
	p := filepath.Join(e.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) appendTo(rel, line string) {
	e.t.Helper()
	e.put(rel, e.file(rel)+line)
}

func TestLinuxPurgeDeletesTheLocalAccount(t *testing.T) {
	t.Parallel()
	e := newLinuxEnv(t)
	e.appendTo("etc/passwd", "heron-agent:x:480:480::/nonexistent:/usr/sbin/nologin\n")
	e.appendTo("etc/group", "heron-agent:x:480:\n")
	out, code := e.run("--uninstall", "--purge")
	if code != 0 || !strings.Contains(out, "heron-agent uninstalled") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, rel := range []string{"usr/local/bin/heron-agent", "etc/systemd/system/heron-agent.service", "etc/heron-agent", "var/log/heron-agent"} {
		if e.exists(rel) {
			t.Errorf("%s left behind", rel)
		}
	}
	if strings.Contains(e.file("etc/passwd"), "heron-agent:") || strings.Contains(e.file("etc/group"), "heron-agent:") {
		t.Fatalf("account left in the local files:\n%s%s", e.file("etc/passwd"), e.file("etc/group"))
	}
}

// 解析得到的同名用户不在本地 /etc/passwd 里：不发 userdel（它会以 does not exist 失败、脚本以它的
// 退出码结束），回查仍能看到这个用户，由脚本自己报出删不掉。
func TestLinuxPurgeDecidesDeletionFromTheLocalRecord(t *testing.T) {
	t.Parallel()
	e := newLinuxEnv(t)
	e.write("remote-passwd", "heron-agent:x:480:480::/nonexistent:/usr/sbin/nologin\n")
	out, code := e.run("--uninstall", "--purge")
	if code != 1 || !strings.Contains(out, "failed to delete user or group heron-agent") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if index(e.calls(), "userdel") >= 0 {
		t.Fatalf("no local record, no userdel: %q", e.calls())
	}
}

// 全新主机上建号避开运行中进程占用的号：postgres 容器进程以 uid 999 运行、宿主账户文件里没有它的记录，
// useradd 自动取号会取到 999。取号应为 998，用户与组同号并显式传给 groupadd/useradd。
func TestLinuxFreshInstallPicksAnIdUnusedByRunningProcesses(t *testing.T) {
	t.Parallel()
	e := newLinuxHost(t)
	e.put("proc/777/status", "Name:\tpostgres\nState:\tS (sleeping)\nUid:\t999\t999\t999\t999\n")
	foreignExe(t, e, "proc/777")
	e.linuxRelease("amd64", "v1")
	out, code := e.linuxInstall()
	if code != 0 || !strings.Contains(out, "heron-agent installed and started (systemd, amd64, heron-agent_linux_amd64.tar.gz)") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	c := e.calls()
	if index(c, "groupadd --system -g 998 heron-agent") < 0 {
		t.Errorf("groupadd must get an explicit id below 999, calls %q", c)
	}
	if index(c, "useradd --system -u 998 -M -d /nonexistent -s /sbin/nologin -g heron-agent heron-agent") < 0 {
		t.Errorf("useradd must get an explicit id below 999, calls %q", c)
	}
	if !strings.Contains(e.file("etc/passwd"), "heron-agent:x:998:998:") {
		t.Errorf("account not created at 998:\n%s", e.file("etc/passwd"))
	}
}

// foreignExe 给假 /proc 里的进程 dir 放一个指向 postgres 的 exe 链接：撞号的外来进程长这样。
func foreignExe(t *testing.T, e *env, dir string) {
	t.Helper()
	if err := os.Symlink(filepath.Join(e.root, "usr/lib/postgresql/15/bin/postgres"), filepath.Join(e.root, dir, "exe")); err != nil {
		t.Fatal(err)
	}
}

// 已有账户的 uid 被外来进程共用（撞号：postgres 容器的 uid 999）时，在任何下载、注册与系统变更之前拒绝，
// 说明换号办法；已退出待收尸的僵尸不构成撞号。
func TestLinuxRefusesAccountUidSharedWithForeignProcesses(t *testing.T) {
	t.Parallel()
	e := newLinuxHost(t)
	e.appendTo("etc/passwd", "heron-agent:x:480:480::/nonexistent:/usr/sbin/nologin\n")
	e.appendTo("etc/group", "heron-agent:x:480:\n")
	e.put("etc/heron-agent/config.json", "{}\n")
	e.put("proc/888/status", "Name:\tpostgres\nState:\tS (sleeping)\nUid:\t480\t480\t480\t480\n")
	foreignExe(t, e, "proc/888")
	e.put("proc/889/status", "Name:\tpostgres\nState:\tZ (zombie)\nUid:\t480\t480\t480\t480\n")
	e.linuxRelease("amd64", "v1")
	e.resetCalls()
	out, code := e.linuxInstall()
	if code != 1 || !strings.Contains(out, "is also used by other running processes") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, " 888:") || strings.Contains(out, " 889:") {
		t.Errorf("only the live foreign process counts as sharing:\n%s", out)
	}
	c := e.calls()
	if index(c, "curl") >= 0 || index(c, "heron-agent register") >= 0 {
		t.Errorf("refused before any download or registration, calls %q", c)
	}
	if !strings.Contains(out, "editing the id in /etc/passwd and /etc/group") {
		t.Errorf("must say how to move the account:\n%s", out)
	}
}

// 撞号机器上的卸载不再被外来进程卡住：停服确认只等本服务二进制的进程，外来同 uid 进程直接略过。
func TestLinuxUninstallIgnoresForeignProcessesSharingTheUid(t *testing.T) {
	t.Parallel()
	e := newLinuxEnv(t)
	e.appendTo("etc/passwd", "heron-agent:x:480:480::/nonexistent:/usr/sbin/nologin\n")
	e.appendTo("etc/group", "heron-agent:x:480:\n")
	e.put("proc/888/status", "Name:\tpostgres\nState:\tS (sleeping)\nUid:\t480\t480\t480\t480\n")
	foreignExe(t, e, "proc/888")
	out, code := e.run("--uninstall")
	if code != 0 || !strings.Contains(out, "heron-agent uninstalled") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// linuxRelease 发布 version：打出 arch 的包，写入脚本。
func (e *env) linuxRelease(arch, version string) {
	e.t.Helper()
	e.linuxPackage(e.dist, arch, version)
	e.publish(version)
}

// linuxPackage 按发布配方的形状把 arch 的 Linux 包打进 dir：heron-agent 与仓库里的 systemd 单元、OpenRC 脚本原件。
func (e *env) linuxPackage(dir, arch, version string) {
	e.t.Helper()
	e.updaterPackage(dir, arch, version)
	unit, err := os.ReadFile("systemd/heron-agent.service")
	if err != nil {
		e.t.Fatal(err)
	}
	openrc, err := os.ReadFile("openrc/heron-agent")
	if err != nil {
		e.t.Fatal(err)
	}
	e.pack(dir, "heron-agent_linux_"+arch+".tar.gz", []packFile{
		{"heron-agent", fakeAgent + "# " + version + " " + arch + "\n", 0o755},
		{"heron-agent.service", string(unit), 0o644},
		{"heron-agent.openrc", string(openrc), 0o755},
	})
}

// newLinuxInstalled 在 newLinuxHost 上以 v1 装好并启动服务；服务用户已在本地账户文件里，不走建账户分支。
func newLinuxInstalled(t *testing.T) *env {
	t.Helper()
	e := newLinuxHost(t)
	e.appendTo("etc/passwd", "heron-agent:x:480:480::/nonexistent:/usr/sbin/nologin\n")
	e.appendTo("etc/group", "heron-agent:x:480:\n")
	e.linuxRelease("amd64", "v1")
	out, code := e.linuxInstall()
	if code != 0 || !strings.Contains(out, "heron-agent installed and started (systemd, amd64, heron-agent_linux_amd64.tar.gz)") {
		t.Fatalf("first install exit %d:\n%s", code, out)
	}
	return e
}

func (e *env) linuxInstall() (string, int) {
	return e.run("--hub", "http://hub.test", "--key", "k", "--base-url", "file://"+e.dist)
}

// 可能失败的操作都在停服务之前：失败时旧服务照常运行。停服务之后只剩替换二进制、装服务定义、启动。
func TestLinuxRerunDoesTheFallibleStepsBeforeStopping(t *testing.T) {
	t.Parallel()
	e := newLinuxInstalled(t)
	e.linuxRelease("amd64", "v2")
	e.resetCalls()
	if out, code := e.linuxInstall(); code != 0 {
		t.Fatalf("rerun exit %d:\n%s", code, out)
	}
	c := e.calls()
	stop := index(c, "systemctl stop heron-agent")
	for _, want := range []string{
		"chown root:heron-agent " + e.root + "/etc/heron-agent",
		"chown heron-agent:heron-agent " + e.root + "/etc/heron-agent/config.json",
	} {
		if i := index(c, want); i < 0 || i > stop {
			t.Errorf("%q must run before the service is stopped, calls %q", want, c)
		}
	}
	for _, call := range c[stop+1:] {
		if strings.HasPrefix(call, "chown ") || strings.HasPrefix(call, "heron-agent ") {
			t.Errorf("%q runs after the service is stopped", call)
		}
	}
	if !strings.Contains(e.file("usr/local/bin/heron-agent"), "# v2 amd64") {
		t.Fatal("the rerun did not install v2")
	}
}

// 配置丢了、重新注册失败，或配置 chown 失败：都在停服务之前失败，旧服务照常运行、二进制不被替换。
func TestLinuxFailuresBeforeStoppingLeaveTheServiceRunning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		vars func(root string) []string
		prep func(root string)
		want string
	}{
		{"re-register", func(string) []string { return []string{"STUB_REGISTER_FAILS=1"} }, func(root string) {
			os.Remove(filepath.Join(root, "etc/heron-agent/config.json"))
		}, "register: hub unreachable"},
		{"config chown", func(root string) []string {
			return []string{"STUB_CHOWN_FAILS=" + root + "/etc/heron-agent/config.json"}
		}, nil, "config.json: Operation not permitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newLinuxInstalled(t)
			if tc.prep != nil {
				tc.prep(e.root)
			}
			e.linuxRelease("amd64", "v2")
			e.vars = tc.vars(e.root)
			e.resetCalls()
			out, code := e.linuxInstall()
			if code == 0 || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			if index(e.calls(), "systemctl stop heron-agent") >= 0 || !strings.Contains(e.file("usr/local/bin/heron-agent"), "# v1 amd64") || !e.exists("proc/4242/status") {
				t.Fatalf("the running service must be left alone: calls %q", e.calls())
			}
		})
	}
}
