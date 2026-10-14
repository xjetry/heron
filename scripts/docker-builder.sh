#!/bin/sh
# 确保 make docker 用的构建器存在，且就是 Makefile 固定的那一个：docker-container 驱动、按 digest 固定的
# BuildKit 镜像、除此之外没有别的驱动选项、单个节点。
#   docker-builder.sh NAME IMAGE
#
# 构建器按名字查找。名字相同而驱动、镜像或驱动选项不同时（改了 BUILDKIT_IMAGE 的 digest 没改版本号、手工以
# 同名建过不带 image 或多带 network=host 的构建器），沿用它就不再是本地、CI 与发布共用的那一版 BuildKit，
# 所以核对后才沿用。scripts/docker-builder-test.sh 在 docker 桩上钉住这些判定。
set -eu
name=${1:?usage: docker-builder.sh NAME IMAGE}
image=${2:?usage: docker-builder.sh NAME IMAGE}

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

if ! out=$(docker buildx inspect "$name" 2>&1); then
  # 只有确认不存在才创建；buildx 对不存在的构建器报 no builder "<名字>" found（buildx v0.33.0 实测）。
  # 其他失败原样报出，不当作不存在：报错文字若随 buildx 版本变了，这里失败而不是误建。
  case $out in
    *"no builder \"$name\" found"*)
      exec docker buildx create --name "$name" --driver docker-container --driver-opt "image=$image" --bootstrap
      ;;
  esac
  echo "$out" >&2
  fail "docker buildx inspect $name failed"
fi

driver=$(printf '%s\n' "$out" | sed -n 's/^Driver: *//p')
opts=$(printf '%s\n' "$out" | sed -n 's/^Driver Options: *//p')
remedy="remove it with 'docker buildx rm $name' and re-run; make recreates it"
[ "$driver" = docker-container ] || fail "builder $name uses driver '$driver', want docker-container; $remedy"
[ "$(printf '%s\n' "$opts" | wc -l)" -eq 1 ] || fail "builder $name has more than one node; $remedy"
# 整串相等：make 建出的构建器只带 image 这一个驱动选项（buildx v0.33.0 打印为 image="<镜像>"）。多出的选项
# （network=host、另给的 buildkitd 配置等）改变构建节点的行为，同名也不是 Makefile 固定的那一个。
[ "$opts" = "image=\"$image\"" ] || fail "builder $name has driver options '$opts', want exactly image=\"$image\"; $remedy"
