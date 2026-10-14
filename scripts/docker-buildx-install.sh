#!/bin/sh
# 给 release.yml 里推送、回读、移动 latest 的三个 job 装上固定版本的 buildx（GitHub 的 linux/amd64 运行器）。
# 运行器自带的 buildx 随运行器镜像漂移，而 scripts/docker-image.sh 的 latest_state 认的是 v0.33.0 对不存在的
# tag 的报错文字。版本与 SHA-256 只写在这里；SHA-256 与 buildx 发布页的 checksums.txt、GitHub 资产的 digest
# 三处一致。装进 ~/.docker/cli-plugins 后核对 docker 实际调用的就是这一版：插件查找顺序不是预期的那样时失败。
set -eu
version=v0.33.0
sha256=9426a15411f35f635afef3f5d3bae53155c3e30d26dee430cc968e13d34be49f
dir=${RUNNER_TEMP:?RUNNER_TEMP is required (this script runs on GitHub Actions runners)}

curl -fsSL -o "$dir/docker-buildx" "https://github.com/docker/buildx/releases/download/$version/buildx-$version.linux-amd64"
echo "$sha256  $dir/docker-buildx" | sha256sum -c -
mkdir -p "$HOME/.docker/cli-plugins"
install -m 0755 "$dir/docker-buildx" "$HOME/.docker/cli-plugins/docker-buildx"
got=$(docker buildx version)
case $got in
  "github.com/docker/buildx $version "*) ;;
  *)
    echo "FAIL: docker buildx is '$got', want $version" >&2
    exit 1
    ;;
esac
echo "$got"
# docker 引擎与 CLI 用运行器自带的（引擎是运行器的系统服务，不能逐 job 替换），版本留在日志里。
docker version
