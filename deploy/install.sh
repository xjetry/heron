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

# init 是否在运行，即 rc-service、systemctl 能不能启停服务。systemd 的标记 /run/systemd/system 只在 systemd 作为 init
# 运行时存在（sd_booted(3) 的判据），判出 systemd 即在运行。OpenRC 的 /sbin/openrc-run 只说明装了；在运行的标记是
# OpenRC 进入运行级时写下的 /run/openrc/softlevel。Alpine 3.22（openrc 0.62.6）实测：容器里 apk add openrc、不以它
# 为 init 启动时没有这个文件，rc-service 的 start、stop 都返回 1（status 提示 openrc did not boot）；rc-update 的
# add、del 只改 runlevel 目录里的链接，照常返回 0。
init_running() { [ "$INIT" = systemd ] || [ -e "$ROOT/run/openrc/softlevel" ]; }

# 在线更新只支持 systemd；OpenRC 主机靠重跑安装器升级，来源参数在那里没有读者，静默接受会让人以为生效了。
if [ -n "$UPDATE_SOURCE" ] && [ "$INIT" != systemd ]; then
  echo "--update-source applies only to systemd online updates; $INIT hosts update by rerunning the installer" >&2
  exit 2
fi

# 安装要由 init 启动服务，而注册排在启动之前：init 不在运行时装下去，注册已在 hub 上建好节点、用掉一个名额，启动
# 却必然失败，留下一个永久离线的节点，之后的重跑也卡在启停上。所以在锁、账户、下载与注册之前拒绝，指向不依赖宿主
# init 的 Docker 镜像。卸载不需要 init 启动任何东西，照常进行（见 stop_service）。
if [ "$UNINSTALL" = 0 ] && ! init_running; then
  echo "OpenRC is installed but not running (no /run/openrc/softlevel), so heron-agent could not be started as a service; nothing was changed" >&2
  echo "in a container, run the agent Docker image ghcr.io/xjetry/heron-agent instead: $REPO/blob/main/docs/install.md#docker" >&2
  exit 1
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

# 暂存文件一律命名为"<目标路径>.tmp.<安装器的 pid>"（下面三个 *_TMP），在目标所在目录写好再 mv 过去，退出时由 on_exit
# 删掉。安装器被 SIGKILL、断电打断时 on_exit 不执行，暂存文件留在 /usr/local/bin 这类持久目录里；pid 每次不同，之后的
# 运行既不覆盖也不认得它们。所以每次启动按同一命名规则回收：只删 pid 部分全是数字的普通文件（不跟随链接），且最后修改
# 超过一天（-mmin +1440，GNU find、busybox find 与 BSD find 实测同一口径）——同时在跑的另一次安装（OpenRC 路径没有
# 安装锁）正在写的暂存文件不到一天，不会被删。工作目录在 mktemp 的临时目录里，归系统的临时目录清理管，不在此列。
BIN_TMP=$BIN.tmp.$$
UPDATER_TMP=$UPDATER_BIN.tmp.$$
UPDATER_CFG_TMP=$UPDATER_CFG.tmp.$$
remove_stale_temps() {
  for target in "$@"; do
    for f in "$target".tmp.*; do
      case ${f#"$target".tmp.} in ''|*[!0-9]*) continue;; esac
      if [ -f "$f" ] && [ ! -L "$f" ] && [ -n "$(find "$f" -prune -mmin +1440)" ]; then rm -f "$f"; fi
    done
  done
}
remove_stale_temps "$BIN" "$UPDATER_BIN" "$UPDATER_CFG"

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

# 退出处理及其调用链用这个函数写 stderr：macOS 自带的 bash 3.2（/bin/sh）里，内建 echo/printf 写到不可写的 fd
# （EIO、EBADF、EPIPE）失败后不会清空 C stdio 的 stdout 缓冲，留下的字节会在之后每一次 $(...) 命令替换里混进捕获结果，
# 让 scan_svc_pids、id -u 拿到错值而打断回滚（bash 3.2.57 实测）。放在子 shell 里写，失败时缓冲随子进程丢弃，不污染父 shell。
warn() { ( printf '%s\n' "$*" >&2 ); }

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
  [ -r "$PROC/self/status" ] || { warn "cannot inspect processes: /proc is not mounted"; return 1; }
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
      warn "cannot read $s"; return 1
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
  warn "heron-agent is still running: processes with uid $svc_uid ($SVC_USER):$svc_pids"
  return 1
}

# 启动之后确认服务进程活着。init 的 start 返回 0 证明不了子进程仍在：
# systemd 的 Type=simple 在 fork 之后即视为已启动；OpenRC 上子进程秒退、反复拉起时 status 仍是 started。
# 判据与 confirm_service_stopped 同一处，只调用 scan_svc_pids，不另写一套扫描。
# 先等到出现有效 uid 为服务用户的进程，记下 pid，3 秒后同一个 pid 仍在：
# 秒退再拉起会换成新 pid，不能算起来了。前提与停服务确认相同，由 create_account 与服务定义保证。
start_log_hint() {
  case "$INIT" in
    systemd) warn "see journalctl -u heron-agent";;
    openrc) warn "see /var/log/heron-agent/heron-agent.err";;
  esac
}
confirm_service_started() {
  svc_uid=$(id -u "$SVC_USER") || { warn "no service user $SVC_USER"; start_log_hint; return 1; }
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
    warn "heron-agent did not start"
    start_log_hint
    return 1
  fi
  sleep 3
  scan_svc_pids "$svc_uid" || return 1
  case " $svc_pids " in
    *" $pid "*) return 0;;
  esac
  warn "heron-agent did not stay running (pid $pid)"
  start_log_hint
  return 1
}

# 停服务确认完成（ROLLBACK 置 1）之后、启动确认成功之前的任何失败退出（某一步命令失败、新版本起不来或没留在运行、
# 被信号打断）都由 on_exit 调到这里，把节点恢复成停服务之前的样子：换过二进制就把 .bak 换回来，再按原 init 启动旧版本。
# 启停的确认口径与安装路径相同，保证"回滚完成"和"安装成功"是同一把尺子。停服务时没有旧二进制（首次安装）就没有可回滚
# 的版本，保持服务停着、脚本非零退出。服务定义与更新器的文件不回滚：旧二进制在新的服务定义下启动。
# 它只由 on_exit 调用，那时信号已被忽略、set -e 已关闭、父 shell 的 stdout 缓冲已清空（理由见 on_exit 与 warn）：
# 每一步都写成条件式，失败只报告，不中断余下的步骤；写诊断经 warn 隔离在子 shell，失败也不污染命令替换。
# 读 stdin 的命令都显式 </dev/null：脚本本身经 curl | sh 从 stdin 来，吞掉 stdin 会让余下脚本消失。
rollback_agent() {
  [ "$HAD_PREVIOUS" = 1 ] || return 0
  warn "installation failed after heron-agent was stopped; rolling back to the previous version"
  case "$INIT" in
    systemd) systemctl stop heron-agent </dev/null || true;;
    openrc) rc-service heron-agent stop </dev/null || true;;
  esac
  if [ "$BACKED_UP" = 1 ]; then
    if ! mv -f "$BIN_BAK" "$BIN"; then
      warn "failed to restore the previous binary; it is kept at $BIN_BAK"
      return 0
    fi
    warn "restored the previous binary"
  fi
  if start_service && confirm_service_started; then
    warn "the previous heron-agent is running again"
  else
    warn "failed to restart the previous heron-agent"
    start_log_hint
    # 换二进制的 mv 与记下 BACKED_UP 之间被信号打断时，旧二进制只剩 .bak 这一份；on_exit 不删它。
    [ ! -e "$BIN_BAK" ] || warn "the previous binary is kept at $BIN_BAK"
  fi
}

start_service() {
  case "$INIT" in
    systemd) systemctl start heron-agent </dev/null;;
    openrc) rc-service heron-agent start </dev/null;;
  esac
}

# 是否发 stop 以服务定义是否安装、init 是否在运行为准，stop 命令失败即失败；确认一步不依赖它，总是执行：
# 定义不在而进程仍在时，不替人收拾，由确认失败退出。OpenRC 不在运行时 rc-service stop 必然返回 1（见 init_running），
# 也没有监督进程在管这个服务，进程在不在只剩 /proc 扫描回答得了。只有卸载会在 init 不在运行时走到这里：安装在那之前
# 已被拒绝。
stop_service() {
  if service_installed && init_running; then
    case "$INIT" in
      systemd) systemctl stop heron-agent </dev/null;;
      openrc) rc-service heron-agent stop </dev/null;;
    esac || { warn "failed to stop heron-agent"; return 1; }
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
  if [ "$UPDATER_RESTORE" = 1 ]; then systemctl restart heron-updater-agent </dev/null || warn 'failed to restore heron-updater-agent; start it manually'; fi
}

# 不给 --update-source 时不动已有文件：例行重跑不能把 hub 来源的节点悄悄切回 github，那样它下一次在线更新必然失败。
# 先写同目录临时文件再 mv，更新器启动时读不到半份文件；目录只有 root 能写，没有可被替换的目录项。
write_update_source() {
  [ -n "$UPDATE_SOURCE" ] || return 0
  mkdir -p "$UPDATER_CFG_DIR"
  chmod 0755 "$UPDATER_CFG_DIR"
  printf '{"source":"%s"}\n' "$UPDATE_SOURCE" > "$UPDATER_CFG_TMP"
  chmod 0644 "$UPDATER_CFG_TMP"
  mv -f "$UPDATER_CFG_TMP" "$UPDATER_CFG"
}

# 回滚状态（rollback_agent 的读者）：ROLLBACK 在停服务确认完成之后置 1、启动确认成功后清 0；HAD_PREVIOUS 记停服务时有没有
# 旧二进制；BACKED_UP 记旧二进制是否已改名成 .bak。work 是下载解包用的工作目录，建出之前为空。
BIN_BAK=$BIN.bak
ROLLBACK=0
HAD_PREVIOUS=0
BACKED_UP=0
work=""
# 退出处理，本脚本唯一的 EXIT trap：停服务之后的失败退出要回滚（rollback_agent），工作目录与暂存文件要删，维护握手
# 停掉的更新器要拉起来（restore_updater）。这些必须在任何退出方式下做完，靠进门的两步保证：
# - 先忽略 HUP、INT、QUIT、TERM、TSTP、PIPE。退出处理期间再来的信号（SSH 断开的 SIGHUP、再按一次 Ctrl-C、外部 TERM）
#   若仍按下面的 trap 转成 exit，会在这里直接结束脚本，回滚停在半路：旧二进制留在 .bak、服务停着；SIGPIPE（tee 之类
#   的读端先退出）与 Ctrl-Z 也一样。忽略的信号由之后启动的子进程继承（execve 保留 SIG_IGN），回滚里的 systemctl、
#   rc-service 因而也不被同一次挂断打断。副作用只落在 OpenRC 上：systemd 与 launchd 由 init 进程派生服务，回滚启动的
#   服务不经本脚本 fork；supervise-daemon 会重设 HUP、INT、QUIT、TERM，PIPE 与 TSTP 的忽略则传到它与 agent
#   （openrc 0.62.6 实测）。agent 不受影响：Go 运行时不论继承什么都为 SIGPIPE 装自己的处理（实测），TSTP 对守护进程
#   没有用处；下一次正常启停（重跑安装器、rc-service restart、重启机器）即恢复默认。
# - 再关掉 set -e，并将父 shell 的 stdout 缓冲刷到 /dev/null 清空。写诊断在这里会失败：SSH 断开后写终端报 EIO，
#   读端退出后写管道报 EPIPE（PIPE 已忽略，不再被它杀掉），set -e 下这一条输出失败就会结束脚本。回滚与清理的每一步
#   都写成条件式、失败只报告，不依赖 set -e；刷空缓冲配合 warn 子 shell 隔离，防止 bash 3.2 下写失败残留的缓冲污染
#   回滚里的 $(...) 命令替换（见 warn）。
# rc 必须第一个取：之后的任何命令都会改写 $?。
on_exit() {
  rc=$?
  trap '' HUP INT QUIT TERM TSTP PIPE
  set +e
  echo >/dev/null
  if [ "$rc" != 0 ] && [ "$ROLLBACK" = 1 ]; then rollback_agent; fi
  # .bak 不在这里删：回滚用 mv 把它换回 BIN，成功安装在启动确认之后才删；回滚换不回去时它是旧版本仅存的一份。
  if [ -n "$work" ]; then rm -rf "$work"; fi
  rm -f "$BIN_TMP" "$UPDATER_TMP" "$UPDATER_CFG_TMP"
  restore_updater
}
trap on_exit EXIT
# EXIT trap 覆盖正常结束、exit 与 set -e 触发的退出。dash 与 busybox ash 被信号终止时不执行 EXIT trap，所以把会终止
# 脚本的信号都转成 exit 1：Ctrl-C（INT）、Ctrl-\（QUIT）、SSH 断开（HUP）、外部 TERM、输出管道的读端退出（PIPE）
# 都经 on_exit 回滚、清理。
trap 'exit 1' HUP INT QUIT TERM PIPE

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
# URL 的 scheme 不区分大小写（RFC 3986 第 3.1 节）：curl 把 HTTPS:// 当 https 连接、按 https 受 --proto 约束（实测），
# agent 解析 --hub 用的 Go url.Parse 也把 scheme 转成小写。这里区分大小写会让 --hub HTTPS://… 漏装 CA 证书包，让
# --base-url HTTPS://… 漏掉下载只走 https 的限制。
is_https() { case "$1" in [Hh][Tt][Tt][Pp][Ss]://*) return 0;; esac; return 1; }
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

# 下载的超时与重试。到 GitHub 的 TLS 握手在国内主机上常被重置，单次下载在那里经常直接失败；没有超时的下载遇到不回应或
# 中途停住的连接会一直挂着。各条规则的依据（curl 8.7.1 与 8.14.1 实测各类失败的退出码与 -w 输出）：
# - 连接阶段（DNS、TCP、TLS 握手）最多 DL_CONNECT_TIMEOUT 秒。--connect-timeout 覆盖 TLS 握手（实测：对端接受 TCP 却不回
#   ServerHello 时按上限以 28 退出）。握手被重置时 curl 当场以 35 或 56 失败，不等这个上限；它只截住完全不回应的对端，
#   30 秒给慢线路的建连留足余地。
# - 连上之后平均速度连续 DL_STALL_TIME 秒低于 DL_SPEED_LIMIT 字节/秒即中止（--speed-limit、--speed-time，退出码 28）。计时
#   从连上起算，发出请求后迟迟收不到响应同样计入（实测）。包有数 MB，低于 1 KiB/s 时一个包要下一个多小时，这样的线路该
#   走离线副本；60 秒的窗口给偶发的停顿留余地。不设总时长上限：慢而一直在走的下载应当下完。
# - 只重试一个字节都没收到的失败，最多 DL_TRIES 次、间隔 DL_RETRY_DELAY 秒：curl 退出码 16、18、35、52、55、56、92（HTTP/2
#   帧层错误、传输中断、TLS 握手失败、空响应、发送失败、接收失败、HTTP/2 流错误）且 %{size_download} 为 0，或 HTTP 502、503、
#   504。这是握手被重置、对端瞬时过载的形态，重来一次不比第一次更糟。
# - 不重试：连不上（7）、解析失败（6）、证书错（60）、超时（28）与其他 HTTP 错误，重来结果不变，只拖延失败；收到部分正文
#   后断开也不重试：线路通但慢而不稳，整包重下大概率再断，提示改用离线副本（--base-url 指向本地目录或近处的服务器，
#   接受哪些字节仍由内嵌清单决定）。
# wget 分支（没有 curl 时用，Alpine 默认如此）只有一个 -T：busybox wget 1.37 与 GNU Wget 1.21 实测都用它限制连接阶段（含
# TLS 握手）与两次读之间的空闲，没有低速中止：一直有字节在流、只是极慢的下载不会被中止（实测 2 字节/秒持续不超时）。取
# DL_STALL_TIME 与 curl 的停滞窗口对齐，代价是连接阶段的上限也是它。wget 的退出码分不出失败种类（busybox 一律 1；GNU 的 4
# 把连不上与握手被重置归在一起），所以按输出文件判：没有或为空即一个字节都没收到，重试；连不上、解析失败、证书错因此也
# 会多试几次才失败。GNU Wget 自带重试（默认 20 次），用 --tries=1 关掉，重试次数只由这里决定；busybox wget 不认
# --tries，按 --help 的输出分辨实现（与 adduser 同法）。
DL_CONNECT_TIMEOUT=30
DL_SPEED_LIMIT=1024
DL_STALL_TIME=60
DL_TRIES=5
DL_RETRY_DELAY=5
WGET_BUSYBOX=0
if [ "$FETCH" = wget ] && wget --help </dev/null 2>&1 | grep -qi busybox; then WGET_BUSYBOX=1; fi
# 下载一次。curl 在 stdout 写 "<HTTP 状态码> <收到的正文字节数>"（-w；失败时照样写，连不上时是 "000 0"，实测）。
fetch_once() {
  if [ "$FETCH" = wget ]; then
    set -- -q -T "$DL_STALL_TIME" -O "$2" "$1"
    [ "$WGET_BUSYBOX" = 1 ] || set -- --tries=1 "$@"
    wget "$@" </dev/null
    return
  fi
  url=$1; dest=$2
  set --
  # 下载地址为 https 时，请求和重定向都只走 https。http（本地验收）不加：--proto '=https' 会拒绝它。wget 没有对应开关。
  if is_https "$url"; then set -- --proto '=https' --proto-redir '=https'; fi
  curl "$@" --connect-timeout "$DL_CONNECT_TIMEOUT" --speed-limit "$DL_SPEED_LIMIT" --speed-time "$DL_STALL_TIME" \
    -fsSL -w '%{http_code} %{size_download}' -o "$dest" "$url" </dev/null
}
dl() {
  attempt=1
  while :; do
    rm -f "$2"
    dl_rc=0
    dl_out=$(fetch_once "$1" "$2") || dl_rc=$?
    [ "$dl_rc" != 0 ] || return 0
    retry=0
    if [ "$FETCH" = curl ]; then
      dl_status=${dl_out%% *}; dl_size=${dl_out##* }
      what="curl exit $dl_rc"
      case "$dl_status" in 502|503|504) retry=1; what="HTTP $dl_status";; esac
      case "$dl_rc" in 16|18|35|52|55|56|92) [ "$dl_size" != 0 ] || retry=1;; esac
      partial=0; [ "$dl_size" = 0 ] || [ -z "$dl_size" ] || partial=1
    else
      what="wget exit $dl_rc"
      partial=0; [ ! -s "$2" ] || partial=1
      [ "$partial" = 1 ] || retry=1
    fi
    if [ "$retry" = 0 ]; then
      if [ "$partial" = 1 ]; then
        echo "download of $1 broke off after part of the file arrived ($what); the link is too slow or unstable to download directly: fetch the release files another way into a local directory or a nearby web server, then rerun with --base-url pointing there" >&2
      fi
      return 1
    fi
    if [ "$attempt" -ge "$DL_TRIES" ]; then
      echo "download of $1 failed $DL_TRIES times ($what); giving up" >&2
      return 1
    fi
    attempt=$((attempt + 1))
    echo "download of $1 failed ($what); retrying in ${DL_RETRY_DELAY}s (attempt $attempt of $DL_TRIES)" >&2
    sleep "$DL_RETRY_DELAY"
  done
}

work=$(mktemp -d)
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
