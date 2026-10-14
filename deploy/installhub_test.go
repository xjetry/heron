package deploy

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// install-hub.sh 的替身。服务用户 heron-hub（uid 481）在 $HERON_INSTALL_ROOT/etc/passwd 与 etc/group 里，
// 不走建账户分支；id、uname 与 install.sh 的替身相同。sleep 只记参数，STUB_START_DIES 时在启动确认的 3 秒复查
// 之前拿走进程 4242。
// systemctl 记下参数；start 在假 /proc 里放一个以服务用户运行、exe 指向临时根里 /usr/local/bin/heron-hub
// （脚本按 uid 加这个 exe 认本服务的进程，端口自查也比对它）的进程 4242，
// stop 把它拿走；show 回答 MainPID 与 DropInPaths（state/dropins 里空格分隔的路径，是目标系统里的真实路径）。
// enable、disable 照 systemd 252 的实测建出与删掉 multi-user.target.wants/heron-hub.service，一条指向
// /etc/systemd/system/heron-hub.service 的符号链接（目标是目标系统里的真实路径，在临时根目录下悬空）；is-enabled
// 按这条链接回答。
// state/dropins 是 systemd 已加载的 drop-in，照 systemd 252 的实测：daemon-reload 时才从 state/dropins-disk
// （磁盘上的 drop-in）取，单元文件不存在时为空。STUB_STOP_FAILS 让 stop 失败，STUB_STOP_LEAVES_PROCESS 让 stop
// 返回 0 却留下进程，STUB_DROPIN_ON_STOP 在 stop 时把一个 drop-in 写进 state/dropins-disk。STUB_START_NO_PROCESS
// 让 start 返回 0 却不起进程。STUB_RELOAD_FAILS 让 daemon-reload 失败，STUB_RELOAD_FAILS_ON_STOP 让它从 stop 之后
// 开始失败；STUB_IS_ENABLED_FAILS_WITH_RELOAD 让 is-enabled 在 reload 失败时一起失败。STUB_SYSTEMCTL_FAILS 与
// STUB_SYSTEMCTL_TERM 同 install.sh 的替身：参数串与前者相同时失败，与后者相同时给安装脚本发 SIGTERM。
// chown 只记参数：测试以普通用户运行，改不了属主，属主由真机验收回读。chmod 记下参数后转调真的，权限断言看的
// 是真实的文件模式。curl 是三个脚本共用的 fileCurl；apt-get 记下参数，install 时建出 CA 证书包。
// systemctl、curl、apt-get 读尽 stdin：脚本以 sh -s 从 stdin 运行，漏掉 </dev/null 的调用会吞掉脚本余下部分，
// 安装在中途无声结束，测试看不到最后一行。真实的 systemd 解析、起停与属主由 scripts/install-accept.sh 在真机上验证。
var hubStubs = map[string]string{
	"flock":    linuxStubs["flock"],
	"id":       linuxStubs["id"],
	"uname":    linuxStubs["uname"],
	"useradd":  linuxStubs["useradd"],
	"groupadd": linuxStubs["groupadd"],
	"sleep": `#!/bin/sh
echo "sleep $*" >> "$STUB_STATE/calls"
if [ -n "${STUB_START_DIES-}" ] && [ "$1" = 3 ]; then rm -rf "$HERON_INSTALL_ROOT/proc/4242"; fi
`,
	"systemctl": `#!/bin/sh
cat > /dev/null
echo "systemctl $*" >> "$STUB_STATE/calls"
[ "$*" != "${STUB_SYSTEMCTL_FAILS-}" ] || { echo "systemctl $*: failed" >&2; exit 1; }
[ "$*" != "${STUB_SYSTEMCTL_TERM-}" ] || kill -TERM "$PPID"
case "$*" in
  "show heron-updater-hub -p ActiveState --value") echo "${STUB_UPDATER_STATE:-active}"; exit 0;;
  *heron-updater-hub*) exit 0;;
esac
P=$HERON_INSTALL_ROOT/proc
W=$HERON_INSTALL_ROOT/etc/systemd/system/multi-user.target.wants
reload_fails() { [ -n "${STUB_RELOAD_FAILS-}" ] || [ -f "$STUB_STATE/reload-fails" ]; }
case "$*" in
  "start heron-hub")
    [ -z "${STUB_START_FAILS-}" ] || { echo "Job for heron-hub.service failed." >&2; exit 1; }
    [ -z "${STUB_START_NO_PROCESS-}" ] || exit 0
    # 包里带这个标记的 heron-hub 模拟"起不来的新二进制"：它在退出前打开过库（schema 迁移），
    # 这里把三个库文件改写成另一份内容，供回滚用例断言旧库被换回来。
    if grep -q 'heron-test-broken' "$HERON_INSTALL_ROOT/usr/local/bin/heron-hub" 2>/dev/null; then
      for f in heron.db heron.db-wal heron.db-shm; do
        printf 'migrated\n' > "$HERON_INSTALL_ROOT/var/lib/heron/$f"
      done
      exit 0
    fi
    uid=$(grep '^heron-hub:' "$HERON_INSTALL_ROOT/etc/passwd" | cut -d: -f3)
    mkdir -p "$P/4242"
    printf 'Uid:\t%s\t%s\t%s\t%s\n' "$uid" "$uid" "$uid" "$uid" > "$P/4242/status"
    rm -f "$P/4242/exe"; ln -s "$HERON_INSTALL_ROOT/usr/local/bin/heron-hub" "$P/4242/exe";;
  "stop heron-hub")
    [ -z "${STUB_STOP_FAILS-}" ] || { echo "Failed to stop heron-hub.service: Access denied" >&2; exit 1; }
    [ -n "${STUB_STOP_LEAVES_PROCESS-}" ] || rm -rf "$P/4242"
    [ -z "${STUB_DROPIN_ON_STOP-}" ] || echo "$STUB_DROPIN_ON_STOP" > "$STUB_STATE/dropins-disk"
    [ -z "${STUB_RELOAD_FAILS_ON_STOP-}" ] || : > "$STUB_STATE/reload-fails";;
  daemon-reload)
    if reload_fails; then
      echo "Failed to reload daemon: Access denied" >&2; exit 1
    fi
    if [ ! -f "$HERON_INSTALL_ROOT/etc/systemd/system/heron-hub.service" ]; then : > "$STUB_STATE/dropins"
    elif [ -f "$STUB_STATE/dropins-disk" ]; then cp "$STUB_STATE/dropins-disk" "$STUB_STATE/dropins"; fi;;
  "enable heron-hub") mkdir -p "$W"; ln -sf /etc/systemd/system/heron-hub.service "$W/heron-hub.service";;
  "disable heron-hub") rm -f "$W/heron-hub.service";;
  "is-enabled --quiet heron-hub")
    if [ -n "${STUB_IS_ENABLED_FAILS_WITH_RELOAD-}" ] && reload_fails; then
      echo "Failed to get unit file state for heron-hub.service: Access denied" >&2; exit 1
    fi
    [ -L "$W/heron-hub.service" ];;
  "show heron-hub -p MainPID --value") if [ -d "$P/4242" ]; then echo 4242; else echo 0; fi;;
  "show heron-hub -p DropInPaths --value") cat "$STUB_STATE/dropins" 2>/dev/null || echo;;
esac
`,
	// STUB_SWAP_DB 是目录外的一个文件：数据目录交给 root 的那一刻，把 heron.db 换成指向它的硬链接，
	// 模拟停服前预检之后、加锁之前服务组替换了库文件。
	"chown": `#!/bin/sh
echo "chown $*" >> "$STUB_STATE/calls"
d=$HERON_INSTALL_ROOT/var/lib/heron
if [ -n "${STUB_SWAP_DB-}" ] && [ "$1" = root:heron-hub ] && [ "$2" = "$d" ]; then
  rm -f "$d/heron.db"; ln "$STUB_SWAP_DB" "$d/heron.db"
fi
`,
	"chmod": `#!/bin/sh
echo "chmod $*" >> "$STUB_STATE/calls"
exec /bin/chmod "$@"
`,
	// STUB_CP_FAILS 是一个文件名：复制的目标是它时只写前 3 个字节就失败，模拟备份库文件时磁盘写满。
	"cp": `#!/bin/sh
for last; do :; done
if [ -n "${STUB_CP_FAILS-}" ] && [ "${last##*/}" = "$STUB_CP_FAILS" ]; then
  head -c 3 "$1" > "$last"; echo "cp: $last: No space left on device" >&2; exit 1
fi
exec /bin/cp "$@"
`,
	"curl": fileCurl,
	"apt-get": `#!/bin/sh
cat > /dev/null
echo "apt-get $*" >> "$STUB_STATE/calls"
if [ "$1" = install ]; then
  mkdir -p "$HERON_INSTALL_ROOT/etc/ssl/certs"
  : > "$HERON_INSTALL_ROOT/etc/ssl/certs/ca-certificates.crt"
fi
`,
}

const (
	hubUnit  = "etc/systemd/system/heron-hub.service"
	hubWants = "etc/systemd/system/multi-user.target.wants/heron-hub.service"
	hubData  = "var/lib/heron"
	hubDone  = "heron-hub installed and started (systemd, amd64, heron-hub_linux_amd64.tar.gz)"
	hubLast  = "Set the administrator password: heron-hub passwd --db /var/lib/heron/heron.db"
	// /proc/net/tcp 的表头；监听行由用例按需追加。
	tcpHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
)

// newHubHost 是一台 systemd 主机：服务用户已在本地账户文件里，有 CA 证书包，没有任何 TCP 监听，
// /proc 下只有 self；dist 里放着 v1 的 hub 包。
func newHubHost(t *testing.T) *env {
	t.Helper()
	e := newStubEnv(t, "install-hub.sh", hubStubs)
	for rel, body := range map[string]string{
		"run/systemd/system/.keep":          "",
		"usr/local/bin/.keep":               "",
		"etc/systemd/system/.keep":          "",
		"etc/ssl/certs/ca-certificates.crt": "",
		"proc/self/status":                  "Uid:\t1000\t1000\t1000\t1000\n",
		"proc/net/tcp":                      tcpHeader,
		"etc/passwd":                        "root:x:0:0:root:/root:/bin/sh\nheron-hub:x:481:481::/nonexistent:/usr/sbin/nologin\n",
		"etc/group":                         "root:x:0:\nheron-hub:x:481:\n",
	} {
		e.put(rel, body)
	}
	e.hubRelease("v1")
	return e
}

// hubRelease 发布 version：打出 hub 包，写入脚本。
func (e *env) hubRelease(version string) {
	e.t.Helper()
	e.hubPackage(e.dist, version)
	e.publish(version)
}

// hubPackage 按发布配方的形状把 hub 包打进 dir：heron-hub 与仓库里的 systemd 单元原件。
func (e *env) hubPackage(dir, version string) {
	e.t.Helper()
	e.updaterPackage(dir, "amd64", version)
	unit, err := os.ReadFile("systemd/heron-hub.service")
	if err != nil {
		e.t.Fatal(err)
	}
	e.pack(dir, "heron-hub_linux_amd64.tar.gz", []packFile{
		{"heron-hub", "#!/bin/sh\n# " + version + " amd64\n", 0o755},
		{"heron-hub.service", string(unit), 0o644},
	})
}

func (e *env) hubInstall(extra ...string) (string, int) {
	return e.run(append([]string{"--base-url", "file://" + e.dist}, extra...)...)
}

// newHubInstalled 在 newHubHost 上以 v1 装好并启动服务，然后换上 v2 的包、清空调用记录，等用例重跑。
func newHubInstalled(t *testing.T, args ...string) *env {
	t.Helper()
	e := newHubHost(t)
	out, code := e.hubInstall(args...)
	if code != 0 || !strings.Contains(out, hubDone) {
		t.Fatalf("first install exit %d:\n%s", code, out)
	}
	e.hubRelease("v2")
	e.resetCalls()
	return e
}

func (e *env) execStart() string {
	e.t.Helper()
	for _, line := range strings.Split(e.file(hubUnit), "\n") {
		if v, ok := strings.CutPrefix(line, "ExecStart="); ok {
			return v
		}
	}
	e.t.Fatalf("no ExecStart in:\n%s", e.file(hubUnit))
	return ""
}

// setExecStart 把已装单元里的 ExecStart 行换成 lines（可以跨行），其余行保持安装器写出的样子。
func (e *env) setExecStart(lines string) {
	e.t.Helper()
	var out []string
	for _, line := range strings.Split(e.file(hubUnit), "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			line = lines
		}
		out = append(out, line)
	}
	e.put(hubUnit, strings.Join(out, "\n"))
}

// enableLink 报告 enable 链接在不在。链接在临时根目录下悬空，不能用跟随链接的 exists。
func (e *env) enableLink() bool {
	e.t.Helper()
	fi, err := os.Lstat(filepath.Join(e.root, hubWants))
	if err != nil && !os.IsNotExist(err) {
		e.t.Fatal(err)
	}
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// assertUntouched 断言重跑没有碰正在运行的旧服务：没发 stop、二进制还是 v1、进程 4242 还在。
func (e *env) assertUntouched(out string) {
	e.t.Helper()
	if index(e.calls(), "systemctl stop") >= 0 || !strings.Contains(e.file("usr/local/bin/heron-hub"), "# v1 amd64") || !e.exists("proc/4242/status") {
		e.t.Fatalf("the running hub must be left alone: calls %q\n%s", e.calls(), out)
	}
	if strings.Contains(out, "heron-hub is stopped") {
		e.t.Fatalf("nothing was stopped, but the installer says so:\n%s", out)
	}
}

const hubCmd = `"/usr/local/bin/heron-hub" "serve"`

func TestHubFreshInstallFromStdin(t *testing.T) {
	t.Parallel()
	e := newHubHost(t)
	out, code := e.hubInstall("--listen", "127.0.0.1:9000", "--timezone", "Asia/Taipei")
	// 最后一行在：脚本没有被哪个读 stdin 的调用截断。
	if code != 0 || !strings.Contains(out, hubDone) || !strings.HasSuffix(strings.TrimSpace(out), hubLast) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got, want := e.execStart(), hubCmd+` "--db=/var/lib/heron/heron.db" "--listen=127.0.0.1:9000" "--timezone=Asia/Taipei"`; got != want {
		t.Fatalf("ExecStart=%s\nwant      %s", got, want)
	}
	e.mode(hubData, 0o770)
	e.mode(hubData+"/heron.db", 0o600)
	c := e.calls()
	for _, want := range []string{"chown root:heron-hub " + e.root + "/" + hubData, "chown heron-hub:heron-hub " + e.root + "/" + hubData + "/heron.db"} {
		if !slices.Contains(c, want) {
			t.Errorf("missing %q in calls %q", want, c)
		}
	}
	if index(c, "apt-get") >= 0 {
		t.Errorf("a CA bundle is present; no package manager call expected: %q", c)
	}
	if !strings.Contains(e.file("usr/local/bin/heron-hub"), "# v1 amd64") {
		t.Fatal("v1 was not installed")
	}
}

// 已装单元里 ExecStart 的各种写法：能逐字还原的沿用并按 flag 名与命令行覆盖合并，写回统一成 --flag=value；
// 还原不了或不认识的，在停服前拒绝，旧服务照常运行。
func TestHubExecStartForms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		exec     string            // 已装单元里代替 ExecStart 行的内容
		crlf     bool              // 已装单元改成 CRLF 行尾
		dropins  map[string]string // drop-in 的真实路径到内容；空内容表示列出来却不存在
		unloaded bool              // drop-in 只在磁盘上，systemd 还没 daemon-reload
		args     []string
		want     string // 重跑后的 ExecStart；为空时 wantErr 非空
		wantErr  string
		preamble string // 加在已装单元开头的行
	}{
		{name: "inherited", exec: `ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db --listen 127.0.0.1:9000 --timezone Asia/Taipei --trusted-proxies 10.0.0.0/8`,
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--listen=127.0.0.1:9000" "--timezone=Asia/Taipei" "--trusted-proxies=10.0.0.0/8"`},
		{name: "go flag spellings", exec: `ExecStart=/usr/local/bin/heron-hub serve -db /var/lib/heron/heron.db --listen=127.0.0.1:9000 -timezone=UTC --retention-1m 72h`,
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--listen=127.0.0.1:9000" "--timezone=UTC" "--retention-1m=72h"`},
		{name: "override replaces by name and appends", exec: `ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db --timezone Asia/Taipei --listen 127.0.0.1:9000`,
			args: []string{"--listen", "127.0.0.1:9100", "--public-dir", "/srv/site"},
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--timezone=Asia/Taipei" "--listen=127.0.0.1:9100" "--public-dir=/srv/site"`},
		{name: "repeated flag keeps its first place and last value", exec: `ExecStart=/usr/local/bin/heron-hub serve --timezone Asia/Taipei --db /var/lib/heron/heron.db --timezone=UTC`,
			want: hubCmd + ` "--timezone=UTC" "--db=/var/lib/heron/heron.db"`},
		{name: "quotes and escapes round-trip", exec: `ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db "--public-dir=/srv/heron site \"x\" $$literal%% \\end" '--trusted-proxies=10.0.0.0/8'`,
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--public-dir=/srv/heron site \"x\" $$literal%% \\end" "--trusted-proxies=10.0.0.0/8"`},
		{name: "override value is kept literally", args: []string{"--public-dir", `/srv/x "y" $z %w \v`},
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--listen=127.0.0.1:8080" "--public-dir=/srv/x \"y\" $$z %%w \\v"`},
		{name: "continuation line", exec: "ExecStart=/usr/local/bin/heron-hub serve \\\n  --db /var/lib/heron/heron.db --listen 127.0.0.1:9000",
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--listen=127.0.0.1:9000"`},
		// 续行判定与 systemd 相同：行尾未转义的反斜杠才续行（Debian 12 上 systemd 252 实测）。
		{name: "three trailing backslashes continue", exec: "ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db --public-dir=/srv/a\\\\\\\n  --listen 127.0.0.1:9000",
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--public-dir=/srv/a\\" "--listen=127.0.0.1:9000"`},
		{name: "escaped backslash at the end does not continue", exec: "ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db --public-dir=/srv/a\\\\\n  --listen 0.0.0.0:80",
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--public-dir=/srv/a\\"`},
		{name: "CRLF continuation", crlf: true, exec: "ExecStart=/usr/local/bin/heron-hub serve \\\n  --db /var/lib/heron/heron.db --listen 127.0.0.1:9000",
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--listen=127.0.0.1:9000"`},
		// 注释行在续行判断之前跳过，续行中间夹的注释行不打断续行，也不被拼进命令。
		{name: "comment inside a continuation", exec: "ExecStart=/usr/local/bin/heron-hub serve \\\n# listen only on loopback\n  --db /var/lib/heron/heron.db --listen 127.0.0.1:9000",
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--listen=127.0.0.1:9000"`},
		{name: "spaces around the equals sign", exec: `ExecStart = /usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db`,
			want: hubCmd + ` "--db=/var/lib/heron/heron.db"`},
		{name: "CRLF line endings", crlf: true,
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--listen=127.0.0.1:8080"`},
		{name: "comment naming a path", preamble: "# /var/lib/heron holds the database",
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--listen=127.0.0.1:8080"`},
		{name: "drop-in without ExecStart", dropins: map[string]string{"/run/systemd/system/service.d/zzz-lxc-service.conf": "[Service]\nProtectProc=default\n"},
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--listen=127.0.0.1:8080"`},

		// systemd 把它当成字面参数 \、下一行因缺 = 被忽略；安装器按未完成的转义拒绝，而不是接成 --listen。
		{name: "backslash before trailing space does not continue", exec: "ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db \\ \n  --listen 0.0.0.0:80",
			wantErr: "unsupported quoting or escape in ExecStart"},
		// systemd 把行内的回车当作换行，后半段成了另一条指令；安装器不模仿，拒绝。
		{name: "carriage return inside a line", exec: "ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db\rEnvironment=X=1",
			wantErr: "cannot read ExecStart from installed unit"},
		{name: "single dollar", exec: `ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db "--public-dir=/srv/$HOME"`,
			wantErr: "dynamic $ or % expansion in ExecStart is not supported"},
		{name: "single percent", exec: `ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db --public-dir=/srv/%h`,
			wantErr: "dynamic $ or % expansion in ExecStart is not supported"},
		{name: "hex escape", exec: `ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db "--public-dir=/srv\x20a"`,
			wantErr: "unsupported quoting or escape in ExecStart"},
		{name: "unclosed quote", exec: `ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db "--public-dir=/srv/a`,
			wantErr: "unsupported quoting or escape in ExecStart"},
		{name: "unknown flag", exec: `ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db --foo=bar`,
			wantErr: "unsupported serve flag in ExecStart: --foo=bar"},
		{name: "removed theme origin", exec: `ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db --theme-origin https://status.example.com`,
			wantErr: "remove --theme-origin from the installed unit before upgrading"},
		{name: "removed theme origin equals", exec: `ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db -theme-origin=https://status.example.com`,
			wantErr: "remove --theme-origin from the installed unit before upgrading"},
		{name: "legacy passkey origin", exec: `ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db --admin-origin https://panel.example.com`,
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--admin-origin=https://panel.example.com"`},
		{name: "offline-after in the installed unit", exec: `ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db --offline-after 60s`,
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--offline-after=60s"`},
		{name: "offline-after override", args: []string{"--offline-after", "60s"},
			want: hubCmd + ` "--db=/var/lib/heron/heron.db" "--listen=127.0.0.1:8080" "--offline-after=60s"`},
		{name: "positional argument", exec: `ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db extra`,
			wantErr: "unexpected positional argument in ExecStart: extra"},
		{name: "another database", exec: `ExecStart=/usr/local/bin/heron-hub serve --db /srv/other.db`,
			wantErr: "installed unit must use --db /var/lib/heron/heron.db"},
		{name: "no database", exec: `ExecStart=/usr/local/bin/heron-hub serve --listen 127.0.0.1:9000`,
			wantErr: "installed unit must use --db /var/lib/heron/heron.db"},
		{name: "another binary", exec: `ExecStart=/usr/bin/heron-hub serve --db /var/lib/heron/heron.db`,
			wantErr: "ExecStart must invoke /usr/local/bin/heron-hub serve"},
		{name: "flag without value", exec: `ExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db --listen`,
			wantErr: "incomplete ExecStart arguments"},
		{name: "ExecStart set twice", exec: "ExecStart=\nExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db",
			wantErr: "installed unit must set ExecStart exactly once"},
		{name: "drop-in sets ExecStart", dropins: map[string]string{"/etc/systemd/system/heron-hub.service.d/override.conf": "[Service]\nExecStart=\nExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db\n"},
			wantErr: "drop-in /etc/systemd/system/heron-hub.service.d/override.conf sets ExecStart"},
		{name: "drop-in sets ExecStart with spaces and CRLF", dropins: map[string]string{"/etc/systemd/system/heron-hub.service.d/override.conf": "[Service] \r\nExecStart = /usr/local/bin/heron-hub serve\r\n"},
			wantErr: "sets ExecStart"},
		{name: "drop-in hides ExecStart behind a carriage return", dropins: map[string]string{"/etc/systemd/system/heron-hub.service.d/override.conf": "[Service]\rExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db\n"},
			wantErr: "cannot parse heron-hub drop-in /etc/systemd/system/heron-hub.service.d/override.conf"},
		{name: "drop-in comment ending in a backslash", dropins: map[string]string{"/etc/systemd/system/heron-hub.service.d/override.conf": "[Service]\n# note \\\nExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db\n"},
			wantErr: "sets ExecStart"},
		// 运行中的 hub 看不到后来落盘的 drop-in，安装器启动前的 daemon-reload 却会让它生效：要先 reload 再查。
		{name: "drop-in written but not yet reloaded", unloaded: true, dropins: map[string]string{"/etc/systemd/system/heron-hub.service.d/late.conf": "[Service]\nExecStart=\nExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db\n"},
			wantErr: "drop-in /etc/systemd/system/heron-hub.service.d/late.conf sets ExecStart"},
		{name: "drop-in listed but missing", dropins: map[string]string{"/etc/systemd/system/heron-hub.service.d/gone.conf": ""},
			wantErr: "cannot read heron-hub drop-in /etc/systemd/system/heron-hub.service.d/gone.conf"},
		{name: "line feed in an override", args: []string{"--timezone", "UTC\n--listen=0.0.0.0:80"},
			wantErr: "arguments must not contain line breaks"},
		{name: "carriage return in an override", args: []string{"--timezone", "UTC\r"},
			wantErr: "arguments must not contain line breaks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newHubInstalled(t)
			if tc.exec != "" {
				e.setExecStart(tc.exec)
			}
			if tc.preamble != "" {
				e.put(hubUnit, tc.preamble+"\n"+e.file(hubUnit))
			}
			if tc.crlf {
				e.put(hubUnit, strings.ReplaceAll(e.file(hubUnit), "\n", "\r\n"))
			}
			var paths []string
			for path, body := range tc.dropins {
				paths = append(paths, path)
				if body != "" {
					e.put(strings.TrimPrefix(path, "/"), body)
				}
			}
			if tc.unloaded {
				e.write("dropins", "\n")
				e.write("dropins-disk", strings.Join(paths, " ")+"\n")
			} else {
				e.write("dropins", strings.Join(paths, " ")+"\n")
			}
			before := e.file(hubUnit)
			out, code := e.hubInstall(tc.args...)
			if tc.wantErr != "" {
				if code != 1 || !strings.Contains(out, tc.wantErr) {
					t.Fatalf("exit %d, want 1 with %q:\n%s", code, tc.wantErr, out)
				}
				e.assertUntouched(out)
				if e.file(hubUnit) != before {
					t.Fatalf("a refused upgrade rewrote the unit:\n%s", e.file(hubUnit))
				}
				return
			}
			if code != 0 || !strings.Contains(out, hubDone) {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			if got := e.execStart(); got != tc.want {
				t.Fatalf("ExecStart=%s\nwant      %s", got, tc.want)
			}
		})
	}
}

// 带同一组参数重跑任意次、再不带参数重跑，单元逐字不变：覆盖按 flag 名替换，不累加。
func TestHubRerunsDoNotAccumulateArguments(t *testing.T) {
	t.Parallel()
	args := []string{"--listen", "127.0.0.1:9000", "--public-dir", `/srv/heron site "x" $literal% \end`, "--timezone", "UTC"}
	e := newHubInstalled(t, args...)
	want := hubCmd + ` "--db=/var/lib/heron/heron.db" "--listen=127.0.0.1:9000" "--public-dir=/srv/heron site \"x\" $$literal%% \\end" "--timezone=UTC"`
	if got := e.execStart(); got != want {
		t.Fatalf("first install ExecStart=%s\nwant                    %s", got, want)
	}
	first := e.file(hubUnit)
	for i, rerun := range [][]string{args, args, nil} {
		out, code := e.hubInstall(rerun...)
		if code != 0 {
			t.Fatalf("rerun %d exit %d:\n%s", i, code, out)
		}
		if got := e.file(hubUnit); got != first {
			t.Fatalf("rerun %d changed the unit:\n%s\nwant\n%s", i, got, first)
		}
	}
}

// 停旧服务之前先查端口：别的进程占着、或查不出持有者时拒绝，旧服务照常运行；持有者是 hub 自己时照常升级。
func TestHubPortConflictLeavesTheServiceRunning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, holder, wantErr string
	}{
		{"another process", "777", "port 9000 is already in use by pid 777; old service was not stopped"},
		{"listener without a holder", "", "port 9000 is in use; cannot identify listener pid; old service was not stopped"},
		{"the hub itself", "4242", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newHubInstalled(t, "--listen", "127.0.0.1:9000")
			// 9000 = 0x2328；状态 0A 是 LISTEN。
			e.appendTo("proc/net/tcp", "   0: 0100007F:2328 00000000:0000 0A 00000000:00000000 00:00000000 00000000   481        0 5555 1 0000000000000000 100 0 0 10 0\n")
			if tc.holder != "" {
				fd := filepath.Join(e.root, "proc", tc.holder, "fd", "3")
				if err := os.MkdirAll(filepath.Dir(fd), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("socket:[5555]", fd); err != nil {
					t.Fatal(err)
				}
			}
			out, code := e.hubInstall()
			if tc.wantErr == "" {
				if code != 0 || !strings.Contains(out, hubDone) {
					t.Fatalf("exit %d:\n%s", code, out)
				}
				return
			}
			if code != 1 || !strings.Contains(out, tc.wantErr) {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			e.assertUntouched(out)
		})
	}
}

// 数据目录不是真目录、库文件不是链接数为 1 的普通文件时，在停服前拒绝：旧服务照常运行，不对任何东西
// chown、chmod——链接指向的文件不被改动。
func TestHubDatabaseThatIsNotPlainIsRefusedBeforeStopping(t *testing.T) {
	t.Parallel()
	notPlain := "exists but is not a regular file with a single link"
	for _, tc := range []struct {
		name, rel, want string
		plant           func(path, outside string) error
	}{
		{"database symlink", "heron.db", notPlain, func(p, outside string) error { os.Remove(p); return os.Symlink(outside, p) }},
		{"dangling database symlink", "heron.db", notPlain, func(p, outside string) error { os.Remove(p); return os.Symlink(outside+".missing", p) }},
		// 硬链接：[ -L ] 为假、[ -f ] 为真，只有链接数看得出它另有名字。
		{"database hard link", "heron.db", notPlain, func(p, outside string) error { os.Remove(p); return os.Link(outside, p) }},
		{"WAL symlink", "heron.db-wal", notPlain, func(p, outside string) error { return os.Symlink(outside, p) }},
		{"WAL hard link", "heron.db-wal", notPlain, func(p, outside string) error { return os.Link(outside, p) }},
		{"SHM is a directory", "heron.db-shm", notPlain, func(p, _ string) error { return os.Mkdir(p, 0o755) }},
		{"data directory symlink", "", "exists but is not a directory", func(p, outside string) error {
			if err := os.RemoveAll(p); err != nil {
				return err
			}
			return os.Symlink(filepath.Dir(outside), p)
		}},
		{"data directory is a file", "", "exists but is not a directory", func(p, _ string) error {
			if err := os.RemoveAll(p); err != nil {
				return err
			}
			return os.WriteFile(p, nil, 0o644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newHubInstalled(t)
			e.put("etc/outside/heron.db", "root's\n")
			outside := filepath.Join(e.root, "etc/outside/heron.db")
			if err := tc.plant(filepath.Join(e.root, hubData, tc.rel), outside); err != nil {
				t.Fatal(err)
			}
			out, code := e.hubInstall()
			if code != 1 || !strings.Contains(out, tc.want) || !strings.Contains(out, "old service was not stopped") {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			e.assertUntouched(out)
			for _, call := range e.calls() {
				if strings.HasPrefix(call, "chown ") || strings.HasPrefix(call, "chmod ") {
					t.Errorf("%q runs although the upgrade was refused", call)
				}
			}
			e.mode("etc/outside/heron.db", 0o644)
		})
	}
}

// 停服前的预检通过之后、目录交给 root 之前，服务组还能替换其中的条目。替身在目录 chown 的那一刻把 heron.db
// 换成指向目录外文件的硬链接：锁内复检要看到它并拒绝，目录外的文件不被改动，目录留在 0750，报错说明 hub 已停。
func TestHubDatabaseSwappedAfterThePreStopCheckIsRefused(t *testing.T) {
	t.Parallel()
	e := newHubInstalled(t)
	e.put("etc/outside", "root's\n")
	e.vars = []string{"STUB_SWAP_DB=" + filepath.Join(e.root, "etc/outside")}
	out, code := e.hubInstall()
	for _, want := range []string{
		"heron.db exists but is not a regular file with a single link",
		"database files changed after the pre-stop check; " + e.root + "/" + hubData + " stays locked at 0750",
		"heron-hub is stopped; rerun the installer or start it manually",
	} {
		if code != 1 || !strings.Contains(out, want) {
			t.Fatalf("exit %d, want %q:\n%s", code, want, out)
		}
	}
	e.mode("etc/outside", 0o644)
	e.mode(hubData, 0o750)
	c := e.calls()
	if index(c, "chown heron-hub:heron-hub") >= 0 || index(c, "systemctl start heron-hub") >= 0 {
		t.Fatalf("nothing may be handed to heron-hub or started: calls %q", c)
	}
	// 安全检查停下时旧二进制换回来、不启动：人查看后手动启动或重跑安装器，跑的都是原来的版本。
	if got := e.file("usr/local/bin/heron-hub"); !strings.Contains(got, "# v1 amd64") || e.exists("usr/local/bin/heron-hub.bak") {
		t.Fatalf("the previous binary must be back without a .bak, got %q", got)
	}
}

// 库文件的属主与权限每次安装都设：root 手工操作留下的 0644、崩溃留下的 WAL 都在重跑后回到服务用户 0600。
// 顺序承载安全性：停服之后先把目录交给 root 并收成 0750，才对其中的文件 chown、chmod，最后放回 0770。
func TestHubDatabaseOwnershipIsRestoredOnEveryInstall(t *testing.T) {
	t.Parallel()
	e := newHubInstalled(t)
	dir := filepath.Join(e.root, hubData)
	if err := os.Chmod(filepath.Join(dir, "heron.db"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.put(hubData+"/heron.db-wal", "")
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := e.hubInstall()
	if code != 0 || !strings.Contains(out, hubDone) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	e.mode(hubData, 0o770)
	e.mode(hubData+"/heron.db", 0o600)
	e.mode(hubData+"/heron.db-wal", 0o600)
	c := e.calls()
	at := func(call string) int {
		i := slices.Index(c, call)
		if i < 0 {
			t.Fatalf("missing %q in calls %q", call, c)
		}
		return i
	}
	stop := at("systemctl stop heron-hub")
	take := at("chown root:heron-hub " + dir)
	lock := at("chmod 0750 " + dir)
	release := at("chmod 0770 " + dir)
	start := at("systemctl start heron-hub")
	if !(stop < take && take < lock && lock < release && release < start) {
		t.Fatalf("want stop < chown root:heron-hub < chmod 0750 < chmod 0770 < start, calls %q", c)
	}
	for _, f := range []string{"heron.db", "heron.db-wal"} {
		for _, call := range []string{"chown heron-hub:heron-hub " + dir + "/" + f, "chmod 0600 " + dir + "/" + f} {
			if i := at(call); i < lock || i > release {
				t.Errorf("%q must run while the directory is locked, calls %q", call, c)
			}
		}
	}
}

// hub 自己的 Telegram 出站走 https：下载地址不是 https 时也要有 CA 证书包，没有就装。
func TestHubCABundleIsInstalledForPlainDownloads(t *testing.T) {
	t.Parallel()
	e := newHubHost(t)
	if err := os.Remove(filepath.Join(e.root, "etc/ssl/certs/ca-certificates.crt")); err != nil {
		t.Fatal(err)
	}
	out, code := e.hubInstall()
	if code != 0 || !strings.Contains(out, hubDone) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if c := e.calls(); !slices.Contains(c, "apt-get install -y ca-certificates") {
		t.Fatalf("file:// download on a host without a CA bundle must still install ca-certificates, calls %q", c)
	}
}

// 停服务之后的失败补一句 hub 已停；停服务之前的失败与成功都不说。
func TestHubSaysItIsStoppedOnlyAfterStopping(t *testing.T) {
	t.Parallel()
	e := newHubInstalled(t)
	e.vars = []string{"STUB_START_FAILS=1"}
	out, code := e.hubInstall()
	if code == 0 || !strings.Contains(out, "Job for heron-hub.service failed.") || !strings.Contains(out, "heron-hub is stopped; rerun the installer or start it manually") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	e.vars = nil
	out, code = e.hubInstall()
	if code != 0 || strings.Contains(out, "heron-hub is stopped") {
		t.Fatalf("a successful rerun must not say the hub is stopped: exit %d:\n%s", code, out)
	}
	e.resetCalls()
	out, code = e.hubInstall("--listen", "127.0.0.1:http")
	if code != 1 || !strings.Contains(out, "listen address must end in a numeric TCP port") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if index(e.calls(), "systemctl stop") >= 0 || !e.exists("proc/4242/status") || strings.Contains(out, "heron-hub is stopped") {
		t.Fatalf("a failure before stopping must leave the hub running and not say it is stopped: calls %q\n%s", e.calls(), out)
	}
}

// 没有终端时卸载必须带 --yes：不带时什么都不停；带上时停服务、删单元与二进制，保留数据与账户。
func TestHubUninstallWithoutATerminalNeedsYes(t *testing.T) {
	t.Parallel()
	e := newHubInstalled(t)
	out, code := e.run("--uninstall")
	if code != 1 || !strings.Contains(out, "uninstall requires --yes without a terminal") || index(e.calls(), "systemctl") >= 0 {
		t.Fatalf("exit %d, calls %q:\n%s", code, e.calls(), out)
	}
	out, code = e.run("--uninstall", "--yes")
	if code != 0 || !strings.Contains(out, "heron-hub uninstalled") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if e.exists(hubUnit) || e.exists("usr/local/bin/heron-hub") || !e.exists(hubData+"/heron.db") || !strings.Contains(e.file("etc/passwd"), "heron-hub:") {
		t.Fatal("uninstall must remove the unit and binary and keep the data and account")
	}
}

// 安装器认得的 serve 参数（SERVE_FLAGS 加上固定的 --db）与 cmd/hub/serve.go 定义的 flag 是同一张表的两处写法。
// serve 加了参数而安装器没加时，装过这个参数的单元升级会在停服前被拒绝；反过来安装器会写出 serve 不认的参数，
// hub 起不来。usage 也要列出每个可覆盖的参数。
// serve 侧的表从源码解析：flag 名既可能是字面量，也可能是 cmd/hub 里的字符串常量（serve 用常量拼错误前缀与
// usage），只认字面量的核对会把常量定义的 flag 当成 serve 没有，安装器漏了它也不会红。解析不出名字就失败，不跳过。
func TestHubFlagTableAgreesWithServe(t *testing.T) {
	t.Parallel()
	serve := serveFlagNames(t)
	script, err := os.ReadFile("install-hub.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^SERVE_FLAGS='([^']*)'$`).FindStringSubmatch(string(script))
	if m == nil {
		t.Fatal("install-hub.sh has no SERVE_FLAGS line")
	}
	overridable := strings.Fields(m[1])
	installer := append([]string{"db"}, overridable...)
	slices.Sort(serve)
	slices.Sort(installer)
	if !slices.Equal(serve, installer) {
		t.Fatalf("serve defines %q; the installer knows %q", serve, installer)
	}
	for _, f := range overridable {
		if !strings.Contains(string(script), "[--"+f+" ") {
			t.Errorf("usage does not list --%s", f)
		}
	}
}

// flagSetMethods 把 *flag.FlagSet 的每个方法分成定义 flag 的（值是 flag 名所在的参数位置）与不定义的（-1）。
// 表必须覆盖全部方法：TestFlagSetMethodsClassified 用反射核对，Go 新增方法时这里先登记，serveFlagNames 才不会
// 把新写法静默跳过。
var flagSetMethods = map[string]int{
	"Bool": 0, "BoolFunc": 0, "BoolVar": 1, "Duration": 0, "DurationVar": 1, "Float64": 0, "Float64Var": 1,
	"Func": 0, "Int": 0, "Int64": 0, "Int64Var": 1, "IntVar": 1, "String": 0, "StringVar": 1, "TextVar": 1,
	"Uint": 0, "Uint64": 0, "Uint64Var": 1, "UintVar": 1, "Var": 1,
	"Arg": -1, "Args": -1, "ErrorHandling": -1, "Init": -1, "Lookup": -1, "NArg": -1, "NFlag": -1, "Name": -1,
	"Output": -1, "Parse": -1, "Parsed": -1, "PrintDefaults": -1, "Set": -1, "SetOutput": -1, "Visit": -1,
	"VisitAll": -1,
}

func TestFlagSetMethodsClassified(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[*flag.FlagSet]()
	var methods []string
	for i := range typ.NumMethod() {
		methods = append(methods, typ.Method(i).Name)
	}
	known := slices.Sorted(maps.Keys(flagSetMethods))
	if !slices.Equal(methods, known) {
		t.Fatalf("*flag.FlagSet has methods %q; flagSetMethods classifies %q", methods, known)
	}
}

// serveFlagNames 返回 cmd/hub/serve.go 里在 flag.NewFlagSet 建出的集合上定义的全部 flag 名。名字参数是标识符时按
// cmd/hub 包（不含测试文件）顶层的字符串常量解析；别的写法（变量、拼接、别处的 FlagSet 方法值）这里认不出，
// 直接失败，由改 serve.go 的人决定是改回常量还是扩展这里。
func serveFlagNames(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("../cmd/hub/*.go")
	if err != nil {
		t.Fatal(err)
	}
	consts := map[string]string{}
	var serve *ast.File
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(name) == "serve.go" {
			serve = f
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							consts[id.Name], _ = strconv.Unquote(lit.Value)
						}
					}
				}
			}
		}
	}
	if serve == nil {
		t.Fatal("cmd/hub/serve.go not found")
	}
	sets := map[string]bool{}
	var names []string
	ast.Inspect(serve, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) == 1 && len(n.Rhs) == 1 && isCall(n.Rhs[0], "flag", "NewFlagSet") {
				if id, ok := n.Lhs[0].(*ast.Ident); ok {
					sets[id.Name] = true
				}
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok || !sets[recv.Name] {
				return true
			}
			pos, ok := flagSetMethods[sel.Sel.Name]
			if !ok {
				t.Fatalf("%s: unclassified FlagSet method %s", fset.Position(n.Pos()), sel.Sel.Name)
			}
			if pos < 0 {
				return true
			}
			if pos >= len(n.Args) {
				t.Fatalf("%s: %s has no name argument", fset.Position(n.Pos()), sel.Sel.Name)
			}
			switch arg := n.Args[pos].(type) {
			case *ast.BasicLit:
				if arg.Kind != token.STRING {
					t.Fatalf("%s: flag name is not a string literal", fset.Position(arg.Pos()))
				}
				name, err := strconv.Unquote(arg.Value)
				if err != nil {
					t.Fatal(err)
				}
				names = append(names, name)
			case *ast.Ident:
				name, ok := consts[arg.Name]
				if !ok {
					t.Fatalf("%s: flag name %s is not a string constant of package cmd/hub", fset.Position(arg.Pos()), arg.Name)
				}
				names = append(names, name)
			default:
				t.Fatalf("%s: flag name is neither a string literal nor a constant", fset.Position(n.Args[pos].Pos()))
			}
		}
		return true
	})
	if len(sets) == 0 {
		t.Fatal("serve.go builds no flag.FlagSet")
	}
	return names
}

func isCall(e ast.Expr, pkg, fn string) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == pkg && sel.Sel.Name == fn
}

// unit_enabled 只看 $WANTS 这条链接，前提是发布包里 heron-hub.service 的 [Install] 恰好只有 WantedBy=<target>，且
// <target> 与安装器 WANTS 路径里的 <target>.wants 是同一个：systemctl enable 建出的才正是这条链接。单元改了
// WantedBy 而安装器没改时，enable 建的是另一条链接，安装器会把仍 enabled 的单元说成没 enable，卸载也删不掉真正
// 的链接；替身把链接路径写死，测不出这种分叉，所以两处写法在这里静态核对。
func TestHubUnitInstallTargetAgreesWithWantsPath(t *testing.T) {
	t.Parallel()
	unit, err := os.ReadFile("systemd/heron-hub.service")
	if err != nil {
		t.Fatal(err)
	}
	install := regexp.MustCompile(`(?ms)^\[Install\]\n(.*?)(?:^\[|\z)`).FindStringSubmatch(string(unit))
	if install == nil {
		t.Fatal("heron-hub.service has no [Install] section")
	}
	var wantedBy []string
	for _, line := range strings.Split(strings.TrimSpace(install[1]), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key != "WantedBy" {
			t.Fatalf("[Install] line %q: unit_enabled assumes the section holds only WantedBy", line)
		}
		wantedBy = append(wantedBy, value)
	}
	if len(wantedBy) != 1 {
		t.Fatalf("[Install] has WantedBy %q; unit_enabled assumes exactly one wants link", wantedBy)
	}
	script, err := os.ReadFile("install-hub.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^WANTS=\$ROOT/etc/systemd/system/([^/]+)\.wants/heron-hub\.service$`).FindStringSubmatch(string(script))
	if m == nil {
		t.Fatal("install-hub.sh has no WANTS=$ROOT/etc/systemd/system/<target>.wants/heron-hub.service line")
	}
	if m[1] != wantedBy[0] {
		t.Fatalf("the unit is WantedBy=%s but the installer looks for the link under %s.wants", wantedBy[0], m[1])
	}
}

// 命令行只接受 SERVE_FLAGS 里的参数：--db 固定、表外的参数与 --flag=value 写法都以用法错误退出，什么都不做。
// 带空格的名字按子串会命中参数表（" listen timezone " 在表里），写进单元就是 serve 不认的参数。
func TestHubCommandLineRefusesFlagsOutsideTheTable(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--db", "/srv/other.db"},
		{"--foo", "bar"},
		{"--listen=127.0.0.1:9000"},
		{"--listen timezone", "x"},
		{"--*", "x"},
	} {
		e := newHubHost(t)
		out, code := e.hubInstall(args...)
		// calls 为空文件时 e.calls() 是一个空串。
		if c := e.calls(); code != 2 || !strings.Contains(out, "usage: install-hub.sh") || len(c) != 1 || c[0] != "" {
			t.Fatalf("%q: exit %d, calls %q:\n%s", args, code, c, out)
		}
	}
}

func TestHubRemovedThemeOriginIsExplained(t *testing.T) {
	for _, args := range [][]string{{"--theme-origin", "https://status.example.com"}, {"--theme-origin=https://status.example.com"}} {
		e := newHubHost(t)
		out, code := e.hubInstall(args...)
		if code != 2 || !strings.Contains(out, "themes now use the panel hostname") {
			t.Fatalf("removed theme flag: exit=%d output=%s", code, out)
		}
		if c := e.calls(); len(c) != 1 || c[0] != "" {
			t.Fatalf("removed flag touched host: %v", c)
		}
	}
}

// stop 失败或旧进程不退时 hub 并没有停下：报出原因、退出非零，不说已停，也不换二进制、不启动。
func TestHubFailedStopDoesNotSayStopped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, env, want string }{
		{"stop fails", "STUB_STOP_FAILS=1", "failed to stop heron-hub"},
		{"old process lingers", "STUB_STOP_LEAVES_PROCESS=1", "heron-hub is still running: uid 481 processes: 4242"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newHubInstalled(t)
			e.vars = []string{tc.env}
			out, code := e.hubInstall()
			if code != 1 || !strings.Contains(out, tc.want) || strings.Contains(out, "heron-hub is stopped") {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			if !strings.Contains(e.file("usr/local/bin/heron-hub"), "# v1 amd64") || index(e.calls(), "systemctl start heron-hub") >= 0 {
				t.Fatalf("nothing may be replaced or started after a failed stop: calls %q", e.calls())
			}
		})
	}
}

// 首装时单元文件还不存在，DropInPaths 为空，heron-hub.service.d/ 里预先写入的 drop-in 查不到。
// 主单元写好之后的那一遍要拦住设了 ExecStart 的 drop-in：不 enable、不 start。提示只说成立的事实：单元装了、
// 没 enable 也没起；该做的是先处理 drop-in 再重跑，不能叫人手动启动，那会按 drop-in 的参数起来。
func TestHubFirstInstallRefusesAnExecStartDropIn(t *testing.T) {
	t.Parallel()
	e := newHubHost(t)
	e.put("etc/systemd/system/heron-hub.service.d/override.conf", "[Service]\nExecStart=\nExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db\n")
	e.write("dropins-disk", "/etc/systemd/system/heron-hub.service.d/override.conf\n")
	out, code := e.hubInstall()
	for _, want := range []string{
		"drop-in /etc/systemd/system/heron-hub.service.d/override.conf sets ExecStart",
		"heron-hub is installed but not enabled or started",
		"fix the drop-in problem reported above, then rerun the installer",
	} {
		if code != 1 || !strings.Contains(out, want) {
			t.Fatalf("exit %d, want %q:\n%s", code, want, out)
		}
	}
	if strings.Contains(out, "start it manually") || strings.Contains(out, "heron-hub is stopped") {
		t.Fatalf("a first install refused over a drop-in must not suggest starting heron-hub:\n%s", out)
	}
	if c := e.calls(); index(c, "systemctl enable heron-hub") >= 0 || index(c, "systemctl start heron-hub") >= 0 {
		t.Fatalf("a refused first install must not enable or start heron-hub: calls %q", c)
	}
}

// 停服前那一遍之后才落盘的 drop-in：主单元写好、start 之前的那一遍要拦住它，不启动。上次安装的 enable 还在，
// 提示要说清 hub 已停但仍是 enabled、手动启动或下次开机都会带着 drop-in 起来，该做的是先处理 drop-in 再重跑。
func TestHubDropInWrittenWhileStoppedIsRefusedBeforeStart(t *testing.T) {
	t.Parallel()
	e := newHubInstalled(t)
	e.put("etc/systemd/system/heron-hub.service.d/late.conf", "[Service]\nExecStart=\nExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db\n")
	e.vars = []string{"STUB_DROPIN_ON_STOP=/etc/systemd/system/heron-hub.service.d/late.conf"}
	out, code := e.hubInstall()
	for _, want := range []string{
		"late.conf sets ExecStart",
		"heron-hub is stopped but still enabled; started by hand or at the next boot, it would run with the drop-ins as they are",
		"fix the drop-in problem reported above, then rerun the installer",
	} {
		if code != 1 || !strings.Contains(out, want) {
			t.Fatalf("exit %d, want %q:\n%s", code, want, out)
		}
	}
	if strings.Contains(out, "start it manually") {
		t.Fatalf("a drop-in refusal must not suggest starting heron-hub by hand:\n%s", out)
	}
	if c := e.calls(); index(c, "systemctl start heron-hub") >= 0 {
		t.Fatalf("heron-hub must not be started: calls %q", c)
	}
	if got := e.file("usr/local/bin/heron-hub"); !strings.Contains(got, "# v1 amd64") || e.exists("usr/local/bin/heron-hub.bak") {
		t.Fatalf("the previous binary must be back without a .bak, got %q", got)
	}
}

// systemctl start 返回 0 之后，hub 已交给 systemd 按 Restart=always 拉起，不再是"停着"：启动确认失败只报启动失败，
// 不补 hub 已停那句。
func TestHubStartConfirmationFailureDoesNotSayStopped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, env, want string }{
		{"process never appears", "STUB_START_NO_PROCESS=1", "heron-hub did not start; see journalctl -u heron-hub"},
		{"process exits right away", "STUB_START_DIES=1", "heron-hub did not stay running (pid 4242); see journalctl -u heron-hub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newHubInstalled(t)
			e.vars = []string{tc.env}
			out, code := e.hubInstall()
			if code != 1 || !strings.Contains(out, tc.want) || strings.Contains(out, "heron-hub is stopped") {
				t.Fatalf("exit %d:\n%s", code, out)
			}
		})
	}
}

// systemctl 本身失败时 drop-in 还没被查过：报 systemctl 的原文，按失败点说该做什么，不说成 drop-in 的问题，
// 也不叫人手动启动。停服前失败时旧服务照常运行。写好主单元之后失败时，现状按 enable 链接说：升级时上次安装的
// 链接还在就是仍 enabled；首装、或此前被 disable 过，就是没 enable。这时的 systemctl 已经出过错，is-enabled 可能
// 随之一起失败，每种情形都在它照常应答与一起失败两种状态下各跑一遍，说出的现状不能变。
func TestHubSystemctlFailureIsNotReportedAsADropInProblem(t *testing.T) {
	t.Parallel()
	const (
		reload     = "systemctl daemon-reload failed: Failed to reload daemon: Access denied"
		enabled    = "heron-hub is stopped but still enabled; started by hand or at the next boot, it would run with the drop-ins as they are"
		notEnabled = "heron-hub is installed but not enabled or started"
		hint       = "fix the systemctl problem reported above, then rerun the installer"
	)
	installed := func(t *testing.T) *env { return newHubInstalled(t) }
	for _, tc := range []struct {
		name      string
		host      func(t *testing.T) *env
		env       string
		want      []string
		forbidden string
	}{
		{"before stopping", installed, "STUB_RELOAD_FAILS=1", []string{reload, "old service was not stopped"}, ""},
		{"after stopping", installed, "STUB_RELOAD_FAILS_ON_STOP=1", []string{reload, enabled, hint}, notEnabled},
		{"after stopping a disabled hub", func(t *testing.T) *env {
			e := newHubInstalled(t)
			if !e.enableLink() {
				t.Fatalf("an installed hub must be enabled through %s", hubWants)
			}
			if err := os.Remove(filepath.Join(e.root, hubWants)); err != nil {
				t.Fatal(err)
			}
			return e
		}, "STUB_RELOAD_FAILS_ON_STOP=1", []string{reload, notEnabled, hint}, enabled},
		{"on first install", newHubHost, "STUB_RELOAD_FAILS=1", []string{reload, notEnabled, hint}, enabled},
	} {
		for _, isEnabled := range []struct{ name, env string }{
			{"is-enabled answers", ""},
			{"is-enabled fails too", "STUB_IS_ENABLED_FAILS_WITH_RELOAD=1"},
		} {
			t.Run(tc.name+"/"+isEnabled.name, func(t *testing.T) {
				t.Parallel()
				e := tc.host(t)
				e.vars = []string{tc.env}
				if isEnabled.env != "" {
					e.vars = append(e.vars, isEnabled.env)
				}
				out, code := e.hubInstall()
				for _, want := range tc.want {
					if code != 1 || !strings.Contains(out, want) {
						t.Fatalf("exit %d, want %q:\n%s", code, want, out)
					}
				}
				if tc.forbidden != "" && strings.Contains(out, tc.forbidden) {
					t.Fatalf("must not say %q:\n%s", tc.forbidden, out)
				}
				if strings.Contains(out, "drop-in problem") || strings.Contains(out, "start it manually") || index(e.calls(), "systemctl start heron-hub") >= 0 {
					t.Fatalf("a systemctl failure must not be reported as a drop-in problem, suggest a manual start, or start heron-hub: calls %q\n%s", e.calls(), out)
				}
				if tc.name == "before stopping" {
					e.assertUntouched(out)
				}
			})
		}
	}
}

// 卸载与安装用同一个判定回答单元是否 enabled，卸载后 enable 链接不能留下。主单元还在时由 systemctl disable 删；
// 主单元已被手动删掉、链接悬空时安装器不调 disable，由它自己删。
func TestHubUninstallRemovesTheEnableLink(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		unitGone bool
	}{{"unit in place", false}, {"unit already deleted", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newHubInstalled(t)
			if !e.enableLink() {
				t.Fatalf("an installed hub must be enabled through %s", hubWants)
			}
			if tc.unitGone {
				// 管理员已停掉服务并删了主单元，只剩悬空的 enable 链接。
				for _, rel := range []string{hubUnit, "proc/4242"} {
					if err := os.RemoveAll(filepath.Join(e.root, rel)); err != nil {
						t.Fatal(err)
					}
				}
			}
			out, code := e.run("--uninstall", "--yes")
			if code != 0 || !strings.Contains(out, "heron-hub uninstalled") || e.enableLink() {
				t.Fatalf("exit %d, enable link left: %v, calls %q:\n%s", code, e.enableLink(), e.calls(), out)
			}
		})
	}
}

// 全新主机上建号避开运行中进程占用的号：postgres 容器进程以 uid 999 运行、宿主账户文件里没有它的记录，
// useradd 自动取号会取到 999。取号应为 998，用户与组同号并显式传给 groupadd/useradd。
func TestHubFreshInstallPicksAnIdUnusedByRunningProcesses(t *testing.T) {
	t.Parallel()
	e := newHubHost(t)
	e.put("etc/passwd", "root:x:0:0:root:/root:/bin/sh\n")
	e.put("etc/group", "root:x:0:\n")
	e.put("proc/777/status", "Name:\tpostgres\nState:\tS (sleeping)\nUid:\t999\t999\t999\t999\n")
	foreignExe(t, e, "proc/777")
	out, code := e.hubInstall()
	if code != 0 || !strings.Contains(out, hubDone) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	c := e.calls()
	if index(c, "groupadd --system -g 998 heron-hub") < 0 || index(c, "useradd --system -u 998 -M -d /nonexistent -s /sbin/nologin -g heron-hub heron-hub") < 0 {
		t.Errorf("both tools must get an explicit id below 999, calls %q", c)
	}
	if !strings.Contains(e.file("etc/passwd"), "heron-hub:x:998:998:") {
		t.Errorf("account not created at 998:\n%s", e.file("etc/passwd"))
	}
}

// 已有账户的 uid 被外来进程共用（撞号）时，在任何下载、停服务与系统变更之前拒绝。
func TestHubRefusesAccountUidSharedWithForeignProcesses(t *testing.T) {
	t.Parallel()
	e := newHubHost(t)
	e.put("proc/777/status", "Name:\tpostgres\nState:\tS (sleeping)\nUid:\t481\t481\t481\t481\n")
	foreignExe(t, e, "proc/777")
	e.resetCalls()
	out, code := e.hubInstall()
	if code != 1 || !strings.Contains(out, "is also used by other running processes") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if c := e.calls(); index(c, "curl") >= 0 || index(c, "systemctl stop heron-hub") >= 0 {
		t.Errorf("refused before any download or stop, calls %q", c)
	}
}

// 撞号机器上的卸载不再被外来进程卡住：停服确认只等本服务二进制的进程，外来同 uid 进程直接略过。
func TestHubUninstallIgnoresForeignProcessesSharingTheUid(t *testing.T) {
	t.Parallel()
	e := newHubInstalled(t)
	e.put("proc/777/status", "Name:\tpostgres\nState:\tS (sleeping)\nUid:\t481\t481\t481\t481\n")
	foreignExe(t, e, "proc/777")
	out, code := e.run("--uninstall", "--yes")
	if code != 0 || !strings.Contains(out, "heron-hub uninstalled") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}
