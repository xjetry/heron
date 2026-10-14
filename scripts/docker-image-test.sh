#!/bin/sh
# 镜像核对、推送与回读判定的回归检查（make ci 调用）：docker-image.sh 在 docker 桩、冒烟桩与 checkimage 桩上逐例
# 运行，不访问 registry、不起容器。两个镜像组各跑一遍：hub 组（不带 HUB_LAYOUT）与 agent 组（冒烟要对着 hub，带
# hub 组的布局与构建记录）。真实 registry 上的路径由 make docker 与各组的 -push、-readback、-promote 目标覆盖。
set -eu
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0
repo=reg.example/heron-hub
tag_digest=sha256:$(printf 'a%.0s' $(seq 64))
other_digest=sha256:$(printf 'b%.0s' $(seq 64))
hub_digest=sha256:$(printf 'd%.0s' $(seq 64))
hub_id=sha256:$(printf 'e%.0s' $(seq 64))
# 构建记录的第三列（make docker 收到的 AGENT_VERSION）。与 VERSION 不同：check 若把 VERSION 写进第三列，这里看得出。
agent_version=v1.1.0

mkdir "$work/bin"
# docker 桩。状态在 $STATE 目录：layout（布局里版本 tag 指向的 digest）、tag（registry 上版本 tag 指向的 digest）、
# latest（digest、absent 或 unreadable）、可选的 create（imagetools create 之后 latest 实际得到的 digest）、可选的
# pushed（推送后版本 tag 实际得到的 digest，模拟推送改写了索引）、anon（匿名读取的退出码）、hub-layout（hub 组布局
# 里版本 tag 指向的 digest；路径含 /hub-layout 的布局读它）、hub-id（--iidfile 写出的镜像 ID）、可选的 no-inspect
# （image inspect 失败，模拟载入的镜像不在本机存储里）。每次调用追加到 $STATE/calls。
cat > "$work/bin/docker" << 'STUB'
#!/bin/sh
echo "$*" >> "$STATE/calls"
last=
for a; do last=$a; done
case "$1 $2 $3" in
  "buildx imagetools inspect")
    case "$*" in
      *Manifest.Manifests*)
        case "$*" in
          *'"amd64"'*) echo "sha256:$(printf '1%.0s' $(seq 64))" ;;
          *'"arm64"'*) echo "sha256:$(printf '2%.0s' $(seq 64))" ;;
        esac
        exit 0
        ;;
    esac
    case $last in
      oci-layout://*/hub-layout:*) printf '%s' "$(cat "$STATE/hub-layout")" ;;
      oci-layout://*) printf '%s' "$(cat "$STATE/layout")" ;;
      *:latest)
        state=$(cat "$STATE/latest")
        case $state in
          absent) echo "ERROR: $last: not found" >&2; exit 1 ;;
          unreadable) echo "ERROR: failed to do request: Head \"https://reg.example/v2/\": EOF" >&2; exit 1 ;;
        esac
        printf '%s' "$state"
        ;;
      *) printf '%s' "$(cat "$STATE/tag")" ;;
    esac
    ;;
  "buildx imagetools create")
    case "$*" in
      *":latest "*)
        if [ -s "$STATE/create" ]; then cp "$STATE/create" "$STATE/latest"; else echo "${last#*@}" > "$STATE/latest"; fi
        ;;
      *)
        if [ -s "$STATE/pushed" ]; then cp "$STATE/pushed" "$STATE/tag"; else echo "${last#*@}" > "$STATE/tag"; fi
        ;;
    esac
    ;;
  "buildx build --builder")
    cat > /dev/null
    prev=
    for a; do
      case $a in type=tar,dest=*) : > "${a#type=tar,dest=}" ;; esac
      [ "$prev" != --iidfile ] || cat "$STATE/hub-id" > "$a"
      prev=$a
    done
    case " $* " in *" --load "*) [ -z "${FAIL_LOAD:-}" ] || exit 1 ;; esac
    ;;
  "image inspect $3") [ ! -e "$STATE/no-inspect" ] ;;
  "manifest inspect $3") exit "$(cat "$STATE/anon")" ;;
  "pull --platform $3") ;;
  "login "*) ;;
  *) echo "docker stub: unexpected: $*" >&2; exit 99 ;;
esac
STUB
# 冒烟桩：FAIL_SMOKE 给出的平台失败，不给 SMOKE_PLATFORM（docker 的默认平台）记作 native。checkimage 桩：
# FAIL_CHECK 非空时失败。docker 桩：FAIL_LOAD 非空时 --load 失败。
cat > "$work/bin/smoke" << 'STUB'
#!/bin/sh
echo "smoke $IMAGE ${SMOKE_PLATFORM:-native}${HUB_IMAGE:+ hub=$HUB_IMAGE}" >> "$STATE/calls"
[ "${SMOKE_PLATFORM:-native}" != "${FAIL_SMOKE:-}" ]
STUB
cat > "$work/bin/checkimage" << 'STUB'
#!/bin/sh
echo "checkimage $*" >> "$STATE/calls"
[ -z "${FAIL_CHECK:-}" ]
STUB
chmod +x "$work/bin/docker" "$work/bin/smoke" "$work/bin/checkimage"

# reset LATEST：新的一组状态。布局与 registry 上的版本 tag 都指向 tag_digest，构建记录也是它（make docker 通过、
# 推送原样复制之后的状态）。
reset() {
  STATE=$(mktemp -d "$work/state.XXXXXX")
  export STATE
  echo "$tag_digest" > "$STATE/layout"
  echo "$tag_digest" > "$STATE/tag"
  echo "v1.2.3 $tag_digest $agent_version" > "$STATE/built"
  echo "$1" > "$STATE/latest"
  echo 0 > "$STATE/anon"
  echo "$hub_digest" > "$STATE/hub-layout"
  echo "v1.2.3 $hub_digest $agent_version" > "$STATE/hub-built"
  printf '%s' "$hub_id" > "$STATE/hub-id"
}

# run NAME WANT_EXIT WANT_TEXT SUBCOMMAND [VAR=VALUE…]：在桩上运行一次，核对退出码与输出里的文字。AGENT_VERSION 默认
# 置空：make 把它导出进 make script-test 的环境，只有 check 读它，其余子命令的结果不能取决于环境里有没有它。
run() {
  name=$1 want_exit=$2 want_text=$3 sub=$4
  shift 4
  rc=0
  env PATH="$work/bin:$PATH" IMAGE_REPO=$repo VERSION=v1.2.3 RECORD="$STATE/record" ARCHES='amd64 arm64' \
    BUILDER=heron-hub-buildkit-test SMOKE="$work/bin/smoke" CHECKIMAGE="$work/bin/checkimage" TOOL_IMAGE=tool \
    LAYOUT=/build/layout BUILT="$STATE/built" MANIFEST=hub IMAGE_BIN_DIR="$STATE/image-bin" AGENT_VERSION= "$@" "$here/docker-image.sh" "$sub" > "$STATE/out" 2>&1 || rc=$?
  if [ "$rc" != "$want_exit" ] || ! grep -qF -- "$want_text" "$STATE/out"; then
    echo "FAIL: $name: exit $rc (want $want_exit), want output containing '$want_text'" >&2
    sed 's/^/    /' "$STATE/out" >&2
    failures=$((failures + 1))
  fi
}

# 正式版本全程通过：两个平台各按平台清单拉取并冒烟一次，核对根文件系统，latest 移到回读过的 digest。
reset "$other_digest"
run "stable verify" 0 "readback ok: $repo@$tag_digest" verify
# 回读记录与构建记录同格式，绑定版本照抄构建记录（run 把环境里的 AGENT_VERSION 置空，这一列只能来自构建记录）。
[ "$(cat "$STATE/record" 2> /dev/null)" = "v1.2.3 $tag_digest $agent_version" ] ||
  { echo "FAIL: verify recorded '$(cat "$STATE/record" 2>&1)', want the build record's line" >&2; failures=$((failures + 1)); }
run "stable promote" 0 "latest ok" promote CHANNEL=stable
[ "$(cat "$STATE/latest")" = "$tag_digest" ] || { echo "FAIL: stable promote left latest at $(cat "$STATE/latest")" >&2; failures=$((failures + 1)); }
[ "$(grep -c '^smoke ' "$STATE/calls")" = 2 ] || { echo "FAIL: stable verify did not smoke each platform once" >&2; failures=$((failures + 1)); }
grep -q "^pull --platform linux/arm64 $repo@sha256:2" "$STATE/calls" || { echo "FAIL: arm64 was not pulled by its platform manifest" >&2; failures=$((failures + 1)); }
grep -q "^checkimage -manifest hub .* amd64 arm64$" "$STATE/calls" || { echo "FAIL: the root filesystems were not checked for both platforms against the hub manifest" >&2; failures=$((failures + 1)); }

# 预发布全程通过：latest 不存在，或指向别的 digest；promote 不移动 latest。
reset absent
run "prerelease verify" 0 "readback ok" verify
run "prerelease promote, latest absent" 0 "latest absent" promote CHANNEL=prerelease
reset "$other_digest"
run "prerelease verify" 0 "readback ok" verify
run "prerelease promote, latest elsewhere" 0 "latest $other_digest" promote CHANNEL=prerelease
! grep -q 'imagetools create' "$STATE/calls" || { echo "FAIL: a prerelease moved latest" >&2; failures=$((failures + 1)); }

# latest 读取失败：一律失败，不当作不存在。
reset unreadable
run "prerelease verify" 0 "readback ok" verify
run "prerelease promote, latest unreadable" 1 "cannot read $repo:latest" promote CHANNEL=prerelease

# latest 指向别处：预发布指向了自己，或正式版本移动之后回读到的不是回读过的 digest。
reset "$tag_digest"
run "prerelease verify" 0 "readback ok" verify
run "prerelease promote, latest points at it" 1 "latest points at this prerelease" promote CHANNEL=prerelease
reset absent
echo "$other_digest" > "$STATE/create"
run "stable verify" 0 "readback ok" verify
run "stable promote, latest lands elsewhere" 1 "latest points at $other_digest after the move" promote CHANNEL=stable

# 某平台 checkimage 或冒烟失败：不写记录，promote 拒绝。
reset absent
run "verify, checkimage fails" 1 "failed checkimage" verify FAIL_CHECK=1
run "promote without a record" 1 "no readback record" promote CHANNEL=stable
reset absent
run "verify, arm64 smoke fails" 1 "smoke of $repo@sha256:2" verify FAIL_SMOKE=linux/arm64
[ ! -e "$STATE/record" ] || { echo "FAIL: a failed smoke still wrote the readback record" >&2; failures=$((failures + 1)); }

# 匿名不可取；回读之后版本 tag 被重推。
reset absent
echo 1 > "$STATE/anon"
run "verify, not anonymously pullable" 1 "is not anonymously pullable" verify
reset absent
run "verify" 0 "readback ok" verify
echo "$other_digest" > "$STATE/tag"
run "promote after the tag moved" 1 "moved from $tag_digest to $other_digest" promote CHANNEL=stable

# check：按布局里版本 tag 的 digest 核对两个平台的根文件系统、装入本机平台并冒烟，通过后写构建记录。
reset absent
rm "$STATE/built"
run "check" 0 "image ok: oci-layout:///build/layout:v1.2.3@$tag_digest" check AGENT_VERSION=$agent_version
[ "$(cat "$STATE/built" 2> /dev/null)" = "v1.2.3 $tag_digest $agent_version" ] || { echo "FAIL: check did not record v1.2.3 $tag_digest $agent_version: $(cat "$STATE/built" 2>&1)" >&2; failures=$((failures + 1)); }
grep -q "^buildx build .*--build-context img=oci-layout:///build/layout:v1.2.3@$tag_digest .*--load" "$STATE/calls" ||
  { echo "FAIL: check did not load the native platform from the layout by digest" >&2; failures=$((failures + 1)); }
grep -q "^buildx build .*--build-context img=oci-layout:///build/layout:v1.2.3@$tag_digest --platform linux/amd64,linux/arm64 " "$STATE/calls" ||
  { echo "FAIL: check did not export both platforms from the layout by digest" >&2; failures=$((failures + 1)); }
grep -q "^checkimage .* amd64 arm64$" "$STATE/calls" || { echo "FAIL: check did not run checkimage on both platforms" >&2; failures=$((failures + 1)); }
grep -qx "smoke $repo:v1.2.3 native" "$STATE/calls" || { echo "FAIL: check did not smoke the loaded image" >&2; failures=$((failures + 1)); }
# 任一核对失败：不写构建记录，push 拒绝。
reset absent
run "check, checkimage fails" 1 "failed checkimage" check FAIL_CHECK=1 AGENT_VERSION=$agent_version
[ ! -e "$STATE/built" ] || { echo "FAIL: a failed checkimage still wrote the build record" >&2; failures=$((failures + 1)); }
run "push without a build record" 1 "no build record" push
reset absent
run "check, load fails" 1 "load the native platform" check FAIL_LOAD=1 AGENT_VERSION=$agent_version
[ ! -e "$STATE/built" ] || { echo "FAIL: a failed load still wrote the build record" >&2; failures=$((failures + 1)); }
reset absent
run "check, smoke fails" 1 "smoke of oci-layout://" check FAIL_SMOKE=native AGENT_VERSION=$agent_version
[ ! -e "$STATE/built" ] || { echo "FAIL: a failed smoke still wrote the build record" >&2; failures=$((failures + 1)); }
# 构建记录的第三列只能取 make 交来的 AGENT_VERSION：没给、或带空白（记录按空白分列，读不回原值）都在核对之前失败，
# 不写记录。
for case in "without AGENT_VERSION" "with whitespace in AGENT_VERSION"; do
  reset absent
  rm "$STATE/built"
  case $case in
    without*) run "check $case" 1 "AGENT_VERSION is required" check ;;
    with*) run "check $case" 1 "AGENT_VERSION 'v1.1 x' contains whitespace" check 'AGENT_VERSION=v1.1 x' ;;
  esac
  [ ! -e "$STATE/built" ] || { echo "FAIL: check $case wrote the build record" >&2; failures=$((failures + 1)); }
  [ ! -e "$STATE/calls" ] || { echo "FAIL: check $case ran docker before failing" >&2; failures=$((failures + 1)); }
done

# push：只按构建记录的 digest 从布局推送版本 tag，不碰 latest。
reset "$other_digest"
echo "$other_digest" > "$STATE/tag"
run "push" 0 "pushed: $repo:v1.2.3 ($tag_digest)" push
[ "$(cat "$STATE/tag")" = "$tag_digest" ] || { echo "FAIL: push left the version tag at $(cat "$STATE/tag")" >&2; failures=$((failures + 1)); }
[ "$(cat "$STATE/latest")" = "$other_digest" ] || { echo "FAIL: push moved latest" >&2; failures=$((failures + 1)); }
grep -qx "buildx imagetools create -t $repo:v1.2.3 oci-layout:///build/layout:v1.2.3@$tag_digest" "$STATE/calls" ||
  { echo "FAIL: push did not push the recorded digest to the version tag alone" >&2; failures=$((failures + 1)); }
# 布局在核对之后被换过：推送之前就失败，版本 tag 不动。
reset absent
echo "$other_digest" > "$STATE/layout"
run "push, layout replaced" 1 "digest mismatch: oci-layout:///build/layout:v1.2.3 holds $other_digest" push
! grep -q 'imagetools create' "$STATE/calls" || { echo "FAIL: push pushed a layout that is not the checked one" >&2; failures=$((failures + 1)); }
# 构建记录属于别的版本。
reset absent
echo "v1.2.2 $tag_digest $agent_version" > "$STATE/built"
run "push, record of another version" 1 "the build record is for v1.2.2" push

# verify：registry 上版本 tag 的 digest 不等于构建记录（推送改写了索引、或推送后被重推）即失败，不写回读记录。
reset absent
echo "$other_digest" > "$STATE/pushed"
run "push, registry rewrites the index" 0 "pushed" push
run "verify after the push rewrote the index" 1 "digest mismatch: $repo:v1.2.3 points at $other_digest, not the index make docker checked ($tag_digest)" verify
[ ! -e "$STATE/record" ] || { echo "FAIL: a digest mismatch still wrote the readback record" >&2; failures=$((failures + 1)); }
run "promote after a failed verify" 1 "no readback record" promote CHANNEL=stable
reset absent
run "verify without a build record" 1 "no build record" verify BUILT="$STATE/missing"

# verify 只在干净的环境里回读：IMAGE_BIN_DIR（镜像二进制的构建目录）里有任何文件即在冒烟之前失败、不写回读记录；
# 目录不存在或为空即放行。目录里的文件也可能是链接，一样算。
for case in file symlink; do
  reset absent
  mkdir -p "$STATE/image-bin/linux/amd64"
  case $case in
    file) : > "$STATE/image-bin/linux/amd64/heron-agent" ;;
    symlink) ln -s /nonexistent "$STATE/image-bin/linux/amd64/heron-agent" ;;
  esac
  run "verify with a built $case under IMAGE_BIN_DIR" 1 "build outputs under $STATE/image-bin ($STATE/image-bin/linux/amd64/heron-agent…)" verify
  [ ! -e "$STATE/calls" ] || ! grep -q '^smoke ' "$STATE/calls" || { echo "FAIL: verify with a built $case under IMAGE_BIN_DIR: the smoke ran" >&2; failures=$((failures + 1)); }
  [ ! -e "$STATE/record" ] || { echo "FAIL: verify with a built $case under IMAGE_BIN_DIR: wrote the readback record" >&2; failures=$((failures + 1)); }
done
reset absent
mkdir -p "$STATE/image-bin/linux/amd64"
run "verify with an empty IMAGE_BIN_DIR" 0 "readback ok" verify
reset absent
run "verify without IMAGE_BIN_DIR" 1 "IMAGE_BIN_DIR is required" verify IMAGE_BIN_DIR=
# 漏掉的输入在哪个 sh 下都以非零退出。docker-image.sh 在 EXIT trap 之后才检查子命令的输入，而 bash 3.2（macOS 的
# /bin/sh）让 ${…:?} 的展开错误带着 trap 的退出码 0 退出，所以它的输入检查走显式判空（need）；RECORD 是 trap 之后
# 才检查的输入，这里钉住退出码。
reset absent
run "verify without RECORD" 1 "RECORD is required" verify RECORD=

# promote：回读记录与构建记录必须是同一个 digest（两份记录经不同 job 的 artifact 传来），否则不碰 latest。
reset "$other_digest"
echo "v1.2.3 $tag_digest $agent_version" > "$STATE/record"
echo "v1.2.3 $other_digest $agent_version" > "$STATE/built"
run "promote, readback record differs from the build record" 1 "digest mismatch: the readback record holds $tag_digest, not the index make docker checked ($other_digest)" promote CHANNEL=stable
[ "$(cat "$STATE/latest")" = "$other_digest" ] || { echo "FAIL: promote moved latest despite mismatched records" >&2; failures=$((failures + 1)); }
reset absent
echo "v1.2.3 $tag_digest $agent_version" > "$STATE/record"
run "promote without a build record" 1 "no build record" promote CHANNEL=stable BUILT="$STATE/missing"

# 记录不是恰好三列（缺绑定版本、或多出一列）：读它的子命令都失败，不推送、不写回读记录、不碰 latest。
for case in "two columns" "four columns"; do
  case $case in
    two*) bad_line="v1.2.3 $tag_digest" ;;
    four*) bad_line="v1.2.3 $tag_digest $agent_version extra" ;;
  esac
  reset absent
  echo "$bad_line" > "$STATE/built"
  run "push, build record of $case" 1 "the build record $STATE/built is not one line '<VERSION> <index digest> <AGENT_VERSION>'" push
  ! grep -q 'imagetools create' "$STATE/calls" 2> /dev/null || { echo "FAIL: push with a build record of $case pushed" >&2; failures=$((failures + 1)); }
  reset absent
  echo "$bad_line" > "$STATE/built"
  run "verify, build record of $case" 1 "the build record $STATE/built is not one line" verify
  [ ! -e "$STATE/record" ] || { echo "FAIL: verify with a build record of $case wrote the readback record" >&2; failures=$((failures + 1)); }
  reset "$other_digest"
  echo "$bad_line" > "$STATE/record"
  run "promote, readback record of $case" 1 "the readback record $STATE/record is not one line" promote CHANNEL=stable
  [ "$(cat "$STATE/latest")" = "$other_digest" ] || { echo "FAIL: promote with a readback record of $case moved latest" >&2; failures=$((failures + 1)); }
done

# only_imagetools NAME：本组状态下 docker 桩只收到过 imagetools 的读与复制。
only_imagetools() {
  if grep -v '^buildx imagetools \(inspect\|create\) ' "$STATE/calls" > "$STATE/other-calls"; then
    echo "FAIL: $1 ran docker commands other than imagetools:" >&2
    sed 's/^/    /' "$STATE/other-calls" >&2
    failures=$((failures + 1))
  fi
}
# 持推送权限的 job 不运行被构建的镜像：push 与 promote 只调用 imagetools（读 digest、复制索引），不拉取、
# 不起容器、不构建。
reset absent
run "push alone" 0 "pushed" push
only_imagetools push
reset "$other_digest"
echo "v1.2.3 $tag_digest $agent_version" > "$STATE/record"
run "stable promote alone" 0 "latest ok" promote CHANNEL=stable
only_imagetools "stable promote"
reset absent
echo "v1.2.3 $tag_digest $agent_version" > "$STATE/record"
run "prerelease promote alone" 0 "latest ok" promote CHANNEL=prerelease
only_imagetools "prerelease promote"
# 运行镜像的 job 不持写权限：verify 不登录、不推送、不改任何 tag。
reset "$other_digest"
run "verify alone" 0 "readback ok" verify
if grep -E '^(login|logout|push|tag|buildx imagetools create)( |$)' "$STATE/calls"; then
  echo "FAIL: verify logged in, pushed or moved a tag" >&2
  failures=$((failures + 1))
fi
if [ "$(cat "$STATE/tag")" != "$tag_digest" ] || [ "$(cat "$STATE/latest")" != "$other_digest" ]; then
  echo "FAIL: verify changed a tag" >&2
  failures=$((failures + 1))
fi

# smoke：冒烟 check 装入本机的 <仓库>:<VERSION>，hub 组不载入任何东西。
reset absent
run "hub smoke" 0 "smoke ok: $repo:v1.2.3" smoke
grep -qx "smoke $repo:v1.2.3 native" "$STATE/calls" || { echo "FAIL: hub smoke did not smoke the loaded image" >&2; failures=$((failures + 1)); }
! grep -q '^buildx build' "$STATE/calls" || { echo "FAIL: hub smoke loaded an image" >&2; failures=$((failures + 1)); }

# agent 组：冒烟要对着 hub。运行冒烟的子命令按 hub 组的构建记录从 hub 布局载入本机平台，冒烟拿到的是载入后的镜像
# ID；checkimage 按 agent 清单核对。agent_run 与 run 相同，另带 agent 组的清单名与 hub 组的布局、构建记录。
agent_repo=reg.example/heron-agent
agent_run() {
  name=$1 want_exit=$2 want_text=$3 sub=$4
  shift 4
  run "agent $name" "$want_exit" "$want_text" "$sub" IMAGE_REPO=$agent_repo MANIFEST=agent \
    HUB_LAYOUT=/build/hub-layout HUB_BUILT="$STATE/hub-built" "$@"
}
hub_load="^buildx build --builder heron-hub-buildkit-test --build-context img=oci-layout:///build/hub-layout:v1.2.3@$hub_digest --iidfile .* --load -\$"

reset absent
rm "$STATE/built"
agent_run "check" 0 "image ok" check AGENT_VERSION=$agent_version
[ "$(cat "$STATE/built" 2> /dev/null)" = "v1.2.3 $tag_digest $agent_version" ] || { echo "FAIL: agent check did not record v1.2.3 $tag_digest $agent_version: $(cat "$STATE/built" 2>&1)" >&2; failures=$((failures + 1)); }
grep -q "^checkimage -manifest agent .* amd64 arm64$" "$STATE/calls" || { echo "FAIL: agent check did not run checkimage with the agent manifest" >&2; failures=$((failures + 1)); }
grep -q "$hub_load" "$STATE/calls" || { echo "FAIL: agent check did not load the hub from its layout by the recorded digest" >&2; failures=$((failures + 1)); }
grep -qx "smoke $agent_repo:v1.2.3 native hub=$hub_id" "$STATE/calls" || { echo "FAIL: agent check did not hand the loaded hub's ID to the smoke" >&2; failures=$((failures + 1)); }
# hub 的记录缺失、缺绑定版本一列、属于别的版本、布局被换、载入后不在本机存储里：都在冒烟之前失败，不写构建记录。
for case in "no hub record" "hub record without the AGENT_VERSION column" "hub record of another version" "hub layout replaced" "loaded hub not in the image store"; do
  reset absent
  rm "$STATE/built"
  case $case in
    "no hub record") rm "$STATE/hub-built"; want="no hub build record at $STATE/hub-built" ;;
    "hub record without the AGENT_VERSION column") echo "v1.2.3 $hub_digest" > "$STATE/hub-built"; want="the hub build record $STATE/hub-built is not one line" ;;
    "hub record of another version") echo "v1.2.2 $hub_digest $agent_version" > "$STATE/hub-built"; want="the hub build record is for v1.2.2" ;;
    "hub layout replaced") echo "$other_digest" > "$STATE/hub-layout"; want="digest mismatch: oci-layout:///build/hub-layout:v1.2.3 holds $other_digest, not the index make docker-hub checked ($hub_digest)" ;;
    "loaded hub not in the image store") : > "$STATE/no-inspect"; want="is not in the local image store as $hub_id" ;;
  esac
  agent_run "check, $case" 1 "$want" check AGENT_VERSION=$agent_version
  ! grep -q '^smoke ' "$STATE/calls" || { echo "FAIL: agent check, $case: the smoke ran" >&2; failures=$((failures + 1)); }
  [ ! -e "$STATE/built" ] || { echo "FAIL: agent check, $case: wrote the build record" >&2; failures=$((failures + 1)); }
done

# verify：hub 只载入一次，两个平台的冒烟都拿到它的 ID；hub 记录缺失时不写回读记录。
reset absent
agent_run "verify" 0 "readback ok: $agent_repo@$tag_digest" verify
[ "$(grep -c "$hub_load" "$STATE/calls")" = 1 ] || { echo "FAIL: agent verify did not load the hub exactly once" >&2; failures=$((failures + 1)); }
for arch in amd64 arm64; do
  grep -q "^smoke $agent_repo@sha256:[0-9a-f]* linux/$arch hub=$hub_id\$" "$STATE/calls" ||
    { echo "FAIL: agent verify did not smoke linux/$arch against the loaded hub" >&2; failures=$((failures + 1)); }
done
grep -q "^checkimage -manifest agent .* amd64 arm64$" "$STATE/calls" || { echo "FAIL: agent verify did not run checkimage with the agent manifest" >&2; failures=$((failures + 1)); }
reset absent
rm "$STATE/hub-built"
agent_run "verify without a hub record" 1 "no hub build record" verify
[ ! -e "$STATE/record" ] || { echo "FAIL: agent verify without a hub record wrote the readback record" >&2; failures=$((failures + 1)); }

# smoke：载入 hub，冒烟 check 装入的那一个。
reset absent
agent_run "smoke" 0 "smoke ok: $agent_repo:v1.2.3" smoke
grep -qx "smoke $agent_repo:v1.2.3 native hub=$hub_id" "$STATE/calls" || { echo "FAIL: agent smoke did not smoke the loaded image against the loaded hub" >&2; failures=$((failures + 1)); }

# push 与 promote 不起容器，也就不载入 hub：即使环境里带着 hub 的布局，也只调用 imagetools。
reset absent
agent_run "push alone" 0 "pushed: $agent_repo:v1.2.3" push
only_imagetools "agent push"
reset "$other_digest"
echo "v1.2.3 $tag_digest $agent_version" > "$STATE/record"
agent_run "stable promote alone" 0 "latest ok" promote CHANNEL=stable
only_imagetools "agent stable promote"

if [ "$failures" != 0 ]; then
  echo "docker-image checks: $failures failure(s)" >&2
  exit 1
fi
echo "docker-image checks ok"
