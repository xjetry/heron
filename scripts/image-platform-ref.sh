#!/bin/sh
# 把多平台镜像的引用解析成某一平台的清单引用：打印 <仓库>@<该平台清单的 digest>。
#   image-platform-ref.sh REF OS/ARCH      如 image-platform-ref.sh alpine:3.21@sha256:… linux/arm64
#
# 为什么不直接按索引 digest 运行：经典镜像存储里，<仓库>@<索引 digest> 这个引用只能对应一个本地镜像，
# 拉过一个平台之后再拉另一个平台会报 cannot overwrite digest；不给平台时 docker 则直接用已有的那一个，
# 可能是经模拟运行的别的架构。按平台清单的 digest 引用，各平台互不冲突，运行的也正是索引里的那一项。
set -eu
ref=${1:?usage: image-platform-ref.sh REF OS/ARCH}
platform=${2:?usage: image-platform-ref.sh REF OS/ARCH}
os=${platform%%/*}
arch=${platform#*/}
arch=${arch%%/*}

# 仓库名：去掉 @digest，再去掉最后一个 / 之后的 :tag（仓库主机可以带 :端口）。
repo=${ref%%@*}
case ${repo##*/} in
  *:*) repo=${repo%:*} ;;
esac

digests=$(docker buildx imagetools inspect --format "{{range .Manifest.Manifests}}{{if and (eq .Platform.OS \"$os\") (eq .Platform.Architecture \"$arch\")}}{{println .Digest}}{{end}}{{end}}" "$ref")
case $digests in
  sha256:*[!0-9a-f]*) ;;
  sha256:*)
    echo "$repo@$digests"
    exit 0
    ;;
esac
echo "FAIL: $ref does not list exactly one manifest for $os/$arch (got: $(echo "$digests" | tr '\n' ' '))" >&2
exit 1
