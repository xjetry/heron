#!/bin/sh
# 安装固定版本的 shellcheck：Makefile 的 $(SHELLCHECK) 文件目标调用它，版本、安装路径与各平台的 SHA-256 都由
# Makefile 给出，这里不另记一份。
#   scripts/shellcheck-install.sh <版本> <安装路径> <发布包平台名>=<SHA-256> ...
#
# 不变式：安装路径上的文件存在，当且仅当它是该版本官方发布包里的二进制，且发布包与 Makefile 记的 SHA-256 相符。
# make 只凭文件存在判定"已安装"、此后不再校验，所以下载、校验、解包都在安装目录下的临时目录里做，全部通过后才
# mv 到安装路径（同一文件系统内的改名）；任何一步失败都连同下载物删掉临时目录并以非 0 退出，安装路径上不会留下
# 半成品或未经校验的文件。
set -eu

if [ $# -lt 3 ]; then
  echo "usage: $0 <version> <dest> <platform>=<sha256> ..." >&2
  exit 2
fi
version=$1
dest=$2
shift 2

# 发布包按 <os>.<arch> 命名（shellcheck-v0.11.0.linux.aarch64.tar.gz）。这里只把 uname 的写法换成发布包的写法；
# 支持哪些平台只由 Makefile 的 SHA-256 清单决定，清单里没有的平台在下面报错。校验工具按操作系统选：Linux 发行版
# 带 coreutils 的 sha256sum，macOS 自带 shasum。
os=$(uname -s)
case $os in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
esac
arch=$(uname -m)
case $arch in
  arm64 | aarch64) arch=aarch64 ;;
  amd64) arch=x86_64 ;;
esac
platform=$os.$arch

want=
supported=
for entry in "$@"; do
  supported="$supported ${entry%%=*}"
  if [ "${entry%%=*}" = "$platform" ]; then
    want=${entry#*=}
  fi
done
if [ -z "$want" ]; then
  echo "shellcheck $version: no pinned release for this host ($(uname -s) $(uname -m) -> $platform); supported:$supported (SHELLCHECK_SHA256 in Makefile)" >&2
  exit 1
fi
case $os in
  linux) sha256() { sha256sum "$1"; } ;;
  darwin) sha256() { shasum -a 256 "$1"; } ;;
  *)
    echo "shellcheck $version: no SHA-256 tool known for $os" >&2
    exit 1
    ;;
esac

dir=$(dirname -- "$dest")
mkdir -p -- "$dir"
tmp=$(mktemp -d "$dir/.install.XXXXXX")
trap 'rm -rf -- "$tmp"' EXIT
trap 'exit 1' HUP INT TERM

archive=shellcheck-$version.$platform.tar.gz
url=https://github.com/koalaman/shellcheck/releases/download/$version/$archive
echo "shellcheck: downloading $url"
curl -fsSL --retry 3 -o "$tmp/$archive" "$url"
got=$(sha256 "$tmp/$archive")
got=${got%% *}
if [ "$got" != "$want" ]; then
  echo "shellcheck: SHA-256 mismatch for $archive: got $got, want $want (SHELLCHECK_SHA256 in Makefile); download discarded" >&2
  exit 1
fi
tar -xzf "$tmp/$archive" -C "$tmp"
bin=$tmp/shellcheck-$version/shellcheck
# SHA-256 只证明拿到的是记录 digest 时的那份字节，不证明那份字节就是这个版本：再核对二进制自报的版本，
# 并把它打印在日志里，CI 日志因此写明 lint 用的是哪一版。
out=$("$bin" --version)
printf '%s\n' "$out"
case $out in
  *"version: ${version#v}"*) ;;
  *)
    echo "shellcheck: $archive does not report version ${version#v}" >&2
    exit 1
    ;;
esac
mv -f -- "$bin" "$dest"
echo "shellcheck: installed $dest"
