#!/bin/sh
# 替身执行测试（make ci 调用）：在最小副本里用 go/pnpm/shellcheck 替身真正执行两个发布目标与 release-manifest，
# 核对产出的资产集合——这是两种 release 的产物分组与清单资产（AGENT_VERSION、IMAGES、SHA256SUMS）
# 唯一被执行验证的地方；发布规则测试只看 make -n。同时核对两个目标用 Makefile 固定的那个 shellcheck 检查写入后的
# 安装脚本，而不是 PATH 上的 shellcheck；以及消费发布目标产物的 scripts/install-accept.sh 的准备路径真的走得通。
# 真正的构建产物由 make release-full 与基点树的对照覆盖，这里不构建，替身产物是占位字节；镜像构建记录是手写的。
set -eu

repo=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0

bad() {
  echo "FAIL: $*" >&2
  if [ -s "$work/out" ]; then sed 's/^/    /' "$work/out" >&2; fi
  failures=$((failures + 1))
}

# 最小副本：Makefile、AGENT_VERSION、deploy/ 整个目录与配方调用的清单脚本，外加被测的 install-accept.sh。配方只把
# ./cmd/... 交给 go，替身不需要源码。
mkdir "$work/stub" "$work/repo" "$work/repo/scripts"
cp "$repo/Makefile" "$repo/AGENT_VERSION" "$work/repo/"
cp -R "$repo/deploy" "$work/repo/deploy"
cp "$repo/scripts/release-manifest.sh" "$repo/scripts/install-accept.sh" "$repo/scripts/web-assets-check.sh" "$work/repo/scripts/"
# 安装脚本写入区的开始行（deploy/releasestamp 的 Begin）。替身按真实写入的形状在它后面写 RELEASE_VERSION='<版本>'，
# release-manifest 读的就是这一行。
stamp_begin='# >>> release stamp >>>'
grep -qxF "$stamp_begin" "$repo/deploy/install-hub.sh" || { echo "FAIL: deploy/install-hub.sh has no '$stamp_begin' line" >&2; exit 1; }

# go 替身：构建写占位文件，脚本子命令按各自语义回应；每次调用追加到调用日志。pnpm 退出 0。
# stampinstall 与 boundagent fetch 写出的脚本都带写入区：前者是源码加上本版版本号，后者是绑定版本 release 的副本。
# tar、cp、mkdir、rm、sha256sum 用真的：替身 go 只替 go，打包与清单仍走真实路径。
cat > "$work/stub/go" <<'STUB'
#!/bin/sh
# shellcheck disable=SC2016 # 记录的是 make 展开前的字面值
printf '%s\n' "go $*" >> "$GO_CALL_LOG"
case $1 in
  build)
    out=
    prev=
    for a in "$@"; do
      if [ "$prev" = "-o" ]; then out=$a; fi
      prev=$a
    done
    if [ -z "$out" ]; then echo "stub go: no -o in: $*" >&2; exit 1; fi
    printf 'stub binary\n' > "$out"
    ;;
  run)
    shift
    cmd=$1
    shift
    case $cmd in
      ./scripts/releasekind)
        printf '%s\n' "$STUB_KIND"
        ;;
      ./scripts/stampinstall)
        version=
        dir=
        while [ $# -gt 0 ]; do
          case $1 in
            -version) version=$2; shift 2 ;;
            -dir) dir=$2; shift 2 ;;
            -*) echo "stub stampinstall: unexpected $1" >&2; exit 1 ;;
            *)
              awk -v b="$STAMP_BEGIN" -v v="$version" -v q="'" '{ print } $0 == b { print "RELEASE_VERSION=" q v q }' "$1" > "$dir/$(basename "$1")"
              shift
              ;;
          esac
        done
        ;;
      ./scripts/boundagent)
        [ "$1" = fetch ] && shift
        version=
        dir=
        while [ $# -gt 0 ]; do
          case $1 in
            -version) version=$2; shift 2 ;;
            -dir) dir=$2; shift 2 ;;
            -linux-arches | -darwin-arches) shift 2 ;;
            *) echo "stub boundagent: unexpected $1" >&2; exit 1 ;;
          esac
        done
        printf '%s\nRELEASE_VERSION=%s\n' "$STAMP_BEGIN" "'$version'" > "$dir/install.sh"
        printf '%s\nRELEASE_VERSION=%s\n' "$STAMP_BEGIN" "'$version'" > "$dir/install-macos.sh"
        ;;
      ./scripts/agentinputs | ./scripts/checkstatic) ;;
      *) echo "stub go: unexpected go run $cmd" >&2; exit 1 ;;
    esac
    ;;
  *) echo "stub go: unexpected args: $*" >&2; exit 1 ;;
esac
STUB
# pnpm 替身：`run build` 只建出两个空的产物目录——make web 在构建后用 scripts/web-assets-check.sh 核对产物，
# 它要求目录存在；空目录里没有 data: 字体，核对通过。真实的构建与核对由 make ci 的 web 目标覆盖。
cat > "$work/stub/pnpm" <<'STUB'
#!/bin/sh
case "$*" in
  *"run build"*) mkdir -p internal/hub/web/dist internal/hub/web/dist-public ;;
esac
exit 0
STUB
# PATH 上的 shellcheck 是绊线：Makefile 只许调用固定版本的 $(SHELLCHECK)，PATH 上这个一旦被调用就留下记录并失败。
# shellcheck disable=SC2016 # 写进替身的是运行时才展开的引用
printf '#!/bin/sh\necho "PATH shellcheck $*" >> "$SHELLCHECK_TRIPWIRE"\nexit 1\n' > "$work/stub/shellcheck"
chmod +x "$work/stub"/*

# 固定版本的 shellcheck 替身放在 Makefile 给出的安装路径上：文件已存在，配方里的 $(MAKE) $(SHELLCHECK) 什么都不做，
# 不下载（副本里也没有安装脚本，路径若对不上，安装那一步就会失败）。替身记下参数，并要求每个被检查的文件都已存在：
# 它检查的必须是写入版本号之后、实际发布的那几个文件。
sc=$(cd "$work/repo" && make --no-print-directory -s shellcheck-path)
case $sc in
  build/tools/*/shellcheck) ;;
  *) echo "FAIL: make shellcheck-path printed '$sc'" >&2; exit 1 ;;
esac
mkdir -p "$work/repo/$(dirname -- "$sc")"
cat > "$work/repo/$sc" <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >> "$SHELLCHECK_CALL_LOG"
for f in "$@"; do
  case $f in
    -*) ;;
    *) [ "$f" = sh ] || [ -f "$f" ] || { echo "stub shellcheck: $f does not exist" >&2; exit 1; } ;;
  esac
done
STUB
chmod +x "$work/repo/$sc"
: > "$work/sc-tripwire"

# checks_installers WANT：这一次发布目标恰好调用固定版本的 shellcheck 一次，参数为 WANT。
checks_installers() {
  if [ "$(cat "$work/sc-calls")" != "$1" ]; then
    cp "$work/sc-calls" "$work/out"
    bad "release target did not run the pinned shellcheck exactly once with: $1"
  fi
}

# stub_make NAME=VALUE… make …：在替身 PATH 下运行；调用日志与绊线文件在 $work 下。
stub_make() {
  env PATH="$work/stub:$PATH" MAKE="$(command -v make)" GO_CALL_LOG="$work/go-calls" \
    SHELLCHECK_CALL_LOG="$work/sc-calls" SHELLCHECK_TRIPWIRE="$work/sc-tripwire" STAMP_BEGIN="$stamp_begin" "$@"
}

# dist_set：dist 里的非隐藏文件，排好序。
dist_set() {
  # shellcheck disable=SC2012 # 与期望清单逐字比对的是目录里的非隐藏文件，正是 ls 的输出
  (cd dist && ls | sort)
}

# expect_set WHAT WANT…：dist 恰为 WANT。
expect_set() {
  what=$1
  shift
  want=$(printf '%s\n' "$@" | sort)
  got=$(dist_set)
  if [ "$got" != "$want" ]; then
    bad "$what produced a wrong asset set:
$(printf '%s\n' "$got" | sed 's/^/    got /')
$(printf '%s\n' "$want" | sed 's/^/    want /')"
  fi
}

# write_built GROUP VERSION HEXCHAR AGENT_VERSION：手写镜像构建记录，格式同 scripts/docker-image.sh 的 check
# （"<VERSION> <索引 digest> <make docker 收到的 AGENT_VERSION>"）。
write_built() {
  mkdir -p "build/image-archive/$1"
  printf '%s sha256:%s %s\n' "$2" "$(printf '%064d' 0 | tr 0 "$3")" "$4" > "build/image-archive/$1/built"
}

# manifest_fails WHY WANT make-args…：release-manifest 以非 0 退出、输出含 WANT，dist 里不留任何清单。
manifest_fails() {
  why=$1
  want=$2
  shift 2
  rc=0
  stub_make make release-manifest "$@" > "$work/out" 2>&1 || rc=$?
  if [ "$rc" = 0 ]; then
    bad "release-manifest accepted $why"
  elif ! grep -qF -- "$want" "$work/out"; then
    bad "release-manifest with $why failed for another reason (want '$want')"
  fi
  # 整份与半份都不留：release_clean 不认识 .tmp，留下的半份文件会一直在 dist 里。
  for f in SHA256SUMS IMAGES AGENT_VERSION IMAGES.tmp SHA256SUMS.tmp; do
    [ ! -e "dist/$f" ] || bad "release-manifest left dist/$f behind after failing on $why"
  done
}

# sums_cover_dist：SHA256SUMS 恰好列出 dist 里其余全部文件，且每一行都与文件内容相符——它是在其余文件都写完
# 之后生成的。
sums_cover_dist() {
  listed=$(awk '{ print $2 }' dist/SHA256SUMS | sort)
  # shellcheck disable=SC2010 # 比对的是目录里的非隐藏文件
  others=$(cd dist && ls | grep -vx SHA256SUMS | sort)
  [ "$listed" = "$others" ] || bad "$1: SHA256SUMS does not list exactly the other files"
  (cd dist && sha256sum -c SHA256SUMS > "$work/sums-check" 2>&1) || { cp "$work/sums-check" "$work/out"; bad "$1: SHA256SUMS does not match the files in dist"; }
}

hex_a=$(printf 'a%.0s' $(seq 64))
hex_b=$(printf 'b%.0s' $(seq 64))
hex_c=$(printf 'c%.0s' $(seq 64))
repo_agent_version=$(cat "$work/repo/AGENT_VERSION")
cd "$work/repo"

# 用例 1：完整 release，AGENT_VERSION 由命令行覆盖为 v0.0.0-check（与仓库文件不同）。release-full 产出 14 个 tar 包与
# 三个安装脚本，不再生成 SHA256SUMS；tar 成员与旧配方一致；调用日志里没有 agentinputs、boundagent；三个写入后的
# 安装脚本过固定版本的 shellcheck；构建记录写着本次的两个版本与种类。
[ "$repo_agent_version" != v0.0.0-check ] || { echo "FAIL: the repository AGENT_VERSION is v0.0.0-check, the override case proves nothing" >&2; exit 1; }
: > "$work/go-calls"
: > "$work/sc-calls"
stub_make STUB_KIND=full make release-full VERSION=v0.0.0-check AGENT_VERSION=v0.0.0-check > "$work/out" 2>&1 || bad "make release-full failed (stub)"
checks_installers '-s sh dist/install.sh dist/install-hub.sh dist/install-macos.sh'
full_assets='heron-agent_darwin_amd64.tar.gz heron-agent_darwin_arm64.tar.gz
  heron-agent_linux_386.tar.gz heron-agent_linux_amd64.tar.gz heron-agent_linux_arm64.tar.gz
  heron-agent_linux_armv7.tar.gz heron-agent_linux_riscv64.tar.gz
  heron-hub_linux_amd64.tar.gz heron-hub_linux_arm64.tar.gz
  heron-updater_linux_386.tar.gz heron-updater_linux_amd64.tar.gz heron-updater_linux_arm64.tar.gz
  heron-updater_linux_armv7.tar.gz heron-updater_linux_riscv64.tar.gz
  install-hub.sh install-macos.sh install.sh'
# shellcheck disable=SC2086 # 资产名按词拆开
expect_set release-full $full_assets
printf 'VERSION=v0.0.0-check\nAGENT_VERSION=v0.0.0-check\nKIND=full\n' | cmp -s - build/release-record ||
  bad "release-full wrote a wrong build record: $(cat build/release-record 2>&1)"
for arch in amd64 arm64 armv7 386 riscv64; do
  members=$(tar -tzf "dist/heron-agent_linux_$arch.tar.gz" | sort)
  [ "$members" = "$(printf 'heron-agent\nheron-agent.openrc\nheron-agent.service\n' | sort)" ] ||
    bad "heron-agent_linux_$arch.tar.gz members: $members"
  members=$(tar -tzf "dist/heron-updater_linux_$arch.tar.gz" | sort)
  [ "$members" = "$(printf 'heron-updater\nheron-updater-agent.service\nheron-updater-hub.service\n' | sort)" ] ||
    bad "heron-updater_linux_$arch.tar.gz members: $members"
done
for arch in amd64 arm64; do
  members=$(tar -tzf "dist/heron-agent_darwin_$arch.tar.gz" | sort)
  [ "$members" = "$(printf 'heron-agent\nxyz.heron.agent.plist\n' | sort)" ] ||
    bad "heron-agent_darwin_$arch.tar.gz members: $members"
  members=$(tar -tzf "dist/heron-hub_linux_$arch.tar.gz" | sort)
  [ "$members" = "$(printf 'heron-hub\nheron-hub.service\n' | sort)" ] ||
    bad "heron-hub_linux_$arch.tar.gz members: $members"
done
if grep -q 'agentinputs\|boundagent' "$work/go-calls"; then
  cp "$work/go-calls" "$work/out"
  bad "release-full ran a hub-only step"
fi

# 用例 1 的清单：两个镜像组的构建记录就位后 release-manifest 写出 AGENT_VERSION（命令行覆盖的值，不是仓库文件）、
# 两行 IMAGES 与覆盖全部文件的 SHA256SUMS。
write_built hub v0.0.0-check a v0.0.0-check
write_built agent v0.0.0-check b v0.0.0-check
stub_make make release-manifest VERSION=v0.0.0-check AGENT_VERSION=v0.0.0-check > "$work/out" 2>&1 || bad "make release-manifest failed after release-full"
# shellcheck disable=SC2086 # 资产名按词拆开
expect_set 'release-full + release-manifest' $full_assets AGENT_VERSION IMAGES SHA256SUMS
[ "$(cat dist/AGENT_VERSION 2>&1)" = v0.0.0-check ] || bad "AGENT_VERSION asset is '$(cat dist/AGENT_VERSION 2>&1)', want the injected v0.0.0-check"
printf 'v0.0.0-check\n' | cmp -s - dist/AGENT_VERSION || bad "AGENT_VERSION asset is not exactly one line"
printf 'ghcr.io/xjetry/heron-hub:v0.0.0-check@sha256:%s\nghcr.io/xjetry/heron-agent:v0.0.0-check@sha256:%s\n' "$hex_a" "$hex_b" | cmp -s - dist/IMAGES ||
  bad "IMAGES of a full release: $(cat dist/IMAGES 2>&1)"
sums_cover_dist 'full release'

# 清单的拒绝条件，各自从上面通过的状态出发，只改一处。
cp build/image-archive/agent/built "$work/agent-built"
rm build/image-archive/agent/built
manifest_fails 'a full release without the agent image build record' 'no image build record for the agent group' VERSION=v0.0.0-check AGENT_VERSION=v0.0.0-check
cp "$work/agent-built" build/image-archive/agent/built
write_built hub v0.0.0-other a v0.0.0-check
manifest_fails 'a hub image built for another VERSION' 'is for v0.0.0-other, not VERSION v0.0.0-check' VERSION=v0.0.0-check AGENT_VERSION=v0.0.0-check
# 分步漂移：make docker 漏给 AGENT_VERSION，镜像里的 hub 绑定的是仓库文件的值，而 tar 包里的 hub 绑定的是覆盖值。
# 两个组的记录都要核对（完整 release 的 make docker 一次构建两组）。
write_built hub v0.0.0-check a "$repo_agent_version"
manifest_fails 'a hub image whose make docker was not given the AGENT_VERSION' "binds AGENT_VERSION $repo_agent_version, not v0.0.0-check: make docker must be given the same AGENT_VERSION" VERSION=v0.0.0-check AGENT_VERSION=v0.0.0-check
write_built hub v0.0.0-check a v0.0.0-check
write_built agent v0.0.0-check b "$repo_agent_version"
manifest_fails 'an agent image whose make docker was not given the AGENT_VERSION' "agent image build record build/image-archive/agent/built binds AGENT_VERSION $repo_agent_version" VERSION=v0.0.0-check AGENT_VERSION=v0.0.0-check
write_built agent v0.0.0-check b v0.0.0-check
# 记录缺了绑定版本一列（不知道镜像里的 hub 绑定的是什么）、或多出一列，一律拒绝。
printf 'v0.0.0-check sha256:%s\n' "$hex_a" > build/image-archive/hub/built
manifest_fails 'a hub image build record without the AGENT_VERSION column' "build/image-archive/hub/built is not '<VERSION> <index digest> <AGENT_VERSION>'" VERSION=v0.0.0-check AGENT_VERSION=v0.0.0-check
printf 'v0.0.0-check sha256:%s v0.0.0-check extra\n' "$hex_a" > build/image-archive/hub/built
manifest_fails 'a hub image build record with a fourth column' "build/image-archive/hub/built is not '<VERSION> <index digest> <AGENT_VERSION>'" VERSION=v0.0.0-check AGENT_VERSION=v0.0.0-check
write_built hub v0.0.0-check a v0.0.0-check
# 分步漂移：发布目标给了 AGENT_VERSION，release-manifest 漏给，取到仓库文件里的值。
manifest_fails 'an AGENT_VERSION that drifted from the release target' "AGENT_VERSION=v0.0.0-check, not $repo_agent_version" VERSION=v0.0.0-check
manifest_fails 'a VERSION that drifted from the release target' 'VERSION=v0.0.0-check, not v0.0.0-other' VERSION=v0.0.0-other AGENT_VERSION=v0.0.0-check
cp dist/install-hub.sh "$work/install-hub.sh"
awk -v q="'" '/^RELEASE_VERSION=/ { $0 = "RELEASE_VERSION=" q "v0.0.0-other" q } { print }' "$work/install-hub.sh" > dist/install-hub.sh
manifest_fails 'an install-hub.sh stamped with another version' "dist/install-hub.sh embeds RELEASE_VERSION='v0.0.0-other'" VERSION=v0.0.0-check AGENT_VERSION=v0.0.0-check
cp "$work/install-hub.sh" dist/install-hub.sh
mv build/release-record "$work/release-record"
manifest_fails 'a missing build record' 'no release record at build/release-record' VERSION=v0.0.0-check AGENT_VERSION=v0.0.0-check
mv "$work/release-record" build/release-record
stub_make make release-manifest VERSION=v0.0.0-check AGENT_VERSION=v0.0.0-check > "$work/out" 2>&1 || bad "make release-manifest failed after the rejection cases restored its inputs"

# 用例 2：只发 hub，VERSION 与 AGENT_VERSION 不同。dist 恰为 hub 与 updater 各两个架构的 tar 包、写入本版的
# install-hub.sh、取自绑定版本的两个 agent 安装脚本；agentinputs 的门禁在第一次 go build 之前；写入后的 install-hub.sh
# 过固定版本的 shellcheck（取自绑定版本的两个脚本不是本版写入的，不在这里检查）。release_clean 删掉了上一用例的
# 清单与构建记录。
: > "$work/go-calls"
: > "$work/sc-calls"
rm -rf dist
stub_make STUB_KIND=hub-only make release-hub-only VERSION=v1.2.4 AGENT_VERSION=v1.2.3 > "$work/out" 2>&1 || bad "make release-hub-only failed (stub)"
checks_installers '-s sh dist/install-hub.sh'
hub_assets='heron-hub_linux_amd64.tar.gz heron-hub_linux_arm64.tar.gz
  heron-updater_linux_amd64.tar.gz heron-updater_linux_arm64.tar.gz
  install-hub.sh install-macos.sh install.sh'
# shellcheck disable=SC2086 # 资产名按词拆开
expect_set release-hub-only $hub_assets
bound_copy=$(printf '%s\nRELEASE_VERSION=%s' "$stamp_begin" "'v1.2.3'")
[ "$(cat dist/install.sh)" = "$bound_copy" ] || bad "install.sh is not the bound version's copy"
[ "$(cat dist/install-macos.sh)" = "$bound_copy" ] || bad "install-macos.sh is not the bound version's copy"
printf 'VERSION=v1.2.4\nAGENT_VERSION=v1.2.3\nKIND=hub-only\n' | cmp -s - build/release-record ||
  bad "release-hub-only wrote a wrong build record: $(cat build/release-record 2>&1)"
gate=$(awk '/agentinputs/ { print NR; exit }' "$work/go-calls")
first_build=$(awk '$2 == "build" { print NR; exit }' "$work/go-calls")
case $gate in '' | *[!0-9]*) bad "release-hub-only never ran scripts/agentinputs" ;; esac
case $first_build in '' | *[!0-9]*) bad "release-hub-only never built" ;; esac
[ "$gate" -lt "$first_build" ] || bad "release-hub-only built before its agent-inputs gate"
grep -q '^go run ./scripts/boundagent fetch' "$work/go-calls" || bad "release-hub-only did not fetch the bound agent's installers"

# 用例 2 的清单：只有 hub 组的一行 IMAGES；agent 组的构建记录是上一次构建留下的、属于另一个版本，只发 hub 不读它。
# install-hub.sh 按 VERSION、两个 agent 安装脚本按 AGENT_VERSION 核对，两者不同也通过。
# 镜像里的 hub 绑定的必须是 AGENT_VERSION 而不是 VERSION：make docker-hub 收到的 AGENT_VERSION 与发布目标不同（这里是
# 误给了 VERSION）时拒绝。
write_built hub v1.2.4 c v1.2.4
manifest_fails 'a hub-only hub image bound to VERSION instead of AGENT_VERSION' 'binds AGENT_VERSION v1.2.4, not v1.2.3' VERSION=v1.2.4 AGENT_VERSION=v1.2.3
write_built hub v1.2.4 c v1.2.3
stub_make make release-manifest VERSION=v1.2.4 AGENT_VERSION=v1.2.3 > "$work/out" 2>&1 || bad "make release-manifest failed after release-hub-only"
# shellcheck disable=SC2086 # 资产名按词拆开
expect_set 'release-hub-only + release-manifest' $hub_assets AGENT_VERSION IMAGES SHA256SUMS
printf 'v1.2.3\n' | cmp -s - dist/AGENT_VERSION || bad "AGENT_VERSION asset of a hub-only release: $(cat dist/AGENT_VERSION 2>&1)"
printf 'ghcr.io/xjetry/heron-hub:v1.2.4@sha256:%s\n' "$hex_c" | cmp -s - dist/IMAGES || bad "IMAGES of a hub-only release: $(cat dist/IMAGES 2>&1)"
sums_cover_dist 'hub-only release'
cp dist/install.sh "$work/install.sh"
awk -v q="'" '/^RELEASE_VERSION=/ { $0 = "RELEASE_VERSION=" q "v1.2.4" q } { print }' "$work/install.sh" > dist/install.sh
manifest_fails 'a hub-only release whose install.sh carries the hub version' "dist/install.sh embeds RELEASE_VERSION='v1.2.4'" VERSION=v1.2.4 AGENT_VERSION=v1.2.3

# 用例 3：种类不符即失败：release-full 撞上 hub-only 的判定，任何 go build 之前退出，dist 原样。
: > "$work/go-calls"
rm -rf dist
mkdir dist
: > dist/.marker
rc=0
stub_make STUB_KIND=hub-only make release-full VERSION=v1.2.3 AGENT_VERSION=v1.2.3 > "$work/out" 2>&1 || rc=$?
if [ "$rc" = 0 ]; then
  bad "release-full accepted a hub-only release"
else
  grep -q "does not match this release's kind" "$work/out" || bad "release-full failed for the wrong reason"
fi
if grep -q '^go build' "$work/go-calls"; then
  cp "$work/go-calls" "$work/out"
  bad "release-full built before failing the kind check"
fi
[ -e dist/.marker ] || bad "release-full touched dist before failing the kind check"

# 用例 4：scripts/install-accept.sh 的准备路径（消费发布目标的产物）：两次 release-full、把两套产物复制进下载目录、
# 起下载服务并核对本轮标记，然后进入 hub 格。orb 替身扮演那台机器：从下载服务取 hub 格用到的每个文件（两个版本的
# install-hub.sh 与本机架构的 hub、更新器包），核对 install-hub.sh 写入的是对应的版本，再打印格子要求的末行。复制的
# 文件与发布目标的产物对不上时 cp 失败、脚本在 set -e 下停在准备阶段，到不了 orb。端口取一个空闲的回环端口。
: > "$work/orb-calls"
cat > "$work/stub/orb" <<'STUB'
#!/bin/sh
printf '%s\n' "orb $*" >> "$ORB_CALL_LOG"
case $1 in
  create | delete) exit 0 ;;
  -m)
    cat > /dev/null
    shift 4
    [ "$1 $2 $3" = "sh -s --" ] || { echo "stub orb: unexpected guest command $*" >&2; exit 1; }
    base=$(printf '%s\n' "$4" | sed 's#//host\.orb\.internal:#//127.0.0.1:#')
    for pair in "a $5" "b $6"; do
      dir=${pair%% *}
      version=${pair#* }
      for f in install-hub.sh heron-hub_linux_amd64.tar.gz heron-updater_linux_amd64.tar.gz; do
        curl -fsS -o "$ORB_FETCHED/$dir-$f" "$base/$dir/$f" || { echo "stub orb: cannot fetch $base/$dir/$f" >&2; exit 1; }
      done
      grep -qxF "RELEASE_VERSION='$version'" "$ORB_FETCHED/$dir-install-hub.sh" || { echo "stub orb: $dir/install-hub.sh is not $version" >&2; exit 1; }
    done
    echo 'HUB ACCEPT OK'
    ;;
  *) echo "stub orb: unexpected $*" >&2; exit 1 ;;
esac
STUB
chmod +x "$work/stub/orb"
mkdir "$work/orb-fetched"
dist_port=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
rc=0
stub_make STUB_KIND=full ORB_CALL_LOG="$work/orb-calls" ORB_FETCHED="$work/orb-fetched" DIST_PORT="$dist_port" \
  sh scripts/install-accept.sh --only hub-debian-amd64 > "$work/out" 2>&1 || rc=$?
[ "$rc" = 0 ] && [ "$(tail -n 1 "$work/out")" = 'INSTALL ACCEPT OK' ] || bad "install-accept's preparation path did not carry the hub cell through (exit $rc)"
grep -q '^orb create -a amd64 ' "$work/orb-calls" || { cp "$work/orb-calls" "$work/out"; bad "install-accept never reached the hub cell's machine"; }

if [ -s "$work/sc-tripwire" ]; then
  cp "$work/sc-tripwire" "$work/out"
  bad "a release target ran the shellcheck on PATH instead of the pinned one"
fi

if [ "$failures" != 0 ]; then
  echo "release assets: $failures failure(s)" >&2
  exit 1
fi
echo "release assets ok"
