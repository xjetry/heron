#!/bin/sh
# 按固定间隔采样给定进程的常驻内存：rss.sh <间隔秒> <pid>...
# 读 /proc/<pid>/status 的 VmRSS（当前常驻）、VmHWM（常驻峰值）、RssAnon（匿名页，Go 堆与栈在这里）、
# RssFile（映射的文件页，主要是二进制本身被读入的部分）、RssShmem，单位 kB；每个采样点对每个 pid 写一行。
# 开头一行写环境事实：核数、内核、页大小。
# 被测进程由调用方按身份给出（systemd 单元的 MainPID、容器里 agent 自己写下的 pid），不按进程名在 /proc 里找：
# 宿主上的 /proc 也列出各容器里的进程，measure-agent.sh 让容器组与 systemd 组同时运行，按名字找会把别组的
# 同名 agent 算进来。某个 pid 消失或成了僵尸（已退出、父进程还没回收，status 里没有内存行）时写一行注释并不再采它
# （进程重启即换了 pid，采样不跟到新进程，由注释看出）；全部退出后结束。
# 只用 POSIX sh 与 /proc：被测环境（精简容器、OrbStack 机器、CI 运行器）不一定有 procps。
set -eu
[ "$#" -ge 2 ] || { echo "usage: rss.sh <interval-seconds> <pid>..." >&2; exit 2; }
every=$1; shift
for pid in "$@"; do
  case "$pid" in ''|0|*[!0-9]*) echo "rss.sh: not a pid: '$pid'" >&2; exit 2;; esac
  [ -r "/proc/$pid/status" ] || { echo "rss.sh: no process $pid" >&2; exit 1; }
done
echo "# nproc=$(nproc 2>/dev/null || echo ?) kernel=$(uname -r) machine=$(uname -m) pagesize=$(getconf PAGESIZE)"
echo "# time_utc comm pid uptime_s VmRSS VmHWM RssAnon RssFile RssShmem Threads"
live=$*
while [ -n "$live" ]; do
  ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  next=""
  for pid in $live; do
    d=/proc/$pid
    # 进程启动时刻（第 22 个字段，单位时钟滴答）换算出已运行秒数：采样点相对进程启动的时刻可比。
    if c=$(cat "$d/comm" 2>/dev/null) && start=$(cut -d' ' -f22 "$d/stat" 2>/dev/null) &&
      line=$(awk -v ts="$ts" -v c="$c" -v pid="$pid" -v up="$(cut -d' ' -f1 /proc/uptime)" -v s="$start" -v h="$(getconf CLK_TCK)" '
        /^State:/ {st=$2} /^VmRSS:/ {r=$2} /^VmHWM:/ {hw=$2} /^RssAnon:/ {a=$2} /^RssFile:/ {f=$2} /^RssShmem:/ {sh=$2} /^Threads:/ {t=$2}
        END { if (st == "Z" || r == "") exit 1; printf "%s %s %s %d %s %s %s %s %s %s\n", ts, c, pid, up - s / h, r, hw, a, f, sh, t }' "$d/status" 2>/dev/null); then
      echo "$line"
      next="$next $pid"
    else
      echo "# $ts pid $pid exited"
    fi
  done
  live=${next# }
  [ -z "$live" ] || sleep "$every"
done
