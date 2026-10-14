#!/bin/sh
# heron-agent 安装脚本：下载、校验、注册并安装服务；重跑即升级。
# 与 libc 无关由静态链接产物承载（发布流水线的静态门禁保证），本脚本不处理。
# 以 curl … | sh -s -- 运行时，脚本本身来自 stdin。脚本里任何读 stdin 的命令都会吞掉
# 脚本余下部分，安装在中途无声结束。所以每个可能读 stdin 的外部命令都显式 </dev/null。
# 不能用 exec </dev/null：那会切断脚本自己的来源。
set -eu

# 脚本读写的系统路径都挂在 HERON_INSTALL_ROOT 下。生产运行时它为空，即真实根目录；脚本的逻辑测试以普通用户
# 把它指向临时目录，连同 PATH 上的 id、systemctl、userdel 等替身一起运行，不触碰真实系统路径。
# 它只改变本脚本读写的位置：服务定义里的可执行路径、传给 useradd 的 shell 是目标系统里的真实路径。
ROOT=${HERON_INSTALL_ROOT-}
BIN=$ROOT/usr/local/bin/heron-agent
CFG_DIR=$ROOT/etc/heron-agent
CFG=$CFG_DIR/config.json
LOG_DIR=$ROOT/var/log/heron-agent
SVC_USER=heron-agent
SYSTEMD_UNIT=$ROOT/etc/systemd/system/heron-agent.service
SYSTEMD_WANTS=$ROOT/etc/systemd/system/multi-user.target.wants/heron-agent.service
UPDATER_BIN=$ROOT/usr/local/bin/heron-updater-agent
UPDATER_UNIT=$ROOT/etc/systemd/system/heron-updater-agent.service
UPDATER_STATE=$ROOT/var/lib/heron-update-agent
# 在线更新取产物的来源：root 的决定，不放进服务用户拥有的 agent 配置。
UPDATER_CFG_DIR=$ROOT/etc/heron-update-agent
UPDATER_CFG=$UPDATER_CFG_DIR/config.json
UPDATER_RESTORE=0
OPENRC_SCRIPT=$ROOT/etc/init.d/heron-agent
OPENRC_LINK=$ROOT/etc/runlevels/default/heron-agent
PROC=$ROOT/proc
REPO=https://github.com/xjetry/heron

# 本脚本所属的版本与该版全部 tar 包的 SHA-256（每行 "<64 位十六进制>  <文件名>"），由完整 release 的发布目标 release-full 经
# deploy/releasestamp 写进下面两行标记之间（只发 hub 的 release 原样带着绑定版本的这份脚本，写入的仍是那个版本）；源码里为空，这时拒绝安装（卸载不下载，照常可用）。下载的包只按这份
# 清单校验：--base-url 能换掉下载目录里的每个文件，同目录的 SHA256SUMS 也在其中，拿它作依据挡不住篡改。
RELEASE_VERSION=""
RELEASE_SHA256=""
# >>> release stamp >>>
# <<< release stamp <<<

usage() {
  echo "usage: install.sh --hub URL --key KEY [--re-register] [--name N] [--insecure-http] [--base-url URL] [--update-source github|hub]" >&2
  echo "       install.sh --uninstall [--purge]" >&2
  exit 2
}

HUB=""; KEY=""; NAME=""; INSECURE_HTTP=0; BASE_URL=""; UNINSTALL=0; PURGE=0; RE_REGISTER=0; UPDATE_SOURCE=""
need_value() { [ "$#" -ge 2 ] || usage; }
while [ $# -gt 0 ]; do
  case "$1" in
    --hub) need_value "$@"; HUB=$2; shift 2;;
    --key) need_value "$@"; KEY=$2; shift 2;;
    --re-register) RE_REGISTER=1; shift;;
    --name) need_value "$@"; NAME=$2; shift 2;;
    --insecure-http) INSECURE_HTTP=1; shift;;
    --base-url) need_value "$@"; BASE_URL=$2; shift 2;;
    --update-source) need_value "$@"; UPDATE_SOURCE=$2; shift 2;;
    # 脚本只装自己所属的版本：版本由取哪个 URL 的脚本决定，没有第二个来源可以和内嵌清单不一致。
    --version|--version=*)
      echo "install.sh has no --version: it installs only the release it belongs to; for another version run $REPO/releases/download/<tag>/install.sh" >&2
      exit 2;;
    --uninstall) UNINSTALL=1; shift;;
    --purge) PURGE=1; shift;;
    *) usage;;
  esac
done
[ "$PURGE" = 0 ] || [ "$UNINSTALL" = 1 ] || usage
# 来源值与卸载互斥都在锁文件、下载与任何系统变更之前拒绝。
case "$UPDATE_SOURCE" in
  ""|github|hub) ;;
  *) echo "--update-source must be github or hub" >&2; exit 2;;
esac
[ -z "$UPDATE_SOURCE" ] || [ "$UNINSTALL" = 0 ] || usage
# 显式重新注册必须提供完整凭据；在锁文件、下载与系统变更之前拒绝缺省或空值。
if [ "$RE_REGISTER" = 1 ] && { [ -z "$HUB" ] || [ -z "$KEY" ]; }; then
  echo "--hub and --key are required for --re-register" >&2
  exit 2
fi

[ "$(id -u)" = 0 ] || { echo "install.sh must run as root" >&2; exit 1; }

# init 判定只认两个真实标记：都没有时是容器等无 init 环境，报错而不静默降级。
if [ -d "$ROOT/run/systemd/system" ]; then INIT=systemd
elif [ -x "$ROOT/sbin/openrc-run" ]; then INIT=openrc
else echo "unsupported init: expected systemd (/run/systemd/system) or OpenRC (/sbin/openrc-run)" >&2; exit 1; fi

# 在线更新只支持 systemd；OpenRC 主机靠重跑安装器升级，来源参数在那里没有读者，静默接受会让人以为生效了。
if [ -n "$UPDATE_SOURCE" ] && [ "$INIT" != systemd ]; then
  echo "--update-source applies only to systemd online updates; $INIT hosts update by rerunning the installer" >&2
  exit 2
fi

# /run 由 root 管理且不可被服务用户写入；锁文件不删除，避免并发安装器锁住不同 inode。
# 此锁只串行人工安装器，在线事务仍由 prepare_updater 的维护握手排除。
if [ "$INIT" = systemd ]; then
  command -v flock >/dev/null 2>&1 || { echo 'flock is required for systemd installation' >&2; exit 1; }
  INSTALL_LOCK=$ROOT/run/heron-install-agent.lock
  if [ -L "$INSTALL_LOCK" ] || { [ -e "$INSTALL_LOCK" ] && [ ! -f "$INSTALL_LOCK" ]; }; then
    echo 'installer lock is not a regular file' >&2; exit 1
  fi
  (umask 077; touch "$INSTALL_LOCK")
  exec 9>>"$INSTALL_LOCK"
  flock -n 9 </dev/null || { echo 'another heron-agent installer is running' >&2; exit 1; }
fi

service_installed() {
  case "$INIT" in
    systemd) [ -e "$SYSTEMD_UNIT" ];;
    openrc) [ -e "$OPENRC_SCRIPT" ];;
  esac
}

# 停服务后确认进程退出的轮询间隔（秒）与次数上限：sleep 合计最多 10 秒，外加每轮一次 /proc 扫描。
# 间隔用整数秒：POSIX 的 sleep 只保证整数，小数在只有整数 sleep 的系统上会让确认本身失败。
STOP_POLL_INTERVAL=1
STOP_POLL_MAX=10
# 启动后等到进程出现的上限，同一口径。出现之后再固定等 3 秒，确认还是同一个 pid。
START_POLL_INTERVAL=1
START_POLL_MAX=10

# 列出本服务的进程 pid（空格分隔，写入 svc_pids）：有效 uid 为 $1 且可执行文件是 $BIN 的进程。
# 有效 uid 是 /proc/<pid>/status 里 Uid: 行的第三列；可执行文件是 /proc/<pid>/exe 链接的目标。
# 判据“有效 uid 为服务用户且可执行文件为本服务二进制”与“是本服务的进程”等价，两个方向各有担保：
# - 选中的都是本服务的进程：$SVC_USER 是专供 agent 的 nologin 账户，由 create_account 建立、取号时避开
#   运行中进程占用的号，已有账户的撞号又被 reject_shared_uid 拒绝过；即便如此仍可能有进程共用这个
#   uid（撞号后新起的容器进程、账户记录之外的残留进程），只看 uid 会把它们算成本服务——官方容器镜像
#   （postgres、valkey）的内置用户就是 uid 999，在宿主 /proc 里以该 uid 运行、宿主账户文件里没有它们，
#   停服确认会一直等它们退出而误报“仍在运行”，可执行文件把它们排除。
# - 本服务的每个进程都被选中：由 systemd 单元的 User=heron-agent 与 ExecStart（deploy/systemd/heron-agent.service）、
#   OpenRC 脚本的 command_user 与 command（deploy/openrc/heron-agent）保证。给服务定义加以其他身份运行的
#   进程（例如以 root 执行的 ExecStartPre=+）或改用 DynamicUser= 时，这里会漏查，必须同步改判据。
# exe 链接的目标在二进制被 mv 覆盖后带 " (deleted)" 后缀，进程仍是本服务的，一并匹配。读不出 exe 的
# 活进程（root 本应读得到，/proc/<pid>/exe 不可读说明环境异常）按本服务的进程计入：宁可误报
# “仍在运行”，不漏判。僵尸进程没有 exe：已退出待收尸，不再是本服务在运行，不计入。
# 枚举与读取之间进程可能已退出，其 status 文件随之消失：读失败且文件已不在的条目按已退出跳过；
# 文件还在却读不到则无法判定，返回失败。
scan_svc_pids() {
  svc_pids=""
  [ -r "$PROC/self/status" ] || { echo "cannot inspect processes: /proc is not mounted" >&2; return 1; }
  for s in "$PROC"/[0-9]*/status; do
    if euid=$(awk '$1 == "Uid:" { print $3; exit }' "$s" 2>/dev/null); then
      if [ "$euid" = "$1" ]; then
        p=${s#"$PROC"/}; p=${p%/status}
        exe=$(readlink "$PROC/$p/exe" 2>/dev/null) || exe=""
        if [ "$exe" = "$BIN" ] || [ "$exe" = "$BIN (deleted)" ]; then
          svc_pids="$svc_pids $p"
        elif [ -z "$exe" ]; then
          state=$(awk '$1 == "State:" { print $2; exit }' "$s" 2>/dev/null)
          [ "$state" = Z ] || svc_pids="$svc_pids $p"
        fi
      fi
    elif [ -e "$s" ]; then
      echo "cannot read $s" >&2; return 1
    fi
  done
}

# 确认没有本服务的进程。它不依赖服务定义是否安装，也不看 init 报告的状态：
# OpenRC 0.55.1（Alpine 3.21）实测，supervise-daemon 被杀、子进程留存时 stop 打印
# "Unable to shut down the supervisor" 却返回 0，之后 status 为 stopped（3），子进程仍在运行。
# 判据在 scan_svc_pids 一处（有效 uid 加可执行文件），共用同一 uid 的外来进程（撞号的容器进程）
# 不等它们：它们不是本服务的进程，也停不掉。
# 用户不存在时没有可比对的 uid，直接通过。这让以下情形查不到仍在运行的旧进程：
# - 卸载路径不建账户：账户被带外删除后，这里直接通过。
# - 安装路径先由 create_account 重建账户：新账户的 uid 可能与旧进程的不同，按新 uid 查不到它。
# 不假定 stop 返回时进程已被回收：有上限地轮询，超过上限仍在才报错。
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

# 启动之后确认服务进程活着。init 的 start 返回 0 证明不了子进程仍在：
# systemd 的 Type=simple 在 fork 之后即视为已启动；OpenRC 上子进程秒退、反复拉起时 status 仍是 started。
# 判据与 confirm_service_stopped 同一处，只调用 scan_svc_pids，不另写一套扫描。
# 先等到出现有效 uid 为服务用户的进程，记下 pid，3 秒后同一个 pid 仍在：
# 秒退再拉起会换成新 pid，不能算起来了。前提与停服务确认相同，由 create_account 与服务定义保证。
start_log_hint() {
  case "$INIT" in
    systemd) echo "see journalctl -u heron-agent" >&2;;
    openrc) echo "see /var/log/heron-agent/heron-agent.err" >&2;;
  esac
}
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

# 停服务之后、启动确认成功之前的任何失败退出（某一步命令失败、新版本起不来或没留在运行、被信号打断）都由 on_exit
# 调到这里，把节点恢复成停服务之前的样子：换过二进制就把 .bak 换回来，再按原 init 启动旧版本。启停的确认口径与安装
# 路径相同，保证"回滚完成"和"安装成功"是同一把尺子。停服务时没有旧二进制（首次安装）就没有可回滚的版本，保持服务
# 停着、脚本非零退出。服务定义与更新器的文件不回滚：旧二进制在新的服务定义下启动。
# 它在 EXIT trap 里执行，set -e 仍然生效：每一步都放在条件里，失败只报告，不中断 on_exit 余下的清理。
# 读 stdin 的命令都显式 </dev/null：脚本本身经 curl | sh 从 stdin 来，吞掉 stdin 会让余下脚本消失。
rollback_agent() {
  [ "$HAD_PREVIOUS" = 1 ] || return 0
  echo "installation failed after heron-agent was stopped; rolling back to the previous version" >&2
  case "$INIT" in
    systemd) systemctl stop heron-agent </dev/null || true;;
    openrc) rc-service heron-agent stop </dev/null || true;;
  esac
  if [ "$BACKED_UP" = 1 ]; then
    if ! mv -f "$BIN_BAK" "$BIN"; then
      echo "failed to restore the previous binary; it is kept at $BIN_BAK" >&2
      return 0
    fi
    echo "restored the previous binary" >&2
  fi
  if start_service && confirm_service_started; then
    echo "the previous heron-agent is running again" >&2
  else
    echo "failed to restart the previous heron-agent" >&2
    start_log_hint
    # 换二进制的 mv 与记下 BACKED_UP 之间被信号打断时，旧二进制只剩 .bak 这一份；on_exit 不删它。
    [ ! -e "$BIN_BAK" ] || echo "the previous binary is kept at $BIN_BAK" >&2
  fi
}

start_service() {
  case "$INIT" in
    systemd) systemctl start heron-agent </dev/null;;
    openrc) rc-service heron-agent start </dev/null;;
  esac
}

# 是否发 stop 以服务定义是否安装为准，stop 命令失败即失败；确认一步不依赖它，总是执行：
# 定义不在而进程仍在时，不替人收拾，由确认失败退出。
stop_service() {
  if service_installed; then
    case "$INIT" in
      systemd) systemctl stop heron-agent </dev/null;;
      openrc) rc-service heron-agent stop </dev/null;;
    esac || { echo "failed to stop heron-agent" >&2; return 1; }
  fi
  confirm_service_stopped
}

# 维护握手由更新器在事务锁内完成；成功之后拒绝新任务，才允许安装器停掉它并替换文件。
prepare_updater() {
  [ "$INIT" = systemd ] || return 0
  if [ -e "$UPDATER_UNIT" ] || [ -e "$UPDATER_BIN" ]; then
    updater_active=$(systemctl show heron-updater-agent -p ActiveState --value </dev/null) || return 1
    case "$updater_active" in
      active|activating|reloading)
        "$UPDATER_BIN" --role agent --maintenance </dev/null || return 1
        UPDATER_RESTORE=1
        systemctl stop heron-updater-agent </dev/null || return 1;;
      inactive|failed) ;;
      *) echo "cannot determine updater service state; refusing installation" >&2; return 1;;
    esac
  fi
  if [ "$UPDATER_RESTORE" = 0 ] && { [ -e "$UPDATER_STATE/state.json" ] || [ -e "$UPDATER_STATE/pending" ]; }; then
    echo "updater recovery state exists; start heron-updater-agent before retrying" >&2; return 1
  fi
}
restore_updater() {
  if [ "$UPDATER_RESTORE" = 1 ]; then systemctl restart heron-updater-agent </dev/null || echo 'failed to restore heron-updater-agent; start it manually' >&2; fi
}

# 不给 --update-source 时不动已有文件：例行重跑不能把 hub 来源的节点悄悄切回 github，那样它下一次在线更新必然失败。
# 先写同目录临时文件再 mv，更新器启动时读不到半份文件；目录只有 root 能写，没有可被替换的目录项。
write_update_source() {
  [ -n "$UPDATE_SOURCE" ] || return 0
  mkdir -p "$UPDATER_CFG_DIR"
  chmod 0755 "$UPDATER_CFG_DIR"
  printf '{"source":"%s"}\n' "$UPDATE_SOURCE" > "$UPDATER_CFG.tmp.$$"
  chmod 0644 "$UPDATER_CFG.tmp.$$"
  mv -f "$UPDATER_CFG.tmp.$$" "$UPDATER_CFG"
}
trap restore_updater EXIT
trap 'exit 1' INT TERM HUP

# 用户与同名组都删并回查：各发行版删除工具对组的处理不一致，不能信退出码，也不能半成功还报成功。
# 遗留：账户 uid 被外来进程共用时（撞号，见 reject_shared_uid），shadow 的 userdel 会以
# “currently used by process”拒绝而卸载失败；这是拒绝自动改账户的同一条理由，由人工换号后重跑。
# 要不要发删除命令看本地 /etc/passwd、/etc/group 的记录，与 userdel、groupdel 操作的对象同一口径：
# id 走 NSS，解析结果可能来自 LDAP、sssd 之类的非本地源，那时 userdel 以"用户不存在"失败（shadow 4.13
# 实测：nss_wrapper 注入的非本地用户 id 解析得到，userdel 报 does not exist 并以 6 退出），
# set -e 让脚本以它的退出码结束、说不出原因。回查用户仍用 id：按名字还解析得到它，就不能报成已删除。
local_user_exists() { grep -q "^$SVC_USER:" "$ROOT/etc/passwd"; }
local_group_exists() { grep -q "^$SVC_USER:" "$ROOT/etc/group"; }
delete_account() {
  if local_user_exists; then
    if command -v userdel >/dev/null 2>&1; then userdel "$SVC_USER" </dev/null
    elif command -v deluser >/dev/null 2>&1; then deluser "$SVC_USER" </dev/null
    fi
  fi
  if local_group_exists; then
    if command -v groupdel >/dev/null 2>&1; then groupdel "$SVC_USER" </dev/null
    elif command -v delgroup >/dev/null 2>&1; then delgroup "$SVC_USER" </dev/null
    fi
  fi
  if id "$SVC_USER" >/dev/null 2>&1 || local_group_exists; then
    echo "failed to delete user or group $SVC_USER" >&2; exit 1
  fi
}

if [ "$UNINSTALL" = 1 ]; then
  prepare_updater
  stop_service
  case "$INIT" in
    systemd)
      if service_installed; then
        systemctl disable heron-agent </dev/null
      fi
      # enable 按单元 [Install] 段的 WantedBy=multi-user.target 建这个链接，两处改一处必须同步。
      # 单元文件被手工删掉时上面不发 disable，链接悬空留下；判链接本身（-L），与 OpenRC 侧同口径。
      if [ -L "$SYSTEMD_WANTS" ]; then
        rm -f "$SYSTEMD_WANTS"
      fi
      rm -f "$SYSTEMD_UNIT"
      if [ -f "$UPDATER_UNIT" ]; then systemctl disable heron-updater-agent </dev/null; fi
      rm -f "$UPDATER_UNIT" "$UPDATER_BIN" "$ROOT/etc/systemd/system/multi-user.target.wants/heron-updater-agent.service"
      # 维护握手已排除在途事务；卸载更新器同时移除其历史和回滚备份，业务配置仍由 --purge 决定。
      rm -rf "$UPDATER_STATE" "$ROOT/run/heron-update-agent"
      UPDATER_RESTORE=0
      # 只清除本服务的本地定制；不用 DropInPaths 展开共享配置，也不跟随目录符号链接。
      if [ "$PURGE" = 1 ]; then
        rm -rf "$SYSTEMD_UNIT.d" "$ROOT/run/systemd/system/heron-agent.service.d"
        rm -rf "$UPDATER_UNIT.d" "$ROOT/run/systemd/system/heron-updater-agent.service.d"
      fi
      systemctl daemon-reload </dev/null;;
    openrc)
      # 判链接本身（-L）而不跟随它：init 脚本被删后链接悬空，-e 会判为不存在而留下它。
      # OpenRC 0.55.1 实测：init 脚本不存在时 rc-update del 仍删掉悬空链接并返回 0。
      if [ -L "$OPENRC_LINK" ]; then
        rc-update del heron-agent default </dev/null
      fi
      rm -f "$OPENRC_SCRIPT";;
  esac
  rm -f "$BIN"
  if [ "$PURGE" = 1 ]; then
    rm -rf "$CFG_DIR" "$LOG_DIR" "$UPDATER_CFG_DIR"
    delete_account
  fi
  echo "heron-agent uninstalled"
  exit 0
fi

# 卸载不需要架构：不支持的架构不应让人连卸载都做不了。
case "$(uname -m)" in
  x86_64) ARCH=amd64;; aarch64) ARCH=arm64;; armv7l) ARCH=armv7;;
  i386|i686) ARCH=386;; riscv64) ARCH=riscv64;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1;;
esac

# 内嵌清单在任何网络操作与账户改动之前查：源码脚本、或清单里没有本机的包，都不该先建用户、装 CA 再失败。
PKG="heron-agent_linux_$ARCH.tar.gz"
[ -n "$RELEASE_VERSION" ] || {
  echo "this install.sh has no embedded release checksums (it is the source copy); use the install.sh attached to a release: $REPO/releases" >&2
  exit 1
}
WANT_SHA256=$(printf '%s\n' "$RELEASE_SHA256" | awk -v p="$PKG" '$2 == p { print $1; n++ } END { exit n != 1 }') || {
  echo "release $RELEASE_VERSION has no embedded checksum for $PKG" >&2; exit 1
}
if [ "$INIT" = systemd ]; then
  UPDATER_PKG=heron-updater_linux_$ARCH.tar.gz
  UPDATER_SHA256=$(printf '%s\n' "$RELEASE_SHA256" | awk -v p="$UPDATER_PKG" '$2 == p { print $1; n++ } END { exit n != 1 }') || {
    echo "release $RELEASE_VERSION has no embedded checksum for $UPDATER_PKG" >&2; exit 1
  }
fi

# 首次安装必须有注册凭据；普通升级沿用现有配置，重新注册必须显式给 --re-register。
if [ ! -f "$CFG" ] && { [ -z "$HUB" ] || [ -z "$KEY" ]; }; then
  echo "--hub and --key are required for the first install" >&2
  exit 2
fi

# 系统号的占用集合：/etc/passwd 的 uid、/etc/group 的 gid，以及每个运行中进程 Uid:/Gid: 行的全部四个
# 字段与 Groups: 补充组。只看账户文件会把“没有账户记录的号”当成空闲：官方容器镜像的内置用户
# （postgres、valkey 都是 uid 999）在宿主 /proc 里以该 uid 运行，宿主账户文件里没有它们，
# useradd 自动取号（SYS_UID_MAX 往下第一个空闲）会取到 999，装出来的账户与这些进程同 uid。
# /proc 不可读时不退回只看账户文件：那正是取不出安全号的状态。
used_system_ids() {
  [ -r "$PROC/self/status" ] || { echo "cannot inspect processes: /proc is not mounted" >&2; return 1; }
  {
    awk -F: '{ print $3 }' "$ROOT/etc/passwd" 2>/dev/null
    awk -F: '{ print $3 }' "$ROOT/etc/group" 2>/dev/null
    awk '$1 == "Uid:" || $1 == "Gid:" { print $2; print $3; print $4; print $5 }
      $1 == "Groups:" { for (i = 2; i <= NF; i++) print $i }' "$PROC"/[0-9]*/status 2>/dev/null
  } | sort -un
}

# 999 往下第一个没被占用的号；用户与组同号，一个号同时通过两个命名空间的检查。
# 下限 100：更低的号段是发行版静态保留的系统账户，不取。号段全满时失败退出，不放宽。
free_system_id() {
  used_system_ids | awk 'BEGIN { for (i = 999; i >= 100; i--) free[i] = 1 }
    { delete free[$1] }
    END { for (i = 999; i >= 100; i--) if (i in free) { print i; exit 0 } exit 1 }'
}

# 列出与服务用户共用 uid 的外来进程（写入 shared，每项 " pid:exe"）。Uid: 行的四个字段
# （real/effective/saved/fs）任一命中都算共用：任一身份都能读 0600 属主文件或切回该 uid。
# 本服务自己的进程（exe 为 $BIN，升级时它正在运行是常态）与已退出待收尸的僵尸不计。
# 判据与 scan_svc_pids 同一处。枚举与读取之间进程已退出的条目跳过；文件还在却读不到则失败。
foreign_uid_procs() {
  shared=""
  [ -r "$PROC/self/status" ] || { echo "cannot inspect processes: /proc is not mounted" >&2; return 1; }
  for s in "$PROC"/[0-9]*/status; do
    if uids=$(awk '$1 == "Uid:" { print $2, $3, $4, $5; exit }' "$s" 2>/dev/null); then
      case " $uids " in
        *" $1 "*)
          p=${s#"$PROC"/}; p=${p%/status}
          exe=$(readlink "$PROC/$p/exe" 2>/dev/null) || exe=""
          if [ "$exe" != "$BIN" ] && [ "$exe" != "$BIN (deleted)" ]; then
            state=$(awk '$1 == "State:" { print $2; exit }' "$s" 2>/dev/null)
            [ "$state" = Z ] || shared="$shared $p:${exe:-unknown}"
          fi;;
      esac
    elif [ -e "$s" ]; then
      echo "cannot read $s" >&2; return 1
    fi
  done
}

# 已有账户的 uid 被外来进程共用时，在任何下载、注册与系统变更之前拒绝：配置 0600 的属主就是这个 uid，
# 共用的进程（撞号的容器进程、被删账户的残留进程）读得到节点凭据，也能对本服务的进程发信号、ptrace。
# 这是安装器不能安全自动修的状态：usermod 改不了运行中进程的 euid，属主为该 uid 的文件也枚举不全，
# 换号由管理员在安装器外做完，重跑沿用已有注册。
reject_shared_uid() {
  foreign_uid_procs "$1" || return 1
  [ -z "$shared" ] || {
    echo "uid $1 ($SVC_USER) is also used by other running processes:$shared" >&2
    echo "they would be able to read the agent config (0600, owned by this uid) and to signal its processes" >&2
    echo "move $SVC_USER to an id no process uses (usermod/userdel may refuse while these processes run; editing the id in /etc/passwd and /etc/group works too) and rerun the installer; the existing registration is kept" >&2
    exit 1
  }
}

# 建用户排在下载与注册之前：它若失败，注册窗口的名额尚未消耗、旧服务尚未停止。
# 主组必须是同名组：OpenRC 的 command_user 与配置文件属主都写 heron-agent:heron-agent，
# 而 busybox 的 adduser 不指定组时会把用户放进 nogroup。先建组、再以它为主组建用户，建完回查。
# 取号避开运行中进程占用的号（free_system_id），显式传给各实现：让 useradd 自动取号只保证“账户文件里空闲”。
create_account() {
  nologin=/sbin/nologin
  [ -x "$ROOT/usr/sbin/nologin" ] && nologin=/usr/sbin/nologin
  # 用户已在时先核对主组与 uid 占用，再决定要不要建组。顺序反了会在主组不符退出时留下本次新建的同名组。
  if id "$SVC_USER" >/dev/null 2>&1; then
    actual_group=$(id -gn "$SVC_USER" 2>/dev/null) || {
      echo "user $SVC_USER exists but its primary group is missing (gid $(id -g "$SVC_USER"))" >&2; exit 1; }
    [ "$actual_group" = "$SVC_USER" ] || {
      echo "user $SVC_USER exists with primary group $actual_group; expected $SVC_USER" >&2; exit 1; }
    reject_shared_uid "$(id -u "$SVC_USER")" || exit 1
    return 0
  fi
  new_id=$(free_system_id) || { echo "no free system id in 100-999 for $SVC_USER (account files and running processes)" >&2; exit 1; }
  if ! local_group_exists; then
    if command -v groupadd >/dev/null 2>&1; then
      groupadd --system -g "$new_id" "$SVC_USER" </dev/null
    elif command -v addgroup >/dev/null 2>&1; then
      if addgroup --help </dev/null 2>&1 | grep -qi busybox; then addgroup -S -g "$new_id" "$SVC_USER" </dev/null
      else addgroup --system --gid "$new_id" "$SVC_USER" </dev/null; fi
    else
      echo "no groupadd or addgroup available to create the system group" >&2; exit 1
    fi
  fi
  if ! id "$SVC_USER" >/dev/null 2>&1; then
    if command -v useradd >/dev/null 2>&1; then
      useradd --system -u "$new_id" -M -d /nonexistent -s "$nologin" -g "$SVC_USER" "$SVC_USER" </dev/null
    elif command -v adduser >/dev/null 2>&1; then
      # adduser 有两种不兼容的实现（busybox 与 Debian perl 版），按实现分支；
      # 建完必须回查——perl 版收到 busybox 风格参数会打印用法、不建用户，却返回 0。
      if adduser --help </dev/null 2>&1 | grep -qi busybox; then
        adduser -S -D -H -u "$new_id" -s "$nologin" -G "$SVC_USER" "$SVC_USER" </dev/null
      else
        adduser --system --no-create-home --shell "$nologin" --ingroup "$SVC_USER" --uid "$new_id" "$SVC_USER" </dev/null
      fi
    else
      echo "no useradd or adduser available to create the system user" >&2; exit 1
    fi
  fi
  id "$SVC_USER" >/dev/null 2>&1 || { echo "failed to create system user $SVC_USER" >&2; exit 1; }
  actual_group=$(id -gn "$SVC_USER" 2>/dev/null) || {
    echo "user $SVC_USER exists but its primary group is missing (gid $(id -g "$SVC_USER"))" >&2; exit 1; }
  [ "$actual_group" = "$SVC_USER" ] || {
    echo "user $SVC_USER exists with primary group $actual_group; expected $SVC_USER" >&2; exit 1; }
}
create_account

# --base-url 只改变从哪里取字节，接受哪些字节仍由内嵌清单决定。
[ -n "$BASE_URL" ] || BASE_URL="$REPO/releases/download/$RELEASE_VERSION"

# 缺下载器或 sha256sum 时在任何网络操作之前退出：否则会先 apt-get update 装 CA，再报没有 curl。
if command -v curl >/dev/null 2>&1; then FETCH=curl
elif command -v wget >/dev/null 2>&1; then FETCH=wget
else echo "neither curl nor wget is available to download $PKG" >&2; exit 1; fi
command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required to verify downloads" >&2; exit 1; }

ca_bundle_present() {
  for f in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt /etc/ssl/cert.pem /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem; do
    [ -f "$ROOT$f" ] && return 0
  done
  return 1
}
is_https() { case "$1" in https://*) return 0;; esac; return 1; }
# systemd 更新器始终通过官方 HTTPS 下载，不能因当前 hub 与安装镜像为 HTTP 就省略 CA。
if [ "$INIT" = systemd ] || is_https "$BASE_URL" || is_https "$HUB"; then
  if ! ca_bundle_present; then
    if command -v apt-get >/dev/null 2>&1; then
      apt-get update </dev/null && DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates </dev/null
    elif command -v apk >/dev/null 2>&1; then
      apk add --no-cache ca-certificates </dev/null
    elif command -v dnf >/dev/null 2>&1; then
      dnf install -y ca-certificates </dev/null
    elif command -v yum >/dev/null 2>&1; then
      yum install -y ca-certificates </dev/null
    else
      echo "no CA bundle found and no supported package manager to install ca-certificates" >&2; exit 1
    fi
    ca_bundle_present || { echo "ca-certificates installation did not produce a CA bundle" >&2; exit 1; }
  fi
fi

work=$(mktemp -d)
BIN_TMP="$BIN.tmp.$$"
BIN_BAK="$BIN.bak"
UPDATER_TMP="$UPDATER_BIN.tmp.$$"
# 回滚状态（rollback_agent 的读者）：ROLLBACK 在停服务之后置 1、启动确认成功后清 0；HAD_PREVIOUS 记停服务时有没有
# 旧二进制；BACKED_UP 记旧二进制是否已改名成 .bak。
ROLLBACK=0
HAD_PREVIOUS=0
BACKED_UP=0
# EXIT trap 覆盖正常结束、exit 与 set -e 触发的退出。dash 与 busybox ash 被信号终止时不执行 EXIT trap，
# 所以把 INT、TERM、HUP 转成 exit 1，Ctrl-C 或 SSH 断开时也会回滚、清掉工作目录与写了一半的临时文件。
# .bak 不在这里删：回滚用 mv 把它换回 BIN，成功安装在启动确认之后才删；回滚换不回去时它是旧版本仅存的一份。
on_exit() {
  rc=$?
  if [ "$rc" != 0 ] && [ "$ROLLBACK" = 1 ]; then rollback_agent; fi
  rm -rf "$work"; rm -f "$BIN_TMP" "$UPDATER_TMP"
  restore_updater
}
trap on_exit EXIT
trap 'exit 1' INT TERM HUP

dl() {
  # 下载地址为 https 时，请求和重定向都只走 https。http（本地验收）不加：--proto '=https' 会拒绝它。
  # wget 没有对应开关。
  if [ "$FETCH" = curl ]; then
    if is_https "$1"; then curl --proto '=https' --proto-redir '=https' -fsSL -o "$2" "$1" </dev/null
    else curl -fsSL -o "$2" "$1" </dev/null; fi
  else wget -q -O "$2" "$1" </dev/null; fi
}
dl "$BASE_URL/$PKG" "$work/$PKG"
# sha256sum 的输出以摘要开头、空白之后是文件名（GNU 与 busybox 相同）；它失败时摘要为空，同样按不符拒绝。
GOT_SHA256=$(sha256sum "$work/$PKG" </dev/null) || GOT_SHA256=""
GOT_SHA256=${GOT_SHA256%% *}
[ "$GOT_SHA256" = "$WANT_SHA256" ] || {
  echo "checksum mismatch for $PKG from $BASE_URL: got ${GOT_SHA256:-nothing}, release $RELEASE_VERSION embeds $WANT_SHA256" >&2; exit 1
}

# 依赖外部条件的操作都在停服务之前做完：解包、检查包内文件、写临时二进制、注册或 configure、设配置的属主与权限。
# 这些失败时正在运行的旧服务不受影响；停服务之后只剩替换二进制、装服务定义、启动，这几步本身也可能失败
# （磁盘满、新二进制秒退），失败时由 on_exit 换回旧二进制并重启旧版本（rollback_agent）。前面的步骤都不需要服务停下：
# agent 只在启动时读一次配置（cmd/agent 的 run 只调用 LoadConfig）。
tar -xzf "$work/$PKG" -C "$work"
for f in heron-agent heron-agent.service heron-agent.openrc; do
  [ -f "$work/$f" ] || { echo "package is missing $f" >&2; exit 1; }
done
install -m 0755 "$work/heron-agent" "$BIN_TMP"
if [ "$INIT" = systemd ]; then
  dl "$BASE_URL/$UPDATER_PKG" "$work/$UPDATER_PKG"
  got=$(sha256sum "$work/$UPDATER_PKG" </dev/null) || got=""
  [ "${got%% *}" = "$UPDATER_SHA256" ] || { echo "checksum mismatch for $UPDATER_PKG" >&2; exit 1; }
  mkdir "$work/updater"
  tar -xzf "$work/$UPDATER_PKG" -C "$work/updater"
  for f in heron-updater heron-updater-agent.service heron-updater-hub.service; do
    if ! { [ -f "$work/updater/$f" ] && [ ! -L "$work/updater/$f" ]; }; then
      echo "package is missing regular file $f" >&2; exit 1
    fi
  done
  install -m 0755 "$work/updater/heron-updater" "$UPDATER_TMP"
fi
prepare_updater

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
  # 改属主之前：属主与权限由下面的 chown、chmod 每次重设。
  if [ "$INSECURE_HTTP" = 1 ]; then
    "$BIN_TMP" configure --config "$CFG" --insecure-http=true </dev/null
  fi
fi
# 每次安装都做，不只在注册之后。只做一次会留下服务用户读不到或别人读得到的配置：
# - 配置不存在时以 root 手工注册（agentconfig.Save 只对已存在的文件沿用属主，新建的文件属调用者）
# - 注册之后、改属主之前被信号打断，重跑走已有配置分支
# - 账户被删后以新 uid 重建，配置仍属旧 uid
# - 人工编辑后权限变了（0600 只由 agentconfig.Save 在注册时保证）
# 目录属 root、0750：服务用户不能增删目录项，这里的 chown 不会被链接劫持。
# 目录无需对服务用户可写：写配置只发生在以 root 执行的 register 与 configure 里，cmd/agent 的 run 只读配置、不写配置。
chown root:"$SVC_USER" "$CFG_DIR"
chmod 0750 "$CFG_DIR"
chown "$SVC_USER:$SVC_USER" "$CFG"
chmod 0600 "$CFG"

stop_service
# 从这里起旧服务已停：之后任何失败退出都由 on_exit 回滚（rollback_agent），直到启动确认成功。
[ ! -e "$BIN" ] || HAD_PREVIOUS=1
ROLLBACK=1
# 同目录 rename 原子替换目录项：exec $BIN 看到的始终是完整的旧文件或完整的新文件；
# 写了一半的临时文件由 on_exit 删除。旧二进制先留成同目录 .bak，回滚时换回来；首次安装没有 $BIN，也就没有 .bak。
if [ "$HAD_PREVIOUS" = 1 ]; then
  mv -f "$BIN" "$BIN_BAK"
  BACKED_UP=1
fi
mv -f "$BIN_TMP" "$BIN"

# 服务定义每次覆盖，单元的改动随升级下发。
# 走到这里时没有以服务用户运行的进程：stop_service 不论服务定义在不在都已确认，查到就已退出。
# 所以用 start，不依赖 restart 对已停服务等价于 start。
case "$INIT" in
  systemd)
    # 更新器在 agent 确认运行之后才 start，启动时读来源配置：先写配置再换二进制。
    write_update_source
    mv -f "$UPDATER_TMP" "$UPDATER_BIN"
    install -m 0644 "$work/updater/heron-updater-agent.service" "$UPDATER_UNIT"
    install -m 0644 "$work/heron-agent.service" "$SYSTEMD_UNIT"
    systemctl daemon-reload </dev/null
    systemctl enable heron-agent </dev/null;;
  openrc)
    install -m 0755 "$work/heron-agent.openrc" "$OPENRC_SCRIPT"
    # 重跑时它已在 default runlevel 里；只在不在时才加，不依赖 rc-update 对重复 add 的退出码。
    [ -L "$OPENRC_LINK" ] || rc-update add heron-agent default </dev/null;;
esac
start_service
confirm_service_started
ROLLBACK=0
rm -f "$BIN_BAK"
# 更新器在 agent 确认运行之后才启动：它起不来不该把已经在跑的新 agent 回滚掉。失败时脚本非零退出；更新器原本在运行的话，
# on_exit 的 restore_updater 会再拉起它一次。
if [ "$INIT" = systemd ]; then
  systemctl enable heron-updater-agent </dev/null
  systemctl start heron-updater-agent </dev/null
  UPDATER_RESTORE=0
fi
echo "heron-agent installed and started ($INIT, $ARCH, $PKG)"
