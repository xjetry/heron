#!/bin/sh
# install.sh、install-hub.sh 与服务单元的真机验收：只在 OrbStack 真实启动的机器上跑，不进 CI。
# 机器名 pia- 前缀是隔离边界；只删除本 run 创建的机器（逐台登记）。
# 机器名还带本 run 的 pid（$$）：同时跑的几轮不争同一个名字；清理没能执行（被 SIGKILL、宿主重启）或删除失败
# 时留下的机器，也不再挡住下一轮的创建。代价是这类残留不再以创建失败显形，会静默累积，需要时用 orb list
# 按 pia- 前缀清点。
# 端口默认 18085（hub）/18086（下载服务），与 e2e 的 18080/18081、macos-accept 的 18087/18088 错开，
# 几个验收可同时跑；本机上别的进程占着默认端口时，用环境变量 HUB_PORT、DIST_PORT 覆盖。
set -eu
cd "$(cd "$(dirname "$0")/.." && pwd)"

TIER2=0
ONLY=""
usage() { echo "usage: install-accept.sh [--tier2] [--only DISTRO-ARCH]..." >&2; exit 2; }
while [ $# -gt 0 ]; do
  case "$1" in
    --tier2) TIER2=1; shift;;
    --only)
      [ "$#" -ge 2 ] || usage
      case "$2" in
        hub-debian-amd64|hub-debian-arm64|debian-amd64|debian-arm64|alpine-amd64|alpine-arm64|ubuntu-amd64|ubuntu-arm64|rocky-amd64|rocky-arm64) ;;
        *) echo "unknown cell: $2" >&2; usage;;
      esac
      ONLY="$ONLY $2"
      shift 2;;
    *) usage;;
  esac
done
RUN_AGENT=0
if [ -z "$ONLY" ]; then RUN_AGENT=1; fi
for selected in $ONLY; do
  case "$selected" in hub-*) ;; *) RUN_AGENT=1;; esac
done

# OrbStack 镜像写法。运行时可用同名环境变量覆盖。二级镜像只在 --tier2 时使用。
IMG_DEBIAN=${IMG_DEBIAN:-debian:12}
IMG_ALPINE=${IMG_ALPINE:-alpine:3.21}
IMG_UBUNTU=${IMG_UBUNTU:-ubuntu:24.04}
IMG_ROCKY=${IMG_ROCKY:-rocky:9}

VERSION_A=${VERSION_A:-v0.0.0-accept-a}
VERSION_B=${VERSION_B:-v0.0.0-accept-b}
HUB_PORT=${HUB_PORT:-18085}
DIST_PORT=${DIST_PORT:-18086}
HOST=host.orb.internal
work=$(mktemp -d)
echo "work=$work"
: > "$work/machines"
hub=""; httpd=""; rootrun=""
admin_pw="accept admin password 2026"

cleanup() {
  if [ -n "$httpd" ]; then kill "$httpd" 2>/dev/null || true; wait "$httpd" 2>/dev/null || true; fi
  if [ -n "$hub" ]; then kill "$hub" 2>/dev/null || true; wait "$hub" 2>/dev/null || true; fi
  if [ -n "$rootrun" ]; then kill "$rootrun" 2>/dev/null || true; wait "$rootrun" 2>/dev/null || true; fi
  while read -r m; do
    case "$m" in pia-*) orb delete -f "$m" > /dev/null 2>&1 || true;; *) echo "refusing to delete non-pia machine: $m" >&2;; esac
  done < "$work/machines"
}
# 被信号打断时 dash 不执行 EXIT trap（容器实测），macOS 的 /bin/sh 实测会执行但 sh 不保证：把 INT、TERM、HUP
# 转成 exit 1，Ctrl-C 时也删掉本轮的机器、停掉 18086 上的 python（非交互 shell 的后台作业忽略 SIGINT）。
trap cleanup EXIT
trap 'exit 1' INT TERM HUP

# 镜像下载可能瞬时 EOF；只重试创建本 run 的隔离机器，不复用或删除其它验收者的机器。
# 名字带本 run 的 pid，同一时刻只属于本 run，所以先登记再创建：orb create 在机器已建出之后才报错时，
# 这台也会被 cleanup 删掉。每次重试前先删掉可能半建成的同名机器，否则重试会因重名失败。
create_machine() {
  machine=$1; arch=$2; img=$3
  case "$machine" in pia-*) ;; *) echo "refusing to create non-pia machine: $machine" >&2; return 1;; esac
  echo "$machine" >> "$work/machines"
  : > "$work/create-$machine.log"
  for delay in 0 10 30 60; do
    if [ "$delay" != 0 ]; then
      echo "retry creating $machine after ${delay}s"
      sleep "$delay"
      orb delete -f "$machine" >> "$work/create-$machine.log" 2>&1 || true
    fi
    if orb create -a "$arch" "$img" "$machine" >> "$work/create-$machine.log" 2>&1; then return 0; fi
  done
  cat "$work/create-$machine.log" >&2
  return 1
}

# 两个版本各打一包再复制走：第二次发布目标会清空 dist/，重跑必须能证出版本从 A 变成 B。验收的是本次构建的
# agent，给出与 VERSION 相同的 AGENT_VERSION，产出完整的一套，不落入只发 hub 的分支。只复制安装要
# 下载的东西：安装脚本按内嵌的 SHA-256 校验 tar 包，不读 SHA256SUMS（发布清单由 release-manifest 在镜像构建之后
# 生成，这里不构建镜像）。准备路径由 scripts/release-assets-test.sh 用替身执行到 hub 格。
make release-full VERSION="$VERSION_A" AGENT_VERSION="$VERSION_A" > "$work/release-a.log" 2>&1 || { echo "FAIL: make release-full A"; tail -20 "$work/release-a.log"; exit 1; }
mkdir -p "$work/dist/a"
cp dist/heron-*.tar.gz dist/install.sh dist/install-hub.sh "$work/dist/a/"
make release-full VERSION="$VERSION_B" AGENT_VERSION="$VERSION_B" > "$work/release-b.log" 2>&1 || { echo "FAIL: make release-full B"; tail -20 "$work/release-b.log"; exit 1; }
mkdir -p "$work/dist/b"
cp dist/heron-*.tar.gz dist/install.sh dist/install-hub.sh "$work/dist/b/"
if [ "$RUN_AGENT" = 1 ]; then
  make binaries > "$work/binaries.log" 2>&1 || { echo "FAIL: make binaries"; tail -20 "$work/binaries.log"; exit 1; }
fi
# 本 run 的标记，HTTP 起来后核对它。install.sh 与包内文件在 deploy/ 未改动时各轮逐字相同，
# 比对被测文件区分不了本轮的服务与上一轮遗留的服务；$work 由本轮的 mktemp 产生，各轮必然不同。
printf '%s\n' "$work" > "$work/dist/run-id"

# 下载服务也供不需要本机 hub 的 hub 安装格使用，先核对本 run 标记再运行任何格。
# HTTP 以 $work/dist 为根，/a 与 /b 是两个版本目录。
# exec 让 $httpd 就是 python：子 shell 被 kill 后 python 会被 init 收养并继续占着端口，
# 下一轮会装到上一轮的包。起来之后核对应答的是本 run 的标记，端口被占时立即失败。
(cd "$work/dist" && exec python3 -m http.server "$DIST_PORT" --bind 127.0.0.1) > "$work/httpd.log" 2>&1 &
httpd=$!
attempt=0
while [ "$attempt" -lt 50 ]; do
  curl -fsS -o "$work/served-run-id" "http://127.0.0.1:$DIST_PORT/run-id" 2>/dev/null && break
  attempt=$((attempt + 1)); sleep 0.2
done
cmp -s "$work/dist/run-id" "$work/served-run-id" || { echo "FAIL: dist HTTP is not serving this run"; cat "$work/httpd.log"; exit 1; }

run_hub_cell() {
  arch=$1
  name="pia-hub-debian-$arch-$$"
  echo "== cell $name =="
  create_machine "$name" "$arch" "$IMG_DEBIAN" || { echo "FAIL($name): orb create"; exit 1; }
  # 脚本与包都从下载服务取得，真实覆盖发布资产和管道安装入口。
  if ! orb -m "$name" -u root sh -s -- "http://$HOST:$DIST_PORT" "$VERSION_A" "$VERSION_B" <<'HUB_ACCEPT' > "$work/hub-$name.log" 2>&1
set -eu
base=$1; version_a=$2; version_b=$3
trap 'rc=$?; if [ "$rc" != 0 ]; then systemctl status heron-hub --no-pager || true; journalctl -u heron-hub -n 40 --no-pager || true; cat /etc/systemd/system/heron-hub.service || true; fi' EXIT
fail() { echo "FAIL: $*"; exit 1; }
fetch() {
  if command -v curl >/dev/null 2>&1; then curl -fsSL -o "$2" "$1" </dev/null
  else wget -q -O "$2" "$1" </dev/null; fi
}
health() {
  code=$(curl -sS -o /root/site.json -w '%{http_code}' "http://127.0.0.1:$1/heron.v1.PublicService/GetSite?connect=v1&encoding=json&message=%7B%7D")
  [ "$code" = 200 ] || fail "anonymous GetSite returned $code"
}
# 安装器写出的单元交给真实的 systemd 解析：引号、$$、%% 都出自安装器。verify 有任何输出也算失败，不只看
# 退出码：Debian 12 上 systemd 252 实测，未知键名（如拼错的加固项）只打印 Unknown key … ignoring，
# 退出码仍是 0，服务照样启动；干净的单元不输出任何东西。
verify_unit() {
  rc=0
  systemd-analyze verify /etc/systemd/system/heron-hub.service > /root/verify.log 2>&1 </dev/null || rc=$?
  if [ "$rc" != 0 ] || [ -s /root/verify.log ]; then cat /root/verify.log; fail "systemd-analyze verify after $1 (exit $rc)"; fi
  echo "systemd-analyze verify after $1: exit 0, no output"
}
# 目录 root:heron-hub 0770；库文件与存在时的 WAL、SHM 属服务用户 0600。
assert_data_layout() {
  [ "$(stat -c '%U:%G %a' /var/lib/heron)" = 'root:heron-hub 770' ] &&
    [ "$(stat -c '%U:%G %a' /var/lib/heron/heron.db)" = 'heron-hub:heron-hub 600' ] || fail "hub data ownership or permissions $1"
  for f in /var/lib/heron/heron.db-wal /var/lib/heron/heron.db-shm; do
    [ ! -e "$f" ] || [ "$(stat -c '%U:%G %a' "$f")" = 'heron-hub:heron-hub 600' ] || fail "hub data ownership or permissions $1: $f"
  done
  stat -c '%n %U:%G %a' /var/lib/heron /var/lib/heron/heron.db*
}
# 按 comm 找 heron-hub 进程，写进 pids。卸载之后用户已删，不能再按有效 uid 扫。
hub_pids() {
  pids=""
  for s in /proc/[0-9]*/comm; do
    c=$(tr -d '\n' < "$s" 2>/dev/null || true)
    if [ "$c" = heron-hub ]; then p=${s#/proc/}; pids="$pids ${p%/comm}"; fi
  done
}
# purge 之后数据、二进制、单元、enable 链接、用户、组与 heron-hub 进程都不在。
assert_purged() {
  if [ -e /var/lib/heron ] || [ -e /usr/local/bin/heron-hub ] || [ -e /etc/systemd/system/heron-hub.service ] ||
    [ -e /usr/local/bin/heron-updater-hub ] || [ -e /var/lib/heron-update-hub ] || [ -e /run/heron-update-hub ] ||
    [ -e /etc/systemd/system/heron-updater-hub.service ] || [ -L /etc/systemd/system/multi-user.target.wants/heron-updater-hub.service ] ||
    [ -L /etc/systemd/system/multi-user.target.wants/heron-hub.service ] || id heron-hub || grep -q '^heron-hub:' /etc/group; then
    fail "hub purge left data, binary, unit, enable link, user or group ($1)"
  fi
  hub_pids
  [ -z "$pids" ] || fail "hub purge left heron-hub processes ($1):$pids"
}
assert_updater() {
  expected=$1
  n=0
  until [ -S /run/heron-update-hub/updater.sock ]; do
    n=$((n + 1)); [ "$n" -le 15 ] || fail 'hub updater socket did not appear'; sleep 1
  done
  p=$(systemctl show heron-updater-hub -p MainPID --value)
  [ "$p" -gt 0 ] && [ "$(awk '/^Uid:/ {print $3}' "/proc/$p/status")" = 0 ] || fail 'hub updater is not running as root'
  [ "$(readlink "/proc/$p/exe")" = /usr/local/bin/heron-updater-hub ] || fail 'unexpected hub updater executable'
  [ "$(/usr/local/bin/heron-updater-hub version)" = "$expected" ] || fail 'hub updater version mismatch'
  [ "$(stat -c '%U:%G %a' /run/heron-update-hub/updater.sock)" = 'root:heron-hub 660' ] || fail 'hub updater socket permissions'
  [ "$(stat -c '%U:%G %a' /var/lib/heron-update-hub)" = 'root:root 755' ] || fail 'hub updater state directory permissions'
}
systemctl --version | head -n 1
fetch "$base/a/install-hub.sh" /root/install-hub.sh
mkdir '/srv/heron site $literal%'
printf '<!doctype html><title>Heron acceptance</title>\n' > '/srv/heron site $literal%/index.html'
if [ -f /etc/ssl/certs/ca-certificates.crt ]; then echo 'CA bundle present before install'; else echo 'no CA bundle before install'; fi
cat /root/install-hub.sh | sh -s -- --base-url "$base/a" --listen 127.0.0.1:18120 --timezone Asia/Taipei --trusted-proxies 127.0.0.1/32 --public-dir '/srv/heron site $literal%'
# 下载地址是 http，hub 自己的 Telegram 出站仍要 CA 证书包。
[ -f /etc/ssl/certs/ca-certificates.crt ] || fail 'no CA bundle after installing over http'
health 18120
assert_updater "$version_a"
printf '%s\n' 'accept hub password 2026' | heron-hub passwd --db /var/lib/heron/heron.db
[ "$(heron-hub version)" = "$version_a" ] || fail 'installed version is not A'
pid=$(systemctl show heron-hub -p MainPID --value)
(
  exec 8>>/run/heron-install-hub.lock
  flock -n 8
  rc=0
  sh /root/install-hub.sh --base-url "$base/a" </dev/null > /root/installer-lock.log 2>&1 || rc=$?
  [ "$rc" != 0 ] && grep -q 'another heron-hub installer is running' /root/installer-lock.log || fail 'concurrent installer was not refused'
  [ "$(systemctl show heron-hub -p MainPID --value)" = "$pid" ] || fail 'concurrent installer stopped the running hub'
)
uid=$(awk '/^Uid:/ {print $3}' "/proc/$pid/status")
cap=$(awk '/^CapEff:/ {print $2}' "/proc/$pid/status")
[ "$uid" = "$(id -u heron-hub)" ] && [ "$uid" != 0 ] && [ "$((0x$cap))" = 0 ] || fail 'hub identity or capabilities'
assert_data_layout 'after install'
verify_unit 'install'
# 安装器判断单元是否 enabled 只看这条链接，首装后它必须在，否则升级时安装器会把仍会开机拉起的单元说成没 enable。
[ -L /etc/systemd/system/multi-user.target.wants/heron-hub.service ] || fail 'no multi-user.target.wants link after install'
# purge 之后按同一扫描为空才算没有残留：先确认它看得见正在运行的 hub。
hub_pids
case " $pids " in *" $pid "*) ;; *) fail "comm scan did not find the running hub (pid $pid):$pids";; esac

# 升级到 B 用 B 的脚本：脚本只装自己所属的版本，A 的脚本按内嵌的 A 哈希拒绝 B 的包，
# 用它做下面的端口冲突检查会先红在哈希上、走不到端口判定。
fetch "$base/b/install-hub.sh" /root/install-hub.sh

# 新端口被其它进程占用时，旧服务的同一个 pid 必须仍活着，不能先停服再发现绑定失败。
systemd-run --unit=pia-port-conflict /usr/local/bin/heron-hub serve --db /root/port-conflict.db --listen 127.0.0.1:8080 --timezone UTC </dev/null
sleep 1
curl -fsS -o /dev/null 'http://127.0.0.1:8080/heron.v1.PublicService/GetSite?connect=v1&encoding=json&message=%7B%7D'
rc=0
sh /root/install-hub.sh --base-url "$base/b" --listen 127.0.0.1:8080 </dev/null > /root/conflict.log 2>&1 || rc=$?
cat /root/conflict.log
[ "$rc" != 0 ] && [ "$(systemctl show heron-hub -p MainPID --value)" = "$pid" ] && kill -0 "$pid" || fail 'port conflict did not fail before stopping the running hub'
grep -q 'port 8080 is already in use' /root/conflict.log || fail 'port conflict did not report listener'
systemctl stop pia-port-conflict

# 单元里的参数是持久事实；重跑升级不能恢复成默认值，显式参数才覆盖。
sh /root/install-hub.sh --base-url "$base/b" </dev/null
[ "$(heron-hub version)" = "$version_b" ] || fail 'upgraded version is not B'
assert_updater "$version_b"
health 18120
pid=$(systemctl show heron-hub -p MainPID --value)
tr '\000' '\n' < "/proc/$pid/cmdline" > /root/args
awk '
  /^--timezone=/ { zone=substr($0,12) }
  /^--trusted-proxies=/ { proxies=substr($0,19) }
  /^--public-dir=/ { dir=substr($0,14) }
  END { exit !(zone == "Asia/Taipei" && proxies == "127.0.0.1/32" && dir == "/srv/heron site $literal%") }
' /root/args || fail 'upgrade lost installed arguments'
sh /root/install-hub.sh --base-url "$base/b" --timezone UTC </dev/null
pid=$(systemctl show heron-hub -p MainPID --value)
tr '\000' '\n' < "/proc/$pid/cmdline" > /root/args
# 覆盖按名替换：cmdline 里恰好一个 timezone 参数且值为 UTC。旧值留在前面、靠 flag 解析后者覆盖前者时，
# 运行结果一样，这条断言看得出来。
grep -E -e '^--?timezone(=|$)' /root/args > /root/timezone-args || [ "$?" = 1 ]
[ "$(cat /root/timezone-args)" = '--timezone=UTC' ] || { cat /root/args; fail 'explicit timezone must replace the installed value exactly once'; }
journalctl _SYSTEMD_UNIT=heron-hub.service "_PID=$pid" --no-pager -o cat > /root/startup.log
grep -q 'timezone=UTC' /root/startup.log || fail 'running hub did not apply explicit timezone'
health 18120
verify_unit 'override upgrade'

# 设 ExecStart 的 drop-in 会盖掉主单元里的参数。刚写到磁盘、还没 daemon-reload 时运行中的 hub 看不到它，
# 安装器自己启动前的 daemon-reload 却会让它生效：安装器要先 reload 再查，在停服前拒绝，旧服务的同一个 pid 仍在。
mkdir /etc/systemd/system/heron-hub.service.d
printf '[Service]\nExecStart =\nExecStart = /usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db --listen 127.0.0.1:18120 --timezone Europe/Berlin\n' > /etc/systemd/system/heron-hub.service.d/pia-exec.conf
# 前提：此刻 DropInPaths 里还没有它，不先 reload 就查不到。
systemctl show heron-hub -p DropInPaths --value > /root/dropins-unreloaded
cat /root/dropins-unreloaded
! grep -q pia-exec.conf /root/dropins-unreloaded || fail 'an unreloaded drop-in is already in DropInPaths'
rc=0
sh /root/install-hub.sh --base-url "$base/b" </dev/null > /root/dropin.log 2>&1 || rc=$?
cat /root/dropin.log
[ "$rc" != 0 ] && grep -q 'pia-exec.conf sets ExecStart' /root/dropin.log && [ "$(systemctl show heron-hub -p MainPID --value)" = "$pid" ] ||
  fail 'unreloaded ExecStart drop-in was not refused before stopping the hub'
# 拦下的是 systemd 真会采用的覆盖（键名两侧带空白也照样采用）：安装器 reload 之后它已生效。
systemctl show heron-hub -p ExecStart --value | grep -q 'Europe/Berlin' || fail 'systemd did not apply the ExecStart drop-in'
rm /etc/systemd/system/heron-hub.service.d/pia-exec.conf
rmdir /etc/systemd/system/heron-hub.service.d
systemctl daemon-reload

# root 手工操作可能把库文件留成 root:root 0644；停服后改坏，重跑要把它交还服务用户 0600，hub 照常起来。
systemctl stop heron-hub
chown root:root /var/lib/heron/heron.db
chmod 0644 /var/lib/heron/heron.db
sh /root/install-hub.sh --base-url "$base/b" </dev/null
assert_data_layout 'after ownership repair'
health 18120

# 在 systemd 真正启动前损坏即将安装的单元，绕开参数预检以独立验证启动后确认。
# 使用真实二进制与 systemd；Type=simple 的 start 成功不能代替进程存活。
mkdir /root/fault-bin
cat > /root/fault-bin/systemctl <<'START_FAULT'
#!/bin/sh
if [ "$1" = start ] && [ "$2" = heron-hub ]; then
  sed -i 's@/var/lib/heron/heron.db@/var/lib/heron/missing/heron.db@g' /etc/systemd/system/heron-hub.service
  /usr/bin/systemctl daemon-reload
fi
exec /usr/bin/systemctl "$@"
START_FAULT
chmod +x /root/fault-bin/systemctl
cp /etc/systemd/system/heron-hub.service /root/good-unit
rc=0
PATH="/root/fault-bin:$PATH" sh /root/install-hub.sh --base-url "$base/b" </dev/null > /root/start-fault.log 2>&1 || rc=$?
cat /root/start-fault.log
[ "$rc" != 0 ] && grep -q 'heron-hub did not' /root/start-fault.log || fail 'installer reported success for a hub with a missing database directory'
cp /root/good-unit /etc/systemd/system/heron-hub.service
systemctl daemon-reload
systemctl restart heron-hub
sleep 3
health 18120

# 不带确认的非交互卸载不得触碰服务；普通卸载保留数据和账户，purge 才删除。
rc=0
sh /root/install-hub.sh --uninstall </dev/null > /root/no-confirm.log 2>&1 || rc=$?
[ "$rc" != 0 ] && systemctl is-active --quiet heron-hub || fail 'unattended uninstall did not require --yes'
sh /root/install-hub.sh --uninstall --yes </dev/null
[ -f /var/lib/heron/heron.db ] && id heron-hub && [ ! -e /usr/local/bin/heron-hub ] || fail 'uninstall did not preserve data and account'
sh /root/install-hub.sh --uninstall --purge --yes </dev/null
assert_purged 'after purge'

# 首装前手工放置 drop-in。单元文件还不存在时，DropInPaths 查不到其中的 drop-in；安装器要在写好主单元
# 之后、enable 与 start 之前拦住设了 ExecStart 的 drop-in。
mkdir /etc/systemd/system/heron-hub.service.d
printf '[Service]\nExecStart=\nExecStart=/usr/local/bin/heron-hub serve --db /var/lib/heron/heron.db --listen 127.0.0.1:18120\n' > /etc/systemd/system/heron-hub.service.d/pia-exec.conf
# 前提：单元不存在时，reload 之后 DropInPaths 里也没有它。
systemctl daemon-reload
systemctl show heron-hub -p DropInPaths --value > /root/dropins-absent
cat /root/dropins-absent
! grep -q pia-exec.conf /root/dropins-absent || fail 'DropInPaths lists drop-ins of a unit that does not exist'
rc=0
sh /root/install-hub.sh --base-url "$base/b" </dev/null > /root/first-dropin.log 2>&1 || rc=$?
cat /root/first-dropin.log
[ "$rc" != 0 ] && grep -q 'pia-exec.conf sets ExecStart' /root/first-dropin.log && ! systemctl is-enabled --quiet heron-hub && ! systemctl is-active --quiet heron-hub ||
  fail 'first install with an ExecStart drop-in was not refused before enabling and starting'
rm -r /etc/systemd/system/heron-hub.service.d
sh /root/install-hub.sh --uninstall --purge --yes </dev/null
assert_purged 'after the refused first install'
echo 'HUB ACCEPT OK'
HUB_ACCEPT
  then
    echo "FAIL($name): hub acceptance"; cat "$work/hub-$name.log"; exit 1
  fi
  cat "$work/hub-$name.log"
  # guest 脚本从 stdin 读入：其中任何一条命令读了 stdin，余下的断言就被吞掉，guest 照样以 0 退出。
  # 退出码 0 证明不了断言都跑完了，末行必须是最后一条断言之后的标记。
  [ "$(tail -n 1 "$work/hub-$name.log")" = 'HUB ACCEPT OK' ] || { echo "FAIL($name): hub acceptance did not reach its last assertion"; exit 1; }
  orb delete -f "$name"
  grep -v "^$name\$" "$work/machines" > "$work/machines.tmp" || [ "$?" = 1 ]
  mv "$work/machines.tmp" "$work/machines"
  echo "== cell $name OK =="
}
run_selected_hubs() {
  for hub_arch in amd64 arm64; do
    case " $ONLY " in
      '  '|*" hub-debian-$hub_arch "*) run_hub_cell "$hub_arch";;
    esac
  done
}
if [ "$RUN_AGENT" = 0 ]; then
  run_selected_hubs
  echo 'INSTALL ACCEPT OK'
  exit 0
fi

# 每格 2 个节点（服务 + root 对照）。窗口名额必须盖住本 run 会注册的节点，否则后一格 register 被拒。
# --only 再留 1 个名额：单格注入若把重跑改成再次注册，窗口要接得住，节点数断言才看得到；
# 名额刚好用完时这次 register 会被拒成 unauthenticated，断言到不了。
# 窗口 TTL 按格数放大（每格 20 分钟，不少于 90 分钟）：八格加上镜像重试可能超过 90 分钟，
# 过期后的失败与 key 错误同是 unauthenticated。
if [ -n "$ONLY" ]; then
  cells=0
  for id in $ONLY; do cells=$((cells + 1)); done
  max_nodes=$((cells * 2 + cells))
else
  cells=4
  [ "$TIER2" = 1 ] && cells=8
  max_nodes=$((cells * 2))
fi
ttl_min=$((cells * 20))
[ "$ttl_min" -lt 90 ] && ttl_min=90
bin/heron-hub window open --db "$work/accept.db" --ttl "${ttl_min}m" --max "$max_nodes" > "$work/window.txt" 2>&1
key=$(sed -n 's/^key: //p' "$work/window.txt")
[ -n "$key" ] || { echo "FAIL: no window key"; exit 1; }

# 用 hub 默认 30s TTL（上报间隔 10s）。3 分钟 TTL 会把间隔抬到 1 分钟：
# 35 秒的 root 对照凑不齐两次 CPU 采样，90 秒也等不到任务下发后再上报的探测结果。
bin/heron-hub serve --db "$work/accept.db" --listen "127.0.0.1:$HUB_PORT" --timezone UTC > "$work/hub.log" 2>&1 &
hub=$!
attempt=0
# 就绪判据是匿名的 GetSite 返回 200，不取决于根路径服务什么（公开页是否构建、是否换了 --public-dir）。
while [ "$attempt" -lt 50 ]; do
  [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$HUB_PORT/heron.v1.PublicService/GetSite?connect=v1&encoding=json&message=%7B%7D")" = 200 ] && break
  attempt=$((attempt + 1)); sleep 0.2
done
# 200 也可能是别人占着 18085。本进程没打出 listening 就不是这次的 hub。
grep -q 'hub listening' "$work/hub.log" || { echo "FAIL: hub did not bind $HUB_PORT"; cat "$work/hub.log"; exit 1; }
printf '%s\n' "$admin_pw" | bin/heron-hub passwd --db "$work/accept.db" > "$work/passwd.log" 2>&1

: > "$work/jar"
base="http://127.0.0.1:$HUB_PORT"
rpc() {
  name=$1; body=$2
  curl -sS -o "$work/$name.json" -w '%{http_code}' -H 'Content-Type: application/json' \
    -b "$work/jar" -c "$work/jar" --data "$body" "$base/heron.v1.AdminService/$name"
}
[ "$(rpc Login "$(jq -nc --arg password "$admin_pw" '{password: $password}')")" = 200 ] || { echo "FAIL: login"; exit 1; }

node_field() { # node_field <node-name> <jq-expr> — 轮询直到 expr 为真（60 次 × 1s）
  i=0
  until [ "$(rpc GetSnapshot '{}')" = 200 ] && jq -e --arg n "$1" ".nodes[] | select(.name == \$n) | $2" "$work/GetSnapshot.json" > /dev/null 2>&1; do
    i=$((i + 1)); [ "$i" -lt 60 ] || return 1
    sleep 1
  done
}
list_nodes() { [ "$(rpc ListNodes '{}')" = 200 ]; }

# 与 install.sh 的 confirm_service_stopped 同一判据：有效 uid 等于 heron-agent 的进程就是本服务的进程。
# 前提由 create_account（专供 agent 的 nologin 账户）与服务定义（systemd User=、OpenRC command_user）保证；
# 服务定义若改以其他身份运行，两边要一起改。
# 不按名字。Alpine 3.21 / BusyBox 1.37.0 / OpenRC 0.55.1 实测：pidof heron-agent 还会列出 pidof 自己
# （comm=pidof，cmdline 含参数 heron-agent，有效 uid 0，exe 是 /bin/busybox）。同机 supervise-daemon
# 的 /proc/comm 是 supervise-daemo（15 字节截断）、有效 uid 0，不在那份 pidof 输出里。
# 按有效 uid 扫描时 root 对照进程不在结果里。
# 有效 uid（Uid 行第三列，与扫描用的同一列）不为 0，CapEff 含 bit 13（CAP_NET_RAW = 0x2000）。
# 服务进程所见的根目录与 init 所见的在同一个文件系统上（设备号相同）：加固项不得让 agent 统计到另一个根分区。
# 不拿两次上报的根分区总量比数值：OrbStack 机器的根是 btrfs，实测同一格里服务与 root 对照相隔 5 秒的
# 两次上报总量相差约 2.4 GiB 而已用量相同；同一时刻在带同样加固的 systemd-run 与普通环境里 statfs 结果相同。
# ping_group_range 写进日志：所测机器上组范围关闭时，ICMP 可用应来自能力而不是组范围。
assert_service_identity() {
  cell=$1
  orb -m "$cell" -u root sh -c '
    svc_uid=$(id -u heron-agent) || { echo "no heron-agent user"; exit 1; }
    pids=""
    for s in /proc/[0-9]*/status; do
      if euid=$(awk "\$1 == \"Uid:\" { print \$3; exit }" "$s" 2>/dev/null); then
        if [ "$euid" = "$svc_uid" ]; then p=${s#/proc/}; pids="$pids ${p%/status}"; fi
      elif [ -e "$s" ]; then
        echo "cannot read $s"; exit 1
      fi
    done
    set -- $pids
    [ "$#" -eq 1 ] || { echo "want exactly one process with euid $svc_uid (heron-agent), got $#:$pids"; exit 1; }
    pid=$1
    uid=$(awk "/^Uid:/ {print \$3; exit}" "/proc/$pid/status")
    cap=$(awk "/^CapEff:/ {print \$2; exit}" "/proc/$pid/status")
    echo "pid=$pid uid=$uid CapEff=$cap ping_group_range=$(cat /proc/sys/net/ipv4/ping_group_range)"
    [ "$uid" != 0 ] || exit 1
    rootdev=$(stat -c %d "/proc/$pid/root/") || { echo "cannot stat /proc/$pid/root"; exit 1; }
    initdev=$(stat -c %d /) || exit 1
    echo "rootdev=$rootdev initdev=$initdev"
    [ "$rootdev" = "$initdev" ] || { echo "service root is on another filesystem"; exit 1; }
    [ $((0x$cap & 0x2000)) -ne 0 ] || exit 1
  ' > "$work/ident-$cell.log" 2>&1 || { echo "FAIL($cell): service identity"; cat "$work/ident-$cell.log"; exit 1; }
  cat "$work/ident-$cell.log"
}

assert_agent_updater() {
  cell=$1; expected=$2
  orb -m "$cell" -u root sh -s -- "$expected" <<'UPDATER'
set -eu
if [ ! -d /run/systemd/system ]; then
  [ ! -e /usr/local/bin/heron-updater-agent ] && [ ! -e /etc/systemd/system/heron-updater-agent.service ]
  exit
fi
n=0
until [ -S /run/heron-update-agent/updater.sock ]; do
  n=$((n + 1)); [ "$n" -le 15 ] || { echo 'agent updater socket did not appear'; exit 1; }; sleep 1
done
p=$(systemctl show heron-updater-agent -p MainPID --value)
[ "$p" -gt 0 ] && [ "$(awk '/^Uid:/ {print $3}' "/proc/$p/status")" = 0 ]
[ "$(readlink "/proc/$p/exe")" = /usr/local/bin/heron-updater-agent ]
[ "$(/usr/local/bin/heron-updater-agent version)" = "$1" ]
[ "$(stat -c '%U:%G %a' /run/heron-update-agent/updater.sock)" = 'root:heron-agent 660' ]
[ "$(stat -c '%U:%G %a' /var/lib/heron-update-agent)" = 'root:root 755' ]
UPDATER
}

# systemd 的声明不代表进程已受限：按 MainPID 的实际 cgroup 回读内核值，安装与升级都检查。
# 期望只来自已安装单元；声明缺失也必须失败，不能把未设置的 max 当成合法上限。
assert_memory_limit() {
  cell=$1
  tag=$2
  rc=0
  orb -m "$cell" -u root sh -s <<'MEMORY_LIMIT' > "$work/memory-$tag-$cell.log" 2>&1 || rc=$?
set -eu
pid=$(systemctl show heron-agent -p MainPID --value)
cg=$(awk -F: '$1 == "0" {print $3}' "/proc/$pid/cgroup")
[ -n "$cg" ] || { echo "no cgroup v2 path for agent pid=$pid"; exit 1; }
actual=$(cat "/sys/fs/cgroup$cg/memory.max")
declared=$(awk '
  /^\[/ { service = ($0 == "[Service]") }
  service && /^[[:space:]]*MemoryMax[[:space:]]*=/ {
    sub(/^[^=]*=[[:space:]]*/, ""); sub(/[[:space:]]*$/, ""); value=$0
  }
  END { print value }
' /etc/systemd/system/heron-agent.service)
# 仓库单元使用正整数字节或二进制单位；拒绝空值、infinity 与百分比，不替无上限生成期望。
expected=$(awk -v value="$declared" 'BEGIN {
  if (value !~ /^[0-9]+[KMGTPE]?$/) exit 1
  unit=substr(value, length(value), 1)
  power=index("KMGTPE", unit)
  bytes=(value + 0) * (1024 ^ power)
  if (bytes <= 0) exit 1
  printf "%.0f", bytes
}') || { echo "memory.max mismatch: pid=$pid cgroup=$cg actual=$actual MemoryMax=${declared:-<missing>} (finite limit required)"; exit 1; }
echo "pid=$pid cgroup=$cg MemoryMax=$declared expected=$expected memory.max=$actual"
[ "$actual" = "$expected" ] || { echo "memory.max mismatch: actual=$actual expected=$expected"; exit 1; }
MEMORY_LIMIT
  cat "$work/memory-$tag-$cell.log"
  [ "$rc" = 0 ] || { echo "FAIL($cell): memory.max ($tag)"; exit 1; }
}

# 配置目录必须是 root:heron-agent 0750：服务用户能读配置，但不能增删目录项，
# root 对配置文件的后续操作才不会被链接劫持。配置文件属主必须是 heron-agent、0600：
# register 以 root 写入，不改属主服务就读不到。这两条由 install.sh 在 register 之后保证。
# OpenRC 日志目录必须保持 root:root 0755：服务用户能增删目录项时，root 按路径做的改属主可以被换成别的文件。
# 两个日志文件属主 heron-agent、0640，由 start_pre 的 checkpath 每次启动建立，不靠安装时建一次。
assert_layout() {
  cell=$1
  logs=$2
  tag=$3
  orb -m "$cell" -u root sh -c '
    fail=0
    want() {
      path=$1; expect=$2
      got=$(stat -c "%U:%G %a" "$path") || { echo "stat failed: $path"; fail=1; return; }
      echo "$path $got"
      [ "$got" = "$expect" ] || { echo "want $path $expect"; fail=1; }
    }
    want_owner() {
      path=$1; user=$2; mode=$3
      full=$(stat -c "%U:%G %a" "$path") || { echo "stat failed: $path"; fail=1; return; }
      got=$(stat -c "%U %a" "$path") || { echo "stat failed: $path"; fail=1; return; }
      echo "$path $full"
      [ "$got" = "$user $mode" ] || { echo "want $path owner $user mode $mode"; fail=1; }
    }
    want /etc/heron-agent "root:heron-agent 750"
    want_owner /etc/heron-agent/config.json heron-agent 600
    if [ "'"$logs"'" = 1 ]; then
      want /var/log/heron-agent "root:root 755"
      want_owner /var/log/heron-agent/heron-agent.log heron-agent 640
      want_owner /var/log/heron-agent/heron-agent.err heron-agent 640
    fi
    exit "$fail"
  ' > "$work/$tag-$cell.log" 2>&1 || { echo "FAIL($cell): ownership"; cat "$work/$tag-$cell.log"; exit 1; }
  cat "$work/$tag-$cell.log"
}

run_cell() {
  img=$1; distro=$2; arch=$3
  name="pia-$distro-$arch-$$"
  echo "== cell $name =="
  create_machine "$name" "$arch" "$img" || { echo "FAIL($name): orb create"; exit 1; }

  # 首次安装用面板命令的管道形态。无 curl 时用 wget（运行时再探一次，不把探测结果写死）；
  # 重跑下载版本 B 的 install.sh 用同一次探测的结果。
  # hub 地址是宿主机上的 http（$HOST 不是 loopback IP 字面量），agent 只在配置放行明文 http 时接受它（规则见 agentconfig.CheckHub），
  # 安装与 register 都带 --insecure-http，与面板对这种 origin 给出的命令相同。
  if orb -m "$name" -u root command -v curl >/dev/null 2>&1; then
    fetch="curl -fsSL http://$HOST:$DIST_PORT/a/install.sh"
    fetch_b="curl -fsSL -o /root/install.sh http://$HOST:$DIST_PORT/b/install.sh"
  else
    fetch="wget -qO- http://$HOST:$DIST_PORT/a/install.sh"
    fetch_b="wget -q -O /root/install.sh http://$HOST:$DIST_PORT/b/install.sh"
  fi
  orb -m "$name" -u root sh -c "$fetch | sh -s -- --hub http://$HOST:$HUB_PORT --key $key --insecure-http --base-url http://$HOST:$DIST_PORT/a" \
    > "$work/install-$name.log" 2>&1 || { echo "FAIL($name): install"; tail -20 "$work/install-$name.log"; exit 1; }

  node_field "$name" '.online == true and .metrics.cpuPct != null' || { echo "FAIL($name): node did not come online"; exit 1; }
  assert_service_identity "$name"
  assert_agent_updater "$name" "$VERSION_A"
  case "$distro" in
    debian|ubuntu|rocky) assert_memory_limit "$name" install;;
  esac
  case "$distro" in
    alpine) assert_layout "$name" 1 layout;;
    *) assert_layout "$name" 0 layout;;
  esac

  list_nodes || { echo "FAIL($name): ListNodes"; exit 1; }
  jq -e --arg n "$name" --arg v "$VERSION_A" '.nodes[] | select(.name == $n) | .facts.icmpAvailable == true and .facts.agentVersion == $v' "$work/ListNodes.json" > /dev/null \
    || { echo "FAIL($name): icmpAvailable or agentVersion A"; jq -r --arg n "$name" '.nodes[] | select(.name == $n) | .facts' "$work/ListNodes.json"; exit 1; }

  # 下面的 ICMP 任务探测本机回环，而回环在 agent 的默认拒绝集里（规则见 prober.Policy）。按宿主机放行的正规方式打开：
  # root 执行 configure，再重启服务让 agent 重新读配置（它只在启动时读）。configure 以 root 重写配置之后，
  # 配置仍须属服务用户、0600，否则重启后的服务读不到它；assert_layout 在真机上钉住这一点。
  orb -m "$name" -u root /usr/local/bin/heron-agent configure --config /etc/heron-agent/config.json --probe-allow 127.0.0.0/8 \
    > "$work/configure-$name.log" 2>&1 </dev/null || { echo "FAIL($name): configure --probe-allow"; cat "$work/configure-$name.log"; exit 1; }
  case "$distro" in
    alpine) orb -m "$name" -u root rc-service heron-agent restart > "$work/restart-$name.log" 2>&1 </dev/null;;
    *) orb -m "$name" -u root systemctl restart heron-agent > "$work/restart-$name.log" 2>&1 </dev/null;;
  esac || { echo "FAIL($name): restart after configure"; cat "$work/restart-$name.log"; exit 1; }
  node_field "$name" '.online == true and .metrics.cpuPct != null' || { echo "FAIL($name): not online after configure"; exit 1; }
  case "$distro" in
    alpine) assert_layout "$name" 1 layout-configure;;
    *) assert_layout "$name" 0 layout-configure;;
  esac

  # ICMP 任务下发并等到有结果：能力由 init 授予，非 root 服务必须有可用 ICMP。
  node_id=$(jq -r --arg n "$name" '.nodes[] | select(.name == $n) | .id' "$work/ListNodes.json")
  [ "$(rpc SaveProbeTask "$(jq -nc --arg id "$node_id" '{task: {kind: "PROBE_KIND_ICMP", target: "127.0.0.1", intervalS: 5, timeoutMs: 1000}, nodeIds: [$id]}')")" = 200 ] \
    || { echo "FAIL($name): SaveProbeTask"; cat "$work/SaveProbeTask.json"; exit 1; }
  task_id=$(jq -r '.task.task.id' "$work/SaveProbeTask.json")
  now=$(date +%s)
  qbody=$(jq -nc --arg id "$node_id" --argjson from "$((now - 600))" --argjson to "$((now + 120))" '{nodeId: $id, from: $from, to: $to, maxPoints: 20}')
  i=0
  # 还没有结果时 series 不出现；any(.series[]) 会报错而不是假，空窗必须当成还没到。
  until [ "$(rpc QueryProbes "$qbody")" = 200 ] && jq -e --arg t "$task_id" 'any(.series[]?; .taskId == $t and any(.samples[]?; .sent > 0 and (.errors // 0) == 0))' "$work/QueryProbes.json" > /dev/null; do
    i=$((i + 1)); [ "$i" -lt 45 ] || { echo "FAIL($name): no ICMP results"; cat "$work/QueryProbes.json"; exit 1; }
    sleep 2
  done

  # 加固不得让采集缩水：字段集合、内存总量与 bootId 与 root 对照一致；根分区是不是同一个由
  # assert_service_identity 按设备号判定。网卡只以合计计数器上报，逐网卡集合经接口观测不到，不作断言。
  orb -m "$name" -u root /usr/local/bin/heron-agent register --hub "http://$HOST:$HUB_PORT" --key "$key" --insecure-http \
    --config /root/root-agent.json --name "$name-root" > "$work/regroot-$name.log" 2>&1 || { echo "FAIL($name): root register"; exit 1; }
  orb -m "$name" -u root timeout 35 /usr/local/bin/heron-agent run --config /root/root-agent.json > "$work/runroot-$name.log" 2>&1 &
  rootrun=$!
  node_field "$name-root" '.online == true and .metrics.cpuPct != null' || { echo "FAIL($name): root node did not come online"; exit 1; }
  jq -e --arg a "$name" --arg b "$name-root" '
    ([.nodes[] | select(.name == $a) | .metrics][0]) as $ma |
    ([.nodes[] | select(.name == $b) | .metrics][0]) as $mb |
    ($ma | keys | sort) == ($mb | keys | sort) and
    $ma.memTotal == $mb.memTotal and $ma.bootId == $mb.bootId' \
    "$work/GetSnapshot.json" > /dev/null || { echo "FAIL($name): service/root metrics mismatch"; cat "$work/GetSnapshot.json"; exit 1; }
  # timeout 35 到点结束 root 对照，退出码非零是预期的；等完即清空，cleanup 不再去 kill 一个可能已被复用的 pid。
  wait "$rootrun" || true
  rootrun=""

  # 重跑即升级：文件形态，base-url 指向版本 B。沿用注册；版本必须变成 B，节点数不变。
  list_nodes || { echo "FAIL($name): ListNodes before rerun"; exit 1; }
  before=$(jq '[.nodes[] | select(.name | startswith("'"$name"'"))] | length' "$work/ListNodes.json")
  orb -m "$name" -u root sh -c "$fetch_b" \
    > "$work/fetchb-$name.log" 2>&1 || { echo "FAIL($name): fetch rerun install.sh"; exit 1; }
  # 手工 register 以 root 重写配置，文件属主回到 root；人工编辑留下 0644。重跑必须都改回来，节点仍在线。
  # 重跑带 --insecure-http 走沿用配置时的 configure 分支（已有 http 部署的升级路径），它在改属主之前执行。
  orb -m "$name" -u root chown root:root /etc/heron-agent/config.json
  orb -m "$name" -u root chmod 0644 /etc/heron-agent/config.json
  orb -m "$name" -u root sh /root/install.sh --hub "http://$HOST:$HUB_PORT" --key "$key" --insecure-http --base-url "http://$HOST:$DIST_PORT/b" \
    > "$work/rerun-$name.log" 2>&1 || { echo "FAIL($name): rerun"; tail -20 "$work/rerun-$name.log"; exit 1; }
  grep -q 'keeping the current registration' "$work/rerun-$name.log" || { echo "FAIL($name): rerun did not keep registration"; exit 1; }
  list_nodes || { echo "FAIL($name): ListNodes after rerun"; exit 1; }
  after=$(jq '[.nodes[] | select(.name | startswith("'"$name"'"))] | length' "$work/ListNodes.json")
  [ "$before" = "$after" ] || { echo "FAIL($name): node count changed on rerun ($before -> $after)"; exit 1; }
  # 新进程的第一次上报才带上新版本；安装脚本返回时这次上报通常还没到。
  i=0
  until list_nodes && jq -e --arg n "$name" --arg v "$VERSION_B" '.nodes[] | select(.name == $n) | .facts.agentVersion == $v' "$work/ListNodes.json" > /dev/null; do
    i=$((i + 1)); [ "$i" -lt 60 ] || { echo "FAIL($name): agentVersion after rerun is not B"; exit 1; }
    sleep 1
  done
  case "$distro" in
    alpine) assert_layout "$name" 1 layout-rereg;;
    *) assert_layout "$name" 0 layout-rereg;;
  esac
  node_field "$name" '.online == true and .metrics.cpuPct != null' || { echo "FAIL($name): not online after ownership repair"; exit 1; }
  assert_agent_updater "$name" "$VERSION_B"
  case "$distro" in
    debian|ubuntu|rocky) assert_memory_limit "$name" upgrade;;
  esac

  case "$distro" in
    debian|ubuntu|rocky)
      # 判据与 hub 格的 verify_unit 相同：未知键名 verify 只告警、照样以 0 退出，有任何输出也算失败。
      rc=0
      orb -m "$name" -u root systemd-analyze verify /etc/systemd/system/heron-agent.service > "$work/verify-$name.log" 2>&1 </dev/null || rc=$?
      if [ "$rc" != 0 ] || [ -s "$work/verify-$name.log" ]; then
        echo "FAIL($name): systemd-analyze verify (exit $rc)"; cat "$work/verify-$name.log"; exit 1
      fi;;
    alpine)
      # 删日志目录后重启仍须健康：start_pre 每次启动都建，不靠安装时建一次。
      orb -m "$name" -u root rm -rf /var/log/heron-agent
      orb -m "$name" -u root rc-service heron-agent restart > "$work/logrestart-$name.log" 2>&1 \
        || { echo "FAIL($name): restart after log dir removed"; cat "$work/logrestart-$name.log"; exit 1; }
      node_field "$name" '.online == true and .metrics.cpuPct != null' || { echo "FAIL($name): not healthy after log restart"; exit 1; }
      assert_layout "$name" 1 layout-restart
      # 重启机器后自启。lastSeenAt 是 protojson 对 int64 的十进制字符串，不是 JSON number，数值比较必须先 tonumber。
      # 同时验证 depend() 的 after net 在真机开机顺序下能自启。
      seen=$(jq -r --arg n "$name" '.nodes[] | select(.name == $n) | .lastSeenAt' "$work/GetSnapshot.json")
      orb stop "$name"
      orb start "$name"
      i=0
      until [ "$(rpc GetSnapshot '{}')" = 200 ] && jq -e --arg n "$name" --arg s "$seen" '.nodes[] | select(.name == $n) | .online == true and (.lastSeenAt | tonumber) > ($s | tonumber)' "$work/GetSnapshot.json" > /dev/null 2>&1; do
        i=$((i + 1)); [ "$i" -lt 120 ] || { echo "FAIL($name): did not come back after reboot"; exit 1; }
        sleep 1
      done
      # 停机前已经在路上的上报也能把 lastSeenAt 推过保存值，那时服务还没被拉起来。
      # Alpine 3.21 / OpenRC 0.55.1 实测 orb start 返回后约 15 秒监督进程才出现。
      i=0
      until orb -m "$name" -u root sh -c '
        uid=$(id -u heron-agent) || exit 1
        for s in /proc/[0-9]*/status; do
          euid=$(awk "\$1 == \"Uid:\" { print \$3; exit }" "$s" 2>/dev/null) || continue
          [ "$euid" = "$uid" ] && exit 0
        done
        exit 1
      ' >/dev/null 2>&1; do
        i=$((i + 1)); [ "$i" -lt 40 ] || { echo "FAIL($name): service process did not appear after reboot"; exit 1; }
        sleep 1
      done
      assert_service_identity "$name";;
  esac

  orb -m "$name" -u root sh /root/install.sh --uninstall --purge > "$work/uninstall-$name.log" 2>&1 || { echo "FAIL($name): uninstall"; tail -20 "$work/uninstall-$name.log"; exit 1; }
  case "$distro" in
    debian|ubuntu|rocky)
      orb -m "$name" -u root sh -c 'test ! -f /etc/systemd/system/heron-agent.service && ! systemctl is-enabled heron-agent >/dev/null 2>&1' \
        || { echo "FAIL($name): systemd service still present after uninstall"; exit 1; };;
    alpine)
      orb -m "$name" -u root sh -c 'test ! -f /etc/init.d/heron-agent && ! rc-update show default 2>/dev/null | grep -q heron-agent' \
        || { echo "FAIL($name): openrc service still present after uninstall"; exit 1; };;
  esac
  # BusyBox 1.37.0（Alpine 3.21，同机 OpenRC 0.55.1）的 pidof 会把参数 heron-agent 匹配到 pidof 自己。
  # 卸载后用户已删除，不能再按有效 uid 扫；comm 精确等于 heron-agent 才是残留的 agent 进程。
  orb -m "$name" -u root sh -c '
    left=""
    for s in /proc/[0-9]*/comm; do
      c=$(tr -d "\n" < "$s" 2>/dev/null || true)
      if [ "$c" = heron-agent ]; then
        p=${s#/proc/}
        left="$left ${p%/comm}"
      fi
    done
    if [ -n "$left" ]; then echo "leftover heron-agent:$left"; exit 1; fi
    if [ -e /var/log/heron-agent ]; then echo "leftover log dir"; exit 1; fi
    test ! -e /usr/local/bin/heron-updater-agent && test ! -e /etc/systemd/system/heron-updater-agent.service &&
      test ! -e /var/lib/heron-update-agent && test ! -e /run/heron-update-agent &&
      test ! -L /etc/systemd/system/multi-user.target.wants/heron-updater-agent.service &&
      test ! -e /usr/local/bin/heron-agent && ! id heron-agent >/dev/null 2>&1 && ! grep -q "^heron-agent:" /etc/group && test ! -e /etc/heron-agent
  ' || { echo "FAIL($name): purge left process, binary, user, group, config, or log dir"; exit 1; }

  orb delete -f "$name" > /dev/null 2>&1
  # grep 把名单滤成空时退出码为 1；set -eu 会把"只剩这一台"当成失败。
  # 名单里只剩本格时 grep 无保留行、退出码为 1，属正常；只放过 1，读不到文件等错误（2）照常失败。
  grep -v "^$name\$" "$work/machines" > "$work/machines.tmp" || [ "$?" = 1 ]
  mv "$work/machines.tmp" "$work/machines"
  echo "== cell $name OK =="
}

run_if() {
  id=$1; tier=$2; img=$3; distro=$4; arch=$5
  if [ -n "$ONLY" ]; then
    case " $ONLY " in
      *" $id "*) ;;
      *) return 0;;
    esac
  elif [ "$tier" != 1 ] && [ "$TIER2" != 1 ]; then
    return 0
  fi
  run_cell "$img" "$distro" "$arch"
}

run_if debian-amd64 1 "$IMG_DEBIAN" debian amd64
run_if debian-arm64 1 "$IMG_DEBIAN" debian arm64
run_if alpine-amd64 1 "$IMG_ALPINE" alpine amd64
run_if alpine-arm64 1 "$IMG_ALPINE" alpine arm64
run_if ubuntu-amd64 2 "$IMG_UBUNTU" ubuntu amd64
run_if ubuntu-arm64 2 "$IMG_UBUNTU" ubuntu arm64
run_if rocky-amd64 2 "$IMG_ROCKY" rocky amd64
run_if rocky-arm64 2 "$IMG_ROCKY" rocky arm64
run_selected_hubs
echo "INSTALL ACCEPT OK"
