#!/bin/sh
# release 的清单资产：AGENT_VERSION、IMAGES，以及覆盖全部资产、由签名 job 签名的 SHA256SUMS。
#   release-manifest.sh record KIND   发布目标（release-full / release-hub-only）的最后一步：写构建记录 RECORD
#   release-manifest.sh write         make release-manifest：核对 RECORD、安装脚本与镜像构建记录，在 DIST 写三个清单
#   release-manifest.sh readback      make release-readback（release.yml 的 publish）：取回 Release 上的清单并逐项核对
# 资产格式、"哪种发布带哪些镜像组"与构建记录的格式只在这里，写入与回读用的是同一份。参数经 make 传入：镜像
# 仓库名在 Makefile 与 deploy/image.mk，构建记录的位置在 Makefile（RELEASE_RECORD）与 deploy/image.mk（image_<组>_built）。
#
# 资产（hub 按 <officialDownloads>/<tag>/AGENT_VERSION 读它，格式是契约）：
#   AGENT_VERSION  一行带换行：本次构建注入 hub 的绑定值，即 make 的 $(AGENT_VERSION)（命令行可覆盖），不是仓库
#                  文件的复制。正式发布时两者相同，回读核对它等于 make -s agent-version。
#   IMAGES         每个镜像组一行 <仓库>:<release tag>@sha256:<64 位十六进制>，digest 取自 make docker-<组> 的构建
#                  记录。推送、回读与 promote 都按同一份构建记录工作（scripts/docker-image.sh），所以这一行就是
#                  registry 上那个 tag 应当指向的索引。
#   SHA256SUMS     sha256sum 的输出，覆盖 tar 包、三个安装脚本、AGENT_VERSION 与 IMAGES；在这两个文件写完之后生成，
#                  签名覆盖它也就覆盖了镜像 digest 与绑定版本。
#
# 分步调用的一致性：发布目标、镜像目标与 release-manifest 是三次 make，每次都从命令行或仓库文件取 VERSION 与
# AGENT_VERSION；漏给一次就会让后一步按默认值覆盖前一步的绑定。所以 write 只接受与构建记录相同的两个值，并按
# 角色核对已写入的产物：install-hub.sh 写入的是 VERSION（hub 组），install.sh / install-macos.sh 写入的是
# AGENT_VERSION（完整 release 由本次写入且两者相同，只发 hub 时是绑定版本 release 的原样副本），镜像构建记录的版本
# 是 VERSION、绑定版本是 AGENT_VERSION（make docker 收到的 AGENT_VERSION 就是镜像里 hub 二进制注入的那一个，见
# built_digest）。资产名不带版本，不以文件名判断；不执行任何发布产物。
set -eu

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# need NAME…：每个输入都必须非空。不用 ${NAME:?}：bash 3.2（macOS 的 /bin/sh）在设了 EXIT trap 时让展开错误以
# trap 最后一条命令的退出码退出（scripts/docker-image.sh 的同名函数有实测记录）；这里显式 exit 1，与 sh 的实现无关。
need() {
  for name; do
    eval "[ -n \"\${$name:-}\" ]" || fail "$name is required"
  done
}

# groups_for KIND：这种发布带哪些镜像组。完整 release 两组都发；只发 hub 只发 hub 组，agent 镜像停在
# 绑定版本已发布的那一个。release.yml 按同一判定调用 make docker 或 make docker-hub。
groups_for() {
  case $1 in
    full) echo 'hub agent' ;;
    hub-only) echo hub ;;
    *) fail "unknown release kind '$1' (want full or hub-only)" ;;
  esac
}

repo_of() {
  case $1 in
    hub) printf '%s\n' "$HUB_REPO" ;;
    agent) printf '%s\n' "$AGENT_REPO" ;;
  esac
}

# 64 位小写十六进制：sha256sum 的输出与 buildx 打印的 digest 都是小写。
is_hex64() {
  [ "${#1}" = 64 ] && [ -z "$(printf '%s' "$1" | LC_ALL=C tr -d '0-9a-f')" ]
}

sha256_of() {
  sum=$(sha256sum < "$1") || fail "sha256sum $1"
  printf '%s\n' "${sum%% *}"
}

# 文件以换行结尾：按行读的工具会悄悄接受缺了末尾换行的最后一行，而读这些资产的 Go 代码不接受。
ends_with_newline() {
  [ -s "$1" ] && [ "$(tail -c 1 "$1" | od -An -tx1 | tr -d ' \n')" = 0a ]
}

# record_field NAME：构建记录里 NAME= 那一行的值；没有或不止一行时失败。
record_field() {
  awk -v k="$1=" 'index($0, k) == 1 { n++; v = substr($0, length(k) + 1) } END { if (n != 1) exit 1; print v }' "$RECORD" ||
    fail "$RECORD has no single $1= line; it is written only by the release targets"
}

# 写入区的开始行，与 deploy/releasestamp 的 Begin 逐字相同；写入区第一行是 RELEASE_VERSION='<版本>'。那边改了
# 这一行而这里没跟上时，下面的核对找不到写入区、release-manifest 失败，不会放过。
stamp_begin='# >>> release stamp >>>'

# check_stamped FILE WANT WHY：FILE 写入的版本必须是 WANT。
check_stamped() {
  [ -f "$DIST/$1" ] || fail "$DIST/$1 is missing; run the release target first"
  got=$(awk -v b="$stamp_begin" 'prev == b { print; exit } { prev = $0 }' "$DIST/$1")
  [ "$got" = "RELEASE_VERSION='$2'" ] ||
    fail "$DIST/$1 embeds ${got:-no release stamp}, want RELEASE_VERSION='$2' ($3)"
}

# built_digest GROUP FILE：make docker-<组> 记下的索引 digest。记录恰为一行 "<版本> sha256:<64 位十六进制> <绑定版本>"
# （scripts/docker-image.sh 的 check 写入，格式与那边的 read_record 相同），版本必须是本次的 VERSION，绑定版本必须是
# 本次的 AGENT_VERSION。绑定版本是 make docker 收到的 AGENT_VERSION，hub 二进制经 HUB_GOFLAGS 注入的是同一个导出
# 变量（deploy/image.mk 的 check 调用行，由 scripts/release-rules-test.sh 钉住）；发布目标的绑定已由 RECORD 核对过，
# 两者相等，镜像里的 hub 与 tar 包里的 hub 才绑定同一个 agent 版本。
built_digest() {
  [ -f "$2" ] ||
    fail "no image build record for the $1 group at $2: a $kind release publishes the $1 image, build it before release-manifest (make docker for a full release, make docker-hub for a hub-only one)"
  if [ "$(awk 'END { print NR }' "$2")" != 1 ] || ! ends_with_newline "$2"; then
    fail "$2 is not a single-line image build record"
  fi
  built_extra=
  read -r built_version digest built_agent built_extra < "$2"
  [ -n "$built_agent" ] && [ -z "$built_extra" ] ||
    fail "$2 is not '<VERSION> <index digest> <AGENT_VERSION>'; rebuild the $1 image with make docker"
  [ "$built_version" = "$VERSION" ] || fail "the $1 image build record $2 is for $built_version, not VERSION $VERSION"
  case $digest in
    sha256:*) is_hex64 "${digest#sha256:}" || fail "$2 holds a malformed digest '$digest'" ;;
    *) fail "$2 holds a malformed digest '$digest'" ;;
  esac
  [ "$built_agent" = "$AGENT_VERSION" ] ||
    fail "the $1 image build record $2 binds AGENT_VERSION $built_agent, not $AGENT_VERSION: make docker must be given the same AGENT_VERSION as the release target (make docker-hub for a hub-only release)"
  printf '%s\n' "$digest"
}

# check_sums FILE：严格解析 sha256sum 的输出：以换行结尾，每行 "<64 位小写十六进制>  <资产名>"（两个空格），资产名
# 只含 release 资产的字符集，不重复。与 scripts/boundagent 解析绑定版本清单的口径相同；这里不容忍空行与 CR。
check_sums() {
  ends_with_newline "$1" || fail "$1 is empty or does not end with a newline"
  rc=0
  bad=$(grep -Evx '[0-9a-f]{64}  [A-Za-z0-9_][A-Za-z0-9_.-]*' "$1") || rc=$?
  case $rc in
    0) fail "$1 has lines that are not \"<64 lowercase hex>  <asset name>\": $bad" ;;
    1) ;;
    *) fail "cannot read $1" ;;
  esac
  dup=$(cut -c 67- "$1" | sort | uniq -d)
  [ -z "$dup" ] || fail "$1 lists $(printf '%s' "$dup" | tr '\n' ' ') more than once"
}

# listed_digest FILE NAME：清单里 NAME 那一行的摘要；check_sums 之后调用，名字至多出现一次。
listed_digest() {
  awk -v n="$2" 'substr($0, 67) == n { print substr($0, 1, 64); found = 1 } END { exit !found }' "$1" ||
    fail "$1 does not list $2"
}

case "${1:-}" in
record)
  need VERSION AGENT_VERSION RECORD
  kind=${2:-}
  groups_for "$kind" > /dev/null
  mkdir -p "$(dirname "$RECORD")"
  printf 'VERSION=%s\nAGENT_VERSION=%s\nKIND=%s\n' "$VERSION" "$AGENT_VERSION" "$kind" > "$RECORD.tmp"
  mv "$RECORD.tmp" "$RECORD"
  ;;

write)
  need VERSION AGENT_VERSION RECORD DIST HUB_REPO AGENT_REPO HUB_BUILT AGENT_BUILT
  # 不变式：write 只有整体成功才在 dist 留下清单。先删掉上一次的清单，核对失败时 dist 里不留下与本次产物对不上的
  # SHA256SUMS；失败发生在哪一步都一样——写到一半的 IMAGES.tmp、SHA256SUMS.tmp 与已写出的 AGENT_VERSION / IMAGES
  # 由 EXIT trap 按退出码清掉（release_clean 不认识 .tmp，留下的半份文件会一直在 dist 里）。退出码原样带出。
  remove_manifests() { rm -f "$DIST/AGENT_VERSION" "$DIST/IMAGES" "$DIST/SHA256SUMS" "$DIST/IMAGES.tmp" "$DIST/SHA256SUMS.tmp"; }
  remove_manifests
  trap 'rc=$?; [ "$rc" -eq 0 ] || remove_manifests; exit $rc' EXIT
  [ -f "$RECORD" ] ||
    fail "no release record at $RECORD: run make release-full or make release-hub-only with this VERSION and AGENT_VERSION first"
  rec_version=$(record_field VERSION)
  rec_agent=$(record_field AGENT_VERSION)
  kind=$(record_field KIND)
  [ "$rec_version" = "$VERSION" ] ||
    fail "$RECORD was written by a release target run with VERSION=$rec_version, not $VERSION"
  [ "$rec_agent" = "$AGENT_VERSION" ] ||
    fail "$RECORD was written by a release target run with AGENT_VERSION=$rec_agent, not $AGENT_VERSION; give release-manifest the same AGENT_VERSION as the release target"
  groups=$(groups_for "$kind")
  printf 'VERSION=%s\nAGENT_VERSION=%s\nKIND=%s\n' "$VERSION" "$AGENT_VERSION" "$kind" | cmp -s - "$RECORD" ||
    fail "$RECORD has lines other than the three the release targets write"

  check_stamped install-hub.sh "$VERSION" "install-hub.sh installs this release's hub"
  for f in install.sh install-macos.sh; do
    check_stamped "$f" "$AGENT_VERSION" "$f installs the agent this hub binds"
  done

  : > "$DIST/IMAGES.tmp"
  for g in $groups; do
    case $g in
      hub) built=$HUB_BUILT ;;
      agent) built=$AGENT_BUILT ;;
    esac
    digest=$(built_digest "$g" "$built")
    printf '%s:%s@%s\n' "$(repo_of "$g")" "$VERSION" "$digest" >> "$DIST/IMAGES.tmp"
  done
  printf '%s\n' "$AGENT_VERSION" > "$DIST/AGENT_VERSION"
  mv "$DIST/IMAGES.tmp" "$DIST/IMAGES"
  (cd "$DIST" && sha256sum -- *.tar.gz install.sh install-hub.sh install-macos.sh AGENT_VERSION IMAGES > SHA256SUMS.tmp) ||
    fail "sha256sum over $DIST"
  mv "$DIST/SHA256SUMS.tmp" "$DIST/SHA256SUMS"
  check_sums "$DIST/SHA256SUMS"
  ;;

readback)
  need VERSION AGENT_VERSION KIND DIR HUB_REPO AGENT_REPO
  # shellcheck disable=SC2153 # KIND 由 make release-readback 经环境给出，need 已核对非空
  groups=$(groups_for "$KIND")
  # 新建目录：核对的只能是这一次从 Release 页面取回的字节，不能是目录里早先留下的文件。
  mkdir -p "$(dirname "$DIR")"
  mkdir "$DIR" || fail "$DIR already exists; read back into a fresh directory"
  gh release download "$VERSION" -p SHA256SUMS -p SHA256SUMS.sig -p AGENT_VERSION -p IMAGES -D "$DIR" ||
    fail "download the manifest assets of release $VERSION"
  go run ./scripts/releasesign verify -version "$VERSION" -dir "$DIR" ||
    fail "the signature of $VERSION's SHA256SUMS does not verify"

  # 已验签的清单逐项核对：两个清单资产各恰好一条、文件已取回、摘要相符。不用 sha256sum -c --ignore-missing：
  # 这里只取回了四个文件，--ignore-missing 会把"清单里没有这一条"也当作通过。
  check_sums "$DIR/SHA256SUMS"
  for f in AGENT_VERSION IMAGES; do
    want=$(listed_digest "$DIR/SHA256SUMS" "$f")
    [ -f "$DIR/$f" ] || fail "$f was not downloaded from release $VERSION"
    got=$(sha256_of "$DIR/$f")
    [ "$got" = "$want" ] || fail "$f of release $VERSION has sha256 $got, but the signed SHA256SUMS lists $want"
  done

  printf '%s\n' "$AGENT_VERSION" | cmp -s - "$DIR/AGENT_VERSION" ||
    fail "release $VERSION's AGENT_VERSION asset is '$(cat "$DIR/AGENT_VERSION")', want the bound agent version $AGENT_VERSION (make -s agent-version)"

  ends_with_newline "$DIR/IMAGES" || fail "IMAGES of release $VERSION is empty or does not end with a newline"
  seen=' '
  while IFS= read -r line; do
    printf '%s\n' "$line" | grep -Eqx '[a-z0-9.-]+(/[a-z0-9._-]+)+:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}@sha256:[0-9a-f]{64}' ||
      fail "IMAGES line '$line' is not <repository>:<tag>@sha256:<64 lowercase hex>"
    ref=${line%@*}
    repo=${ref%:*}
    tag=${ref##*:}
    case $repo in
      "$HUB_REPO") g=hub ;;
      "$AGENT_REPO") g=agent ;;
      *) fail "IMAGES line '$line' names $repo, which is neither $HUB_REPO nor $AGENT_REPO" ;;
    esac
    [ "$tag" = "$VERSION" ] || fail "IMAGES line '$line' is tagged $tag, not the release tag $VERSION"
    case $seen in *" $g "*) fail "IMAGES lists the $g image more than once" ;; esac
    case " $groups " in *" $g "*) ;; *) fail "IMAGES lists the $g image, which a $KIND release does not publish" ;; esac
    seen="$seen$g "
    # 版本 tag 当前指向的索引必须就是清单里的那个：匿名读取，与回读 job 一样不登录 registry。
    current=$(docker buildx imagetools inspect --format '{{.Manifest.Digest}}' "$ref") || fail "cannot read $ref from the registry"
    [ "$current" = "${line#*@}" ] || fail "$ref points to $current, but IMAGES lists ${line#*@}"
  done < "$DIR/IMAGES"
  for g in $groups; do
    case $seen in *" $g "*) ;; *) fail "IMAGES of release $VERSION lacks the $g image; a $KIND release publishes it" ;; esac
  done

  # 只发 hub 的 release 原样带着绑定版本的两个 agent 安装脚本：取回 Release 页面上的字节，核对它们
  # 等于绑定版本已验签清单里的摘要；本版清单里的那两行由 boundagent 读上面已验签的 SHA256SUMS 核对。
  if [ "$KIND" = hub-only ]; then
    gh release download "$VERSION" -p install.sh -p install-macos.sh -D "$DIR" ||
      fail "download the agent installers of release $VERSION"
    go run ./scripts/boundagent readback -version "$VERSION" -bound "$AGENT_VERSION" -dir "$DIR" ||
      fail "release $VERSION's agent installers are not the bound agent $AGENT_VERSION's"
  fi
  echo "release $VERSION read back: signed SHA256SUMS, AGENT_VERSION $AGENT_VERSION, IMAGES for $groups"
  ;;

*)
  echo "usage: release-manifest.sh record KIND | write | readback" >&2
  exit 2
  ;;
esac
