#!/bin/sh
# 打印 VERSION 做 buf breaking 的基线：VERSION 的第一父提交及其祖先里最近的发布 tag（v 开头；只发 hub 与预发布都算
# 发布）。那里没有任何发布 tag 时打印 none：VERSION 是这个仓库的第一个发布，此前没有发出去的 proto，没有可对照的对象。
# 只有这一种情形打印 none——VERSION 解析不到提交、git 出错都照常失败，不能被当作首发而跳过门禁；buf-breaking.sh
# 收到 none 时调用本脚本复核，判定只在这里一处。
set -eu
[ "$#" = 1 ] || { echo "usage: $0 VERSION" >&2; exit 2; }
commit=$(git rev-parse --verify --quiet "$1^{commit}") || { echo "release-breaking-base: $1 does not name a commit" >&2; exit 1; }
# 根提交没有父提交，也就没有先于它的发布。
parent=$(git rev-parse --verify --quiet "$commit^") || { echo none; exit 0; }
tags=$(git tag --list 'v*' --merged "$parent") || exit 1
if [ -z "$tags" ]; then
  echo none
  exit 0
fi
git describe --tags --abbrev=0 --match 'v*' "$parent"
