#!/bin/sh
# 从一个 Linux 容器抓取采集层要读的文件，作为解析测试的 fixture。
# /proc 下的文件 stat 出来大小为 0，tar 直接打包会得到空文件，所以先 cat 成普通文件。
set -eu
name=${1:?usage: capture-proc.sh <fixture-name> [image]}
image=${2:-debian:bookworm-slim}
dest="$(cd "$(dirname "$0")/.." && pwd)/internal/agent/collect/testdata/$name"
mkdir -p "$dest"
docker run --rm "$image" sh -c '
  set -e
  out=/tmp/fixture
  for f in proc/stat proc/meminfo proc/loadavg proc/uptime proc/net/sockstat proc/net/sockstat6 \
           proc/sys/kernel/random/boot_id proc/sys/kernel/osrelease proc/sys/kernel/hostname proc/cpuinfo \
           proc/1/environ etc/os-release; do
    mkdir -p "$out/$(dirname "$f")"
    cat "/$f" > "$out/$f" 2>/dev/null || true
  done
  # 进程数按 /proc 下的进程目录计。git 不存空目录，每个进程带上它的 comm 让目录留在 fixture 里；
  # 列出之后、读 comm 之前退出的进程读不到 comm，不记。
  for d in /proc/[0-9]*; do
    if c=$(cat "$d/comm" 2>/dev/null); then
      mkdir -p "$out$d"
      printf "%s\n" "$c" > "$out$d/comm"
    fi
  done
  for i in /sys/class/net/*; do
    n=$(basename "$i")
    mkdir -p "$out/sys/class/net/$n/statistics"
    cat "$i/statistics/rx_bytes" > "$out/sys/class/net/$n/statistics/rx_bytes"
    cat "$i/statistics/tx_bytes" > "$out/sys/class/net/$n/statistics/tx_bytes"
  done
  tar -c -C "$out" .
' | tar -x -C "$dest"
echo "captured into $dest"
find "$dest" -type f | sort
