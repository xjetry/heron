#!/bin/sh
# agent 与更新器的发布体积（linux/amd64，链接 -s -w：agent 约 8.5 MB，常驻更新器约 7.8 MB）：sizes.sh <正式产物目录> <本树产物目录> <工作目录>
#
# 两个产物目录都是一套发布资产：正式的一套用 `gh release download <tag>` 取得（带 SHA256SUMS），本树的一套用
# `make release-full VERSION=<tag> AGENT_VERSION=<tag>` 构建后从 dist/ 复制（只有 tar 包与安装脚本：SHA256SUMS 由
# make release-manifest 在镜像构建之后生成，量体积不需要镜像）。先按 SHA256SUMS 核对正式的一套下载完整，再逐包给出
# tar.gz 字节、解包后二进制字节与两套二进制是否逐字节相同，最后对每个 agent 二进制按模块
# 归并符号（wirebench sizes）。以 -s -w 链接的发布产物没有符号表，wirebench sizes 对它明说并只给段大小；按模块的
# 表要对同一提交与同一 Go 版本、去掉这两个参数的构建另跑（加载段相同，见 sizes.go）。工作目录放解包结果，调用方给一个被
# .gitignore 覆盖的目录。
set -eu
[ "$#" = 3 ] || { echo "usage: sizes.sh <official-assets-dir> <tree-assets-dir> <work-dir>" >&2; exit 2; }
official=$(cd "$1" && pwd)
tree=$(cd "$2" && pwd)
mkdir -p "$3"
work=$(cd "$3" && pwd)
cd "$(cd "$(dirname "$0")/../.." && pwd)"

# 正式的一套按它的 SHA256SUMS 核对；sha256sum 与 shasum 二选一（Linux 运行器与 macOS）。本树的一套是本机刚构建的
# 产物，没有可核对的清单。
if command -v sha256sum > /dev/null 2>&1; then
  (cd "$official" && sha256sum -c --ignore-missing SHA256SUMS > /dev/null)
else
  (cd "$official" && shasum -a 256 -c SHA256SUMS > /dev/null)
fi
echo "checksums: the official asset set matches its SHA256SUMS"

go build -o "$work/wirebench" ./scripts/wirebench
size_of() { wc -c < "$1" | tr -d ' '; }

echo "| package | tar.gz official | tar.gz tree | binary official | binary tree | binaries identical |"
echo "|---|---:|---:|---:|---:|---|"
for tgz in "$official"/heron-agent_*.tar.gz "$official"/heron-updater_*.tar.gz; do
  pkg=$(basename "$tgz" .tar.gz)
  case "$pkg" in heron-agent_*) bin=heron-agent;; *) bin=heron-updater;; esac
  for side in official tree; do
    if [ "$side" = official ]; then src=$official; else src=$tree; fi
    rm -rf "${work:?}/$side/$pkg"
    mkdir -p "$work/$side/$pkg"
    tar -xzf "$src/$pkg.tar.gz" -C "$work/$side/$pkg"
  done
  a=$work/official/$pkg/$bin
  b=$work/tree/$pkg/$bin
  same=no
  if cmp -s "$a" "$b"; then same=yes; fi
  echo "| $pkg | $(size_of "$tgz") | $(size_of "$tree/$pkg.tar.gz") | $(size_of "$a") | $(size_of "$b") | $same |"
done

for pkg in heron-agent_linux_amd64 heron-agent_linux_arm64 heron-agent_darwin_arm64; do
  echo
  "$work/wirebench" sizes -bin "$work/official/$pkg/heron-agent"
done
