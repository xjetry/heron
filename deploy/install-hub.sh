#!/bin/sh
# hub 安装与升级共用此入口；服务参数保存在 systemd 单元，不另建一份配置。
# 管道安装时 stdin 是脚本源码，外部命令不能读取它；确认只从 /dev/tty 读取。
set -eu

# 脚本读写的系统路径都挂在 HERON_INSTALL_ROOT 下。生产运行时它为空，即真实根目录；deploy/installhub_test.go
# 把它指向临时目录，连同 PATH 上的 systemctl、chown、curl 等替身一起运行，不触碰真实系统路径。它只改变本脚本
# 读写的位置：写进单元的可执行路径与库路径、systemctl 报告的 drop-in 路径都是目标系统里的真实路径，读文件时
# 才拼上这个前缀。
ROOT=${HERON_INSTALL_ROOT-}
BIN=$ROOT/usr/local/bin/heron-hub
DATA=$ROOT/var/lib/heron
UNIT=$ROOT/etc/systemd/system/heron-hub.service
WANTS=$ROOT/etc/systemd/system/multi-user.target.wants/heron-hub.service
UPDATER_BIN=$ROOT/usr/local/bin/heron-updater-hub
UPDATER_UNIT=$ROOT/etc/systemd/system/heron-updater-hub.service
UPDATER_STATE=$ROOT/var/lib/heron-update-hub
UPDATER_RESTORE=0
PROC=$ROOT/proc
SVC_USER=heron-hub
REPO=https://github.com/xjetry/heron
# 本脚本所属的版本与该版全部 tar 包的 SHA-256（每行 "<64 位十六进制>  <文件名>"），由发布目标（release-full / release-hub-only）经
# deploy/releasestamp 写进下面两行标记之间；源码里为空，这时拒绝安装（卸载不下载，照常可用）。下载的包只按这份
# 清单校验：--base-url 能换掉下载目录里的每个文件，同目录的 SHA256SUMS 也在其中，拿它作依据挡不住篡改。
RELEASE_VERSION=""
RELEASE_SHA256=""
# >>> release stamp >>>
# <<< release stamp <<<
BASE_URL=""; UNINSTALL=0; PURGE=0; YES=0; OVERRIDES=""
# 安装器写进单元的 serve 参数，与 cmd/hub/serve.go 定义的 flag 一一对应，由 deploy/installhub_test.go 核对。
# 命令行覆盖与已装单元的解析共用这一张表；--db 固定为 /var/lib/heron/heron.db，不接受覆盖。
SERVE_FLAGS='listen timezone offline-after trusted-proxies public-dir admin-origin geo-mmdb retention-1m retention-5m retention-1h retention-alert-events'
nl='
'
cr=$(printf '\r')
usage() {
  echo 'usage: install-hub.sh [--base-url URL] [--listen ADDR] [--timezone ZONE] [--offline-after DURATION] [--trusted-proxies CIDRS] [--public-dir DIR] [--admin-origin ORIGIN] [--geo-mmdb FILE] [--retention-1m DURATION] [--retention-5m DURATION] [--retention-1h DURATION] [--retention-alert-events DURATION] [--yes]' >&2
  echo '       --admin-origin is only for migrating existing Passkeys; new registrations bind the current HTTPS hostname' >&2
  echo '       install-hub.sh --uninstall [--purge] [--yes]' >&2
  exit 2
}
fail() { echo "$*" >&2; exit 1; }
# 写回的单元一行一条指令，参数里带换行会把 ExecStart 断成两行；回车一并拒绝，不依赖 systemd 对行内回车的处理。
# 命令行覆盖值按行暂存到合并那一步，带换行的值还会被拆成两个参数，所以解析命令行时就要拒绝。
reject_line_breaks() {
  case "$1" in *"$nl"*|*"$cr"*) fail 'arguments must not contain line breaks';; esac
}
# 每个参数单独引用；systemd 会展开 $ 与 %，两者必须双写才能保持字面值。
quote_arg() {
  reject_line_breaks "$1"
  quoted=$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g; s/\$/$$/g; s/%/%%/g')
  quoted="\"$quoted\""
}
while [ "$#" -gt 0 ]; do
  case "$1" in
    --base-url)
      [ "$#" -ge 2 ] || usage
      BASE_URL=$2
      shift 2;;
    # 脚本只装自己所属的版本：版本由取哪个 URL 的脚本决定，没有第二个来源可以和内嵌清单不一致。
    --version|--version=*)
      echo "install-hub.sh has no --version: it installs only the release it belongs to; for another version run $REPO/releases/download/<tag>/install-hub.sh" >&2
      exit 2;;
    --theme-origin|--theme-origin=*)
      echo 'themes now use the panel hostname; remove --theme-origin and manage themes in /admin/' >&2
      exit 2;;
    --uninstall) UNINSTALL=1; shift;;
    --purge) PURGE=1; shift;;
    --yes) YES=1; shift;;
    --*)
      case "${1#--}" in ''|*[!a-z0-9-]*) usage;; esac
      case " $SERVE_FLAGS " in *" ${1#--} "*) ;; *) usage;; esac
      [ "$#" -ge 2 ] || usage
      reject_line_breaks "$2"
      # 每行一条字面值的 --flag=value，稍后与已装参数按 flag 名合并。
      OVERRIDES="$OVERRIDES$1=$2$nl"
      shift 2;;
    *) usage;;
  esac
done
[ "$PURGE" = 0 ] || [ "$UNINSTALL" = 1 ] || usage
[ "$(id -u)" = 0 ] || fail 'install-hub.sh must run as root'
[ -d "$ROOT/run/systemd/system" ] || fail 'unsupported init: expected systemd (/run/systemd/system)'

# /run 由 root 管理且不可被服务用户写入；锁文件不删除，避免并发安装器锁住不同 inode。
# 此锁只串行人工安装器，在线事务仍由 prepare_updater 的维护握手排除。
command -v flock >/dev/null 2>&1 || fail 'flock is required for systemd installation'
INSTALL_LOCK=$ROOT/run/heron-install-hub.lock
if [ -L "$INSTALL_LOCK" ] || { [ -e "$INSTALL_LOCK" ] && [ ! -f "$INSTALL_LOCK" ]; }; then
  fail 'installer lock is not a regular file'
fi
(umask 077; touch "$INSTALL_LOCK")
exec 9>>"$INSTALL_LOCK"
flock -n 9 </dev/null || fail 'another heron-hub installer is running'

confirm_removal() {
  [ "$YES" = 0 ] || return 0
  # 能打开 /dev/tty 才算有终端。它的权限位对所有人可读写，[ -r ] 只看权限，没有控制终端时照样为真，
  # 打开却失败；那时往下走，提示写不出去，报错说的是 /dev/tty 而不是缺 --yes。
  if [ ! -t 1 ] || ! ( : </dev/tty ) 2>/dev/null; then
    fail 'uninstall requires --yes without a terminal'
  fi
  printf 'Stop and remove heron-hub (purge data: %s)? [y/N] ' "$PURGE" > /dev/tty
  IFS= read -r answer < /dev/tty || fail 'confirmation canceled'
  case "$answer" in y|Y|yes|YES) ;; *) fail 'confirmation canceled';; esac
}
local_user_exists() { grep -q "^$SVC_USER:" "$ROOT/etc/passwd"; }
local_group_exists() { grep -q "^$SVC_USER:" "$ROOT/etc/group"; }
# 系统号的占用集合：/etc/passwd 的 uid、/etc/group 的 gid，以及每个运行中进程 Uid:/Gid: 行的全部四个
# 字段与 Groups: 补充组。只看账户文件会把“没有账户记录的号”当成空闲：官方容器镜像的内置用户
# （postgres、valkey 都是 uid 999）在宿主 /proc 里以该 uid 运行，宿主账户文件里没有它们，
# useradd 自动取号（SYS_UID_MAX 往下第一个空闲）会取到 999，装出来的账户与这些进程同 uid。
# /proc 不可读时不退回只看账户文件：那正是取不出安全号的状态。
used_system_ids() {
  [ -r "$PROC/self/status" ] || fail 'cannot inspect processes: /proc is not mounted'
  {
    awk -F: '{ print $3 }' "$ROOT/etc/passwd" 2>/dev/null
    awk -F: '{ print $3 }' "$ROOT/etc/group" 2>/dev/null
    awk '$1 == "Uid:" || $1 == "Gid:" { print $2; print $3; print $4; print $5 }
      $1 == "Groups:" { for (i = 2; i <= NF; i++) print $i }' "$PROC"/[0-9]*/status 2>/dev/null
  } | sort -un
}
# 999 往下第一个没被占用的号；用户与组同号。下限 100 是发行版静态保留的系统账户号段，不取；
# 号段全满时失败退出，不放宽。
free_system_id() {
  used_system_ids | awk 'BEGIN { for (i = 999; i >= 100; i--) free[i] = 1 }
    { delete free[$1] }
    END { for (i = 999; i >= 100; i--) if (i in free) { print i; exit 0 } exit 1 }'
}
# 列出与服务用户共用 uid 的外来进程（写入 shared，每项 " pid:exe"）。Uid: 行的四个字段任一命中都算
# 共用；本服务自己的进程（exe 为 $BIN）与已退出待收尸的僵尸不计。判据与 scan_svc_pids 同一处。
foreign_uid_procs() {
  shared=""
  [ -r "$PROC/self/status" ] || fail 'cannot inspect processes: /proc is not mounted'
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
    elif [ -e "$s" ]; then fail "cannot read $s"; fi
  done
}
# 已有账户的 uid 被外来进程共用时，在任何下载与系统变更之前拒绝：库文件属这个 uid、0600，共用的进程
# （撞号的容器进程）读得到全部节点凭据，也能对本服务进程发信号、ptrace。安装器不能安全自动修这个
# 状态（usermod 改不了运行中进程的 euid），换号由管理员做完再重跑。
reject_shared_uid() {
  foreign_uid_procs "$1" || return 1
  [ -z "$shared" ] || {
    fail "uid $1 ($SVC_USER) is also used by other running processes:$shared
move $SVC_USER to an id no process uses (usermod/userdel may refuse while these processes run; editing the id in /etc/passwd and /etc/group works too) and rerun the installer"
  }
}
create_account() {
  nologin=/sbin/nologin
  [ ! -x "$ROOT/usr/sbin/nologin" ] || nologin=/usr/sbin/nologin
  if id "$SVC_USER" >/dev/null 2>&1; then
    actual_group=$(id -gn "$SVC_USER") || fail "primary group missing for $SVC_USER"
    [ "$actual_group" = "$SVC_USER" ] || fail "user $SVC_USER has primary group $actual_group; expected $SVC_USER"
    [ "$(id -u "$SVC_USER")" != 0 ] || fail 'heron-hub must not have uid 0'
    reject_shared_uid "$(id -u "$SVC_USER")" || exit 1
    return
  fi
  new_id=$(free_system_id) || fail "no free system id in 100-999 for $SVC_USER (account files and running processes)"
  if ! local_group_exists; then
    if command -v groupadd >/dev/null 2>&1; then groupadd --system -g "$new_id" "$SVC_USER" </dev/null
    elif command -v addgroup >/dev/null 2>&1; then
      if addgroup --help </dev/null 2>&1 | grep -qi busybox; then addgroup -S -g "$new_id" "$SVC_USER" </dev/null
      else addgroup --system --gid "$new_id" "$SVC_USER" </dev/null; fi
    else fail 'no groupadd or addgroup available to create the system group'; fi
  fi
  if command -v useradd >/dev/null 2>&1; then
    useradd --system -u "$new_id" -M -d /nonexistent -s "$nologin" -g "$SVC_USER" "$SVC_USER" </dev/null
  elif command -v adduser >/dev/null 2>&1; then
    if adduser --help </dev/null 2>&1 | grep -qi busybox; then
      adduser -S -D -H -u "$new_id" -s "$nologin" -G "$SVC_USER" "$SVC_USER" </dev/null
    else adduser --system --no-create-home --shell "$nologin" --ingroup "$SVC_USER" --uid "$new_id" "$SVC_USER" </dev/null; fi
  else fail 'no useradd or adduser available to create the system user'; fi
  id "$SVC_USER" >/dev/null 2>&1 || fail "failed to create system user $SVC_USER"
  [ "$(id -gn "$SVC_USER")" = "$SVC_USER" ] || fail "primary group must be $SVC_USER"
}
delete_account() {
  if local_user_exists; then
    if command -v userdel >/dev/null 2>&1; then userdel "$SVC_USER" </dev/null
    elif command -v deluser >/dev/null 2>&1; then deluser "$SVC_USER" </dev/null; fi
  fi
  if local_group_exists; then
    if command -v groupdel >/dev/null 2>&1; then groupdel "$SVC_USER" </dev/null
    elif command -v delgroup >/dev/null 2>&1; then delgroup "$SVC_USER" </dev/null; fi
  fi
  if id "$SVC_USER" >/dev/null 2>&1 || local_group_exists; then fail "failed to delete user or group $SVC_USER"; fi
}
# 列出本服务的进程 pid（空格分隔，写入 svc_pids）：有效 uid 为 $1 且可执行文件是 $BIN 的进程。
# 判据“有效 uid 为服务用户且可执行文件为本服务二进制”与“是本服务的进程”等价：
# - 专用账户由 create_account 建立，取号时避开运行中进程占用的号，已有账户的撞号又被 reject_shared_uid
#   拒绝过；即便如此仍可能有进程共用这个 uid（撞号后新起的容器进程），只看 uid 会把它们算成本服务。
# - 单元的 User= 与 ExecStart 保证服务进程以该用户运行、可执行文件为 $BIN；两者改变时须同步修改判据。
# exe 链接的目标在二进制被 mv 覆盖后带 " (deleted)" 后缀，一并匹配。读不出 exe 的活进程按本服务的进程
# 计入（宁可误报不漏判）；僵尸进程没有 exe，已退出待收尸，不计。
scan_svc_pids() {
  svc_pids=""
  [ -r "$PROC/self/status" ] || fail 'cannot inspect processes: /proc is not mounted'
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
    elif [ -e "$s" ]; then fail "cannot read $s"; fi
  done
}
stop_service() {
  if [ -f "$UNIT" ]; then systemctl stop heron-hub </dev/null || fail 'failed to stop heron-hub'; fi
  id "$SVC_USER" >/dev/null 2>&1 || return 0
  svc_uid=$(id -u "$SVC_USER")
  polls=0
  while :; do
    scan_svc_pids "$svc_uid"
    [ -n "$svc_pids" ] || return 0
    [ "$polls" -lt 10 ] || fail "heron-hub is still running: uid $svc_uid processes:$svc_pids"
    sleep 1
    polls=$((polls + 1))
  done
}
confirm_service_started() {
  # 失败只 return 1，不 fail 退出：rollback_hub 也调用它，在 EXIT trap 里 exit 会打断回滚余下的步骤。
  svc_uid=$(id -u "$SVC_USER") || { echo 'no service user heron-hub; see journalctl -u heron-hub' >&2; return 1; }
  polls=0; pid=""
  while :; do
    scan_svc_pids "$svc_uid"
    if [ -n "$svc_pids" ]; then pid=${svc_pids# }; pid=${pid%% *}; break; fi
    if [ "$polls" -ge 10 ]; then echo 'heron-hub did not start; see journalctl -u heron-hub' >&2; return 1; fi
    sleep 1
    polls=$((polls + 1))
  done
  sleep 3
  scan_svc_pids "$svc_uid"
  case " $svc_pids " in *" $pid "*) return 0;; esac
  echo "heron-hub did not stay running (pid $pid); see journalctl -u heron-hub" >&2
  return 1
}
# 停服务之后、启动确认成功之前的任何失败退出都由 on_exit 调到这里：换过二进制就把 .bak 换回来，库备份完整时把三个
# 库文件也换回来，再按 systemd 重启旧版本。库文件必须一起回滚：候选 hub 在启动确认之前就会以 MigrateSchema 打开库
# （cmd/hub/serve.go 的 store.Open）。比本二进制新的库由 Store 拒绝打开，只换二进制会把 hub 停在"旧程序
# 打不开新库"。库备份没做完（DB_BACKED_UP=0）时候选 hub 还没启动过，库原样未动；这时不能拿可能只复制了一半的
# .bak 去覆盖它，也不能按"备份时不存在"删掉原库正在用的 -wal。停服务时没有旧二进制（首次安装）就没有可回滚的
# 版本，保持 hub 已停、脚本非零退出。安全检查失败（RESTART_ON_ROLLBACK=0）只换回文件，不启动：那些失败要人看过
# 再重跑。它在 EXIT trap 里执行，set -e 仍然生效：每一步都放在条件里，失败只报告；换不回去的 .bak 留给人手工恢复
# （KEEP_BAK=1，on_exit 不删）。读 stdin 的命令都显式 </dev/null：脚本经 curl | sh 从 stdin 来。
rollback_hub() {
  [ "$HAD_PREVIOUS" = 1 ] || return 0
  echo "installation failed after heron-hub was stopped; rolling back to the previous version" >&2
  systemctl stop heron-hub </dev/null || true
  if [ "$BACKED_UP" = 1 ]; then
    if ! mv -f "$BIN_BAK" "$BIN"; then
      KEEP_BAK=1
      echo "failed to restore the previous binary; it is kept at $BIN_BAK" >&2
      return 0
    fi
    echo "restored the previous binary" >&2
  fi
  if [ "$DB_BACKED_UP" = 1 ]; then
    for file in "$DATA/heron.db" "$DATA/heron.db-wal" "$DATA/heron.db-shm"; do
      if [ -e "$file.bak" ]; then
        if ! { mv -f "$file.bak" "$file" && chown "$SVC_USER:$SVC_USER" "$file" && chmod 0600 "$file"; }; then
          KEEP_BAK=1
          echo "failed to restore $file; the copy taken before the upgrade is kept at $file.bak" >&2
          return 0
        fi
      else
        # 备份时不存在、新 hub 启动后新出现的 -wal/-shm 属于新版本，回滚时删掉，别让新 schema 的页留在旧库旁。
        rm -f "$file" || true
      fi
    done
    echo "restored the previous database" >&2
  fi
  [ "$RESTART_ON_ROLLBACK" = 1 ] || return 0
  # 停服务后收紧成 0750 的数据目录放回 0770，与正常路径收尾相同：0750 下旧 hub 建不了 WAL/SHM。走到这里说明
  # 安全检查都已通过（没通过的走 fail_stopped，上一行已返回）。
  if [ "$DATA_LOCKED" = 1 ] && ! chmod 0770 "$DATA"; then echo "failed to reopen $DATA to heron-hub" >&2; fi
  if systemctl start heron-hub </dev/null; then
    # start 返回 0 即已交给 systemd（Restart=always），与正常路径成功后的口径一致：不再补"hub 已停"。
    EXIT_HINT=""
    if confirm_service_started; then
      echo "the previous heron-hub is running again" >&2
    else
      echo "the previous heron-hub did not stay running; see journalctl -u heron-hub" >&2
    fi
  else
    echo "failed to start the previous heron-hub; see journalctl -u heron-hub" >&2
  fi
}
# 数据目录与三个库文件的形态核对，停服前的预检与停服后的复检共用。停服后，目录本身的复检在收紧目录之前，
# 库文件的复检在目录收成 0750 之后（锁内）。
# 目录在 root 属主的 /var/lib 下，服务用户换不掉这个目录项，所以对目录本身的核对不依赖锁；下文的 chown
# 不带 -h，目录是链接就会改到链接指向的目录。库文件要么不存在，要么是链接数为 1 的普通文件：符号链接与
# 硬链接都会让 root 的 chown、chmod 改到另一个名字所指的文件。硬链接 [ -L ] 为假、[ -f ] 为真，只有链接数
# 看得出；链接数用 find -links 取（BSD 与 GNU 的 find 都支持，替身测试也在 macOS 上跑），find 失败时输出
# 同样为空，所以先看它的退出码。
data_dir_ok() {
  if [ -L "$DATA" ] || { [ -e "$DATA" ] && [ ! -d "$DATA" ]; }; then
    echo "$DATA exists but is not a directory" >&2; return 1
  fi
}
db_files_ok() {
  for file in "$DATA/heron.db" "$DATA/heron.db-wal" "$DATA/heron.db-shm"; do
    [ -L "$file" ] || [ -e "$file" ] || continue
    extra=""
    if [ ! -L "$file" ] && [ -f "$file" ]; then
      extra=$(find "$file" -prune -links +1) || { echo "cannot read the link count of $file" >&2; return 1; }
    fi
    if [ -L "$file" ] || [ ! -f "$file" ] || [ -n "$extra" ]; then
      echo "$file exists but is not a regular file with a single link" >&2; return 1
    fi
  done
}
# 单元是否 enabled 只看 $WANTS 这条链接，卸载与写好主单元之后的现状说明共用这一个判定。安装器每次写入的主单元
# 取自发布包里的 heron-hub.service，只替换 ExecStart，[Install] 只有 WantedBy=multi-user.target（与 $WANTS 里的
# target 一致，由 deploy/installhub_test.go 静态核对），所以 systemctl enable 建出的就是这条链接，disable 删掉它。判定不向 systemd 查询：写好主单元之后回答它的路径里，
# 有一条正是 systemctl 刚出过错，而 systemctl is-enabled --quiet 查询出错与 disabled 都是非零退出，照它回答会把
# 仍 enabled 的单元说成没 enable。用 -L 不用 -e：主单元被删、链接悬空时也算，卸载要清掉它。管理员另用
# add-wants 等挂到别的 target 下的链接不在这个判定里。
unit_enabled() { [ -L "$WANTS" ]; }
# 维护握手由更新器在事务锁内完成；成功之后拒绝新任务，才允许安装器停掉它并替换文件。
prepare_updater() {
  if [ -e "$UPDATER_UNIT" ] || [ -e "$UPDATER_BIN" ]; then
    updater_active=$(systemctl show heron-updater-hub -p ActiveState --value </dev/null) || return 1
    case "$updater_active" in
      active|activating|reloading)
        "$UPDATER_BIN" --role hub --maintenance </dev/null || return 1
        UPDATER_RESTORE=1
        systemctl stop heron-updater-hub </dev/null || return 1;;
      inactive|failed) ;;
      *) fail 'cannot determine updater service state; refusing installation';;
    esac
  fi
  if [ "$UPDATER_RESTORE" = 0 ] && { [ -e "$UPDATER_STATE/state.json" ] || [ -e "$UPDATER_STATE/pending" ]; }; then
    fail 'updater recovery state exists; start heron-updater-hub before retrying'
  fi
}
restore_updater() {
  if [ "$UPDATER_RESTORE" = 1 ]; then systemctl restart heron-updater-hub </dev/null || echo 'failed to restore heron-updater-hub; start it manually' >&2; fi
}
trap restore_updater EXIT
trap 'exit 1' INT TERM HUP
if [ "$UNINSTALL" = 1 ]; then
  confirm_removal
  prepare_updater
  stop_service
  if [ -f "$UNIT" ]; then systemctl disable heron-hub </dev/null; fi
  if unit_enabled; then rm -f "$WANTS"; fi
  rm -f "$UNIT" "$BIN"
  if [ -f "$UPDATER_UNIT" ]; then systemctl disable heron-updater-hub </dev/null; fi
  rm -f "$UPDATER_UNIT" "$UPDATER_BIN" "$ROOT/etc/systemd/system/multi-user.target.wants/heron-updater-hub.service"
  # 维护握手已排除在途事务；卸载更新器同时移除其历史和回滚备份，业务数据仍由 --purge 决定。
  rm -rf "$UPDATER_STATE" "$ROOT/run/heron-update-hub"
  UPDATER_RESTORE=0
  # 只清除本服务的本地定制；不用 DropInPaths 展开共享配置，也不跟随目录符号链接。
  if [ "$PURGE" = 1 ]; then
    rm -rf "$UNIT.d" "$ROOT/run/systemd/system/heron-hub.service.d"
    rm -rf "$UPDATER_UNIT.d" "$ROOT/run/systemd/system/heron-updater-hub.service.d"
  fi
  systemctl daemon-reload </dev/null
  if [ "$PURGE" = 1 ]; then rm -rf "$DATA"; delete_account; fi
  echo 'heron-hub uninstalled'
  exit 0
fi
case "$(uname -m)" in
  x86_64) ARCH=amd64;; aarch64) ARCH=arm64;;
  *) fail "unsupported architecture: $(uname -m)";;
esac
# 内嵌清单在任何网络操作与账户改动之前查：源码脚本、或清单里没有本机的包，都不该先建用户、装 CA 再失败。
PKG=heron-hub_linux_$ARCH.tar.gz
[ -n "$RELEASE_VERSION" ] ||
  fail "this install-hub.sh has no embedded release checksums (it is the source copy); use the install-hub.sh attached to a release: $REPO/releases"
WANT_SHA256=$(printf '%s\n' "$RELEASE_SHA256" | awk -v p="$PKG" '$2 == p { print $1; n++ } END { exit n != 1 }') ||
  fail "release $RELEASE_VERSION has no embedded checksum for $PKG"
UPDATER_PKG=heron-updater_linux_$ARCH.tar.gz
UPDATER_SHA256=$(printf '%s\n' "$RELEASE_SHA256" | awk -v p="$UPDATER_PKG" '$2 == p { print $1; n++ } END { exit n != 1 }') ||
  fail "release $RELEASE_VERSION has no embedded checksum for $UPDATER_PKG"
create_account
if command -v curl >/dev/null 2>&1; then FETCH=curl
elif command -v wget >/dev/null 2>&1; then FETCH=wget
else fail "neither curl nor wget is available to download $PKG"; fi
command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum is required to verify downloads'
# --base-url 只改变从哪里取字节，接受哪些字节仍由内嵌清单决定。
[ -n "$BASE_URL" ] || BASE_URL=$REPO/releases/download/$RELEASE_VERSION
is_https() { case "$1" in https://*) return 0;; esac; return 1; }
ca_bundle_present() {
  for f in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt /etc/ssl/cert.pem /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem; do
    [ ! -f "$ROOT$f" ] || return 0
  done
  return 1
}
# 不论下载地址是不是 https，hub 进程自己都要做 TLS 出站：serve 以空基址构造告警队列（cmd/hub/serve.go），
# Telegram 于是固定走 https://api.telegram.org（internal/hub/alert/notify.go），webhook 也可以是 https。
# Go 的 crypto/x509 在 Linux 上只从系统的证书文件与目录（或 SSL_CERT_FILE、SSL_CERT_DIR 指定的位置）加载根证书，
# 本仓库没有引入内置根证书。缺证书包时安装照样成功、面板正常，告警投递却一直因证书校验失败被记成 transport 失败。
# 探测按文件做，不按发行版判断。
if ! ca_bundle_present; then
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update </dev/null && DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates </dev/null
  elif command -v apk >/dev/null 2>&1; then apk add --no-cache ca-certificates </dev/null
  elif command -v dnf >/dev/null 2>&1; then dnf install -y ca-certificates </dev/null
  elif command -v yum >/dev/null 2>&1; then yum install -y ca-certificates </dev/null
  else fail 'no CA bundle found and no supported package manager to install ca-certificates'; fi
  ca_bundle_present || fail 'ca-certificates installation did not produce a CA bundle'
fi

work=$(mktemp -d)
BIN_TMP=$BIN.tmp.$$
BIN_BAK=$BIN.bak
UPDATER_TMP=$UPDATER_BIN.tmp.$$
# 回滚状态（rollback_hub 的读者）：ROLLBACK 在停服务之后置 1、启动确认成功后清 0；HAD_PREVIOUS 记停服务时有没有旧
# 二进制；BACKED_UP 记旧二进制是否已改名成 .bak；DB_BACKED_UP 记三个库文件是否都已完整复制；DATA_LOCKED 记数据目录
# 是否正处于停服务后的 0750；RESTART_ON_ROLLBACK 在安全检查失败时清 0；KEEP_BAK 记回滚换不回去、要留给人手工恢复的 .bak。
ROLLBACK=0
HAD_PREVIOUS=0
BACKED_UP=0
DB_BACKED_UP=0
DATA_LOCKED=0
RESTART_ON_ROLLBACK=1
KEEP_BAK=0
# 停服务之后的失败退出先由 rollback_hub 回滚。各步的报错只说自己的原因，失败退出时在报错之后补一句现状与该做什么，
# 免得人以为旧服务还在跑。EXIT_HINT 随步骤更新，空串表示不必补（旧版本重新交给 systemd 或正常路径成功后清空）。
# 库的 .bak 在这里清：回滚用 mv 把它们换回去，正常路径在启动确认之后才删，没复制完的残片也在这里删。二进制的 .bak
# 不在这里删：回滚换回、或成功后删；回滚换不回去时它是旧版本仅存的一份（KEEP_BAK 同时保住库的 .bak）。
EXIT_HINT=""
on_exit() {
  rc=$?
  if [ "$rc" != 0 ] && [ "$ROLLBACK" = 1 ]; then rollback_hub; fi
  rm -rf "$work"; rm -f "$BIN_TMP" "$UPDATER_TMP"
  [ "$KEEP_BAK" = 1 ] || rm -f "$DATA/heron.db.bak" "$DATA/heron.db-wal.bak" "$DATA/heron.db-shm.bak"
  restore_updater
  if [ "$rc" != 0 ] && [ -n "$EXIT_HINT" ]; then echo "$EXIT_HINT" >&2; fi
}
# 安全检查在停服务之后失败：回滚只换回文件、不启动，留给人查看后重跑（RESTART_ON_ROLLBACK 见 rollback_hub）。
fail_stopped() { RESTART_ON_ROLLBACK=0; fail "$@"; }
trap on_exit EXIT
trap 'exit 1' INT TERM HUP
dl() {
  if [ "$FETCH" = curl ]; then
    if is_https "$1"; then curl --proto '=https' --proto-redir '=https' -fsSL -o "$2" "$1" </dev/null
    else curl -fsSL -o "$2" "$1" </dev/null; fi
  else wget -q -O "$2" "$1" </dev/null; fi
}
dl "$BASE_URL/$PKG" "$work/$PKG"
# sha256sum 的输出以摘要开头、空白之后是文件名（GNU 与 busybox 相同）；它失败时摘要为空，同样按不符拒绝。
GOT_SHA256=$(sha256sum "$work/$PKG" </dev/null) || GOT_SHA256=""
GOT_SHA256=${GOT_SHA256%% *}
[ "$GOT_SHA256" = "$WANT_SHA256" ] ||
  fail "checksum mismatch for $PKG from $BASE_URL: got ${GOT_SHA256:-nothing}, release $RELEASE_VERSION embeds $WANT_SHA256"
tar -xzf "$work/$PKG" -C "$work"
for f in heron-hub heron-hub.service; do
  if ! { [ -f "$work/$f" ] && [ ! -L "$work/$f" ]; }; then fail "package is missing regular file $f"; fi
done
install -m 0755 "$work/heron-hub" "$BIN_TMP"
dl "$BASE_URL/$UPDATER_PKG" "$work/$UPDATER_PKG"
got=$(sha256sum "$work/$UPDATER_PKG" </dev/null) || got=""
[ "${got%% *}" = "$UPDATER_SHA256" ] || fail "checksum mismatch for $UPDATER_PKG"
mkdir "$work/updater"
tar -xzf "$work/$UPDATER_PKG" -C "$work/updater"
for f in heron-updater heron-updater-agent.service heron-updater-hub.service; do
  if ! { [ -f "$work/updater/$f" ] && [ ! -L "$work/updater/$f" ]; }; then
    fail "package is missing regular file $f"
  fi
done
install -m 0755 "$work/updater/heron-updater" "$UPDATER_TMP"

# 单元里 [Service] 段的 ExecStart 值，每个一行。主单元与 drop-in 用这同一个谓词，逐项照 systemd 的解析
# （Debian 12 上 systemd 252 以 systemctl show -p ExecStart 与 systemd-analyze verify 实测）：
# - 行尾的一个回车随换行算作行尾（CRLF）。行内的回车 systemd 也当作换行，这里不模仿，整个文件判为无法解析。
# - 注释行先跳过，再判断续行：以反斜杠结尾的注释行不吞下一行。
# - 行尾是未转义的反斜杠（行尾连续的反斜杠为奇数个）才是续行，那个反斜杠换成空格再接上下一行。反斜杠后面
#   还有空白不算续行，systemd 把它当成一个字面的 \ 参数、下一行因缺 = 被忽略，这里随后按未完成的转义拒绝；
#   行尾两个反斜杠也不算，那是一个转义过的 \。
# - 首尾空白去掉，键名与等号之间允许空白（`ExecStart = …` 照样生效）。
# 口径不一时，drop-in 里 systemd 会采用的 ExecStart 可能漏过检查，主单元的参数也会被读成另一组。以续行结尾的
# 文件视为无法解析。
exec_starts() {
  awk -v cr="$cr" '
    {
      line = $0
      if (substr(line, length(line), 1) == cr) line = substr(line, 1, length(line) - 1)
      if (index(line, cr)) exit 1
      if (line ~ /^[[:space:]]*[#;]/) next
      line = pending line; pending = ""
      if (match(line, /\\+$/) && RLENGTH % 2 == 1) { pending = substr(line, 1, length(line) - 1) " "; next }
      sub(/^[[:space:]]+/, "", line); sub(/[[:space:]]+$/, "", line)
      if (line ~ /^\[/) service = (line == "[Service]")
      else if (service && line ~ /^ExecStart[[:space:]]*=/) { sub(/^ExecStart[[:space:]]*=[[:space:]]*/, "", line); print line }
    }
    END { if (pending != "") exit 1 }
  ' "$1"
}

# 设了 ExecStart 的 drop-in 会盖掉写进主单元的参数，显式覆盖也随之失效；不涉及命令的 drop-in（如全局加固项）
# 不受影响。drop-in 列表取 systemd 自己报告的 DropInPaths。它反映的是 systemd 已加载的单元，与磁盘可能不一致
# （Debian 12 上 systemd 252 实测）：
# - 单元文件还不存在时它为空，即使 heron-hub.service.d/ 里已有 drop-in。所以首装要等主单元写好之后才查得到：
#   enable 与 start 之前再查一遍，这一遍也兜住升级时停服前那一遍之后才落盘的 drop-in。
# - 运行中的 heron-hub 看不到之后才落盘的 drop-in，而新单元写好之后总要经过 reload 才能启动，那时它就生效了
#   （实测：不先 reload 就查，旧服务被停，新进程带着这个 drop-in 起来）。所以每次先 daemon-reload 再列：
#   它只重读单元文件，不停也不重启运行中的服务（实测 MainPID 不变）。
# 查分两步，失败的含义不同：list_dropins 失败的是 systemctl 本身，drop-in 还没被查过，报错带上 systemctl 的原文；
# dropins_ok 失败才是某个 drop-in 的问题。
list_dropins() {
  if ! systemctl daemon-reload </dev/null 2>"$work/systemctl.err"; then
    echo "systemctl daemon-reload failed: $(cat "$work/systemctl.err")" >&2; return 1
  fi
  cat "$work/systemctl.err" >&2
  if ! dropins=$(systemctl show heron-hub -p DropInPaths --value </dev/null 2>"$work/systemctl.err"); then
    echo "systemctl show heron-hub failed: $(cat "$work/systemctl.err")" >&2; return 1
  fi
  cat "$work/systemctl.err" >&2
}
# list_dropins 列出的路径拼上 HERON_INSTALL_ROOT 再读；列出来却读不到的无法判定，一并拒绝。
# 在子 shell 里跑，set -f 不外泄。
dropins_ok() (
  set -f
  for dropin in $dropins; do
    [ -f "$ROOT$dropin" ] || fail "cannot read heron-hub drop-in $dropin"
    exec_starts "$ROOT$dropin" > "$work/dropin-exec" || fail "cannot parse heron-hub drop-in $dropin"
    [ ! -s "$work/dropin-exec" ] || fail "drop-in $dropin sets ExecStart; merge it into heron-hub.service"
  done
)

# 不执行单元内容，也不按空格粗拆：带引号的目录必须作为一个参数保留。
# 未支持的 systemd 动态展开与转义在停服前拒绝，而不是静默变成另一组参数。
source_unit=$work/heron-hub.service
if [ -f "$UNIT" ]; then
  if ! list_dropins || ! dropins_ok; then fail 'old service was not stopped'; fi
  source_unit=$UNIT
fi
exec_starts "$source_unit" > "$work/command" || fail 'cannot read ExecStart from installed unit'
awk 'NR == 1 && $0 != "" { ok = 1 } END { exit !(ok && NR == 1) }' "$work/command" ||
  fail 'installed unit must set ExecStart exactly once'
awk '
  function bad() { failed = 1; exit 1 }
  {
    token = ""; quote = ""; active = 0
    for (i = 1; i <= length($0); i++) {
      c = substr($0, i, 1)
      if (c == "\\") {
        c = substr($0, ++i, 1)
        if (c == "s") c = " "
        else if (c != "\\" && c != "\"" && c != "\047") bad()
        token = token c; active = 1
      } else if (quote != "") {
        if (c == quote) quote = ""; else token = token c
      } else if (c == "\"" || c == "\047") { quote = c; active = 1 }
      else if (c ~ /[ \t]/) {
        if (active) { print token; token = ""; active = 0 }
      } else { token = token c; active = 1 }
    }
    if (quote != "") bad()
    if (active) print token
  }
  END { if (failed) exit 1 }
' "$work/command" > "$work/raw-args" || fail 'unsupported quoting or escape in ExecStart'
# 写回时 quote_arg 把每个 $、% 双写。这里把读到的 $$、%% 还原成字面字符，写回后仍是原来的 $$、%%。单写的
# $、% 是 systemd 的动态展开（环境变量、specifier），读进来再写回会被双写成字面值、改变参数的意思；
# 所以不论出现在哪个参数里都在停服前拒绝，而不只拒绝影响监听端口的那几个。
awk '{
  out = ""
  for (i = 1; i <= length($0); i++) {
    c = substr($0, i, 1)
    if (c == "$" || c == "%") { if (substr($0, ++i, 1) != c) exit 1 }
    out = out c
  }
  print out
}' "$work/raw-args" > "$work/args" || fail 'dynamic $ or % expansion in ExecStart is not supported'
printf '%s' "$OVERRIDES" > "$work/overrides"
# 已装参数与命令行覆盖按 flag 名合并成一张表，写回单元的就是这张表：
# - 顺序取 flag 首次出现的位置；已装参数里同名 flag 取最后一个值，与 serve 的解析（Go flag 包，后者覆盖前者）一致。
# - 命令行覆盖按名替换表里的值，表里没有才追加。
# - 写回统一为 --flag=value、每个 flag 一次：单元里只留生效的值，带同样参数重跑不会累加。
# 接受 Go flag 包的 --f v、-f v、--f=v、-f=v 四种写法。认得的 flag 是 --db 加上 SERVE_FLAGS：表外的 flag 在
# 停服前拒绝，而不是原样带过去。
awk -v flags="db $SERVE_FLAGS" '
  function die(msg) { print msg | "cat 1>&2"; failed = 1; exit 1 }
  function set(name, value) { if (!(name in val)) order[++count] = name; val[name] = value }
  BEGIN {
    split(flags, names, " ")
    for (i in names) known[names[i]] = 1
  }
  FILENAME == ARGV[1] {
    position++
    if (position <= 2) {
      if ($0 != (position == 1 ? "/usr/local/bin/heron-hub" : "serve")) die("ExecStart must invoke /usr/local/bin/heron-hub serve")
      next
    }
    if (pending != "") { set(pending, $0); pending = ""; next }
    if ($0 !~ /^--?[^-=]/) die("unexpected positional argument in ExecStart: " $0)
    name = $0; sub(/^--?/, "", name)
    eq = index(name, "=")
    if (eq) { value = substr(name, eq + 1); name = substr(name, 1, eq - 1) }
    if (name == "theme-origin") die("remove --theme-origin from the installed unit before upgrading; themes now use the panel hostname; old service was not stopped")
    if (!(name in known)) die("unsupported serve flag in ExecStart: " $0)
    if (eq) set(name, value); else pending = name
    next
  }
  {
    eq = index($0, "=")
    set(substr($0, 3, eq - 3), substr($0, eq + 1))
  }
  END {
    if (failed) exit 1
    if (position < 2 || pending != "") die("incomplete ExecStart arguments")
    print "/usr/local/bin/heron-hub"; print "serve"
    for (i = 1; i <= count; i++) print "--" order[i] "=" val[order[i]]
  }
' "$work/args" "$work/overrides" > "$work/merged" || exit 1
# 没有 --listen 时 serve 用自己的默认值 127.0.0.1:8080（cmd/hub/serve.go），端口预检按同一个值查。
EXEC=""; listen=127.0.0.1:8080; db=""
while IFS= read -r arg; do
  case "$arg" in
    --listen=*) listen=${arg#--listen=};;
    --db=*) db=${arg#--db=};;
  esac
  quote_arg "$arg"
  EXEC="$EXEC $quoted"
done < "$work/merged"
[ "$db" = /var/lib/heron/heron.db ] || fail 'installed unit must use --db /var/lib/heron/heron.db'
port=${listen##*:}
case "$port" in ''|*[!0-9]*) fail "listen address must end in a numeric TCP port: $listen";; esac
if ! { [ "$port" -ge 1 ] && [ "$port" -le 65535 ]; }; then fail "invalid TCP port: $port"; fi
HERON_HUB_EXEC=${EXEC# }
export HERON_HUB_EXEC
awk '/^ExecStart=/ { print "ExecStart=" ENVIRON["HERON_HUB_EXEC"]; next } { print }' "$work/heron-hub.service" > "$work/unit"

check_port() {
  # /proc 提供监听 inode，再经 fd 定位持有者；不能只按进程名排除，另一个同名进程也会占端口。
  own=0
  if [ -f "$UNIT" ]; then
    own=$(systemctl show heron-hub -p MainPID --value </dev/null)
    case "$own" in ''|*[!0-9]*) fail 'cannot determine existing hub MainPID';; esac
    if [ "$own" != 0 ]; then
      exe=$(readlink "$PROC/$own/exe") || fail 'cannot inspect existing hub executable'
      [ "$exe" = "$BIN" ] || fail "unexpected executable for existing hub: $exe"
    fi
  fi
  hex=$(printf '%04X' "$port")
  [ -r "$PROC/net/tcp" ] || fail 'cannot inspect TCP listeners'
  set -- "$PROC/net/tcp"
  [ ! -f "$PROC/net/tcp6" ] || set -- "$@" "$PROC/net/tcp6"
  inodes=$(awk -v p="$hex" '$4 == "0A" { split($2,a,":"); if (toupper(a[2]) == p) print $10 }' "$@")
  for inode in $inodes; do
    found=0
    for fd in "$PROC"/[0-9]*/fd/*; do
      target=$(readlink "$fd" 2>/dev/null) || continue
      [ "$target" = "socket:[$inode]" ] || continue
      holder=${fd#"$PROC"/}; holder=${holder%%/*}
      [ "$holder" = "$own" ] || fail "port $port is already in use by pid $holder; old service was not stopped"
      found=1
    done
    [ "$found" = 1 ] || fail "port $port is in use; cannot identify listener pid; old service was not stopped"
  done
}
# 停服前先核对一遍，这里失败时旧服务照常运行。这一遍只为早失败：目录此时是 0770，服务组还能增删其中的条目，
# 改属主的依据是下面停服加锁之后的复检。
if ! data_dir_ok || ! db_files_ok; then fail 'refusing to hand the database to heron-hub; old service was not stopped'; fi
check_port
prepare_updater
stop_service
# 从这里起旧服务已停：之后任何失败退出都由 on_exit 回滚（rollback_hub），直到启动确认成功。
[ ! -e "$BIN" ] || HAD_PREVIOUS=1
ROLLBACK=1
EXIT_HINT='heron-hub is stopped; rerun the installer or start it manually'
# 旧二进制先留成同目录 .bak，回滚时换回来（回滚要连带库，理由见 rollback_hub）。
if [ "$HAD_PREVIOUS" = 1 ]; then
  mv -f "$BIN" "$BIN_BAK"
  BACKED_UP=1
fi
mv -f "$BIN_TMP" "$BIN"

# SQLite 要创建和删除 WAL/SHM，目录必须可写，不能照搬只读配置目录的 0750。
# 停服务并确认该 uid 无进程之后，先把目录交给 root 并收成 0750：此后只有 root 能增删其中的条目，库文件的复检
# 看到的就是接下来要改属主的条目。预检之后、加锁之前，服务组仍能替换目录里的条目，所以这次复检不能省。
# 库文件复检失败时服务已停、目录留在 0750：服务用户建不了 WAL/SHM，没有现成 WAL/SHM 时 hub 被拉起也打不开库
# （0750 下实测报 attempt to write a readonly database），直到有人查看后重跑。目录本身的复检排在收紧之前，
# 它失败时目录没有被改动。不跟随链接、不递归 chown。
data_dir_ok || fail_stopped "$DATA changed after the pre-stop check"
mkdir -p "$DATA"
chown root:"$SVC_USER" "$DATA"
chmod 0750 "$DATA"
DATA_LOCKED=1
db_files_ok || fail_stopped "database files changed after the pre-stop check; $DATA stays locked at 0750 until you inspect it and rerun the installer"
# 新建的空库与已有的库走同一个交还步骤；umask 077 让它在交还之前也只有 root 可读。
[ -e "$DATA/heron.db" ] || (umask 077 && : > "$DATA/heron.db")
for file in "$DATA/heron.db" "$DATA/heron.db-wal" "$DATA/heron.db-shm"; do
  [ -e "$file" ] || continue
  chown "$SVC_USER:$SVC_USER" "$file"
  chmod 0600 "$file"
  # 目录此刻是 root 属主的 0750、服务用户无进程，库不会再被写入，这份副本没有竞态；候选 hub 启动时会以
  # MigrateSchema 迁移 schema，起不来或确认失败都要靠它把旧库换回来（.bak 与库文件同目录，换回来是同目录 rename）。
  cp "$file" "$file.bak"
done
DB_BACKED_UP=1
chmod 0770 "$DATA"
DATA_LOCKED=0
install -m 0644 "$work/unit" "$UNIT"
mv -f "$UPDATER_TMP" "$UPDATER_BIN"
install -m 0644 "$work/updater/heron-updater-hub.service" "$UPDATER_UNIT"
# 主单元写好之后再查一遍 drop-in（理由见 list_dropins 上方）。这里失败时不能叫人手动启动：设了 ExecStart 的
# drop-in 会让单元按它的参数起来，systemctl 失败时 drop-in 则还没被查过。单元仍 enabled 时（升级时上次安装建的
# 链接还在），下次开机也会这样起来。该做什么按失败点分开说，现状由 unit_state 按 unit_enabled 说。
unit_state() {
  if unit_enabled; then
    echo 'heron-hub is stopped but still enabled; started by hand or at the next boot, it would run with the drop-ins as they are'
  else
    echo 'heron-hub is installed but not enabled or started'
  fi
}
if ! list_dropins; then
  EXIT_HINT='fix the systemctl problem reported above, then rerun the installer'
  fail_stopped "$(unit_state)"
fi
if ! dropins_ok; then
  EXIT_HINT='fix the drop-in problem reported above, then rerun the installer'
  fail_stopped "$(unit_state)"
fi
systemctl enable heron-hub </dev/null
systemctl start heron-hub </dev/null
confirm_service_started
ROLLBACK=0
EXIT_HINT=""
rm -f "$BIN_BAK" "$DATA/heron.db.bak" "$DATA/heron.db-wal.bak" "$DATA/heron.db-shm.bak"
systemctl enable heron-updater-hub </dev/null
systemctl start heron-updater-hub </dev/null
UPDATER_RESTORE=0
echo "heron-hub installed and started (systemd, $ARCH, $PKG)"
echo 'Set the administrator password: heron-hub passwd --db /var/lib/heron/heron.db'
