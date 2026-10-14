#!/bin/sh
# 在有 systemd 的 Linux 主机上按生产方式装 agent，run.sh 的 machine（OrbStack 机器）与 systemd（本机，CI 运行器）
# 两种形态共用。以 root 运行：
#   install-agent.sh <ca.pem> <dist-url> <hub-ip> <relay-port> <key> <name> <probe-allow-prefix> [<gomaxprocs>]
# hub.wirebench.test 写进 /etc/hosts 指向 hub-ip（relay 发布的地址），测试 CA 进系统信任库（update-ca-certificates），
# 再用 dist-url 下的 install.sh 装好 agent 与更新器、注册到 https://hub.wirebench.test:<relay-port>。
# probe-allow-prefix 是探测目标所在的前缀：目标是 hub 发布到宿主回环或 OrbStack 约定地址上的端口，都落在默认拒绝
# 集里，由宿主机本地策略放行（规则见 prober.Policy）。gomaxprocs 非空时以 drop-in 给 agent 与更新器两个单元设 GOMAXPROCS。
set -eu
[ "$#" -ge 7 ] || { echo "usage: install-agent.sh <ca.pem> <dist-url> <hub-ip> <relay-port> <key> <name> <probe-allow-prefix> [<gomaxprocs>]" >&2; exit 2; }
ca=$1; dist=$2; hub_ip=$3; relay_port=$4; key=$5; name=$6; allow=$7; gomaxprocs=${8:-}
[ "$(id -u)" = 0 ] || { echo "install-agent.sh must run as root" >&2; exit 1; }
[ -d /run/systemd/system ] || { echo "install-agent.sh needs a systemd host" >&2; exit 1; }

grep -q ' hub\.wirebench\.test$' /etc/hosts || echo "$hub_ip hub.wirebench.test" >> /etc/hosts
if ! command -v update-ca-certificates > /dev/null 2>&1 || ! command -v curl > /dev/null 2>&1; then
  apt-get update -qq && apt-get install -y -qq ca-certificates curl > /dev/null
fi
cp "$ca" /usr/local/share/ca-certificates/wirebench-test-root.crt
update-ca-certificates > /dev/null
curl -fsSL -o /root/wirebench-install.sh "$dist/install.sh"
sh /root/wirebench-install.sh --base-url "$dist" --hub "https://hub.wirebench.test:$relay_port" --key "$key" --name "$name"
if [ -n "$gomaxprocs" ]; then
  for unit in heron-agent heron-updater-agent; do
    mkdir -p "/etc/systemd/system/$unit.service.d"
    printf '[Service]\nEnvironment=GOMAXPROCS=%s\n' "$gomaxprocs" > "/etc/systemd/system/$unit.service.d/wirebench.conf"
  done
  systemctl daemon-reload
fi
/usr/local/bin/heron-agent configure --config /etc/heron-agent/config.json --probe-allow "$allow"
systemctl restart heron-updater-agent heron-agent
systemctl is-active heron-agent heron-updater-agent
