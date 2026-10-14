#!/bin/sh
# scripts/release-manifest.sh readback 的替身执行测试（make script-test 调用）：release.yml 的 publish 在建 Release 之后
# 调用它，真实的回读只有发布时才跑得到，这里用 gh、go、docker 替身把它在本机跑一遍。Release 页面上的资产由同一个
# 脚本的 write 子命令从占位产物写出，回读读的就是写入那一侧产出的格式。
#   gh      release download 从 $ASSETS 复制与 -p 相符的文件；一个都不相符时失败（与 gh 相同：只有全部模式都落空才报错）
#   go      run ./scripts/releasesign verify：签名替身是 sha256("heron-release-v1\n" + 版本 + "\n" + SHA256SUMS)，与真实
#           签名同样把版本与清单原文绑在一起（真实的 Ed25519 验签由 internal/releasesig 的测试覆盖）；
#           run ./scripts/boundagent readback：记下参数，核对两个安装脚本已取回
#   docker  buildx imagetools inspect：从 $REGISTRY（每行 "<引用> <digest>"）查版本 tag 当前的索引 digest
# 每个拒绝用例从通过的那份资产出发只改一处；改的若是清单本身就重新签名，失败只能来自被测的那一条核对。
set -eu

repo=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
script=$repo/scripts/release-manifest.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0

bad() {
  echo "FAIL: $*" >&2
  if [ -s "$work/out" ]; then sed 's/^/    /' "$work/out" >&2; fi
  failures=$((failures + 1))
}

mkdir "$work/stub"
cat > "$work/stub/gh" <<'STUB'
#!/bin/sh
printf '%s\n' "gh $*" >> "$CALL_LOG"
[ "$1 $2" = "release download" ] || { echo "stub gh: unexpected $*" >&2; exit 1; }
tag=$3
shift 3
[ "$tag" = "$ASSETS_TAG" ] || { echo "stub gh: release $tag not found" >&2; exit 1; }
patterns=
dir=
while [ $# -gt 0 ]; do
  case $1 in
    -p) patterns="$patterns $2"; shift 2 ;;
    -D) dir=$2; shift 2 ;;
    *) echo "stub gh: unexpected $1" >&2; exit 1 ;;
  esac
done
matched=0
for p in $patterns; do
  if [ -f "$ASSETS/$p" ]; then
    cp "$ASSETS/$p" "$dir/$p"
    matched=1
  fi
done
[ "$matched" = 1 ] || { echo "no assets match the file pattern" >&2; exit 1; }
STUB
cat > "$work/stub/go" <<'STUB'
#!/bin/sh
printf '%s\n' "go $*" >> "$CALL_LOG"
[ "$1" = run ] || { echo "stub go: unexpected $*" >&2; exit 1; }
case "$2 $3" in
  './scripts/releasesign verify')
    [ "$4" = -version ] && [ "$6" = -dir ] || { echo "stub releasesign: unexpected $*" >&2; exit 1; }
    want=$({ printf 'heron-release-v1\n%s\n' "$5"; cat "$7/SHA256SUMS"; } | sha256sum | cut -c 1-64)
    [ "$(cat "$7/SHA256SUMS.sig")" = "$want" ] || { echo "releasesign: signature does not verify" >&2; exit 1; }
    ;;
  './scripts/boundagent readback')
    [ -f "$9/install.sh" ] && [ -f "$9/install-macos.sh" ] || { echo "stub boundagent: installers missing" >&2; exit 1; }
    ;;
  *) echo "stub go: unexpected $*" >&2; exit 1 ;;
esac
STUB
cat > "$work/stub/docker" <<'STUB'
#!/bin/sh
printf '%s\n' "docker $*" >> "$CALL_LOG"
[ "$1 $2 $3 $4 $5" = "buildx imagetools inspect --format {{.Manifest.Digest}}" ] || { echo "stub docker: unexpected $*" >&2; exit 1; }
awk -v r="$6" '$1 == r { print $2; found = 1 } END { exit !found }' "$REGISTRY" || { echo "$6: not found" >&2; exit 1; }
STUB
chmod +x "$work/stub"/*

hub_repo=ghcr.io/xjetry/heron-hub
agent_repo=ghcr.io/xjetry/heron-agent
hex() { printf '%064d' 0 | tr 0 "$1"; }

# fake_sign DIR VERSION：替身签名，见文件开头。
fake_sign() {
  { printf 'heron-release-v1\n%s\n' "$2"; cat "$1/SHA256SUMS"; } | sha256sum | cut -c 1-64 > "$1/SHA256SUMS.sig"
}

# relist DIR NAME：把清单里 NAME 那一行的摘要换成文件现在的摘要，然后重新签名。
relist() {
  sum=$(sha256sum < "$1/$2" | cut -c 1-64)
  awk -v n="$2" -v s="$sum" 'substr($0, 67) == n { $0 = s "  " n } { print }' "$1/SHA256SUMS" > "$1/SHA256SUMS.new"
  mv "$1/SHA256SUMS.new" "$1/SHA256SUMS"
  fake_sign "$1" "$3"
}

# release_assets DIR KIND VERSION AGENT_VERSION：用 write 子命令从占位产物写出一个 release 的全部资产。
release_assets() {
  d=$1
  mkdir -p "$d/dist" "$d/build"
  for f in heron-hub_linux_amd64.tar.gz heron-updater_linux_amd64.tar.gz; do printf '%s\n' "$f" > "$d/dist/$f"; done
  printf '# >>> release stamp >>>\nRELEASE_VERSION=%s\n' "'$3'" > "$d/dist/install-hub.sh"
  for f in install.sh install-macos.sh; do printf '# >>> release stamp >>>\nRELEASE_VERSION=%s\n' "'$4'" > "$d/dist/$f"; done
  printf '%s sha256:%s %s\n' "$3" "$(hex a)" "$4" > "$d/build/hub-built"
  printf '%s sha256:%s %s\n' "$3" "$(hex b)" "$4" > "$d/build/agent-built"
  VERSION=$3 AGENT_VERSION=$4 RECORD=$d/build/record "$script" record "$2"
  VERSION=$3 AGENT_VERSION=$4 RECORD=$d/build/record DIST=$d/dist HUB_REPO=$hub_repo AGENT_REPO=$agent_repo \
    HUB_BUILT=$d/build/hub-built AGENT_BUILT=$d/build/agent-built "$script" write
  fake_sign "$d/dist" "$3"
}

release_assets "$work/full" full v1.2.3 v1.2.3
release_assets "$work/hubonly" hub-only v1.2.4 v1.2.3
printf '%s:v1.2.3 sha256:%s\n%s:v1.2.3 sha256:%s\n%s:v1.2.4 sha256:%s\n' \
  "$hub_repo" "$(hex a)" "$agent_repo" "$(hex b)" "$hub_repo" "$(hex a)" > "$work/registry"

# readback ASSETS KIND VERSION AGENT_VERSION：对 ASSETS 这份 release 跑一次回读，取回的文件放进新目录。
n=0
readback() {
  n=$((n + 1))
  : > "$work/calls"
  rc=0
  (cd "$work" && env PATH="$work/stub:$PATH" CALL_LOG="$work/calls" ASSETS="$1" ASSETS_TAG="$3" REGISTRY="$work/registry" \
    VERSION="$3" AGENT_VERSION="$4" KIND="$2" DIR="$work/readback-$n" HUB_REPO=$hub_repo AGENT_REPO=$agent_repo \
    "$script" readback) > "$work/out" 2>&1 || rc=$?
}

# passes WHAT ASSETS KIND VERSION AGENT_VERSION
passes() {
  what=$1
  shift
  readback "$@"
  if [ "$2" = full ]; then groups='hub agent'; else groups=hub; fi
  [ "$rc" = 0 ] && [ "$(tail -n 1 "$work/out")" = "release $3 read back: signed SHA256SUMS, AGENT_VERSION $4, IMAGES for $groups" ] ||
    bad "readback of $what failed"
}

# rejects WHAT WANT ASSETS KIND VERSION AGENT_VERSION：以非 0 退出，输出含 WANT。
rejects() {
  what=$1
  want=$2
  shift 2
  readback "$@"
  if [ "$rc" = 0 ]; then
    bad "readback accepted $what"
  elif ! grep -qF -- "$want" "$work/out"; then
    bad "readback of $what failed for another reason (want '$want')"
  fi
}

# mutate NAME FROM：从通过的那份资产复制出一个用例，打印它的目录。
mutate() {
  rm -rf "$work/case-$1"
  cp -R "$2" "$work/case-$1"
  printf '%s\n' "$work/case-$1"
}

passes 'a full release' "$work/full/dist" full v1.2.3 v1.2.3
grep -qx 'gh release download v1.2.3 -p SHA256SUMS -p SHA256SUMS.sig -p AGENT_VERSION -p IMAGES -D .*' "$work/calls" ||
  { cp "$work/calls" "$work/out"; bad "readback did not download the four manifest assets"; }
for ref in "$hub_repo:v1.2.3" "$agent_repo:v1.2.3"; do
  grep -qxF "docker buildx imagetools inspect --format {{.Manifest.Digest}} $ref" "$work/calls" ||
    { cp "$work/calls" "$work/out"; bad "readback did not read $ref from the registry"; }
done
! grep -q 'boundagent\|install.sh' "$work/calls" || { cp "$work/calls" "$work/out"; bad "a full release's readback ran the hub-only installer check"; }

passes 'a hub-only release binding an older agent' "$work/hubonly/dist" hub-only v1.2.4 v1.2.3
grep -qx 'go run ./scripts/boundagent readback -version v1.2.4 -bound v1.2.3 -dir .*' "$work/calls" ||
  { cp "$work/calls" "$work/out"; bad "a hub-only release's readback did not check the bound agent's installers"; }
! grep -q "$agent_repo" "$work/calls" || { cp "$work/calls" "$work/out"; bad "a hub-only release's readback read the agent image"; }

# 签名与清单。
d=$(mutate badsig "$work/full/dist")
printf '%s  extra\n' "$(hex 0)" >> "$d/SHA256SUMS"
rejects 'a SHA256SUMS changed after signing' 'signature of v1.2.3' "$d" full v1.2.3 v1.2.3
rejects 'the signature of another version' 'signature of v1.2.4' "$work/full/dist" full v1.2.4 v1.2.4
d=$(mutate noimagesline "$work/full/dist")
grep -v '  IMAGES$' "$d/SHA256SUMS" > "$d/SHA256SUMS.new" && mv "$d/SHA256SUMS.new" "$d/SHA256SUMS" && fake_sign "$d" v1.2.3
rejects 'a SHA256SUMS without the IMAGES line' 'does not list IMAGES' "$d" full v1.2.3 v1.2.3
d=$(mutate noagentline "$work/full/dist")
grep -v '  AGENT_VERSION$' "$d/SHA256SUMS" > "$d/SHA256SUMS.new" && mv "$d/SHA256SUMS.new" "$d/SHA256SUMS" && fake_sign "$d" v1.2.3
rejects 'a SHA256SUMS without the AGENT_VERSION line' 'does not list AGENT_VERSION' "$d" full v1.2.3 v1.2.3
d=$(mutate dupline "$work/full/dist")
grep '  IMAGES$' "$d/SHA256SUMS" > "$work/line" && cat "$work/line" >> "$d/SHA256SUMS" && fake_sign "$d" v1.2.3
rejects 'a SHA256SUMS listing IMAGES twice' 'lists IMAGES more than once' "$d" full v1.2.3 v1.2.3
d=$(mutate malformed "$work/full/dist")
sed 's/  IMAGES$/ IMAGES/' "$d/SHA256SUMS" > "$d/SHA256SUMS.new" && mv "$d/SHA256SUMS.new" "$d/SHA256SUMS" && fake_sign "$d" v1.2.3
rejects 'a SHA256SUMS line with one space' 'not "<64 lowercase hex>  <asset name>"' "$d" full v1.2.3 v1.2.3
d=$(mutate noagentfile "$work/full/dist")
rm "$d/AGENT_VERSION"
rejects 'a release without the AGENT_VERSION asset' 'AGENT_VERSION was not downloaded' "$d" full v1.2.3 v1.2.3

# 两个清单资产的内容。
d=$(mutate imagesbyte "$work/full/dist")
sed 's/heron-hub/heron-hux/' "$d/IMAGES" > "$d/IMAGES.new" && mv "$d/IMAGES.new" "$d/IMAGES"
rejects 'an IMAGES changed by one byte after the listing' 'IMAGES of release v1.2.3 has sha256' "$d" full v1.2.3 v1.2.3
rejects 'an AGENT_VERSION asset that is not the bound version' 'want the bound agent version v1.2.2' "$work/full/dist" full v1.2.3 v1.2.2
d=$(mutate noagentimage "$work/full/dist")
grep -v "^$agent_repo:" "$d/IMAGES" > "$d/IMAGES.new" && mv "$d/IMAGES.new" "$d/IMAGES" && relist "$d" IMAGES v1.2.3
rejects 'a full release whose IMAGES lacks the agent line' 'lacks the agent image' "$d" full v1.2.3 v1.2.3
d=$(mutate dupimage "$work/full/dist")
sed -n 1p "$d/IMAGES" > "$work/line" && cat "$work/line" >> "$d/IMAGES" && relist "$d" IMAGES v1.2.3
rejects 'an IMAGES listing the hub image twice' 'lists the hub image more than once' "$d" full v1.2.3 v1.2.3
d=$(mutate wrongtag "$work/full/dist")
sed "s#^$hub_repo:v1.2.3@#$hub_repo:v1.2.2@#" "$d/IMAGES" > "$d/IMAGES.new" && mv "$d/IMAGES.new" "$d/IMAGES" && relist "$d" IMAGES v1.2.3
rejects 'an IMAGES line tagged with another version' 'is tagged v1.2.2, not the release tag v1.2.3' "$d" full v1.2.3 v1.2.3
d=$(mutate badline "$work/full/dist")
sed "s#@sha256:#@sha512:#" "$d/IMAGES" > "$d/IMAGES.new" && mv "$d/IMAGES.new" "$d/IMAGES" && relist "$d" IMAGES v1.2.3
rejects 'an IMAGES line that is not <repository>:<tag>@sha256:<hex>' 'is not <repository>:<tag>@sha256:<64 lowercase hex>' "$d" full v1.2.3 v1.2.3
d=$(mutate otherrepo "$work/full/dist")
sed "s#^$agent_repo:#ghcr.io/xjetry/heron-other:#" "$d/IMAGES" > "$d/IMAGES.new" && mv "$d/IMAGES.new" "$d/IMAGES" && relist "$d" IMAGES v1.2.3
rejects 'an IMAGES line for an unknown repository' 'which is neither' "$d" full v1.2.3 v1.2.3
d=$(mutate hubonlyagent "$work/hubonly/dist")
printf '%s:v1.2.4@sha256:%s\n' "$agent_repo" "$(hex b)" >> "$d/IMAGES" && relist "$d" IMAGES v1.2.4
rejects 'a hub-only release whose IMAGES lists the agent image' 'which a hub-only release does not publish' "$d" hub-only v1.2.4 v1.2.3

# registry 与回读目录。
cp "$work/registry" "$work/registry.good"
sed "s#^$agent_repo:v1.2.3 .*#$agent_repo:v1.2.3 sha256:$(hex f)#" "$work/registry.good" > "$work/registry"
rejects 'a registry tag that moved away from the listed index' "$agent_repo:v1.2.3 points to sha256:$(hex f)" "$work/full/dist" full v1.2.3 v1.2.3
grep -v "^$agent_repo:v1.2.3 " "$work/registry.good" > "$work/registry"
rejects 'a tag missing from the registry' "cannot read $agent_repo:v1.2.3" "$work/full/dist" full v1.2.3 v1.2.3
cp "$work/registry.good" "$work/registry"
mkdir "$work/readback-$((n + 1))"
rejects 'a readback directory that already exists' 'already exists' "$work/full/dist" full v1.2.3 v1.2.3
rejects 'an unknown release kind' "unknown release kind 'partial'" "$work/full/dist" partial v1.2.3 v1.2.3

if [ "$failures" != 0 ]; then
  echo "release manifest: $failures failure(s)" >&2
  exit 1
fi
echo "release manifest ok"
