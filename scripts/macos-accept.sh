#!/bin/sh
# macOS agent 的本机验收：以普通用户在本机起 hub，用本次 make release-full 的 darwin 产物注册并运行 agent，
# 经管理 API（curl + jq）断言各指标、facts、ICMP 与流量差分；再把包里的 plist 改成用户域作业交给 launchd，
# 验证 KeepAlive 拉起、ThrottleInterval 与日志文件由 launchd 创建。
# 不 sudo，不执行安装脚本：system 域、专用账户与 root 属主不在本脚本里验证，须在真机上核对 LaunchDaemon 的 UserName 与文件属主。
# 端口默认 18087（hub）/18088（回环流量源），与 e2e（18080/18081）、install-accept（18085/18086）错开，
# 几个验收可同时跑；本机上别的进程占着默认端口时，用环境变量 PORT、BLOB_PORT 覆盖。
set -eu
cd "$(cd "$(dirname "$0")/.." && pwd)"
[ "$(uname -s)" = Darwin ] || { echo "macos-accept.sh runs on macOS only" >&2; exit 2; }
# 以 root 跑会掩盖本验收要证明的事：macOS 上非 root 进程可用数据报 ICMP socket。
[ "$(id -u)" != 0 ] || { echo "run macos-accept.sh as a normal user" >&2; exit 2; }

VERSION=v0.0.0-macos-accept
PORT=${PORT:-18087}
BLOB_PORT=${BLOB_PORT:-18088}
base="http://127.0.0.1:$PORT"
uid=$(id -u)
label="xyz.heron.agent.accept.$$"
work=$(mktemp -d)
echo "work=$work"
admin_pw="macos accept password 2026"
hub=""; agent=""; blob=""; job=""; hub_starts=0

cleanup() {
  if [ -n "$job" ]; then launchctl bootout "gui/$uid/$label" > /dev/null 2>&1 || true; fi
  for p in $agent $blob $hub; do kill "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; done
  rm -rf "$work/blob"
}
# EXIT trap 覆盖正常结束、exit 与 set -e。被信号打断时：dash 不执行 EXIT trap（容器实测），本机的 /bin/sh
# （bash 3.2 的 sh 模式）实测会执行，但 sh 不保证这一点；把 INT、TERM、HUP 转成 exit 1，两种 sh 下清理都会跑，
# 退出码也是可辨的 1。清理必须跑：launchd 起的作业不在本脚本的进程组里、收不到 Ctrl-C，有 KeepAlive 会一直活着；
# 非交互 shell 的后台作业忽略 SIGINT（实测），18088 上的 python 也不会自己退出。
trap cleanup EXIT
trap 'exit 1' INT TERM HUP

# SIGKILL 与断电 trap 不到，标签又按 $$ 生成，之后没有哪一轮会卸下上一轮的孤儿作业。开始前列出来，
# 只报不卸：它可能属于同时在跑的另一轮。
orphans=$(launchctl print "gui/$uid" | awk '{ for (i = 1; i <= NF; i++) if ($i ~ /^xyz\.heron\.agent\.accept\./) print $i }' | sort -u)
if [ -n "$orphans" ]; then
  echo "FAIL: launchd jobs left by an earlier run:"
  for o in $orphans; do echo "  launchctl bootout gui/$uid/$o"; done
  exit 1
fi

if [ "$(sysctl -in hw.optional.arm64)" = 1 ]; then arch=arm64; else arch=amd64; fi

# 验收的是本次构建的 agent，给出与 VERSION 相同的 AGENT_VERSION，产出完整的一套。
make release-full VERSION="$VERSION" AGENT_VERSION="$VERSION" > "$work/release.log" 2>&1 || { echo "FAIL: make release-full"; tail -20 "$work/release.log"; exit 1; }
for a in amd64 arm64; do
  mkdir -p "$work/pkg-$a"
  tar -xzf "dist/heron-agent_darwin_$a.tar.gz" -C "$work/pkg-$a"
  [ -x "$work/pkg-$a/heron-agent" ] && [ -f "$work/pkg-$a/xyz.heron.agent.plist" ] || { echo "FAIL: darwin/$a package contents"; ls -l "$work/pkg-$a"; exit 1; }
done
bin="$work/pkg-$arch/heron-agent"
[ "$("$bin" version)" = "$VERSION" ] || { echo "FAIL: $arch binary version"; exit 1; }
# 上一行原生执行本机架构的产物。arm64 原生执行必须有签名：Go 的链接器为 darwin/arm64 写入 ad-hoc 签名，
# 去掉签名的 arm64 二进制被内核杀掉（实测退出 137）。
# amd64 产物没有签名（codesign 报 not signed at all），在 Rosetta 下照常运行（实测），这一段证明的只是
# amd64 产物能在 Apple Silicon 上经 Rosetta 起来。两段都只覆盖本机 make release-full 的产物，覆盖不到
# 发布流水线在 ubuntu 上构建的产物。
# 跳过 Rosetta 这一段要显式给 ACCEPT_SKIP_ROSETTA=1，末行随之写明 amd64 未执行。
amd64_note=""
if [ "$arch" = arm64 ]; then
  if arch -x86_64 /usr/bin/true > "$work/rosetta.log" 2>&1; then
    [ "$(arch -x86_64 "$work/pkg-amd64/heron-agent" version)" = "$VERSION" ] || { echo "FAIL: amd64 binary under Rosetta"; exit 1; }
  elif [ "${ACCEPT_SKIP_ROSETTA-}" = 1 ]; then
    amd64_note=", amd64 not executed"
  else
    echo "FAIL: cannot run x86_64 code (Rosetta); set ACCEPT_SKIP_ROSETTA=1 to accept without it"; cat "$work/rosetta.log"; exit 1
  fi
fi
pkg_plist="$work/pkg-$arch/xyz.heron.agent.plist"
plutil -lint "$pkg_plist" > /dev/null || { echo "FAIL: packaged plist does not lint"; exit 1; }
# 用户域作业只替换路径与 Label；包里的程序参数一变，这里就红，不让验收悄悄跑一份与发布不同的参数。
plutil -extract ProgramArguments json -o - "$pkg_plist" |
  jq -e '. == ["/usr/local/bin/heron-agent", "run", "--config", "/etc/heron-agent/config.json"]' > /dev/null ||
  { echo "FAIL: packaged ProgramArguments changed"; plutil -extract ProgramArguments json -o - "$pkg_plist"; exit 1; }
for k in UserName GroupName; do
  [ "$(plutil -extract "$k" raw -o - "$pkg_plist")" = _heron-agent ] || { echo "FAIL: packaged $k is not _heron-agent"; exit 1; }
done

go build -o "$work/heron-hub" ./cmd/hub
"$work/heron-hub" window open --db "$work/hub.db" --ttl 10m --max 1 > "$work/window.txt"
key=$(sed -n 's/^key: //p' "$work/window.txt")
[ -n "$key" ] || { echo "FAIL: no window key"; exit 1; }

# HERON_OFFLINE_AFTER=12s：上报间隔 = TTL/agentwire.ReportsPerTTL = 4s，退避上限 = ReportsPerTTL × 间隔 = 12s。
start_hub() {
  HERON_OFFLINE_AFTER=12s "$work/heron-hub" serve --db "$work/hub.db" --listen "127.0.0.1:$PORT" --timezone UTC >> "$work/hub.log" 2>&1 &
  hub=$!
  i=0
  # 就绪判据是匿名的 GetSite 返回 200，不取决于根路径服务什么（公开页是否构建、是否换了 --public-dir）。
  until [ "$(curl -s -o /dev/null -w '%{http_code}' "$base/heron.v1.PublicService/GetSite?connect=v1&encoding=json&message=%7B%7D")" = 200 ]; do
    i=$((i + 1)); [ "$i" -lt 50 ] || { echo "FAIL: hub did not answer"; cat "$work/hub.log"; exit 1; }
    sleep 0.2
  done
  # 200 也可能来自占着端口的别的进程：每起一次，日志里就必须多一行 listening。
  hub_starts=$((hub_starts + 1))
  [ "$(grep -c 'hub listening' "$work/hub.log")" = "$hub_starts" ] || { echo "FAIL: hub did not bind $PORT"; cat "$work/hub.log"; exit 1; }
}
stop_hub() { stop_child "$hub" hub; hub=""; }
# stop_child <pid> <名>：停掉本脚本起的子进程。它若已自行退出，kill 失败，说明它在不该退出时退出了。
stop_child() {
  kill "$1" 2> /dev/null || { echo "FAIL: $2 exited on its own"; exit 1; }
  wait "$1" || true
}
start_hub
printf '%s\n' "$admin_pw" | "$work/heron-hub" passwd --db "$work/hub.db" > "$work/passwd.log" 2>&1

: > "$work/jar"
rpc() {
  name=$1; body=$2
  curl -sS -o "$work/$name.json" -w '%{http_code}' -H 'Content-Type: application/json' \
    -b "$work/jar" -c "$work/jar" --data "$body" "$base/heron.v1.AdminService/$name"
}
login() { [ "$(rpc Login "$(jq -nc --arg p "$admin_pw" '{password: $p}')")" = 200 ] || { echo "FAIL: login"; exit 1; }; }
login
# until_node <秒> <jq 表达式>：在上限内轮询快照，直到唯一节点满足表达式。
until_node() {
  deadline=$(($(date +%s) + $1))
  until [ "$(rpc GetSnapshot '{}')" = 200 ] && jq -e ".nodes | length == 1 and (.[0] | $2)" "$work/GetSnapshot.json" > /dev/null; do
    [ "$(date +%s)" -lt "$deadline" ] || { echo "FAIL: node did not satisfy $2 within $1s"; cat "$work/GetSnapshot.json"; exit 1; }
    sleep 0.5
  done
}

"$bin" register --hub "$base" --key "$key" --config "$work/agent.json" --name macos-accept > "$work/register.log" 2>&1 || { echo "FAIL: register"; cat "$work/register.log"; exit 1; }
# 下面的 ICMP 与 TCP 任务探测本机回环，而回环在 agent 的默认拒绝集里（规则见 prober.Policy）。按宿主机放行的正规方式打开：
# configure 改本地策略。agent 只在启动时读配置，这里在第一次 run 之前改，不必重启。hub 在 127.0.0.1 上，是 loopback
# IP 字面量，register 不需要 --insecure-http。
"$bin" configure --config "$work/agent.json" --probe-allow 127.0.0.0/8 > "$work/configure.log" 2>&1 || { echo "FAIL: configure --probe-allow"; cat "$work/configure.log"; exit 1; }
"$bin" run --config "$work/agent.json" > "$work/agent.log" 2>&1 &
agent=$!

# cpu_pct 与 net_*_bps 要两次采样才有；第二次上报后每个字段都必须在。字段表从 proto 的 message Metrics
# 枚举（proto3 JSON 用 lowerCamel 名、省略未设置的 optional 字段），proto 新增的字段自动进入检查。
until_node 60 '.online and .metrics.cpuPct != null and .metrics.netRxBps != null'
metric_fields=$(awk '/^message Metrics \{/ { m = 1; next } m && /^\}/ { m = 0 } m && /= [0-9]+;/ { print ($1 == "optional" ? $3 : $2) }' proto/heron/v1/*.proto |
  jq -Rsc 'split("\n") | map(select(. != "") | split("_") | .[0] + ([.[1:][] | (.[:1] | ascii_upcase) + .[1:]] | join("")))')
[ "$(printf '%s' "$metric_fields" | jq 'length')" -gt 0 ] || { echo "FAIL: no fields enumerated from message Metrics"; exit 1; }
missing=$(jq -c --argjson f "$metric_fields" '.nodes[0].metrics as $m | [$f[] | select($m[.] == null)]' "$work/GetSnapshot.json")
[ "$missing" = "[]" ] || { echo "FAIL: missing metrics $missing"; cat "$work/GetSnapshot.json"; exit 1; }
m() { jq -r ".nodes[0].metrics.$1" "$work/GetSnapshot.json"; }
[ "$(m bootId)" = "$(sysctl -n kern.bootsessionuuid)" ] || { echo "FAIL: boot id $(m bootId) is not kern.bootsessionuuid"; exit 1; }
[ "$(m memTotal)" = "$(sysctl -n hw.memsize)" ] || { echo "FAIL: mem total $(m memTotal) is not hw.memsize"; exit 1; }
[ "$(m memUsed)" -le "$(m memTotal)" ] || { echo "FAIL: mem used $(m memUsed) above total"; exit 1; }
[ "$(m diskUsed)" -le "$(m diskTotal)" ] || { echo "FAIL: disk used $(m diskUsed) above total"; exit 1; }
node=$(jq -r '.nodes[0].id' "$work/GetSnapshot.json")

[ "$(rpc ListNodes '{}')" = 200 ] || { echo "FAIL: ListNodes"; exit 1; }
jq -e --arg os "macOS $(sw_vers -productVersion)" --arg kernel "$(uname -r)" --arg arch "$arch" \
  --arg cpu "$(sysctl -n machdep.cpu.brand_string)" --argjson cores "$(sysctl -n hw.logicalcpu)" --arg v "$VERSION" \
  '.nodes[0].facts | .os == $os and .kernel == $kernel and .arch == $arch and .cpuModel == $cpu and .cpuCores == $cores and .agentVersion == $v and .icmpAvailable == true' \
  "$work/ListNodes.json" > /dev/null || { echo "FAIL: facts"; cat "$work/ListNodes.json"; exit 1; }

# 非特权数据报 ICMP 探测回环、TCP 连 hub：两个任务都要有成功的样本且没有 error。
icmp_body=$(jq -nc --arg n "$node" '{task: {kind: "PROBE_KIND_ICMP", target: "127.0.0.1", intervalS: 5, timeoutMs: 1000}, nodeIds: [$n]}')
[ "$(rpc SaveProbeTask "$icmp_body")" = 200 ] || { echo "FAIL: SaveProbeTask icmp"; cat "$work/SaveProbeTask.json"; exit 1; }
icmp_task=$(jq -r '.task.task.id' "$work/SaveProbeTask.json")
tcp_body=$(jq -nc --arg n "$node" --arg t "127.0.0.1:$PORT" '{task: {kind: "PROBE_KIND_TCP", target: $t, intervalS: 5, timeoutMs: 1000}, nodeIds: [$n]}')
[ "$(rpc SaveProbeTask "$tcp_body")" = 200 ] || { echo "FAIL: SaveProbeTask tcp"; cat "$work/SaveProbeTask.json"; exit 1; }
tcp_task=$(jq -r '.task.task.id' "$work/SaveProbeTask.json")
now=$(date +%s)
probe_body=$(jq -nc --arg n "$node" --argjson from "$((now - 600))" --argjson to "$((now + 600))" '{nodeId: $n, from: $from, to: $to, maxPoints: 100}')
# 结果在分钟桶刷出后才可查：最坏约一分钟加首次偏移与上报间隔。
deadline=$(($(date +%s) + 90))
until [ "$(rpc QueryProbes "$probe_body")" = 200 ] && jq -e --arg i "$icmp_task" --arg t "$tcp_task" \
  '(.series // []) as $s | ([$s[].taskId] | sort) == ([$i, $t] | sort) and all($s[]; any(.samples[]; .sent > 0 and .rttMeanUs != null and (.errors // 0) == 0))' \
  "$work/QueryProbes.json" > /dev/null; do
  [ "$(date +%s)" -lt "$deadline" ] || { echo "FAIL: probe results"; cat "$work/QueryProbes.json" "$work/agent.log"; exit 1; }
  sleep 2
done
echo "icmp and tcp probe results present"

# 流量差分依赖 boot_id 在 agent 重启之间不变（规则见 traffic.Book）：agent 停着时在回环上走一批字节，
# 重启后的首次上报必须把它们计入总量。boot_id 若随进程变化，hub 会只重置基线而丢掉这批字节。
# 前提：重启前后上报的是同一组计数器（两轮都只计 lo0），hub 的基线在读 before 时已是这组——
# 所以先起一轮只计 lo0 的 agent，等到它有一份上报落地再停。
stop_child "$agent" agent; agent=""
t=$(date +%s)
"$bin" run --config "$work/agent.json" --net-include lo0 >> "$work/agent.log" 2>&1 &
agent=$!
until_node 30 "(.lastSeenAt | tonumber) > $t"
stop_child "$agent" agent; agent=""
[ "$(rpc GetSnapshot '{}')" = 200 ] || { echo "FAIL: GetSnapshot before the transfer"; exit 1; }
before=$(jq -r '.nodes[0].traffic.totalRx // "0"' "$work/GetSnapshot.json")
seen=$(jq -r '.nodes[0].lastSeenAt' "$work/GetSnapshot.json")
mkdir -p "$work/blob"
dd if=/dev/zero of="$work/blob/64m" bs=1048576 count=64 2> /dev/null
python3 -m http.server "$BLOB_PORT" --bind 127.0.0.1 --directory "$work/blob" > "$work/blob.log" 2>&1 &
blob=$!
i=0
# 就绪前的连接失败是预期的，只在超过上限时看日志；下载本身由 -f 判定，下载量必须是整整 64 MiB。
until got=$(curl -fs -o /dev/null -w '%{size_download}' "http://127.0.0.1:$BLOB_PORT/64m"); do
  i=$((i + 1)); [ "$i" -lt 50 ] || { echo "FAIL: blob server"; cat "$work/blob.log"; exit 1; }
  sleep 0.2
done
[ "$got" = 67108864 ] || { echo "FAIL: downloaded $got bytes over lo0, want 67108864"; exit 1; }
stop_child "$blob" "blob server"; blob=""
rm -rf "$work/blob"
# lastSeenAt 以秒计：墙钟走过 seen 那一秒之后再起新进程，它的第一份上报必然晚于 seen。
while [ "$(date +%s)" -le "$seen" ]; do sleep 0.2; done
"$bin" run --config "$work/agent.json" --net-include lo0 >> "$work/agent.log" 2>&1 &
agent=$!
# 只判新进程的第一份上报：之后的上报会把窗口里其他回环流量也记进来，并发的大流量能掩盖 boot_id 的缺陷。
until_node 30 "(.lastSeenAt | tonumber) > $seen"
after=$(jq -r '.nodes[0].traffic.totalRx' "$work/GetSnapshot.json")
[ "$after" -ge $((before + 67108864)) ] || { echo "FAIL: traffic across agent restart $before -> $after, want at least +67108864"; exit 1; }
echo "traffic across agent restart: $before -> $after"
stop_child "$agent" agent; agent=""

# launchd 用户域：plist 取自发布包，只改 Label、程序与配置的路径（包里的参数已在上面核对），并去掉只对
# system 域有效的 UserName/GroupName；KeepAlive 与 ThrottleInterval 原样保留，验证的就是它们。
# 日志目录事先建好、对本人可写，文件不建，看 launchd 是否创建。
job_plist="$work/$label.plist"
cp "$pkg_plist" "$job_plist"
plutil -replace Label -string "$label" "$job_plist"
plutil -replace ProgramArguments -json "[\"$bin\", \"run\", \"--config\", \"$work/agent.json\"]" "$job_plist"
plutil -remove UserName "$job_plist"
plutil -remove GroupName "$job_plist"
mkdir -p "$work/logs"
plutil -replace StandardOutPath -string "$work/logs/heron-agent.log" "$job_plist"
plutil -replace StandardErrorPath -string "$work/logs/heron-agent.err" "$job_plist"
plutil -lint "$job_plist" > /dev/null
t_job=$(date +%s)
launchctl bootstrap "gui/$uid" "$job_plist"
job=1
job_pid() { launchctl print "gui/$uid/$label" 2> /dev/null | awk '$1 == "pid" && $2 == "=" { print $3; exit }'; }
# wait_new_pid <旧 pid> <秒>：等到作业换成另一个 pid，打印新 pid。
wait_new_pid() {
  deadline=$(($(date +%s) + $2))
  while :; do
    p=$(job_pid)
    if [ -n "$p" ] && [ "$p" != "$1" ]; then echo "$p"; return 0; fi
    [ "$(date +%s)" -lt "$deadline" ] || return 1
    sleep 0.2
  done
}
# wait_exec <pid>：等到拉起的子进程 exec 成 agent（comm 等于 $bin）。exec 之前它是 xpcproxy，
# 由 launchd 以更高的权限派生，本人发的信号会以 Operation not permitted 失败（实测）。
wait_exec() {
  i=0
  until [ "$(ps -o comm= -p "$1" 2> /dev/null)" = "$bin" ]; do
    i=$((i + 1)); [ "$i" -lt 50 ] || { echo "FAIL: pid $1 did not exec $bin"; ps -o uid=,comm= -p "$1"; exit 1; }
    sleep 0.1
  done
}
pid1=$(wait_new_pid none 15) || { echo "FAIL: launchd did not start the job"; launchctl print "gui/$uid/$label"; exit 1; }
# 节点在上一个 agent 停下后仍在 TTL 内显示在线；以作业启动之后的上报时刻为准。
until_node 30 "(.lastSeenAt | tonumber) > $t_job"
# install-macos.sh 按 ps 的 comm 等于 ProgramArguments[0] 认服务进程，这里钉住它的前提。拉起之后、exec 之前
# 子进程的 comm 是 xpcproxy（实测），所以等作业有一份上报落地、必然已 exec 之后再核对。
comm=$(ps -o comm= -p "$pid1") || { echo "FAIL: ps -p $pid1"; exit 1; }
[ "$comm" = "$bin" ] || { echo "FAIL: job comm $comm is not ProgramArguments[0] $bin"; exit 1; }
[ -f "$work/logs/heron-agent.err" ] && [ -f "$work/logs/heron-agent.log" ] || { echo "FAIL: launchd did not create the log files"; ls -l "$work/logs"; exit 1; }
grep -q 'agent starting' "$work/logs/heron-agent.err" || { echo "FAIL: agent log not in StandardErrorPath"; cat "$work/logs/heron-agent.err"; exit 1; }

# hub 停 15 秒：agent 在进程内退避，不能退出让 launchd 拉起（pid 不变）；hub 回来后重新在线。
# 这里分不出退避有没有上限（15 秒停机、30 秒窗口），上限由 internal/agent/client 的测试钉住。
stop_hub
sleep 15
[ "$(job_pid)" = "$pid1" ] || { echo "FAIL: agent process changed while the hub was down"; exit 1; }
start_hub
login
until_node 30 '.online'
[ "$(job_pid)" = "$pid1" ] || { echo "FAIL: agent process changed across the hub outage"; exit 1; }

# 进程被杀：KeepAlive 拉起。第二次在拉起后立刻再杀，下一次拉起受 ThrottleInterval 节流：
# 与上一次拉起相隔约 5 秒（默认值 10 秒会落在区间外）。
kill -9 "$pid1" || { echo "FAIL: cannot kill $pid1"; exit 1; }
pid2=$(wait_new_pid "$pid1" 15) || { echo "FAIL: KeepAlive did not restart the agent"; exit 1; }
t2=$(date +%s)
wait_exec "$pid2"
kill -9 "$pid2" || { echo "FAIL: cannot kill $pid2"; exit 1; }
pid3=$(wait_new_pid "$pid2" 20) || { echo "FAIL: KeepAlive gave up after a quick second exit"; exit 1; }
gap=$(($(date +%s) - t2))
[ "$gap" -ge 3 ] && [ "$gap" -le 8 ] || { echo "FAIL: respawn after a quick exit took ${gap}s, want about 5 (ThrottleInterval)"; exit 1; }
t3=$(date +%s)
until_node 30 "(.lastSeenAt | tonumber) >= $t3"
echo "launchd: pids $pid1 -> $pid2 -> $pid3, throttled respawn ${gap}s"

# 普通用户下每个来源都读得到：采集失败只让字段缺失并记日志（不以失败显形），所以直接查日志。
# 只有退出码 1 是"没有匹配"；0 是有匹配，2 是文件读不到——检查没跑成与通过不能长得一样。
rc=0
grep -h 'partial collection' "$work/agent.log" "$work/logs/heron-agent.err" || rc=$?
case $rc in
  1) ;;
  0) echo "FAIL: collection errors as a normal user"; exit 1;;
  *) echo "FAIL: cannot read the agent logs (grep exit $rc)"; exit 1;;
esac

launchctl bootout "gui/$uid/$label"
job=""
if launchctl print "gui/$uid/$label" > /dev/null 2>&1; then echo "FAIL: job still loaded after bootout"; exit 1; fi
i=0
while kill -0 "$pid3" 2> /dev/null; do
  i=$((i + 1))
  [ "$i" -lt 50 ] || { echo "FAIL: agent survived bootout"; kill -9 "$pid3"; exit 1; }
  sleep 0.2
done
echo "MACOS ACCEPT OK ($arch$amd64_note)"
