#!/bin/sh
# 用真实内核核对网卡结构分类：交叉编译 collect 包的实测测试，在 privileged 容器里运行。实测条件：内核 7.0.14-orbstack，容器架构与本机一致。
# 容器平台与本机架构一致：openat2 在转译执行（OrbStack 的 amd64 经 Rosetta）下返回 ENOSYS。
# 依赖：Docker 能起 privileged 容器；容器内 apk 能联网装 iproute2。
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
# 容器名带进程号：同机并发的两次运行互不干扰；外层被中断时 EXIT 清理仍删掉还在跑的容器（--rm 只管正常退出）。
name="heron-netlive-$$"
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 1' INT TERM HUP
arch=$(go env GOARCH)
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -count=1 -c -tags netstructurelive -o "$work/collect.test" ./internal/agent/collect)
cp "$root/scripts/netstructure-live-setup.sh" "$work/setup.sh"
echo "docker server $(docker version --format '{{.Server.Version}} {{.Server.Os}}/{{.Server.Arch}}')"
docker run --rm --name "$name" --privileged --platform "linux/$arch" -v "$work:/w:ro" "${NETSTRUCTURE_IMAGE:-alpine:3.21}" sh /w/setup.sh
echo "net structure rules OK on the running kernel"
