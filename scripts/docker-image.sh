#!/bin/sh
# 一个镜像组（deploy/image.mk：hub 或 agent）的核对、推送与回读。镜像只构建一次：make docker-<组> 把两个
# 平台导出为 OCI 布局（LAYOUT，索引带 <VERSION> 这个 tag），之后每一步都按索引 digest 引用这份布局或 registry 上的
# 同一个索引。
#   docker-image.sh check     核对 LAYOUT 里 <VERSION> 指向的索引（逐平台根文件系统、本机平台冒烟），通过后写 BUILT
#   docker-image.sh smoke     冒烟本机 docker 里 check 装入的 <IMAGE_REPO>:<VERSION>
#   docker-image.sh push      把 BUILT 记下的索引从 LAYOUT 推到 <IMAGE_REPO>:<VERSION>，不碰 latest
#   docker-image.sh verify    回读 registry 上 <IMAGE_REPO>:<VERSION> 指向的索引，通过后写 RECORD
#   docker-image.sh promote   按 RECORD 里回读过的 digest 处理 latest
# release.yml 里四个 job 各调用一个：build 经 make docker-<组> 调用 check，把 LAYOUT 与 BUILT 作为 artifact 交出；
# image 经 make docker-<组>-push 调用 push；readback 经 make docker-<组>-readback 调用 verify（不登录、不持写权限，
# 运行镜像的只有它），把 RECORD 作为 artifact 交出；promote 经 make docker-<组>-promote 调用 promote；publish 最后
# 才 gh release create。push 与 promote 不起任何容器，持推送权限的 job 因而不运行被构建的镜像。
# 不变式：latest 只会指向回读通过的镜像；回读通过的索引 digest 等于 check 核对过并记进 BUILT 的那一个。由四处
# 显式比对承载：push 先核对布局里的索引仍是记下的 digest 再按它取源（布局被换），verify 要求 registry 上版本 tag
# 的 digest 等于它（推送改写了索引、推送后 tag 被别的写者重推），promote 要求 RECORD 与 BUILT 是同一个 digest
# （两份记录来自不同的 job，经 artifact 传过来），并要求版本 tag 仍指向它（回读后被重推）。回读核对的是 registry
# 上实际存在的内容，不是构建时的中间产物。
# 冒烟要对着一个 hub 跑的组（agent）另给 HUB_LAYOUT 与 HUB_BUILT：运行冒烟的子命令（check、smoke、verify）按 hub
# 组的构建记录从它的布局载入本机平台，以镜像 ID 交给冒烟（HUB_IMAGE，见 load_hub）。
# 参数经 make 传入：镜像名、架构集合、构建器、布局位置、checkimage 清单名与预发布判定只在 deploy/image.mk 与
# Makefile 定义。
set -eu

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# need NAME…：每个输入都必须非空，缺一个就 fail。不用 ${NAME:?}：下面的 EXIT trap 设好之后，bash 3.2（macOS 的
# /bin/sh）让展开错误触发的退出取 trap 里最后一条命令的退出码，漏参数的调用会以 0 退出（dash 与 busybox ash 退 2，
# zsh 退 1；2026-10-12 实测）。fail 显式 exit 1，与运行它的 sh 无关。
need() {
  for name; do
    eval "[ -n \"\${$name:-}\" ]" || fail "$name is required"
  done
}

need IMAGE_REPO VERSION

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# index_digest REF：REF 当前指向的索引 digest；读不到时失败。REF 可以是 registry 引用，也可以是
# oci-layout://<目录>:<tag>。
index_digest() {
  docker buildx imagetools inspect --format '{{.Manifest.Digest}}' "$1"
}

# latest_state：latest 当前指向的 digest；registry 确认不存在时打印 absent。其余读取失败一律中止，不当作
# 不存在——当作不存在会让"预发布没有碰 latest"在读不到时照样通过。buildx 对不存在的 tag 与仓库报
# "<引用>: not found"（buildx v0.33.0 对 ghcr.io 与 registry:2 实测；连接失败、DNS 失败是别的文字）。
# 文字若随 buildx 版本变了，这里按读取失败中止，失败方向是封闭的。
latest_state() {
  if index_digest "$IMAGE_REPO:latest" 2> "$work/latest.err"; then
    return 0
  fi
  case $(cat "$work/latest.err") in
    *"$IMAGE_REPO:latest: not found"*) echo absent ;;
    *)
      cat "$work/latest.err" >&2
      fail "cannot read $IMAGE_REPO:latest (a failed read is not taken as absent)"
      ;;
  esac
}

# 构建记录（BUILT，check 写）与回读记录（RECORD，verify 写）是同一种格式：恰为一行 "<VERSION> <索引 digest>
# <AGENT_VERSION>"。第三列是 make docker-<组> 收到的 AGENT_VERSION——hub 二进制经 HUB_GOFLAGS 注入的是同一个导出
# 变量（Makefile 的 export AGENT_VERSION，deploy/image.mk 的 check 调用行），所以它就是镜像里 hub 绑定的 agent
# 版本。本脚本只搬运这一列（verify 从 BUILT 抄进 RECORD），核对它等于发布目标的 AGENT_VERSION 的是
# scripts/release-manifest.sh 的 built_digest；那边按同一格式读 BUILT，两处改格式要一起改。
# read_record FILE WHAT：把 FILE 读进 rec_version、rec_digest、rec_agent；不是恰好三列、或版本不是本次的 VERSION
# 即失败。WHAT 是报错里这份记录的称呼。
read_record() {
  rec_extra=
  read -r rec_version rec_digest rec_agent rec_extra < "$1" || fail "$2 $1 is not a complete line"
  [ -n "$rec_agent" ] && [ -z "$rec_extra" ] ||
    fail "$2 $1 is not one line '<VERSION> <index digest> <AGENT_VERSION>'"
  [ "$rec_version" = "$VERSION" ] || fail "$2 is for $rec_version, not $VERSION"
}

# read_built：读 BUILT 里 check 记下的索引 digest 与绑定版本到 built_digest、built_agent；记录必须属于本次的 VERSION。
read_built() {
  need BUILT
  [ -s "$BUILT" ] || fail "no build record at $BUILT; run make docker first (the release workflow takes it from the build job's image artifact)"
  read_record "$BUILT" "the build record"
  built_digest=$rec_digest
  built_agent=$rec_agent
}

# check_rootfs SRC：按 SRC（buildx 的具名构建上下文，带 digest）导出每个平台的根文件系统，交给 checkimage。
# tar 布局与 make docker 相同（每个平台一个 linux_<arch>/）。不用 docker export：它导出的是容器的文件系统，带着
# 运行时注入的 /.dockerenv、/dev、/proc、/sys、/etc/hosts 等条目。冒烟照不到架构错配（amd64 运行器会原生跑通
# 装错了 amd64 二进制的 arm64 条目），这里读 ELF 头能照到。
check_rootfs() {
  # shellcheck disable=SC2086 # 架构清单按词拆成参数
  platforms=$(printf 'linux/%s,' $ARCHES)
  printf 'FROM img\n' |
    docker buildx build --builder "$BUILDER" --build-context "img=$1" --platform "${platforms%,}" \
      --output "type=tar,dest=$work/rootfs.tar" - ||
    fail "export the root filesystems of $1"
  # shellcheck disable=SC2086 # 架构清单按词拆成参数
  "$CHECKIMAGE" -manifest "$MANIFEST" "$work/rootfs.tar" $ARCHES || fail "the root filesystems of $1 failed checkimage"
}

need_check_tools() {
  need ARCHES BUILDER SMOKE CHECKIMAGE MANIFEST TOOL_IMAGE
}

# load_hub：HUB_LAYOUT 给出时，把冒烟要对着注册的 hub 载入本机 docker，镜像 ID 导出为 HUB_IMAGE。hub 取自 hub 组
# 这次核对过的布局（make docker-hub 刚产出的，或 release 里同一份 image artifact 里的），按 HUB_BUILT 记下的索引
# digest 引用，布局里的索引不是记下的那个就失败（与 push 同一道比对）。只载入本机平台：冒烟的 hub 以本机平台运行，
# 受测的是另一组的镜像。交出的是镜像 ID 而不是 tag：ID 是镜像配置的 digest，冒烟按它运行，本机或 registry 上同名
# 的旧镜像都顶替不了它；载入不打 tag，也就不覆盖 make docker-hub 装入的 <hub 仓库>:<VERSION>。
load_hub() {
  [ -n "${HUB_LAYOUT:-}" ] || return 0
  [ -n "${HUB_BUILT:-}" ] || fail "HUB_BUILT is required with HUB_LAYOUT"
  [ -s "$HUB_BUILT" ] ||
    fail "no hub build record at $HUB_BUILT; build the hub image first (make docker-hub; the release workflow takes it from the build job's image artifact)"
  read_record "$HUB_BUILT" "the hub build record"
  hub_digest=$rec_digest
  hub_layout_ref=oci-layout://$HUB_LAYOUT:$VERSION
  hub_archived=$(index_digest "$hub_layout_ref") || fail "cannot read $hub_layout_ref"
  [ "$hub_archived" = "$hub_digest" ] ||
    fail "digest mismatch: $hub_layout_ref holds $hub_archived, not the index make docker-hub checked ($hub_digest)"
  rm -f "$work/hub.iid"
  printf 'FROM img\n' |
    docker buildx build --builder "$BUILDER" --build-context "img=$hub_layout_ref@$hub_digest" --iidfile "$work/hub.iid" --load - ||
    fail "load the native platform of $hub_layout_ref@$hub_digest"
  HUB_IMAGE=$(cat "$work/hub.iid")
  # --iidfile 写的是镜像配置的 digest；经典镜像存储里它就是载入后的镜像 ID（buildx v0.33.0、Docker 29.4.0 的 overlay2
  # 存储实测）。存储换了口径时这里找不到它而失败，不会把一个对不上号的引用交给冒烟。
  docker image inspect "$HUB_IMAGE" > /dev/null ||
    fail "the hub loaded from $hub_layout_ref@$hub_digest is not in the local image store as $HUB_IMAGE"
  export HUB_IMAGE
}

case "${1:-}" in
check)
  need_check_tools
  need LAYOUT BUILT AGENT_VERSION
  # 记录按空白分列，带空白的值写进去就读不回原值。
  case $AGENT_VERSION in *[[:space:]]*) fail "AGENT_VERSION '$AGENT_VERSION' contains whitespace and cannot go into the build record" ;; esac
  rm -f "$BUILT"
  layout_ref=oci-layout://$LAYOUT:$VERSION
  digest=$(index_digest "$layout_ref") || fail "cannot read $layout_ref"
  # 之后的每一步都按这个 digest 引用布局：布局是内容寻址的，核对的对象由 digest 唯一确定。
  src=$layout_ref@$digest
  check_rootfs "$src"
  # 本机平台装进 docker 后冒烟。只有 FROM 一行的构建不产生新层；buildx v0.33.0、经典镜像存储上实测装入后的
  # 镜像 ID 等于布局里本机平台清单的 config digest，冒烟的就是布局里那一份配置与层。多平台结果不能
  # --load：经典镜像存储不接受多平台索引（docker exporter does not currently support exporting manifest lists）。
  image=$IMAGE_REPO:$VERSION
  printf 'FROM img\n' |
    docker buildx build --builder "$BUILDER" --build-context "img=$src" -t "$image" --load - ||
    fail "load the native platform of $src"
  load_hub
  IMAGE=$image VERSION=$VERSION TOOL_IMAGE=$TOOL_IMAGE "$SMOKE" || fail "smoke of $src failed"
  printf '%s %s %s\n' "$VERSION" "$digest" "$AGENT_VERSION" > "$BUILT"
  echo "image ok: $src"
  ;;
smoke)
  # 冒烟 check 装入本机 docker 的那一个；冒烟脚本只用本机已有的镜像（docker run --pull=never），本机没有就失败，
  # 不会去 registry 拉同名的已发布镜像。
  need SMOKE TOOL_IMAGE
  if [ -n "${HUB_LAYOUT:-}" ]; then need BUILDER; fi
  load_hub
  image=$IMAGE_REPO:$VERSION
  IMAGE=$image VERSION=$VERSION TOOL_IMAGE=$TOOL_IMAGE "$SMOKE" || fail "smoke of $image failed"
  echo "smoke ok: $image"
  ;;
push)
  need LAYOUT
  read_built
  layout_ref=oci-layout://$LAYOUT:$VERSION
  # 布局与记录出自同一次 make docker；布局里 <VERSION> 指向的不是记下的索引，说明布局在核对之后被换过。
  archived=$(index_digest "$layout_ref") || fail "cannot read $layout_ref"
  [ "$archived" = "$built_digest" ] ||
    fail "digest mismatch: $layout_ref holds $archived, not the index make docker checked ($built_digest)"
  # 按 digest 取源：推出去的只能是核对过的那个索引。来源是单个索引时 imagetools create 原样复制，版本 tag 得到
  # 同一个 digest（registry:2 上实测）；verify 回读确认，不依赖这一点。只推版本 tag，latest 由 promote 移动。
  docker buildx imagetools create -t "$IMAGE_REPO:$VERSION" "$layout_ref@$built_digest" ||
    fail "push $layout_ref@$built_digest to $IMAGE_REPO:$VERSION"
  echo "pushed: $IMAGE_REPO:$VERSION ($built_digest)"
  ;;
verify)
  need_check_tools
  need RECORD IMAGE_BIN_DIR
  rm -f "$RECORD"
  read_built
  # 干净环境：回读要证明的是 registry 上那份镜像，冒烟的对照二进制也取自拉回的镜像本身。IMAGE_BIN_DIR 是 make
  # docker-<组> 放镜像二进制的目录（deploy/image.mk）；回读的环境里那里有文件，说明它与一次本地构建共用了工作树，
  # 任何一步取到它验的就不是回读的产物，所以在冒烟之前失败，不留给各步自己避开。目录不存在或只有空目录即干净。
  if [ -e "$IMAGE_BIN_DIR" ]; then
    find "$IMAGE_BIN_DIR" ! -type d > "$work/bin-dir" 2>&1 || fail "cannot list $IMAGE_BIN_DIR: $(cat "$work/bin-dir")"
    [ ! -s "$work/bin-dir" ] ||
      fail "build outputs under $IMAGE_BIN_DIR ($(head -n 1 "$work/bin-dir")…); the readback must run where no image was built"
  fi
  image=$IMAGE_REPO:$VERSION
  digest=$(index_digest "$image") || fail "cannot read $image"
  [ "$digest" = "$built_digest" ] ||
    fail "digest mismatch: $image points at $digest, not the index make docker checked ($built_digest)"
  # 之后的每一步都按这个 digest 引用：同一个 tag 在回读期间被重推，也改变不了这里核对的对象。
  ref=$IMAGE_REPO@$digest

  # 匿名可取：新建的 ghcr 包默认私有时匿名 docker pull 会失败，与 Release 资产要求仓库公开同一理由。
  # 首次发布在这里失败时，把包的可见性改为公开后重跑 readback job。release.yml 的 readback job 本就不登录；这里仍以
  # 空的 DOCKER_CONFIG 显式检查，不依赖那个 job 没有登录步骤。
  mkdir "$work/anonymous"
  DOCKER_CONFIG=$work/anonymous docker manifest inspect "$ref" > /dev/null ||
    fail "$ref is not anonymously pullable; make the ghcr package public, then re-run the readback job"

  # 逐平台冒烟：按平台清单的 digest 拉取并运行（原因见 image-platform-ref.sh）。在 amd64 运行器上 arm64 经
  # QEMU 运行。要对着 hub 跑的组先从 hub 组的布局载入 hub（同一份 image artifact，不从 registry 取）。
  load_hub
  for arch in $ARCHES; do
    platform_ref=$("$(dirname "$0")/image-platform-ref.sh" "$ref" "linux/$arch")
    docker pull --platform "linux/$arch" "$platform_ref" || fail "pull $platform_ref"
    IMAGE=$platform_ref VERSION=$VERSION SMOKE_PLATFORM=linux/$arch TOOL_IMAGE=$TOOL_IMAGE "$SMOKE" ||
      fail "smoke of $platform_ref (linux/$arch) failed"
  done

  # 逐平台核对根文件系统：按 digest 导出——索引、平台清单与层全是内容寻址，构建器本地已有同 digest 的 blob
  # 时可能不重新下载，但按 digest 校验过的字节与 registry 上的相同。
  check_rootfs "docker-image://$ref"

  # 绑定版本照抄构建记录：回读不构建任何东西，registry 上的索引就是 check 记下的那一个（上面的 digest 比对）。
  printf '%s %s %s\n' "$VERSION" "$digest" "$built_agent" > "$RECORD"
  echo "readback ok: $ref"
  ;;
promote)
  need CHANNEL RECORD
  [ -s "$RECORD" ] || fail "no readback record at $RECORD; run make docker-readback first"
  read_record "$RECORD" "the readback record"
  digest=$rec_digest
  # 回读过的就是 make docker 核对过的那一个：verify 已比对过一次，这里再比对，因为两份记录经不同 job 的 artifact
  # 传到 promote，任一份被换或取错都会让 latest 指向没经过两边核对的索引。
  read_built
  [ "$digest" = "$built_digest" ] ||
    fail "digest mismatch: the readback record holds $digest, not the index make docker checked ($built_digest)"
  # 回读之后版本 tag 没有被重推：要移动或核对的正是回读过的那一个索引。
  now=$(index_digest "$IMAGE_REPO:$VERSION") || fail "cannot read $IMAGE_REPO:$VERSION"
  [ "$now" = "$digest" ] || fail "$IMAGE_REPO:$VERSION moved from $digest to $now after the readback"
  case $CHANNEL in
  stable)
    # 来源是索引时，imagetools create 原样复制，latest 与版本 tag 是同一个 digest（registry:2 上实测）；
    # 这里仍回读确认，不依赖这一点。
    docker buildx imagetools create -t "$IMAGE_REPO:latest" "$IMAGE_REPO@$digest"
    latest=$(latest_state)
    [ "$latest" = "$digest" ] || fail "latest points at $latest after the move, want $digest"
    ;;
  prerelease)
    latest=$(latest_state)
    [ "$latest" != "$digest" ] || fail "latest points at this prerelease ($digest)"
    ;;
  *) fail "CHANNEL must be stable or prerelease, got '$CHANNEL'" ;;
  esac
  echo "latest ok: $IMAGE_REPO:$VERSION ($CHANNEL, latest $latest)"
  ;;
*)
  echo "usage: docker-image.sh check | smoke | push | verify | promote" >&2
  exit 2
  ;;
esac
