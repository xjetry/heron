#!/bin/sh
# 在一次性的 Linux 主机（GitHub 托管运行器）上测 agent 与更新器的常驻内存：measure-agent.sh <发布资产目录> <分钟>
#
# 八组，每组运行 <分钟>（至少 10）、每 60 秒采一次（run.sh + rss.sh）：
#   systemd 形态（install.sh 真装在本机，agent 与常驻更新器）×{T=0, T=64}×{GOMAXPROCS 默认, 1}：装在本机，逐组执行；
#   container 形态（容器里的 agent）×{T=0, T=64}×{GOMAXPROCS 默认, 1}：四组并行，与 systemd 组同时进行。
# 并行的几组互不共享端口与容器名（run.sh 按 RUN 命名）。被测量不依赖 CPU 速度，并行只拉长墙钟，不改变常驻内存
# 的口径；GOMAXPROCS 的默认值由运行时按本机核数取，并行与否不改变它。并行成立的另一个前提是各组只采自己的进程：
# 本机的 /proc 也列出各容器里的进程，rss.sh 只采 run.sh 按身份给出的 pid（systemd 单元的 MainPID），不按进程名找。
# 产物在 build/wirebench/runs/<组名>/，另有 runner-env.txt 记主机事实。任一组失败，脚本在全部结束后以非零退出。
set -eu
[ "$#" = 2 ] || { echo "usage: measure-agent.sh <release-assets-dir> <minutes>" >&2; exit 2; }
dist=$1; minutes=$2
case "$minutes" in ''|*[!0-9]*) echo "minutes must be a whole number" >&2; exit 2;; esac
[ "$minutes" -ge 10 ] || { echo "minutes must be at least 10" >&2; exit 2; }
cd "$(cd "$(dirname "$0")/../.." && pwd)"
# 提交号只能描述这棵树自己（provenance.sh）；run.sh 对同一棵树再做同样的检查。
. scripts/wirebench/provenance.sh
tree_check .
commit_line=$(tree_commit)
secs=$((minutes * 60))
runs=build/wirebench/runs
mkdir -p "$runs"
version=$(sed -n 1p AGENT_VERSION)

{
  echo "date_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ) $commit_line agent_assets=$dist container_agent_version=$version"
  uname -a
  echo "nproc=$(nproc) pagesize=$(getconf PAGESIZE)"
  lscpu
  free -m
  cat /etc/os-release
  systemctl --version | head -n 1
  docker version --format 'docker {{.Server.Version}} {{.Server.Os}}/{{.Server.Arch}}'
  go version
} > "$runs/runner-env.txt" 2>&1

# one <组名> <形态> <任务数> <GOMAXPROCS 或空> <管理端口>
one() {
  name=$1; agent=$2; tasks=$3; gmp=$4; port=$5
  rc=0
  RUN=$name AGENT=$agent AGENT_GOMAXPROCS=$gmp RSS_EVERY=60 ADMIN_PORT=$port DIST=$dist VERSION=$version PHASES="$tasks:$secs" \
    scripts/wirebench/run.sh > "$runs/$name.log" 2>&1 || rc=$?
  echo "$name exit $rc"
  return "$rc"
}

pids=""
one ctr-t0 container 0 "" 18600 & pids="$pids $!"
one ctr-t64 container 64 "" 18603 & pids="$pids $!"
one ctr-t0-p1 container 0 1 18606 & pids="$pids $!"
one ctr-t64-p1 container 64 1 18609 & pids="$pids $!"
failed=0
for spec in "systemd-t0 0 ''" "systemd-t64 64 ''" "systemd-t0-p1 0 1" "systemd-t64-p1 64 1"; do
  eval "set -- $spec"
  one "$1" systemd "$2" "$3" 18612 || failed=1
done
for pid in $pids; do
  wait "$pid" || failed=1
done
exit "$failed"
