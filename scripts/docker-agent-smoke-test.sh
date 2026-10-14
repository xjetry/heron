#!/bin/sh
# agent 镜像冒烟的输入检查（make script-test 调用）：scripts/docker-agent-smoke.sh 只接受显式输入，缺任一个、给的不是
# 规定形状、引用的镜像不在本机、或回读环境里留着旧的版本 tag，都必须在起任何容器之前失败。docker 桩只回应两种只读
# 查询（image ls、image inspect），其余调用一律记下并失败；每例核对退出码、报错文字，以及没有走到只读查询之外的调用。
# 冒烟本身的通过路径由 make docker（真实的 hub 与 agent 镜像）覆盖。
set -eu
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0
repo=reg.example/heron-agent
hub_id=sha256:$(printf 'c%.0s' $(seq 64))
platform_ref=$repo@sha256:$(printf '1%.0s' $(seq 64))

mkdir "$work/bin"
# docker 桩：image ls -q <引用> 打印 $STALE（模拟本机已有这个 tag）；image inspect <引用> 在引用列于 $MISSING 时失败。
# 其余子命令记为越过了输入检查。
cat > "$work/bin/docker" << 'STUB'
#!/bin/sh
echo "$*" >> "$CALLS"
case "$1 $2" in
  "image ls") [ -z "${STALE:-}" ] || echo "$STALE" ;;
  "image inspect")
    for m in ${MISSING:-}; do
      [ "$m" != "$3" ] || exit 1
    done
    ;;
  *) echo "docker stub: called past the input checks: $*" >&2; exit 97 ;;
esac
STUB
chmod +x "$work/bin/docker"

# rejects NAME WANT_TEXT VAR=VALUE…：在桩上运行一次（只带给出的变量），必须非 0 退出、输出含 WANT_TEXT，且 docker
# 只收到过 image ls 与 image inspect。
rejects() {
  name=$1 want=$2
  shift 2
  : > "$work/calls"
  rc=0
  env -i PATH="$work/bin:/usr/bin:/bin" CALLS="$work/calls" "$@" sh "$here/docker-agent-smoke.sh" > "$work/out" 2>&1 || rc=$?
  if [ "$rc" = 0 ] || ! grep -qF -- "$want" "$work/out"; then
    echo "FAIL: $name: exit $rc, want a failure containing '$want'" >&2
    sed 's/^/    /' "$work/out" >&2
    failures=$((failures + 1))
  fi
  if grep -v '^image \(ls\|inspect\) ' "$work/calls" > "$work/past"; then
    echo "FAIL: $name: ran docker past the input checks:" >&2
    sed 's/^/    /' "$work/past" >&2
    failures=$((failures + 1))
  fi
}

ok="IMAGE=$repo:v1.2.3 HUB_IMAGE=$hub_id VERSION=v1.2.3 TOOL_IMAGE=tool"
# shellcheck disable=SC2086 # 变量串按词拆成 env 的赋值参数
{
  rejects "no IMAGE" "IMAGE is required" HUB_IMAGE=$hub_id VERSION=v1.2.3 TOOL_IMAGE=tool
  rejects "no HUB_IMAGE" "HUB_IMAGE is required" IMAGE=$repo:v1.2.3 VERSION=v1.2.3 TOOL_IMAGE=tool
  rejects "no VERSION" "VERSION is required" IMAGE=$repo:v1.2.3 HUB_IMAGE=$hub_id TOOL_IMAGE=tool
  rejects "no TOOL_IMAGE" "TOOL_IMAGE is required" IMAGE=$repo:v1.2.3 HUB_IMAGE=$hub_id VERSION=v1.2.3
  rejects "empty HUB_IMAGE" "HUB_IMAGE is required" IMAGE=$repo:v1.2.3 HUB_IMAGE= VERSION=v1.2.3 TOOL_IMAGE=tool
  # hub 只能按内容寻址的镜像 ID 给出：tag 会被本机或 registry 上同名的旧镜像顶替。
  rejects "hub given by tag" "is not an image ID" IMAGE=$repo:v1.2.3 HUB_IMAGE=reg.example/heron-hub:v1.2.3 VERSION=v1.2.3 TOOL_IMAGE=tool
  rejects "hub ID too short" "is not an image ID" IMAGE=$repo:v1.2.3 HUB_IMAGE=sha256:abc VERSION=v1.2.3 TOOL_IMAGE=tool
  rejects "hub ID not hex" "is not an image ID" IMAGE=$repo:v1.2.3 "HUB_IMAGE=sha256:$(printf 'z%.0s' $(seq 64))" VERSION=v1.2.3 TOOL_IMAGE=tool
  # 受测引用只能是 check 装入的 <仓库>:<VERSION> 或 verify 按 digest 拉回的 <仓库>@sha256:…。
  rejects "agent given by another tag" "is neither $repo:v1.2.3" IMAGE=$repo:latest HUB_IMAGE=$hub_id VERSION=v1.2.3 TOOL_IMAGE=tool
  # 回读环境里本机已有版本 tag：不是干净环境。
  rejects "stale version tag before the readback smoke" "$repo:v1.2.3 already exists locally" \
    IMAGE=$platform_ref HUB_IMAGE=$hub_id VERSION=v1.2.3 TOOL_IMAGE=tool STALE=sha256:deadbeef
  # 引用的镜像不在本机：不拉取，直接失败。
  rejects "agent image not local" "$repo:v1.2.3 is not in the local image store" $ok "MISSING=$repo:v1.2.3"
  rejects "hub image not local" "hub image $hub_id is not in the local image store" $ok "MISSING=$hub_id"
  # 期望的执行环境种类拼错：断言会永远不成立，在输入检查里拒绝。
  rejects "unknown expected scope kind" "SMOKE_EXPECT_SCOPE_KIND 'SCOPE_KIND_HOST' is not a scope kind" $ok SMOKE_EXPECT_SCOPE_KIND=SCOPE_KIND_HOST
}

# 对照：两种合法的输入形状都越过输入检查，走到第一条起容器的调用（桩在那里失败）。上面的拒绝因而落在输入检查上，
# 不是桩对任何输入都失败。
for image in "$repo:v1.2.3" "$platform_ref"; do
  : > "$work/calls"
  env -i PATH="$work/bin:/usr/bin:/bin" CALLS="$work/calls" IMAGE="$image" HUB_IMAGE="$hub_id" VERSION=v1.2.3 \
    TOOL_IMAGE=tool SMOKE_EXPECT_SCOPE_KIND=HOST sh "$here/docker-agent-smoke.sh" > "$work/out" 2>&1 || true
  if ! grep -q 'called past the input checks' "$work/out"; then
    echo "FAIL: valid inputs ($image) did not get past the input checks" >&2
    sed 's/^/    /' "$work/out" >&2
    failures=$((failures + 1))
  fi
done

if [ "$failures" != 0 ]; then
  echo "docker-agent-smoke input checks: $failures failure(s)" >&2
  exit 1
fi
echo "docker-agent-smoke input checks ok"
