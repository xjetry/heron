#!/bin/sh
# 按固定间隔采样 macOS 上一个进程的内存：rss-darwin.sh <间隔秒> <pid>
# 两种口径各记一列，单位字节：
#   rss        ps -o rss（常驻集，KiB 换算成字节）：含共享的、可回收的干净页（__TEXT 等映射的二进制），与 Linux 的
#              VmRSS 同类；
#   footprint  footprint 的 Footprint 合计（脏页与压缩页，即进程独占、不能直接丢弃的内存），与它的 Auxiliary 里的
#              phys_footprint（内核记账的同一量，含页表等）各一列。macOS 内存压力与 Activity Monitor 的"内存"
#              列看的是后者。
# 进程退出后停止。开头一行写环境事实。
set -eu
[ "$#" = 2 ] || { echo "usage: rss-darwin.sh <interval-seconds> <pid>" >&2; exit 2; }
every=$1; pid=$2
echo "# ncpu=$(sysctl -n hw.ncpu) pagesize=$(sysctl -n vm.pagesize) macos=$(sw_vers -productVersion) kernel=$(uname -r)"
echo "# time_utc pid elapsed rss_bytes footprint_bytes phys_footprint_bytes"
while kill -0 "$pid" 2> /dev/null; do
  ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  rss_kb=$(ps -o rss= -p "$pid" | tr -d ' ')
  elapsed=$(ps -o etime= -p "$pid" | tr -d ' ')
  fp=$(footprint -p "$pid" -f bytes 2> /dev/null | awk '
    /Footprint:/ { for (i = 1; i <= NF; i++) if ($i == "Footprint:") f = $(i + 1) }
    /phys_footprint:/ { p = $2 }
    END { print f, p }')
  [ -n "$rss_kb" ] || break
  echo "$ts $pid $elapsed $((rss_kb * 1024)) $fp"
  sleep "$every"
done
