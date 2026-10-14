#!/bin/sh
# 构建器核对的回归检查（make ci 调用）：docker-builder.sh 在 docker 桩上逐例运行，不起构建器。桩按
# buildx v0.33.0 的 docker buildx inspect 输出格式打印（Driver: 与每个节点一行 Driver Options:）。
set -eu
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0
name=heron-hub-buildkit-test
image='moby/buildkit:v0.33.0@sha256:6c2fa84a6b61ccd72899dde4239f8d5717f05f9a8ca6f3cad185fb1a95a94de3'

mkdir "$work/bin"
# docker 桩：$STATE/inspect 是 inspect 的输出，文件不存在时按 buildx 的措辞报构建器不存在；$STATE/inspect-err
# 存在时以它的内容作为别的读取失败。每次调用追加到 $STATE/calls。
cat > "$work/bin/docker" << 'STUB'
#!/bin/sh
echo "$*" >> "$STATE/calls"
case "$1 $2" in
  "buildx inspect")
    if [ -e "$STATE/inspect-err" ]; then cat "$STATE/inspect-err" >&2; exit 1; fi
    if [ ! -e "$STATE/inspect" ]; then echo "ERROR: no builder \"$3\" found" >&2; exit 1; fi
    cat "$STATE/inspect"
    ;;
  "buildx create") ;;
  *) echo "docker stub: unexpected: $*" >&2; exit 99 ;;
esac
STUB
chmod +x "$work/bin/docker"

# builder DRIVER OPTS…：一个构建器的 inspect 输出，每个 OPTS 一个节点。
builder() {
  driver=$1
  shift
  printf 'Name:          %s\nDriver:        %s\nLast Activity: 2026-10-11 05:00:31 +0000 UTC\n\nNodes:\n' "$name" "$driver"
  n=0
  for o; do
    printf 'Name:                  %s%s\nEndpoint:              default\nDriver Options:        %s\nStatus:                running\n\n' "$name" "$n" "$o"
    n=$((n + 1))
  done
}

reset() {
  STATE=$(mktemp -d "$work/state.XXXXXX")
  export STATE
  : > "$STATE/calls"
}

# rejects CASE WANT_TEXT：以 1 失败、输出含 WANT_TEXT，且没有建构建器（同名的那个原样留着给人处理）。
rejects() {
  run "$1" 1 "$2"
  ! grep -q 'buildx create' "$STATE/calls" || { echo "FAIL: $1: a builder was created" >&2; failures=$((failures + 1)); }
}

# run CASE WANT_EXIT WANT_TEXT：运行一次，核对退出码与输出里的文字（WANT_TEXT 为空时只核对退出码）。
run() {
  rc=0
  env PATH="$work/bin:$PATH" "$here/docker-builder.sh" "$name" "$image" > "$STATE/out" 2>&1 || rc=$?
  if [ "$rc" != "$2" ] || { [ -n "$3" ] && ! grep -qF -- "$3" "$STATE/out"; }; then
    echo "FAIL: $1: exit $rc (want $2), want output containing '$3'" >&2
    sed 's/^/    /' "$STATE/out" >&2
    failures=$((failures + 1))
  fi
}

# 就是 Makefile 固定的那一个：沿用，不重建。
reset
builder docker-container "image=\"$image\"" > "$STATE/inspect"
run "pinned builder" 0 ""
! grep -q 'buildx create' "$STATE/calls" || { echo "FAIL: a matching builder was recreated" >&2; failures=$((failures + 1)); }

# 确认不存在：按固定的驱动与镜像创建。
reset
run "missing builder" 0 ""
grep -qx "buildx create --name $name --driver docker-container --driver-opt image=$image --bootstrap" "$STATE/calls" ||
  { echo "FAIL: a missing builder was not created with the pinned driver and image" >&2; failures=$((failures + 1)); }

# 同名而不是那一个：多带驱动选项、镜像不同、驱动不同、多个节点，一律拒绝，不沿用也不重建。
reset
builder docker-container "image=\"$image\" network=\"host\"" > "$STATE/inspect"
rejects "extra driver option" "has driver options 'image=\"$image\" network=\"host\"', want exactly image=\"$image\""
reset
builder docker-container "network=\"host\" image=\"$image\"" > "$STATE/inspect"
rejects "extra driver option first" "want exactly image="
reset
builder docker-container 'image="moby/buildkit:v0.33.0"' > "$STATE/inspect"
rejects "image without the pinned digest" "want exactly image="
reset
builder docker-container "" > "$STATE/inspect"
rejects "no driver options" "want exactly image="
reset
builder docker "image=\"$image\"" > "$STATE/inspect"
rejects "docker driver" "uses driver 'docker', want docker-container"
reset
builder docker-container "image=\"$image\"" "image=\"$image\"" > "$STATE/inspect"
rejects "two nodes" "has more than one node"

# inspect 的其他失败原样报出，不当作不存在而误建。
reset
echo 'ERROR: Cannot connect to the Docker daemon' > "$STATE/inspect-err"
rejects "inspect fails" "docker buildx inspect $name failed"

if [ "$failures" != 0 ]; then
  echo "builder checks: $failures failure(s)" >&2
  exit 1
fi
echo "builder checks ok"
