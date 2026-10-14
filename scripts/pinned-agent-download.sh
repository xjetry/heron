#!/bin/sh
# 下载哪一版已发布 agent 只由 AGENT_PIN 决定：它是清单文件的路径，tag 与摘要来自已验签的 SHA256SUMS。
# 未设置就失败，不回退到脚本旁边的任何文件；否则漏传清单会被旁边碰巧存在的文件当成通过。
set -eu
[ "$#" = 1 ] || { echo "usage: $0 NEW_OUTPUT_DIRECTORY" >&2; exit 1; }
if [ -z "${AGENT_PIN:-}" ]; then
  echo "FAIL: AGENT_PIN is required" >&2
  exit 1
fi
pin=$AGENT_PIN
# 清单只来自 boundagent pin，而 pin 只接受正式版，所以 tag 不接受预发布后缀。清单只有 repository、tag、assets。
jq -e '
  .repository == "xjetry/heron" and
  (.tag | type == "string" and test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) and
  ([.assets[].arch] | sort) == ["amd64", "arm64"] and
  all(.assets[]; .sha256 | type == "string" and test("^[a-f0-9]{64}$"))
' "$pin" > /dev/null || { echo "FAIL: invalid or absent published-agent pin" >&2; exit 1; }
tag=$(jq -r .tag "$pin")
repository=$(jq -r .repository "$pin")
[ ! -e "$1" ] || { echo "FAIL: output directory already exists: $1" >&2; exit 1; }
mkdir -p "$1"
out=$(cd "$1" && pwd)
complete=false
cleanup() {
  if [ "$complete" = false ]; then rm -rf "$out"; fi
}
trap cleanup EXIT
trap 'exit 1' INT TERM HUP
echo "published agent: $repository $tag"
for arch in amd64 arm64; do
  asset="heron-agent_linux_$arch.tar.gz"
  expected=$(jq -r --arg arch "$arch" '.assets[] | select(.arch == $arch) | .sha256' "$pin")
  curl --fail --show-error --silent --location --retry 3 --connect-timeout 15 --max-time 180 \
    "https://github.com/$repository/releases/download/$tag/$asset" -o "$out/$asset"
  actual=$(sha256sum "$out/$asset")
  actual=${actual%% *}
  [ "$actual" = "$expected" ] || { echo "FAIL: SHA256 mismatch for $tag/$asset: $actual, expected $expected" >&2; exit 1; }
  # 校验成功之后才解包，且只取固定名称；其它包内路径不能写入输出目录。
  tar -xzf "$out/$asset" -C "$out" heron-agent
  if ! { [ -f "$out/heron-agent" ] && [ ! -L "$out/heron-agent" ]; }; then echo "FAIL: $asset has no regular heron-agent" >&2; exit 1; fi
  mv "$out/heron-agent" "$out/heron-agent-linux-$arch"
  chmod +x "$out/heron-agent-linux-$arch"
  rm "$out/$asset"
  echo "verified $tag/$asset sha256:$expected"
done
complete=true
