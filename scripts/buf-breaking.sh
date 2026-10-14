#!/bin/sh
# make lint 的 buf breaking 入口。REF 是对照的 git ref；none 来自 release-breaking-base.sh，表示没有已发布的 proto
# 可对照，门禁这一次不生效。放行面因此必须收窄：收到 none 时对 HEAD 复核同一判定，HEAD 之前已有发布 tag 就失败，
# none 不能在第一个发布之外被用来跳过检查。
set -eu
[ "$#" = 1 ] || { echo "usage: $0 REF" >&2; exit 2; }
if [ "$1" = none ]; then
  base=$("$(dirname "$0")/release-breaking-base.sh" HEAD)
  [ "$base" = none ] || { echo "buf-breaking: REF=none but HEAD follows release $base" >&2; exit 1; }
  echo "buf breaking: skipped, no release precedes HEAD"
  exit 0
fi
exec buf breaking --against ".git#ref=$1"
