// Package deploy 的测试以普通用户运行安装脚本：脚本读写的系统路径经 HERON_INSTALL_ROOT 挂到临时目录，
// 系统管理命令由 PATH 上的替身接管。下载的脚本是发布目标产出的那种写入了本版清单的脚本：dist 里的包由各平台的
// 发布函数打出，再由 deploy/releasestamp（发布目标用的同一个写入实现）写进脚本，用例执行的是写入后的副本。
// 本文件测 install-macos.sh：dscl、launchctl、ps、id、sysctl、uname、chown、sleep、curl 是替身，find、chmod 经替身
// 记下参数后转调真的，shasum、tar 用真的；真实 launchd、目录服务与 root 属主只在真机上验证
// （没有 macOS 虚拟机可用）。install.sh 的替身在 installlinux_test.go。
package deploy

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/xjetry/heron/deploy/releasestamp"
)

const svcUser = "_heron-agent"

// 替身共用的状态目录：users/<名> 存 "uid gid"，groups/<名> 存 "gid"，procs 每行 "uid pid comm"，
// loaded 表示作业已载入，calls 按调用顺序记下每个替身收到的参数。
// launchd 起的 agent 的 comm 是 plist 的 ProgramArguments[0]；STUB_HELPER 另起一个同 uid 的 cfprefsd，
// 它不属于作业，bootout 不带走它（launchd 按 uid 派生的辅助进程就是这样）。
// dscl、launchctl 与假 agent 读尽 stdin：脚本以 sh -s 从 stdin 运行，
// 漏掉 </dev/null 的调用会吞掉脚本余下部分，安装在中途无声结束，测试看不到最后一行。
var stubs = map[string]string{
	"id": `#!/bin/sh
S=$STUB_STATE
if [ "$#" = 1 ] && [ "$1" = -u ]; then echo "${STUB_UID:-0}"; exit 0; fi
flag=""; [ "$#" = 2 ] && { flag=$1; shift; }
f="$S/users/$1"
# remote-users 是解析得到、但不在本地节点里的账户（别的目录节点的同名账户，或陈旧的缓存）。
[ -f "$f" ] || f="$S/remote-users/$1"
[ -f "$f" ] || { echo "id: $1: no such user" >&2; exit 1; }
read -r uid gid < "$f"
case "$flag" in
  "") echo "uid=$uid($1) gid=$gid";;
  -u) echo "$uid";;
  -g) echo "$gid";;
  -gn)
    for g in "$S"/groups/*; do
      [ -f "$g" ] || continue
      if [ "$(cat "$g")" = "$gid" ]; then basename "$g"; exit 0; fi
    done
    echo "id: group $gid not found" >&2; exit 1;;
esac
`,
	"dscl": `#!/bin/sh
cat > /dev/null
S=$STUB_STATE
echo "dscl $*" >> "$S/calls"
[ "$1" = . ] || exit 64
op=$2; path=$3; key=${4-}; val=${5-}
kind=${path#/}; kind=${kind%%/*}; name=${path##*/}
case "$op" in
  -list)
    [ -z "${STUB_DSCL_LIST_FAILS-}" ] || { echo "list failed" >&2; exit 71; }
    cat "$S/sys-$kind" 2>/dev/null
    for f in "$S/$(echo "$kind" | tr 'UG' 'ug')"/*; do
      [ -f "$f" ] || continue
      read -r a b < "$f"
      echo "$(basename "$f") $a"
    done;;
  -read)
    f="$S/$(echo "$kind" | tr 'UG' 'ug')/$name"
    [ -f "$f" ] || { echo "<dscl_cmd> DS Error: -14136 (eDSRecordNotFound)" >&2; exit 56; }
    read -r a b < "$f"; echo "$key: $a";;
  -create)
    case "$kind/$key" in
      Groups/PrimaryGroupID) echo "$val" > "$S/groups/$name";;
      Users/UniqueID) [ -n "${STUB_DSCL_DROP_USER-}" ] || echo "$val" > "$S/users/$name";;
      Users/PrimaryGroupID) [ -f "$S/users/$name" ] && { read -r u g < "$S/users/$name"; echo "$u $val" > "$S/users/$name"; };;
    esac;;
  -delete)
    f="$S/$(echo "$kind" | tr 'UG' 'ug')/$name"
    [ -f "$f" ] || { echo "<main> delete status: eDSRecordNotFound" >&2; exit 56; }
    [ -n "${STUB_DSCL_KEEP-}" ] || rm -f "$f";;
esac
exit 0
`,
	// STUB_LAUNCHCTL_FAILS_ONCE 与 STUB_LAUNCHCTL_TERM_ONCE 各是一条完整的参数串，只在第一次匹配时生效：前者让这次
	// 调用失败（bootout 刚返回就 bootstrap 报 EIO 这类一次性故障），后者给安装脚本发 SIGTERM（SSH 断开）。只生效一次，
	// 回滚路径里的同一条命令照常成功。包里带 heron-test-broken 标记的 heron-agent 模拟起不来的新二进制：bootstrap
	// 照常返回 0，进程不在。
	"launchctl": `#!/bin/sh
cat > /dev/null
S=$STUB_STATE
echo "launchctl $*" >> "$S/calls"
if [ "$*" = "${STUB_LAUNCHCTL_FAILS_ONCE-}" ] && [ ! -f "$S/failed-once" ]; then
  : > "$S/failed-once"; echo "Bootstrap failed: 5: Input/output error" >&2; exit 5
fi
if [ "$*" = "${STUB_LAUNCHCTL_TERM_ONCE-}" ] && [ ! -f "$S/termed-once" ]; then
  : > "$S/termed-once"; kill -TERM "$PPID"
fi
case "$1" in
  print) [ -f "$S/loaded" ] && exit 0; echo "Could not find service \"${2#*/}\" in domain for system" >&2; exit 113;;
  bootout)
    [ -f "$S/loaded" ] || { echo "Boot-out failed: 3: No such process" >&2; exit 3; }
    rm -f "$S/loaded"
    if [ -z "${STUB_BOOTOUT_LEAVES_PROCESS-}" ]; then
      grep -v ' /usr/local/bin/heron-agent$' "$S/procs" > "$S/procs.new"; mv "$S/procs.new" "$S/procs"
    fi
    exit 0;;
  enable) exit 0;;
  bootstrap)
    [ -f "$3" ] || { echo "Bootstrap failed: 2: No such file or directory" >&2; exit 5; }
    cp "$3" "$S/bootstrapped.plist"
    : > "$S/loaded"
    read -r uid gid < "$S/users/_heron-agent"
    [ -z "${STUB_HELPER-}" ] || grep -q ' 999 ' "$S/procs" || echo "$uid 999 /usr/libexec/cfprefsd" >> "$S/procs"
    [ -n "${STUB_START_FAILS-}" ] && exit 0
    grep -q heron-test-broken "$HERON_INSTALL_ROOT/usr/local/bin/heron-agent" 2>/dev/null && exit 0
    echo "$uid 4242 /usr/local/bin/heron-agent" >> "$S/procs"
    exit 0;;
esac
exit 64
`,
	"ps": `#!/bin/sh
S=$STUB_STATE
case "$*" in
  "-axo uid=,pid=,comm=")
    echo "    0     1 /sbin/launchd"
    echo "  501   777 /Applications/Some App.app/Contents/MacOS/Some App"
    if [ -n "${STUB_RESPAWN-}" ] && [ -s "$S/procs" ]; then
      awk '$3 == "/usr/local/bin/heron-agent" { $2 = $2 + 1 } { print }' "$S/procs" > "$S/procs.new" && mv "$S/procs.new" "$S/procs"
    fi
    cat "$S/procs" 2>/dev/null
    exit 0;;
  # free_id 的取号扫描（uid/gid 是有效 id）：基础进程之外，STUB_PS_IDS 指向的文件每行补一个 "uid gid"。
  "-axo uid=,gid=")
    echo "    0     0"
    echo "  501    20"
    awk '{ print $1, $1 }' "$S/procs" 2>/dev/null
    [ -z "${STUB_PS_IDS-}" ] || cat "$STUB_PS_IDS"
    exit 0;;
esac
echo "unexpected ps $*" >&2; exit 64
`,
	"sysctl": `#!/bin/sh
[ "$*" = "-in hw.optional.arm64" ] || { echo "unexpected sysctl $*" >&2; exit 64; }
printf '%s' "${STUB_ARM64-}"
[ -z "${STUB_ARM64-}" ] || echo
`,
	"uname": `#!/bin/sh
[ "$*" = -m ] || { echo "unexpected uname $*" >&2; exit 64; }
echo "${STUB_UNAME_M:-arm64}"
`,
	"chown": `#!/bin/sh
echo "chown $*" >> "$STUB_STATE/calls"
# STUB_SWAP_LOG 模拟日志目录此前对服务用户可写：在目录交给 root 的那一刻之前，服务用户把 heron-agent.log
# 换成指向目录外文件的硬链接。
d=$HERON_INSTALL_ROOT/Library/Logs/heron-agent
if [ -n "${STUB_SWAP_LOG-}" ] && [ "$1" = root:wheel ] && [ "$2" = "$d" ]; then
  rm -f "$d/heron-agent.log"; ln "$STUB_SWAP_LOG" "$d/heron-agent.log"
fi
# STUB_CHOWN_FAILS 是一个路径：对它 chown 时像文件带了不可变标志那样失败。
if [ -n "${STUB_CHOWN_FAILS-}" ] && [ "$2" = "$STUB_CHOWN_FAILS" ]; then
  echo "chown: $2: Operation not permitted" >&2; exit 1
fi
`,
	"sleep": `#!/bin/sh
echo "sleep $*" >> "$STUB_STATE/calls"
`,
	"curl": fileCurl,
	// find 用真的，先记下参数；STUB_FIND_FAILS 让它像读不到文件时那样只报错、stdout 为空、退出 1。
	"find": `#!/bin/sh
echo "find $*" >> "$STUB_STATE/calls"
[ -z "${STUB_FIND_FAILS-}" ] || { echo "find: $1: Permission denied" >&2; exit 1; }
exec /usr/bin/find "$@"
`,
	// chmod 先记下参数再转调真的，权限断言看的是真实的文件模式。GNU chmod 没有 -N（去掉 ACL），
	// 这组测试也在 Linux 上跑：那里的 -N 只记录。
	"chmod": `#!/bin/sh
echo "chmod $*" >> "$STUB_STATE/calls"
# uname 本身也是替身、只认 -m，取系统名走 command -p 的默认 PATH。
if [ "$1" = -N ] && [ "$(command -p uname -s)" != Darwin ]; then exit 0; fi
exec /bin/chmod "$@"
`,
}

// fileCurl 是三个安装脚本共用的 curl 替身：记下参数（用例据此判断有没有发生下载、下载的是哪个地址），只认 file://
// 地址并复制文件，其余地址像解析失败那样以 6 退出。读尽 stdin，理由同其他替身。
const fileCurl = `#!/bin/sh
cat > /dev/null
echo "curl $*" >> "$STUB_STATE/calls"
out=""; url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2;;
    --proto|--proto-redir) shift 2;;
    -*) shift;;
    *) url=$1; shift;;
  esac
done
case "$url" in
  file://*) cp "${url#file://}" "$out";;
  *) echo "curl: (6) unexpected URL $url" >&2; exit 6;;
esac
`

// fakeAgent 是包里的 heron-agent：register 写出配置并记下参数，与真 agent 的 agentconfig.Save 同为 0600；
// 注册前已有配置时保存其原文，供用例确认安装器没有通过删除配置绕过重新注册。
// configure 只记参数，STUB_CONFIGURE_FAILS 时像拒绝现有配置那样失败。
const fakeAgent = `#!/bin/sh
cat > /dev/null
echo "heron-agent $*" >> "$STUB_STATE/calls"
if [ "$1" = configure ] && [ -n "${STUB_CONFIGURE_FAILS-}" ]; then echo "configure: invalid config" >&2; exit 1; fi
[ "$1" = register ] || exit 0
[ -z "${STUB_REGISTER_FAILS-}" ] || { echo "register: hub unreachable" >&2; exit 1; }
while [ $# -gt 0 ]; do [ "$1" = --config ] && cfg=$2; shift; done
if [ -f "$cfg" ]; then cp "$cfg" "$STUB_STATE/registration-input"; fi
mkdir -p "$(dirname "$cfg")"
echo '{"hub":"h","token":"t"}' > "$cfg"
chmod 0600 "$cfg"
echo "registered as node 1; config written to $cfg"
`

// script 是 run 执行的脚本：起初是仓库里的源码 source（没有写入清单，只能卸载），publish 之后是 dist 里写入后的副本。
type env struct {
	t                *testing.T
	source, script   string
	root, state, bin string
	dist             string
	vars             []string
}

// newStubEnv 建出临时的根目录、替身状态目录与放替身的 PATH 目录，脚本与替身集由调用方给。
func newStubEnv(t *testing.T, script string, stubs map[string]string) *env {
	t.Helper()
	d := t.TempDir()
	e := &env{t: t, source: script, script: script, root: filepath.Join(d, "root"), state: filepath.Join(d, "state"), bin: stubBin(t, stubs), dist: filepath.Join(d, "dist")}
	for _, dir := range []string{e.root, e.state, e.dist} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.write("calls", "")
	return e
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := newStubEnv(t, "install-macos.sh", stubs)
	for _, dir := range []string{"users", "groups"} {
		if err := os.MkdirAll(filepath.Join(e.state, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Apple 的系统账户从 300 往上连号占用（macOS 26.3.1 本机到 308），两个命名空间合起来占满 300–308；
	// 取号从 499 往下，不取紧挨着的 309。系统账户不在替身状态里，只出现在 -list 的输出中。
	e.write("sys-Users", "_taken300 300\n_taken302 302\n_taken304 304\n_taken306 306\n_taken308 308\n")
	e.write("sys-Groups", "_taken301 301\n_taken303 303\n_taken305 305\n_taken307 307\n")
	e.write("procs", "")
	e.release("arm64", "v1")
	e.release("amd64", "v1")
	return e
}

func (e *env) write(name, body string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.state, name), []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// release 发布 version：打出 arch 的包，写入脚本。
func (e *env) release(arch, version string) {
	e.t.Helper()
	e.darwinPackage(e.dist, arch, version)
	e.publish(version)
}

// darwinPackage 按发布配方的形状把 arch 的包打进 dir：包内是 heron-agent 与仓库里的 plist 原件。
func (e *env) darwinPackage(dir, arch, version string) {
	e.t.Helper()
	plist, err := os.ReadFile("launchd/xyz.heron.agent.plist")
	if err != nil {
		e.t.Fatal(err)
	}
	e.pack(dir, "heron-agent_darwin_"+arch+".tar.gz", []packFile{
		{"heron-agent", fakeAgent + "# " + version + " " + arch + "\n", 0o755},
		{"xyz.heron.agent.plist", string(plist), 0o644},
	})
}

type packFile struct {
	name, body string
	mode       int64
}

// pack 把 files 打成 dir 下的 pkg。只打包，不写入脚本：写入由 publish 做，篡改用例靠这一点让脚本里的清单
// 与包不符。
func (e *env) pack(dir, pkg string, files []packFile) {
	e.t.Helper()
	f, err := os.Create(filepath.Join(dir, pkg))
	if err != nil {
		e.t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, m := range files {
		if err := tw.WriteHeader(&tar.Header{Name: m.name, Mode: m.mode, Size: int64(len(m.body))}); err != nil {
			e.t.Fatal(err)
		}
		tw.Write([]byte(m.body))
	}
	tw.Close()
	gz.Close()
	f.Close()
}

// publish 照发布目标的顺序发布 dist 里现有的包：用发布目标的同一个写入实现把版本号与清单写进源码脚本、
// 输出到 dist，再写 SHA256SUMS；此后 run 执行写入后的脚本。
func (e *env) publish(version string) {
	e.t.Helper()
	if err := releasestamp.WriteDir(version, e.dist, e.source); err != nil {
		e.t.Fatal(err)
	}
	e.script = filepath.Join(e.dist, filepath.Base(e.source))
	e.sums(e.dist)
}

// sums 按 dir 下现有的包写 SHA256SUMS，与发布产物同一格式。
func (e *env) sums(dir string) {
	e.t.Helper()
	pkgs, err := filepath.Glob(filepath.Join(dir, "*.tar.gz"))
	if err != nil {
		e.t.Fatal(err)
	}
	var b strings.Builder
	for _, p := range pkgs {
		data, err := os.ReadFile(p)
		if err != nil {
			e.t.Fatal(err)
		}
		fmt.Fprintf(&b, "%x  %s\n", sha256.Sum256(data), filepath.Base(p))
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(b.String()), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// run 以面板命令的形态执行：脚本来自 stdin（sh -s --）。
func (e *env) run(args ...string) (string, int) {
	e.t.Helper()
	script, err := os.Open(e.script)
	if err != nil {
		e.t.Fatal(err)
	}
	defer script.Close()
	cmd := exec.Command("sh", append([]string{"-s", "--"}, args...)...)
	cmd.Stdin = script
	// 末尾补上 sbin：macOS 的 sha256sum 在 /sbin。
	cmd.Env = append(os.Environ(), "PATH="+e.bin+string(os.PathListSeparator)+os.Getenv("PATH")+":/usr/sbin:/sbin",
		"HERON_INSTALL_ROOT="+e.root, "STUB_STATE="+e.state)
	cmd.Env = append(cmd.Env, e.vars...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return string(out), code
}

func (e *env) install(extra ...string) (string, int) {
	return e.run(append([]string{"--hub", "http://hub.test", "--key", "k", "--base-url", "file://" + e.dist}, extra...)...)
}

func (e *env) calls() []string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.state, "calls"))
	if err != nil {
		e.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (e *env) resetCalls() { e.write("calls", "") }

// index 返回第一条以 prefix 开头的调用的位置，没有时为 -1。
func index(calls []string, prefix string) int {
	return slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func (e *env) file(rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.root, rel))
	if err != nil {
		e.t.Fatalf("%s: %v", rel, err)
	}
	return string(b)
}

func (e *env) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(e.root, rel))
	return err == nil
}

const done = "heron-agent installed and started (launchd, arm64, heron-agent_darwin_arm64.tar.gz)"

func TestFreshInstallFromStdin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	out, code := e.install("--name", "mac-1")
	if code != 0 || !strings.Contains(out, done) {
		t.Fatalf("exit %d, want the final line %q:\n%s", code, done, out)
	}
	c := e.calls()
	// 从 499 往下的第一个空闲号同时给组与用户，不取 Apple 追加前沿上的 309；组先于用户建。
	if g, u := index(c, "dscl . -create /Groups/_heron-agent PrimaryGroupID 499"), index(c, "dscl . -create /Users/_heron-agent UniqueID 499"); g < 0 || u < g {
		t.Fatalf("group then user with id 499, got %q", c)
	}
	reg := index(c, "heron-agent register --hub http://hub.test --key k --config "+e.root+"/etc/heron-agent/config.json --name mac-1")
	boot := index(c, "launchctl bootstrap system "+e.root+"/Library/LaunchDaemons/xyz.heron.agent.plist")
	if reg < 0 || boot < reg || index(c, "launchctl bootout") >= 0 {
		t.Fatalf("register before bootstrap and no bootout on a fresh host, got %q", c)
	}
	// 注册在停服务（它的第一步是 launchctl print）之前：注册失败时不留下停掉的服务。
	if stop := index(c, "launchctl print system/xyz.heron.agent"); stop < reg {
		t.Fatalf("register must precede stopping the service, got %q", c)
	}
	for _, want := range []string{
		"chown root:_heron-agent " + e.root + "/etc/heron-agent",
		"chown _heron-agent:_heron-agent " + e.root + "/etc/heron-agent/config.json",
		"chown root:wheel " + e.root + "/Library/LaunchDaemons/xyz.heron.agent.plist",
		"chown root:wheel " + e.root + "/Library/Logs/heron-agent",
		"chown _heron-agent:_heron-agent " + e.root + "/Library/Logs/heron-agent/heron-agent.log",
		"chown _heron-agent:_heron-agent " + e.root + "/Library/Logs/heron-agent/heron-agent.err",
		"launchctl enable system/xyz.heron.agent",
	} {
		if i := index(c, want); i < 0 || i > boot {
			t.Errorf("missing %q before bootstrap in %q", want, c)
		}
	}
	if !strings.Contains(e.file("usr/local/bin/heron-agent"), "# v1 arm64") {
		t.Fatal("installed binary is not the arm64 package's")
	}
	e.mode("etc/heron-agent", 0o750)
	e.mode("etc/heron-agent/config.json", 0o600)
	// 目录属 root，两个日志文件由脚本建好交给服务用户：launchd 不论以哪个身份打开都写得进去。
	e.mode("Library/Logs/heron-agent", 0o755)
	e.mode("Library/Logs/heron-agent/heron-agent.log", 0o640)
	e.mode("Library/Logs/heron-agent/heron-agent.err", 0o640)
}

func (e *env) mode(rel string, want os.FileMode) {
	e.t.Helper()
	st, err := os.Lstat(filepath.Join(e.root, rel))
	if err != nil {
		e.t.Fatalf("%s: %v", rel, err)
	}
	if st.Mode().Perm() != want || !st.Mode().IsRegular() && !st.IsDir() {
		e.t.Fatalf("%s: mode %v; want %v", rel, st.Mode(), want)
	}
}

// 499 只被用作 GID 也不能取：组与用户同号，要两个命名空间都空闲。
func TestFreeIDMustBeFreeInBothNamespaces(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.write("sys-Groups", "_taken301 301\n_taken303 303\n_taken305 305\n_taken307 307\n_taken499 499\n")
	if out, code := e.install(); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if c := e.calls(); index(c, "dscl . -create /Users/_heron-agent UniqueID 498") < 0 {
		t.Fatalf("want id 498, got %q", c)
	}
}

// 建号避开运行中进程占用的号：ps 里有个无账户记录的进程用着 499（STUB_PS_IDS 注入），取号应为 498。
func TestFreeIdAvoidsIdsUsedByRunningProcesses(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.write("ps-extra", "  499   499\n")
	e.vars = append(e.vars, "STUB_PS_IDS="+filepath.Join(e.state, "ps-extra"))
	if out, code := e.install(); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if c := e.calls(); index(c, "dscl . -create /Groups/_heron-agent PrimaryGroupID 498") < 0 ||
		index(c, "dscl . -create /Users/_heron-agent UniqueID 498") < 0 {
		t.Fatalf("want id 498 for both the group and the user, got %q", c)
	}
}

// 上次安装在建组之后、建用户之前中断：沿用已有的组，用户另取一个空闲号，不再建组。
func TestExistingGroupWithoutUserIsReused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.write("groups/_heron-agent", "310\n")
	if out, code := e.install(); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	c := e.calls()
	if index(c, "dscl . -create /Groups/") >= 0 {
		t.Fatalf("the existing group must be reused, got %q", c)
	}
	if index(c, "dscl . -create /Users/_heron-agent PrimaryGroupID 310") < 0 || index(c, "dscl . -create /Users/_heron-agent UniqueID 499") < 0 {
		t.Fatalf("user must join group 310 with a free id, got %q", c)
	}
}

// 同名用户已在但主组不是同名组：plist 的 GroupName 与配置目录的组对不上，退出且不建组。
func TestExistingUserWithWrongPrimaryGroupIsRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.write("users/_heron-agent", "450 20\n")
	e.write("groups/staff", "20\n")
	out, code := e.install()
	if code != 1 || !strings.Contains(out, "exists with primary group staff; expected _heron-agent") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if index(e.calls(), "dscl . -create") >= 0 || e.exists("usr/local/bin/heron-agent") {
		t.Fatal("nothing may be created or downloaded")
	}
}

// 配置与日志文件的属主、权限每次安装都设，不只在首次注册之后：手工重新注册以 root 重写配置、
// 注册之后被打断、人工编辑改了权限，重跑都要修回来。
func TestOwnershipIsRestoredOnEveryInstall(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if out, code := e.install(); code != 0 {
		t.Fatalf("first install exit %d:\n%s", code, out)
	}
	os.Chmod(filepath.Join(e.root, "etc/heron-agent"), 0o755)
	os.Chmod(filepath.Join(e.root, "etc/heron-agent/config.json"), 0o644)
	os.Chmod(filepath.Join(e.root, "Library/Logs/heron-agent"), 0o700)
	os.Chmod(filepath.Join(e.root, "Library/Logs/heron-agent/heron-agent.err"), 0o644)
	// 日志文件被删后，以服务用户身份打开它的 launchd 建不回来；重跑安装脚本把它建回来。
	os.Remove(filepath.Join(e.root, "Library/Logs/heron-agent/heron-agent.log"))
	e.resetCalls()
	if out, code := e.install(); code != 0 {
		t.Fatalf("rerun exit %d:\n%s", code, out)
	}
	c := e.calls()
	if index(c, "heron-agent register") >= 0 {
		t.Fatalf("rerun must not register: %q", c)
	}
	out, boot := index(c, "launchctl bootout"), index(c, "launchctl bootstrap")
	// 日志目录先交给 root、再去掉 ACL，然后才查其中的文件，这些都在停服务之前。
	logDir := e.root + "/Library/Logs/heron-agent"
	take, noACL, check := index(c, "chown root:wheel "+logDir), index(c, "chmod -N "+logDir), index(c, "find "+logDir+"/heron-agent.err")
	if take < 0 || noACL < take || check < noACL || out < check {
		t.Errorf("rerun: want chown root:wheel < chmod -N < the file check < bootout on the log directory, calls %q", c)
	}
	// 每个按数字模式设权限的对象都去掉 ACL：数字 chmod 与 chown 都不动 ACL。
	for _, rel := range []string{"etc/heron-agent", "etc/heron-agent/config.json", "Library/Logs/heron-agent/heron-agent.log", "Library/Logs/heron-agent/heron-agent.err"} {
		if i := index(c, "chmod -N "+e.root+"/"+rel); i < 0 || i > out {
			t.Errorf("rerun: missing chmod -N %s before bootout, calls %q", rel, c)
		}
	}
	// 可能失败的操作都在停服务之前：失败时旧服务照常运行。停服务之后只剩替换二进制、写 plist、bootstrap，
	// bootout 与 bootstrap 之间的 chown、chmod 只作用于 plist。
	for _, call := range c[out+1 : boot] {
		for _, p := range []string{"chown ", "chmod ", "find ", "heron-agent "} {
			if strings.HasPrefix(call, p) && !strings.HasSuffix(call, "/Library/LaunchDaemons/xyz.heron.agent.plist") {
				t.Errorf("rerun: %q runs after the service is stopped", call)
			}
		}
	}
	for _, want := range []string{
		"chown root:_heron-agent " + e.root + "/etc/heron-agent",
		"chown _heron-agent:_heron-agent " + e.root + "/etc/heron-agent/config.json",
		"chown _heron-agent:_heron-agent " + e.root + "/Library/Logs/heron-agent/heron-agent.log",
		"chown _heron-agent:_heron-agent " + e.root + "/Library/Logs/heron-agent/heron-agent.err",
	} {
		if i := index(c, want); i < 0 || i > out {
			t.Errorf("rerun: %q must run before bootout, calls %q", want, c)
		}
	}
	e.mode("etc/heron-agent", 0o750)
	e.mode("etc/heron-agent/config.json", 0o600)
	e.mode("Library/Logs/heron-agent", 0o755)
	e.mode("Library/Logs/heron-agent/heron-agent.log", 0o640)
	e.mode("Library/Logs/heron-agent/heron-agent.err", 0o640)
}

// launchd 以服务 uid 派生的辅助进程（cfprefsd、trustd……）不是本服务：按 uid 加可执行路径认进程，
// 升级与卸载不会因为它们等满 10 秒后失败，也不会把它们当成已启动的 agent。
func TestHelperProcessesOfTheServiceUserAreNotTheService(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.vars = []string{"STUB_HELPER=1"}
	if out, code := e.install(); code != 0 || !strings.Contains(out, done) {
		t.Fatalf("install exit %d:\n%s", code, out)
	}
	e.release("arm64", "v2")
	if out, code := e.install(); code != 0 || !strings.Contains(out, done) {
		t.Fatalf("upgrade with a helper process still running: exit %d:\n%s", code, out)
	}
	if out, code := e.run("--uninstall"); code != 0 {
		t.Fatalf("uninstall with a helper process still running: exit %d:\n%s", code, out)
	}
}

func TestHelperProcessDoesNotCountAsStarted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.vars = []string{"STUB_HELPER=1", "STUB_START_FAILS=1"}
	if out, code := e.install(); code != 1 || !strings.Contains(out, "heron-agent did not start") {
		t.Fatalf("only a helper process running must not count as started: exit %d:\n%s", code, out)
	}
}

// 包里的 plist 与脚本的常量是同一件事的两处写法：脚本按 Label 管作业、按 UserName 的 uid 认进程、
// 把二进制放到 ProgramArguments[0]、建 StandardErrorPath 所在的目录。
func TestPlistAgreesWithScript(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if out, code := e.install(); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	p := e.file("Library/LaunchDaemons/xyz.heron.agent.plist")
	for _, want := range []string{
		"<key>Label</key>\n\t<string>xyz.heron.agent</string>",
		"<string>/usr/local/bin/heron-agent</string>",
		"<string>/etc/heron-agent/config.json</string>",
		"<key>UserName</key>\n\t<string>" + svcUser + "</string>",
		"<key>GroupName</key>\n\t<string>" + svcUser + "</string>",
		"<string>/Library/Logs/heron-agent/heron-agent.err</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	for _, rel := range []string{"usr/local/bin/heron-agent", "etc/heron-agent/config.json", "Library/Logs/heron-agent"} {
		if !e.exists(rel) {
			t.Errorf("script did not create %s, which the plist refers to", rel)
		}
	}
}

func TestRerunUpgradesWithoutRegistering(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if out, code := e.install(); code != 0 {
		t.Fatalf("first install exit %d:\n%s", code, out)
	}
	e.release("arm64", "v2")
	e.resetCalls()
	out, code := e.install("--name", "ignored")
	if code != 0 || !strings.Contains(out, "keeping the current registration (--key ignored)") || !strings.Contains(out, "--name ignored") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	c := e.calls()
	if index(c, "heron-agent register") >= 0 || index(c, "dscl . -create") >= 0 {
		t.Fatalf("rerun must neither register nor recreate the account: %q", c)
	}
	if out := index(c, "launchctl bootout system/xyz.heron.agent"); out < 0 || out > index(c, "launchctl bootstrap") {
		t.Fatalf("a loaded job must be booted out before the new one is bootstrapped: %q", c)
	}
	if !strings.Contains(e.file("usr/local/bin/heron-agent"), "# v2 arm64") {
		t.Fatal("binary was not replaced")
	}
}

func TestArchitectureFromHardwareNotShell(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		arm64, uname, pkg string
	}{
		{"1", "x86_64", "arm64"}, // Rosetta 转译的终端：uname -m 报 x86_64，硬件是 Apple Silicon
		{"", "x86_64", "amd64"},  // Intel：hw.optional.arm64 不存在
	} {
		e := newEnv(t)
		e.vars = []string{"STUB_ARM64=" + tc.arm64, "STUB_UNAME_M=" + tc.uname}
		out, code := e.install()
		if code != 0 || !strings.Contains(out, "heron-agent_darwin_"+tc.pkg+".tar.gz)") {
			t.Fatalf("arm64=%q uname=%q: exit %d, want the %s package:\n%s", tc.arm64, tc.uname, code, tc.pkg, out)
		}
	}
}

// 作业被手工 bootout 后 plist 还在：没有可停的作业，bootout 会以 3 失败，重跑不能因此失败。
func TestUnloadedJobIsNotBootedOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if out, code := e.install(); code != 0 {
		t.Fatalf("first install exit %d:\n%s", code, out)
	}
	os.Remove(filepath.Join(e.state, "loaded"))
	e.write("procs", "")
	e.resetCalls()
	out, code := e.install()
	if code != 0 || !strings.Contains(out, done) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if index(e.calls(), "launchctl bootout") >= 0 {
		t.Fatalf("an unloaded job must not be booted out: %q", e.calls())
	}
}

// dscl 退出码为 0 却没建出用户：回查挡住，下载与注册都还没发生。
func TestAccountCreationIsVerified(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.vars = []string{"STUB_DSCL_DROP_USER=1"}
	out, code := e.install()
	if code != 1 || !strings.Contains(out, "failed to create system user _heron-agent") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if index(e.calls(), "heron-agent register") >= 0 || e.exists("usr/local/bin/heron-agent") {
		t.Fatal("nothing may be downloaded or registered after the account check fails")
	}
}

func TestLingeringProcessBlocksReplacement(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if out, code := e.install(); code != 0 {
		t.Fatalf("first install exit %d:\n%s", code, out)
	}
	e.release("arm64", "v2")
	e.vars = []string{"STUB_BOOTOUT_LEAVES_PROCESS=1"}
	out, code := e.install()
	if code != 1 || !strings.Contains(out, "heron-agent is still running: processes with uid 499 (_heron-agent): 4242") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(e.file("usr/local/bin/heron-agent"), "# v1 arm64") {
		t.Fatal("binary must not be replaced while the old process runs")
	}
}

func TestStartIsConfirmedBySamePID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ knob, want string }{
		{"STUB_START_FAILS=1", "heron-agent did not start"},
		{"STUB_RESPAWN=1", "heron-agent did not stay running (pid 4243)"},
	} {
		e := newEnv(t)
		e.vars = []string{tc.knob}
		out, code := e.install()
		if code != 1 || !strings.Contains(out, tc.want) || !strings.Contains(out, "/Library/Logs/heron-agent/heron-agent.err") {
			t.Fatalf("%s: exit %d, want %q and the log path:\n%s", tc.knob, code, tc.want, out)
		}
	}
}

func TestUninstallAndPurge(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if out, code := e.install(); code != 0 {
		t.Fatalf("install exit %d:\n%s", code, out)
	}
	if out, code := e.run("--purge"); code != 2 {
		t.Fatalf("--purge alone must be a usage error, exit %d:\n%s", code, out)
	}
	out, code := e.run("--uninstall")
	if code != 0 || e.exists("usr/local/bin/heron-agent") || e.exists("Library/LaunchDaemons/xyz.heron.agent.plist") {
		t.Fatalf("uninstall exit %d:\n%s", code, out)
	}
	if !e.exists("etc/heron-agent/config.json") || !e.exists("../state/users/_heron-agent") {
		t.Fatal("plain uninstall keeps config and account")
	}
	e.vars = []string{"STUB_DSCL_KEEP=1"}
	if out, code := e.run("--uninstall", "--purge"); code != 1 || !strings.Contains(out, "failed to delete user or group _heron-agent") {
		t.Fatalf("undeleted account must fail purge, exit %d:\n%s", code, out)
	}
	e.vars = nil
	if out, code := e.run("--uninstall", "--purge"); code != 0 || e.exists("etc/heron-agent") || e.exists("Library/Logs/heron-agent") || e.exists("../state/users/_heron-agent") || e.exists("../state/groups/_heron-agent") {
		t.Fatalf("purge exit %d:\n%s", code, out)
	}
}

func TestRefusals(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.vars = []string{"STUB_UID=501"}
	if out, code := e.install(); code != 1 || !strings.Contains(out, "must run as root") {
		t.Fatalf("non-root: exit %d:\n%s", code, out)
	}
	e.vars = nil
	if out, code := e.run("--base-url", "file://"+e.dist); code != 2 || !strings.Contains(out, "--hub and --key are required for the first install") {
		t.Fatalf("missing credentials: exit %d:\n%s", code, out)
	}
	if out, code := e.run("--hub"); code != 2 {
		t.Fatalf("flag without value: exit %d:\n%s", code, out)
	}
}

// 解析得到的同名用户不在本地节点里：不对本地节点发 -delete（它会以 eDSRecordNotFound 失败、
// 脚本以 dscl 的退出码结束），回查仍能看到这个用户，由脚本自己报出删不掉。
func TestPurgeDecidesDeletionFromTheLocalRecord(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := os.MkdirAll(filepath.Join(e.state, "remote-users"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.write("remote-users/_heron-agent", "480 480\n")
	out, code := e.run("--uninstall", "--purge")
	if code != 1 || !strings.Contains(out, "failed to delete user or group _heron-agent") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if index(e.calls(), "dscl . -delete /Users/_heron-agent") >= 0 {
		t.Fatalf("no local record, no -delete: %q", e.calls())
	}
}

// 目录服务读不出账户列表与号段耗尽是两回事，报错各说各的。
func TestAccountFailuresNameTheirCause(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.vars = []string{"STUB_DSCL_LIST_FAILS=1"}
	if out, code := e.install(); code != 1 || !strings.Contains(out, "cannot list user ids") || strings.Contains(out, "no free id") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	f := newEnv(t)
	var b strings.Builder
	for i := 300; i < 500; i++ {
		fmt.Fprintf(&b, "_taken%d %d\n", i, i)
	}
	f.write("sys-Users", b.String())
	if out, code := f.install(); code != 1 || !strings.Contains(out, "no free id in 300-499 for _heron-agent") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	g := newEnv(t)
	g.write("groups/_heron-agent", "\n")
	if out, code := g.install(); code != 1 || !strings.Contains(out, "group _heron-agent has no PrimaryGroupID") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// 会让脚本中止的前置检查都在停服务之前：日志目录不是真目录、日志文件不是链接数为 1 的普通文件时拒绝，
// 旧服务照常运行、二进制不被替换，也不对日志文件 chown——不改到链接指向的对象。
// 日志目录不是真目录时连目录也不 chown；其余情形目录已先交给 root（见 TestLogFileSwappedBeforeTheDirectoryIsTakenIsRefused）。
func TestLogPathsThatAreNotPlainAreRefusedBeforeStopping(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, rel, want string
		dirTaken        bool
		plant           func(path, root string)
	}{
		{"directory symlink", "Library/Logs/heron-agent", "exists but is not a directory", false, func(p, root string) {
			os.RemoveAll(p)
			os.Symlink(filepath.Join(root, "etc"), p)
		}},
		{"file symlink", "Library/Logs/heron-agent/heron-agent.err", "exists but is not a regular file with a single link", true, func(p, root string) {
			os.Remove(p)
			os.Symlink(filepath.Join(root, "etc/heron-agent/config.json"), p)
		}},
		{"file is a directory", "Library/Logs/heron-agent/heron-agent.log", "exists but is not a regular file with a single link", true, func(p, root string) {
			os.Remove(p)
			os.Mkdir(p, 0o755)
		}},
		// 硬链接：[ -L ] 为假、[ -f ] 为真，只有链接数看得出它另有名字。
		{"file hard link", "Library/Logs/heron-agent/heron-agent.log", "exists but is not a regular file with a single link", true, func(p, root string) {
			os.Remove(p)
			os.Link(filepath.Join(root, "outside"), p)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			if out, code := e.install(); code != 0 {
				t.Fatalf("first install exit %d:\n%s", code, out)
			}
			outside := filepath.Join(e.root, "outside")
			if err := os.WriteFile(outside, []byte("root's\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.plant(filepath.Join(e.root, tc.rel), e.root)
			e.release("arm64", "v2")
			e.resetCalls()
			out, code := e.install()
			if code != 1 || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			c := e.calls()
			var chowns []string
			for _, call := range c {
				if strings.HasPrefix(call, "chown ") {
					chowns = append(chowns, call)
				}
			}
			wantChowns := []string(nil)
			if tc.dirTaken {
				wantChowns = []string{"chown root:wheel " + e.root + "/Library/Logs/heron-agent"}
			}
			if index(c, "launchctl bootout") >= 0 || !slices.Equal(chowns, wantChowns) || !strings.Contains(e.file("usr/local/bin/heron-agent"), "# v1 arm64") {
				t.Fatalf("the running service and the log files must be left alone: calls %q", c)
			}
			e.mode("outside", 0o644)
		})
	}
}

// 日志目录此前对服务用户可写（手工建的，或由把目录交给服务用户的版本装的）时，检查之后、目录交给 root 之前
// 服务用户还能换掉其中的条目。目录必须先交给 root 再检查：替身在目录 chown 的那一刻把 heron-agent.log 换成
// 指向目录外文件的硬链接，检查要看到它并拒绝，目录外文件的权限不被改动。
func TestLogFileSwappedBeforeTheDirectoryIsTakenIsRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if out, code := e.install(); code != 0 {
		t.Fatalf("first install exit %d:\n%s", code, out)
	}
	outside := filepath.Join(e.root, "outside")
	if err := os.WriteFile(outside, []byte("root's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.vars = []string{"STUB_SWAP_LOG=" + outside}
	e.resetCalls()
	out, code := e.install()
	e.mode("outside", 0o644)
	if code != 1 || !strings.Contains(out, "heron-agent.log exists but is not a regular file with a single link") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if index(e.calls(), "launchctl bootout") >= 0 {
		t.Fatalf("the running service must be left alone: calls %q", e.calls())
	}
}

// 读不到链接数时拒绝，而不是把空输出当成"链接数为 1"放行。
func TestLogFileLinkCountUnreadableIsRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if out, code := e.install(); code != 0 {
		t.Fatalf("first install exit %d:\n%s", code, out)
	}
	e.vars = []string{"STUB_FIND_FAILS=1"}
	e.resetCalls()
	out, code := e.install()
	if code != 1 || !strings.Contains(out, "cannot read the link count of") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if index(e.calls(), "launchctl bootout") >= 0 {
		t.Fatalf("the running service must be left alone: calls %q", e.calls())
	}
}

// macOS 上真实的 ACL：日志目录此前归服务用户时，它作为属主能加上 allow add_file,delete_child，这类允许项
// 越过数字模式生效，数字 chmod 与 chown 都不去掉它。重跑安装之后目录与日志文件上不能再有 ACL。
func TestLogACLsAreRemovedOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("ACLs set by chmod +a exist only on macOS")
	}
	t.Parallel()
	e := newEnv(t)
	if out, code := e.install(); code != 0 {
		t.Fatalf("first install exit %d:\n%s", code, out)
	}
	paths := []string{"Library/Logs/heron-agent", "Library/Logs/heron-agent/heron-agent.log"}
	for _, rel := range paths {
		if out, err := exec.Command("/bin/chmod", "+a", "everyone allow add_file,delete_child,write", filepath.Join(e.root, rel)).CombinedOutput(); err != nil {
			t.Fatalf("chmod +a %s: %v\n%s", rel, err, out)
		}
		if !hasACL(t, filepath.Join(e.root, rel)) {
			t.Fatalf("fixture: %s has no ACL after chmod +a", rel)
		}
	}
	if out, code := e.install(); code != 0 {
		t.Fatalf("rerun exit %d:\n%s", code, out)
	}
	for _, rel := range paths {
		if hasACL(t, filepath.Join(e.root, rel)) {
			t.Errorf("%s still has an ACL after reinstalling", rel)
		}
	}
}

// hasACL 看 ls -lde 在条目那一行之后有没有 ACL 行。
func hasACL(t *testing.T, path string) bool {
	t.Helper()
	out, err := exec.Command("/bin/ls", "-lde", path).Output()
	if err != nil {
		t.Fatalf("ls -lde %s: %v", path, err)
	}
	return len(strings.Split(strings.TrimSpace(string(out)), "\n")) > 1
}

// 日志文件或配置 chown 失败（服务用户给自己的文件设了不可变标志之类），或者配置丢了、重新注册失败：
// 都在停服务之前失败，旧服务照常运行、二进制不被替换。
func TestFailuresBeforeStoppingLeaveTheServiceRunning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		vars func(root string) []string
		prep func(root string)
		want string
	}{
		{"log file chown", func(root string) []string {
			return []string{"STUB_CHOWN_FAILS=" + root + "/Library/Logs/heron-agent/heron-agent.err"}
		}, nil, "heron-agent.err: Operation not permitted"},
		{"config chown", func(root string) []string {
			return []string{"STUB_CHOWN_FAILS=" + root + "/etc/heron-agent/config.json"}
		}, nil, "config.json: Operation not permitted"},
		{"re-register", func(string) []string { return []string{"STUB_REGISTER_FAILS=1"} }, func(root string) {
			os.Remove(filepath.Join(root, "etc/heron-agent/config.json"))
		}, "register: hub unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			if out, code := e.install(); code != 0 {
				t.Fatalf("first install exit %d:\n%s", code, out)
			}
			if tc.prep != nil {
				tc.prep(e.root)
			}
			e.release("arm64", "v2")
			e.vars = tc.vars(e.root)
			e.resetCalls()
			out, code := e.install()
			if code == 0 || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			if index(e.calls(), "launchctl bootout") >= 0 || !strings.Contains(e.file("usr/local/bin/heron-agent"), "# v1 arm64") {
				t.Fatalf("the running service must be left alone: calls %q", e.calls())
			}
		})
	}
}
