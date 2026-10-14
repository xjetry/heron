#!/bin/sh
# heron-agent 的 macOS 安装脚本：下载、校验、注册并装成 LaunchDaemon；重跑即升级。
# 参数、步骤顺序与提示与 Linux 的 install.sh 同形，差异只在平台工具：dscl 建账户、shasum 校验、
# launchctl 管服务、ps 扫进程（macOS 没有 /proc）。
# 以 curl … | sh -s -- 运行时，脚本本身来自 stdin。脚本里任何读 stdin 的命令都会吞掉
# 脚本余下部分，安装在中途无声结束。所以每个可能读 stdin 的外部命令都显式 </dev/null。
# 不能用 exec </dev/null：那会切断脚本自己的来源。
set -eu

# 落盘路径都挂在 HERON_INSTALL_ROOT 下。生产运行时它为空，即真实根目录；脚本的逻辑测试以普通用户
# 把它指向临时目录，连同 PATH 上的 dscl、launchctl、ps 等替身一起运行，不触碰真实系统路径。
# 它只改变本脚本写文件的位置：plist 里的 ProgramArguments 与日志路径是固定的真实路径。
ROOT=${HERON_INSTALL_ROOT-}
# 服务进程的可执行路径，即 plist 的 ProgramArguments[0]；按它认进程，所以不带 ROOT 前缀。
SVC_BIN=/usr/local/bin/heron-agent
BIN=$ROOT$SVC_BIN
CFG_DIR=$ROOT/etc/heron-agent
CFG=$CFG_DIR/config.json
LOG_DIR=$ROOT/Library/Logs/heron-agent
LABEL=xyz.heron.agent
PLIST=$ROOT/Library/LaunchDaemons/$LABEL.plist
SVC_USER=_heron-agent
REPO=https://github.com/xjetry/heron

# 本脚本所属的版本与该版全部 tar 包的 SHA-256（每行 "<64 位十六进制>  <文件名>"），由完整 release 的发布目标 release-full 经
# deploy/releasestamp 写进下面两行标记之间（只发 hub 的 release 原样带着绑定版本的这份脚本，写入的仍是那个版本）；源码里为空，这时拒绝安装（卸载不下载，照常可用）。下载的包只按这份
# 清单校验：--base-url 能换掉下载目录里的每个文件，同目录的 SHA256SUMS 也在其中，拿它作依据挡不住篡改。
RELEASE_VERSION=""
RELEASE_SHA256=""
# >>> release stamp >>>
# <<< release stamp <<<

usage() {
  echo "usage: install-macos.sh --hub URL --key KEY [--re-register] [--name N] [--insecure-http] [--base-url URL]" >&2
  echo "       install-macos.sh --uninstall [--purge]" >&2
  exit 2
}

HUB=""; KEY=""; NAME=""; INSECURE_HTTP=0; BASE_URL=""; UNINSTALL=0; PURGE=0; RE_REGISTER=0
need_value() { [ "$#" -ge 2 ] || usage; }
while [ $# -gt 0 ]; do
  case "$1" in
    --hub) need_value "$@"; HUB=$2; shift 2;;
    --key) need_value "$@"; KEY=$2; shift 2;;
    --re-register) RE_REGISTER=1; shift;;
    --name) need_value "$@"; NAME=$2; shift 2;;
    --insecure-http) INSECURE_HTTP=1; shift;;
    --base-url) need_value "$@"; BASE_URL=$2; shift 2;;
    # 脚本只装自己所属的版本：版本由取哪个 URL 的脚本决定，没有第二个来源可以和内嵌清单不一致。
    --version|--version=*)
      echo "install-macos.sh has no --version: it installs only the release it belongs to; for another version run $REPO/releases/download/<tag>/install-macos.sh" >&2
      exit 2;;
    --uninstall) UNINSTALL=1; shift;;
    --purge) PURGE=1; shift;;
    *) usage;;
  esac
done
[ "$PURGE" = 0 ] || [ "$UNINSTALL" = 1 ] || usage
# 显式重新注册必须提供完整凭据；在锁文件、下载与系统变更之前拒绝缺省或空值。
if [ "$RE_REGISTER" = 1 ] && { [ -z "$HUB" ] || [ -z "$KEY" ]; }; then
  echo "--hub and --key are required for --re-register" >&2
  exit 2
fi

[ "$(id -u)" = 0 ] || { echo "install-macos.sh must run as root" >&2; exit 1; }

# 停服务后确认进程退出的轮询间隔（秒）与次数上限：sleep 合计最多 10 秒，外加每轮一次 ps。
STOP_POLL_INTERVAL=1
STOP_POLL_MAX=10
# 启动后等到进程出现的上限，同一口径。出现之后再固定等 3 秒，确认还是同一个 pid。
START_POLL_INTERVAL=1
START_POLL_MAX=10

# 列出本服务的进程 pid（空格分隔，写入 svc_pids）：有效 uid 为 $1 且可执行路径为 $SVC_BIN。
# BSD ps 的 uid 列是有效 uid（ps(1)）；comm 列是进程的 argv[0]（实测：不截断，文件被替换后不变），
# launchd 作业的 argv[0] 是 plist 的 ProgramArguments[0]（用户域作业实测）。comm 里可能有空格，取前两列之后的整段比较。
# 拉起之后、exec 之前的瞬间子进程的 comm 是 xpcproxy（实测）：启动确认是轮询，只会晚一轮认出它。
scan_svc_pids() {
  procs=$(ps -axo uid=,pid=,comm=) || { echo "cannot list processes" >&2; return 1; }
  svc_pids=$(printf '%s\n' "$procs" | awk -v u="$1" -v p="$SVC_BIN" '
    { c = $0; sub(/^[ \t]*[0-9]+[ \t]+[0-9]+[ \t]+/, "", c) }
    $1 == u && c == p { printf " %s", $2 }')
}

# 确认没有本服务的进程。它不看 launchd 报告的状态，也不依赖 plist 是否还在：
# plist 被手工删掉而进程仍在时也只有这一步抓得到。
# 判据"有效 uid 为服务用户且可执行路径为 $SVC_BIN"与"是本服务的进程"等价，两个方向各有担保：
# - 选中的都是本服务的进程：$SVC_USER 是专供 agent 的账户（登录 shell /usr/bin/false），由 create_account 建立；
#   launchd 会以该 uid 按需派生 cfprefsd、trustd 之类的辅助进程，只看 uid 会把它们算进来（升级、卸载误报仍在运行，
#   秒退的 agent 被辅助进程顶替成"已启动"），可执行路径把它们排除。
# - 本服务的每个进程都被选中：plist 的 UserName 与 ProgramArguments[0]（deploy/launchd/xyz.heron.agent.plist）
#   保证，TestPlistAgreesWithScript 把两处钉在一起。plist 若改以别的身份或别的程序运行任何进程，这里会漏查，必须同步改判据。
# 用户不存在时没有可比对的 uid，直接通过；此时查不到仍在运行的旧进程，与 Linux 脚本相同。
confirm_service_stopped() {
  id "$SVC_USER" >/dev/null 2>&1 || return 0
  svc_uid=$(id -u "$SVC_USER")
  polls=0
  while :; do
    scan_svc_pids "$svc_uid" || return 1
    [ -n "$svc_pids" ] || return 0
    [ "$polls" -lt "$STOP_POLL_MAX" ] || break
    sleep "$STOP_POLL_INTERVAL"
    polls=$((polls + 1))
  done
  echo "heron-agent is still running: processes with uid $svc_uid ($SVC_USER):$svc_pids" >&2
  return 1
}

# 启动之后确认服务进程活着：bootstrap 返回 0 只说明作业已载入，证明不了子进程没有秒退。
# 先等到出现本服务的进程，记下 pid，3 秒后同一个 pid 仍在；
# 秒退再被 KeepAlive 拉起会换成新 pid，不能算起来了。判据与 confirm_service_stopped 同一处。
start_log_hint() { echo "see $LOG_DIR/heron-agent.err" >&2; }
confirm_service_started() {
  svc_uid=$(id -u "$SVC_USER") || { echo "no service user $SVC_USER" >&2; start_log_hint; return 1; }
  polls=0
  pid=""
  while :; do
    scan_svc_pids "$svc_uid" || return 1
    if [ -n "$svc_pids" ]; then
      pid=${svc_pids# }
      pid=${pid%% *}
      break
    fi
    [ "$polls" -lt "$START_POLL_MAX" ] || break
    sleep "$START_POLL_INTERVAL"
    polls=$((polls + 1))
  done
  if [ -z "$pid" ]; then
    echo "heron-agent did not start" >&2
    start_log_hint
    return 1
  fi
  sleep 3
  scan_svc_pids "$svc_uid" || return 1
  case " $svc_pids " in
    *" $pid "*) return 0;;
  esac
  echo "heron-agent did not stay running (pid $pid)" >&2
  start_log_hint
  return 1
}

# 是否发 bootout 看作业是否已载入 system 域，而不是 plist 文件在不在：launchd 按已载入的作业管进程，
# 文件在而作业未载入时没有可停的东西（bootout 会以 3 失败），作业已载入而文件被删时进程照样在跑。
# bootout 失败即失败；确认一步不依赖它，总是执行。
service_loaded() { launchctl print "system/$LABEL" >/dev/null 2>&1 </dev/null; }
stop_service() {
  if service_loaded; then
    launchctl bootout "system/$LABEL" </dev/null || { echo "failed to stop heron-agent" >&2; return 1; }
  fi
  confirm_service_stopped
}

group_exists() { dscl . -read "/Groups/$SVC_USER" PrimaryGroupID >/dev/null 2>&1 </dev/null; }
user_record_exists() { dscl . -read "/Users/$SVC_USER" UniqueID >/dev/null 2>&1 </dev/null; }

# 用户与同名组都删并回查：不信 dscl 的退出码，也不能半成功还报成功。
# 要不要发 dscl . -delete 看本地节点的记录，与这条命令操作的对象同一口径：id 的解析结果可能来自别的目录节点
# 或陈旧的缓存，那时 -delete 以 eDSRecordNotFound 失败，脚本以 dscl 的退出码结束、说不出原因。
# 回查仍用 id：chown 与 launchd 按名字取身份走的就是这条解析路径。
delete_account() {
  if user_record_exists; then dscl . -delete "/Users/$SVC_USER" </dev/null; fi
  if group_exists; then dscl . -delete "/Groups/$SVC_USER" </dev/null; fi
  if id "$SVC_USER" >/dev/null 2>&1 || group_exists; then
    echo "failed to delete user or group $SVC_USER" >&2; exit 1
  fi
}

if [ "$UNINSTALL" = 1 ]; then
  stop_service
  # 作业已由 stop_service 卸下；plist 删掉后开机也不再载入。
  rm -f "$PLIST" "$BIN"
  if [ "$PURGE" = 1 ]; then
    rm -rf "$CFG_DIR" "$LOG_DIR"
    delete_account
  fi
  echo "heron-agent uninstalled"
  exit 0
fi

# 卸载不需要架构：不支持的架构不应让人连卸载都做不了。
# 在 Rosetta 转译的终端里 uname -m 报 x86_64，而 hw.optional.arm64 仍为 1：先看它，只看 uname 会在 Apple Silicon 上装 amd64 包。
# 它不存在或不为 1 时按 uname -m 判定；-i 让未知 OID 输出为空而不报错。
if [ "$(sysctl -in hw.optional.arm64 </dev/null)" = 1 ]; then ARCH=arm64
else
  case "$(uname -m)" in
    x86_64) ARCH=amd64;; arm64) ARCH=arm64;;
    *) echo "unsupported architecture: $(uname -m)" >&2; exit 1;;
  esac
fi

# 内嵌清单在任何网络操作与账户改动之前查：源码脚本、或清单里没有本机的包，都不该先建账户再失败。
PKG="heron-agent_darwin_$ARCH.tar.gz"
[ -n "$RELEASE_VERSION" ] || {
  echo "this install-macos.sh has no embedded release checksums (it is the source copy); use the install-macos.sh attached to a release: $REPO/releases" >&2
  exit 1
}
WANT_SHA256=$(printf '%s\n' "$RELEASE_SHA256" | awk -v p="$PKG" '$2 == p { print $1; n++ } END { exit n != 1 }') || {
  echo "release $RELEASE_VERSION has no embedded checksum for $PKG" >&2; exit 1
}

# 首次安装必须有注册凭据；普通升级沿用现有配置，重新注册必须显式给 --re-register。
if [ ! -f "$CFG" ] && { [ -z "$HUB" ] || [ -z "$KEY" ]; }; then
  echo "--hub and --key are required for the first install" >&2
  exit 2
fi

# 300–499 中同时未被用作 UniqueID 与 PrimaryGroupID、也未被运行中进程的 uid/gid 占用的最大值：低于 500 的账户
# 不出现在登录窗口，两个命名空间都空闲的号可以让新建的组与用户同号。从上端往下取：Apple 的系统账户从 300 往上
# 逐版追加（macOS 26.3.1 已连号占到 308），升级会替换占着这些号的第三方账户（macOS 15 替换过占 301–304 的
# _nixbld1–4）；从 499 取离这条前沿最远。低于 500 的号段没有留给第三方的保证，这只是把风险推到最远。
# 运行中进程的 uid 与 gid（ps 的这两列是有效 id）也算占用：账户记录里没有的号也可能被进程用着（被删账户的
# 残留进程）。起停确认按 uid 加可执行路径认进程（scan_svc_pids），撞上不会被误报"仍在运行"，
# 但配置 0600 的属主读取权会共享出去，取号时就避开。
free_id() {
  uids=$(dscl . -list /Users UniqueID </dev/null) || { echo "cannot list user ids" >&2; return 1; }
  gids=$(dscl . -list /Groups PrimaryGroupID </dev/null) || { echo "cannot list group ids" >&2; return 1; }
  pgids=$(ps -axo uid=,gid=) || { echo "cannot list processes" >&2; return 1; }
  printf '%s\n%s\n%s\n' "$uids" "$gids" "$pgids" |
    awk '{ used[$1] = 1; used[$2] = 1 } END { for (i = 499; i >= 300; i--) if (!(i in used)) { print i; exit 0 } exit 1 }' ||
    { echo "no free id in 300-499 for $SVC_USER" >&2; return 1; }
}

# 建用户排在下载与注册之前：它若失败，注册窗口的名额尚未消耗、旧服务尚未停止。
# 主组必须是同名组：plist 的 GroupName 与配置目录的组都写 $SVC_USER。先建组、再以它为主组建用户，建完回查，
# 不信 dscl 的退出码。
create_account() {
  # 用户已在时先核对主组，再决定要不要建组。顺序反了会在主组不符退出时留下本次新建的同名组。
  if id "$SVC_USER" >/dev/null 2>&1; then
    actual_group=$(id -gn "$SVC_USER" 2>/dev/null) || {
      echo "user $SVC_USER exists but its primary group is missing (gid $(id -g "$SVC_USER"))" >&2; exit 1; }
    [ "$actual_group" = "$SVC_USER" ] || {
      echo "user $SVC_USER exists with primary group $actual_group; expected $SVC_USER" >&2; exit 1; }
    return 0
  fi
  if group_exists; then
    rec=$(dscl . -read "/Groups/$SVC_USER" PrimaryGroupID </dev/null) || { echo "cannot read group $SVC_USER" >&2; exit 1; }
    gid=$(printf '%s\n' "$rec" | awk '$1 == "PrimaryGroupID:" { print $2 }')
    [ -n "$gid" ] || { echo "group $SVC_USER has no PrimaryGroupID" >&2; exit 1; }
    uid=$(free_id) || exit 1
  else
    gid=$(free_id) || exit 1
    uid=$gid
    dscl . -create "/Groups/$SVC_USER" PrimaryGroupID "$gid" </dev/null
    dscl . -create "/Groups/$SVC_USER" RealName "Heron agent" </dev/null
    dscl . -create "/Groups/$SVC_USER" Password '*' </dev/null
  fi
  dscl . -create "/Users/$SVC_USER" UniqueID "$uid" </dev/null
  dscl . -create "/Users/$SVC_USER" PrimaryGroupID "$gid" </dev/null
  dscl . -create "/Users/$SVC_USER" UserShell /usr/bin/false </dev/null
  dscl . -create "/Users/$SVC_USER" NFSHomeDirectory /var/empty </dev/null
  dscl . -create "/Users/$SVC_USER" RealName "Heron agent" </dev/null
  dscl . -create "/Users/$SVC_USER" Password '*' </dev/null
  dscl . -create "/Users/$SVC_USER" IsHidden 1 </dev/null
  id "$SVC_USER" >/dev/null 2>&1 || { echo "failed to create system user $SVC_USER" >&2; exit 1; }
  actual_group=$(id -gn "$SVC_USER" 2>/dev/null) || {
    echo "user $SVC_USER exists but its primary group is missing (gid $(id -g "$SVC_USER"))" >&2; exit 1; }
  [ "$actual_group" = "$SVC_USER" ] || {
    echo "user $SVC_USER exists with primary group $actual_group; expected $SVC_USER" >&2; exit 1; }
}
create_account

# --base-url 只改变从哪里取字节，接受哪些字节仍由内嵌清单决定。
[ -n "$BASE_URL" ] || BASE_URL="$REPO/releases/download/$RELEASE_VERSION"

# 缺下载器或 shasum 时在任何网络操作之前退出。macOS 自带二者与系统 CA，不需要装 CA 的分支。
command -v curl >/dev/null 2>&1 || { echo "curl is required to download $PKG" >&2; exit 1; }
command -v shasum >/dev/null 2>&1 || { echo "shasum is required to verify downloads" >&2; exit 1; }

work=$(mktemp -d)
BIN_TMP="$BIN.tmp.$$"
BIN_BAK="$BIN.bak"
# 回滚状态（rollback_agent 的读者）：ROLLBACK 在停服务之后置 1、启动确认成功后清 0；HAD_PREVIOUS 记停服务时有没有
# 旧二进制；BACKED_UP 记旧二进制是否已改名成 .bak。
ROLLBACK=0
HAD_PREVIOUS=0
BACKED_UP=0
# 停服务之后、启动确认成功之前的任何失败退出都由这里调到：换过二进制就把 .bak 换回来，再按原样载入旧版本，启停的
# 确认口径与安装路径相同。停服务时没有旧二进制（首次安装）就没有可回滚的版本，保持服务停着、脚本非零退出。plist
# 不回滚：旧二进制在新的 plist 下启动。它在 EXIT trap 里执行，set -e 仍然生效：每一步都放在条件里，失败只报告。
rollback_agent() {
  [ "$HAD_PREVIOUS" = 1 ] || return 0
  echo "installation failed after heron-agent was stopped; rolling back to the previous version" >&2
  # 新版本可能已被载入（起来后秒退时 KeepAlive 还在反复拉起它），先卸下、确认进程退出，才能换回二进制重新载入。
  if ! stop_service; then
    echo "failed to stop the new heron-agent; the previous binary is kept at $BIN_BAK" >&2
    return 0
  fi
  if [ "$BACKED_UP" = 1 ]; then
    if ! mv -f "$BIN_BAK" "$BIN"; then
      echo "failed to restore the previous binary; it is kept at $BIN_BAK" >&2
      return 0
    fi
    echo "restored the previous binary" >&2
  fi
  if [ -f "$PLIST" ] && launchctl enable "system/$LABEL" </dev/null && launchctl bootstrap system "$PLIST" </dev/null && confirm_service_started; then
    echo "the previous heron-agent is running again" >&2
  else
    echo "failed to restart the previous heron-agent" >&2
    start_log_hint
    # 换二进制的 mv 与记下 BACKED_UP 之间被信号打断时，旧二进制只剩 .bak 这一份；on_exit 不删它。
    [ ! -e "$BIN_BAK" ] || echo "the previous binary is kept at $BIN_BAK" >&2
  fi
}
# EXIT trap 覆盖正常结束、exit 与 set -e 触发的退出；INT、TERM、HUP 转成 exit 1，
# Ctrl-C 或 SSH 断开时也会回滚、清掉工作目录与写了一半的临时二进制。
# .bak 不在这里删：回滚用 mv 把它换回 BIN，成功安装在启动确认之后才删；回滚换不回去时它是旧版本仅存的一份。
on_exit() {
  rc=$?
  if [ "$rc" != 0 ] && [ "$ROLLBACK" = 1 ]; then rollback_agent; fi
  rm -rf "$work"; rm -f "$BIN_TMP"
}
trap on_exit EXIT
trap 'exit 1' INT TERM HUP

is_https() { case "$1" in https://*) return 0;; esac; return 1; }
dl() {
  # 下载地址为 https 时，请求和重定向都只走 https。其他协议（本地验收）不加：--proto '=https' 会拒绝它。
  if is_https "$1"; then curl --proto '=https' --proto-redir '=https' -fsSL -o "$2" "$1" </dev/null
  else curl -fsSL -o "$2" "$1" </dev/null; fi
}
dl "$BASE_URL/$PKG" "$work/$PKG"
# shasum 的输出以摘要开头、空白之后是文件名；它失败时摘要为空，同样按不符拒绝。
GOT_SHA256=$(shasum -a 256 "$work/$PKG" </dev/null) || GOT_SHA256=""
GOT_SHA256=${GOT_SHA256%% *}
[ "$GOT_SHA256" = "$WANT_SHA256" ] || {
  echo "checksum mismatch for $PKG from $BASE_URL: got ${GOT_SHA256:-nothing}, release $RELEASE_VERSION embeds $WANT_SHA256" >&2; exit 1
}

# 解包、检查包内文件、写临时二进制都在停服务之前做完：这些准备失败时，正在运行的旧服务不受影响。
tar -xzf "$work/$PKG" -C "$work"
for f in heron-agent "$LABEL.plist"; do
  [ -f "$work/$f" ] || { echo "package is missing $f" >&2; exit 1; }
done
mkdir -p "$(dirname "$BIN")"
install -m 0755 "$work/heron-agent" "$BIN_TMP"
# 依赖外部条件的操作（注册或 configure、账户、目录与文件的属主权限）都在停服务之前完成：它们失败时旧服务照常运行。停服务
# 之后只剩换二进制、写 plist、enable/bootstrap 与启动确认，这几步本身也可能失败（bootout 刚返回就 bootstrap 可能报
# EIO、磁盘满、新二进制秒退），失败时由 on_exit 换回旧二进制并重新载入旧版本（rollback_agent）。注册可能因 hub
# 不可达失败；对服务用户自己的文件 chown、chmod 也可能失败：文件带 uchg 标志时，本机以属主身份实测 chmod
# 报 Operation not permitted，以 root 执行时是否同样被拦本机验证不了（chflags(2) 只写 "may not be changed"）。
# 停服务之后只剩替换二进制、写 plist、bootstrap。这些操作都不需要服务停下：agent 只在启动时读一次配置
# （cmd/agent 的 run 只调用 LoadConfig），日志写在 launchd 已打开的 fd 上，改属主、权限、ACL 不影响它们。
#
# 后面以 root 对两个日志文件 chown、chmod；它们若是链接（符号链接或硬链接），就会改到另一个名字所指的文件。
# 三步的顺序承载这一点：
# 1. 日志目录若已存在必须是真目录：它在 root 属主的 /Library/Logs 下，服务用户换不掉这个目录项；下一步的
#    chown 不带 -h，是链接就会改到链接指向的目录。
# 2. 先把日志目录交给 root:wheel、0755，再去掉它的全部 ACL，然后才查其中的文件。目录此前可能对服务用户
#    可写（手工建的，或旧的安装交给了服务用户），那时服务用户能随时增删、替换其中的条目，作为属主还能给目录
#    加上 allow add_file,delete_child 这类 ACL：允许项越过数字模式生效（本机实测：带这条 ACL 的 0555 目录里
#    仍能建文件），数字 chmod 与 chown 都不去掉它，只有 chmod -N 去掉。chmod -N 必须排在 chown 之后：
#    交给 root 之前服务用户随时能把 ACL 加回来。这一步之后只有 root 能动目录里的条目，看到的条目就是后面
#    操作的条目，所以它必须排在文件检查前面。
# 3. 两个日志文件不存在，或是链接数为 1 的普通文件。硬链接 [ -L ] 为假、[ -f ] 为真，只有链接数看得出它
#    另有名字。链接数用 find -links 取：BSD 与 GNU 的 find 都支持，替身测试也在 Linux 上跑；find 失败时
#    输出同样为空，所以先看它的退出码。
if [ -L "$LOG_DIR" ] || { [ -e "$LOG_DIR" ] && [ ! -d "$LOG_DIR" ]; }; then
  echo "$LOG_DIR exists but is not a directory; refusing to take it over" >&2; exit 1
fi
mkdir -p "$LOG_DIR"
chown root:wheel "$LOG_DIR"
chmod 0755 "$LOG_DIR"
chmod -N "$LOG_DIR"
for f in "$LOG_DIR/heron-agent.log" "$LOG_DIR/heron-agent.err"; do
  if [ -L "$f" ] || [ -e "$f" ]; then
    extra=""
    if [ ! -L "$f" ] && [ -f "$f" ]; then
      extra=$(find "$f" -prune -links +1) || { echo "cannot read the link count of $f" >&2; exit 1; }
    fi
    if [ -L "$f" ] || [ ! -f "$f" ] || [ -n "$extra" ]; then
      echo "$f exists but is not a regular file with a single link; refusing to hand it to $SVC_USER" >&2; exit 1
    fi
  fi
done

# 日志目录属 root:wheel、0755、没有 ACL（上面已设好）：服务用户不能在其中增删条目，日志路径换不成链接，
# launchd 不论以哪个身份打开 StandardOutPath/StandardErrorPath 都不会被引到别处。安全性由目录属主承载，
# 不依赖 launchd 的打开身份。
# 两个日志文件每次安装都建好并交给服务用户（0640）：以服务用户身份打开时可写，以 root 打开时也可写。
# 残余：用户域作业实测，日志所在目录对作业用户不可写且文件不存在时，launchd 建不出文件，作业以 78（EX_CONFIG）
# 退出。所以文件被删后，若 launchd 以服务用户身份打开，作业会反复以 EX_CONFIG 退出，直到重跑本脚本把文件
# 建回来。这是健壮性问题，不是安全问题。文件不存在时 launchd 按 UserName 创建，属主分不出打开身份，须在真机上看作业是否以 EX_CONFIG 退出。
# 这里操作的条目就是上面检查过的那两个：目录那时已属 root 且没有 ACL，之后只有 root 能增删其中的条目。
for f in "$LOG_DIR/heron-agent.log" "$LOG_DIR/heron-agent.err"; do
  [ -e "$f" ] || : > "$f"
  chown "$SVC_USER:$SVC_USER" "$f"
  chmod 0640 "$f"
  chmod -N "$f"
done

if [ ! -f "$CFG" ] || [ "$RE_REGISTER" = 1 ]; then
  # 普通重跑不注册；显式重新注册把原配置交给 register 读取，保留已有探测策略，不先删除配置。
  # 用还没换上的新二进制注册：注册可能失败（hub 不可达、key 失效），必须在停服务之前。
  set -- register --hub "$HUB" --key "$KEY" --config "$CFG"
  if [ -n "$NAME" ]; then set -- "$@" --name "$NAME"; fi
  if [ "$INSECURE_HTTP" = 1 ]; then set -- "$@" --insecure-http; fi
  "$BIN_TMP" "$@" </dev/null
else
  if [ -n "$KEY" ]; then echo "existing config found; keeping the current registration (--key ignored)"; fi
  if [ -n "$NAME" ]; then echo "existing config found; --name ignored"; fi
  # 放行明文 http 是宿主机本地策略，只有 configure 能改（规则见 agentconfig.Config）；已有的 http 部署升级后 run 会拒绝启动，
  # 靠这一步在同一次重跑里补上。它会重写配置，可能失败（新二进制拒绝现有配置），所以与注册同在停服务之前、
  # 改属主之前：属主、权限与 ACL 由下面每次重设。
  if [ "$INSECURE_HTTP" = 1 ]; then
    "$BIN_TMP" configure --config "$CFG" --insecure-http=true </dev/null
  fi
fi
# 每次安装都做，不只在注册之后（手工重新注册、注册后被打断、账户重建后 uid 变了、人工编辑改了权限，
# 都会留下服务用户读不到或别人读得到的配置）。
# 目录属 root、0750：服务用户不能增删目录项，这里的 chown 不会被链接劫持。
# 目录无需对服务用户可写：写配置只发生在以 root 执行的 register 与 configure 里，run 只读配置。
# 数字模式只管属主、组、其他人三类；chmod -N 去掉 ACL，否则挂在上面的允许项仍让别人读得到配置。
chown "root:$SVC_USER" "$CFG_DIR"
chmod 0750 "$CFG_DIR"
chmod -N "$CFG_DIR"
chown "$SVC_USER:$SVC_USER" "$CFG"
chmod 0600 "$CFG"
chmod -N "$CFG"

stop_service
# 从这里起旧服务已停：之后任何失败退出都由 on_exit 回滚（rollback_agent），直到启动确认成功。
[ ! -e "$BIN" ] || HAD_PREVIOUS=1
ROLLBACK=1
# 同目录 rename 原子替换目录项：launchd 执行 $BIN 时看到的始终是完整的旧文件或完整的新文件。
# 旧二进制先留成同目录 .bak，回滚时换回来；首次安装没有 $BIN，也就没有 .bak。
if [ "$HAD_PREVIOUS" = 1 ]; then
  mv -f "$BIN" "$BIN_BAK"
  BACKED_UP=1
fi
mv -f "$BIN_TMP" "$BIN"

# plist 每次覆盖，改动随升级下发。它决定以什么身份运行什么程序，只能由 root 改：root:wheel、0644。
# enable 清掉可能残留的禁用覆盖（launchctl disable 跨重启有效），与 systemctl enable 同位。
# KeepAlive 隐含 RunAtLoad（launchd.plist(5)），bootstrap 即启动。
# 走到这里时 stop_service 已确认本服务的进程（按 uid 与可执行路径认）退出；同 uid 的辅助进程（launchd 派生的
# cfprefsd 之类）可能还在，它们不持有二进制或 plist。
mkdir -p "$(dirname "$PLIST")"
install -m 0644 "$work/$LABEL.plist" "$PLIST"
chown root:wheel "$PLIST"
launchctl enable "system/$LABEL" </dev/null
launchctl bootstrap system "$PLIST" </dev/null
confirm_service_started
ROLLBACK=0
rm -f "$BIN_BAK"
echo "heron-agent installed and started (launchd, $ARCH, $PKG)"
