#!/bin/sh
# 上报线上字节与常驻内存的一次完整运行。字节口径：agent v0.11.2 稳态约 1.1 KB/上报；常驻内存在 linux/amd64、每 60 秒读 /proc/<pid>/status 时，agent VmRSS 约 17.5–21.6 MB（峰值 22.5 MB），常驻更新器约 18.5–19.1 MB。hub 一侧全部在本机 Docker 的一个网络里：
#
#   agent ──► relay（外侧计数，TLS 记录）──► 反代（Caddy 或 nginx，终止 TLS，对 agent 讲 HTTP/2）
#         ──► tap（内侧计数，HTTP/1.1 解码）──► hub
#
# agent 有三种形态（AGENT）：
#   container  Docker 容器里的 agent，以 nobody 运行，经网络别名 hub.wirebench.test 连到 relay，经 Go 标准的
#              SSL_CERT_FILE 信任本轮现生成的测试 CA，不改 agent 代码。
#   machine    OrbStack 机器（真实启动的 Debian 12，有 systemd）里用发布资产 DIST 的 install.sh 真装：agent 以
#              专用用户在加固的 systemd 单元里运行，在线更新的更新器作为 root 服务常驻——更新器只在 systemd 下
#              启动（internal/update/system_linux.go 的 serve），容器形态没有它，上报里的更新状态也就不同。
#              relay 发布到本机回环 RELAY_PORT，机器经 host.orb.internal 连它，测试 CA 进系统信任库。
#   systemd    与 machine 相同的真装，装在运行本脚本的这台 Linux 主机自己身上（CI 的一次性运行器）：改本机的
#              /etc/hosts 与系统信任库、以 sudo 运行安装脚本，结束时 --uninstall --purge。只在一次性主机上用。
#   host       本机（darwin）直接运行本树构建的 agent，经回环明文连 hub 的管理端口：在 macOS 上让 agent 信任
#              测试 CA 要改用户的钥匙串，这里不做；线上字节不在这个形态下测，只测常驻内存。
# hub、container 与 host 形态的 agent 由本树按发布目标的参数构建：CGO_ENABLED=0，其余参数（-trimpath、去符号、
# ldflags 注入版本）经 make release-goflags 与 hub-goflags 取自 deploy/agent.mk 与 Makefile 的 HUB_GOFLAGS，不在
# 这里另抄一份。版本号默认取 git describe：它进 Facts.agent_version 与每次上报都带的 UpdateStatus.version，长度
# 直接计入字节；发布 tag 上是 v0.11.1 这样的形状，之后的提交会长出 -<n>-g<hash>。machine 形态装的是 DIST 里的
# 发布资产（install.sh 只认它写进去的摘要）。
#
# 阶段（PHASES）按顺序执行，每个阶段的起点写进 phases.txt，analyze 据此分组：
#   <n>:<秒>    把本节点的 TCP 探测任务数调到 n（间隔取 probelimit.MinIntervalS，当前 5s，即满速），保持这么久；
#   nic:<秒>    给 agent 容器接上第二张网卡，Facts 的诊断（网卡集合）随之变化，hub 要回 Facts（只限 container）；
#   hold:<秒>   什么都不改，只等。
# RSS_EVERY 非零时每隔这么多秒采一次常驻内存：Linux 形态读 /proc/<pid>/status（rss.sh），host 形态读 ps 与
# footprint（rss-darwin.sh）。两者都只采按身份给出的 pid（容器里 agent 自己写下的 pid、systemd 单元的 MainPID、
# host 形态 agent 的子进程号），采到的 pid 记进 env.txt 的 rss_pids 行。AGENT_GOMAXPROCS 非空时给 agent（machine 形态连同更新器）设这个环境变量。
#
# 每个阶段开始时（动作之前）与 agent 停下之后，traffic.txt 记一行 hub 此刻对本节点累计记下的字节（GetTraffic），
# analyze 把它与 tap 里 agent 报的计数按同一组上报对齐，列出每段里 hub 没记上的字节（nic 阶段的网卡集合变化看这里）。
#
# 产物在 OUT（默认 build/wirebench/runs/<RUN>）：relay.jsonl、tap.jsonl、phases.txt、traffic.txt、proxy.log、hub.log、
# agent.log、env.txt、rss.txt、analysis.md、exchanges.jsonl。日志里没有凭据：tap 只记字段尺寸，agent 配置留在
# 容器与机器里（host 形态在 OUT/secret），管理密码与会话 cookie 只在 OUT/secret（0700，运行结束删除）。
set -eu
cd "$(cd "$(dirname "$0")/../.." && pwd)"
# 提交号与 VERSION 的默认值只能描述这棵树自己（provenance.sh）；树解在别的检出里面时这里就停下。
. scripts/wirebench/provenance.sh
tree_check .
commit_line=$(tree_commit)

PROXY=${PROXY:-caddy}
TTL=${TTL:-30s}
AGENT=${AGENT:-container}
PHASES=${PHASES:-0:150 nic:40 8:150 64:150}
RUN=${RUN:-$PROXY-$TTL}
# 本机端口都在 18600–18619 之内：hub 的管理接口（ADMIN_PORT），machine 形态另用 relay（RELAY_PORT）与发布资产
# 的下载服务（DIST_PORT）。
ADMIN_PORT=${ADMIN_PORT:-18600}
RELAY_PORT=${RELAY_PORT:-$((ADMIN_PORT + 1))}
DIST_PORT=${DIST_PORT:-$((ADMIN_PORT + 2))}
DIST=${DIST:-build/wirebench/release-v0.11.1}
OUT=${OUT:-build/wirebench/runs/$RUN}
if [ -z "${VERSION:-}" ]; then VERSION=$(tree_version); fi
AGENT_GOMAXPROCS=${AGENT_GOMAXPROCS:-}
RSS_EVERY=${RSS_EVERY:-0}
# 镜像按 digest 固定：反代的版本决定它的 TLS 栈与 HTTP/2 实现，换版本即换被测对象。
CADDY_IMAGE=${CADDY_IMAGE:-caddy:2@sha256:f2a1290d0463aad60660d4ec134943f183ee2a5f6c3eb7bf32dd984f2f020772}
NGINX_IMAGE=${NGINX_IMAGE:-nginx:stable@sha256:9bf97bd7714f5e24c1ccd545ecb9eb5435cb6d109c97cebb15e7e455e0239edb}
# 容器形态的 agent 与各中继的基础镜像：二进制都是静态的，镜像只决定 agent 报的系统名（/etc/os-release）。按 tag 引用、
# 以本机架构的变体运行，实际用到的镜像 ID 与 debian_version 记进 env.txt。
BASE_IMAGE=${BASE_IMAGE:-debian:bookworm-slim}
MACHINE_IMAGE=${MACHINE_IMAGE:-debian:12}
HUB_NAME=hub.wirebench.test

case "$PROXY" in caddy|nginx) ;; *) echo "PROXY must be caddy or nginx" >&2; exit 2;; esac
case "$AGENT" in container|machine|systemd|host) ;; *) echo "AGENT must be container, machine, systemd or host" >&2; exit 2;; esac
if [ "$AGENT" = systemd ] && { [ "$(uname -s)" != Linux ] || [ ! -d /run/systemd/system ]; }; then
  echo "AGENT=systemd installs onto this host and needs Linux with systemd" >&2; exit 2
fi
case "$RUN" in *[!A-Za-z0-9_.-]*) echo "RUN may only contain [A-Za-z0-9_.-]" >&2; exit 2;; esac
case "$VERSION" in *[!A-Za-z0-9_.-]*|'') echo "VERSION may only contain [A-Za-z0-9_.-]" >&2; exit 2;; esac
case "$AGENT_GOMAXPROCS" in *[!0-9]*) echo "AGENT_GOMAXPROCS must be a number" >&2; exit 2;; esac
p=wirebench-$RUN
rm -rf "$OUT"
# container 形态的 agent 以 nobody 写 logs/agent.log，其余容器以 root 写；目录对所有容器可写。
mkdir -p "$OUT/certs" "$OUT/hub" "$OUT/logs" "$OUT/secret" "$OUT/bin"
chmod 777 "$OUT/logs"
chmod 700 "$OUT/secret"
OUT=$(cd "$OUT" && pwd)
BIN_DIR=$OUT/bin
SCRIPTS=$(pwd)/scripts/wirebench

arch=$(docker version --format '{{.Server.Arch}}')
agent_version=$(sed -n 1p AGENT_VERSION)
# release_build <GOOS> <GOARCH> <make 目标> <go build 的其余参数>：make 打印的是发布配方交给 shell 的那段参数原文
# （VERSION 已过 check_version，只含 [A-Za-z0-9_.-]），eval 按同一 shell 词法拆成参数，hub 那段里的 $AGENT_VERSION
# 在这里展开。平台经 env 交给 go：写在函数调用前的赋值是否导出给函数里的命令，POSIX 没有规定。
release_build() {
  goos=$1
  goarch=$2
  flags=$(make --no-print-directory -s "$3" VERSION="$VERSION")
  shift 3
  # shellcheck disable=SC2034 # 由下一行 eval 的参数文字引用（hub-goflags 打印的 $AGENT_VERSION）
  AGENT_VERSION=$agent_version
  eval "set -- $flags \"\$@\""
  env GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build "$@"
}
release_build linux "$arch" release-goflags -o "$BIN_DIR/heron-agent" ./cmd/agent
release_build linux "$arch" hub-goflags -o "$BIN_DIR/heron-hub" ./cmd/hub
GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -o "$BIN_DIR/wirebench" ./scripts/wirebench
go build -o "$BIN_DIR/wirebench-host" ./scripts/wirebench
if [ "$AGENT" = host ]; then
  release_build "$(go env GOHOSTOS)" "$(go env GOHOSTARCH)" release-goflags -o "$BIN_DIR/heron-agent-host" ./cmd/agent
fi

machine=""
systemd_installed=""
host_agent=""
rss_pid=""
dist_pid=""
# 只回收本轮的东西：容器与网络带 wirebench-<RUN> 前缀与标签，机器名同一前缀，后台进程逐个登记。
cleanup() {
  for pid in $rss_pid $host_agent $dist_pid; do
    kill "$pid" 2> /dev/null || true
    wait "$pid" 2> /dev/null || true
  done
  if [ -n "$machine" ]; then
    case "$machine" in wirebench-*) orb delete -f "$machine" > /dev/null 2>&1 || true;; esac
  fi
  if [ -n "$systemd_installed" ]; then
    sudo sh "$DIST/install.sh" --uninstall --purge > /dev/null 2>&1 || true
  fi
  ids=$(docker ps -aq --filter "label=wirebench.run=$RUN") || true
  if [ -n "$ids" ]; then
    # shellcheck disable=SC2086 # 容器 id 列表按空白分词是有意的
    docker rm -f $ids > /dev/null || true
  fi
  docker network rm "$p" "$p-aux" > /dev/null 2>&1 || true
  # 二进制每轮约 70 MB，结论只依赖日志；env.txt 里记了 hub 与 agent 的摘要，需要时按同一提交重建。
  rm -rf "$OUT/secret" "$BIN_DIR"
}
trap cleanup EXIT
trap 'exit 1' INT TERM HUP

now_ns() { python3 -c 'import time; print(time.time_ns())'; }
phase() { echo "$(now_ns) $1" >> "$OUT/phases.txt"; echo "phase $1"; }
# unit_pids [命令前缀...]：agent 与常驻更新器两个单元的 MainPID，空格分隔。前缀是在哪台机器上执行 systemctl（machine
# 形态为 orb -m <机器>，本机为空）。单元没在运行时 MainPID 为 0，直接失败：拿 0 去采样等于什么都没采。
unit_pids() {
  out=""
  for unit in heron-agent heron-updater-agent; do
    pid=$("$@" systemctl show -p MainPID --value "$unit")
    case "$pid" in ''|0|*[!0-9]*) echo "FAIL: $unit MainPID '$pid'" >&2; return 1;; esac
    out="$out $pid"
    echo "rss_pids $unit=$pid" >> "$OUT/env.txt"
  done
  echo "${out# }"
}

{
  echo "run=$RUN agent=$AGENT proxy=$PROXY ttl=$TTL phases=$PHASES agent_gomaxprocs=${AGENT_GOMAXPROCS:-unset} version=$VERSION bound_agent=$agent_version"
  echo "$commit_line go=$(go env GOVERSION) proxy_image=$(if [ "$PROXY" = caddy ]; then echo "$CADDY_IMAGE"; else echo "$NGINX_IMAGE"; fi)"
  echo "date_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "host=$(uname -srm)"
  docker version --format 'docker_server={{.Server.Version}} {{.Server.Os}}/{{.Server.Arch}}'
  docker info --format 'docker_kernel={{.KernelVersion}} ncpu={{.NCPU}} mem={{.MemTotal}}'
  for b in heron-hub heron-agent; do
    if command -v sha256sum > /dev/null 2>&1; then sum=$(sha256sum "$BIN_DIR/$b"); else sum=$(shasum -a 256 "$BIN_DIR/$b"); fi
    echo "$b: ${sum%% *}"
  done
} > "$OUT/env.txt"

docker network create --label "wirebench.run=$RUN" "$p" > /dev/null
docker network create --label "wirebench.run=$RUN" "$p-aux" > /dev/null
subnet=$(docker network inspect "$p" --format '{{(index .IPAM.Config 0).Subnet}}')

"$BIN_DIR/wirebench-host" certs -dir "$OUT/certs" -names "$HUB_NAME"
cat "$OUT/certs/chain-sizes.txt" >> "$OUT/env.txt"

run_ctr() {
  name=$1; shift
  docker run -d --name "$p-$name" --label "wirebench.run=$RUN" --network "$p" "$@" > /dev/null
}

# hub：反代之后的部署形状。--trusted-proxies 写本网络的网段，hub 才采信 tap 转来的 X-Forwarded-For。
run_ctr hub -p "127.0.0.1:$ADMIN_PORT:18600" -v "$BIN_DIR:/wb:ro" -v "$OUT/hub:/data" "$BASE_IMAGE" \
  /wb/heron-hub serve --db /data/hub.db --listen 0.0.0.0:18600 --offline-after "$TTL" --trusted-proxies "$subnet" --timezone UTC
base="http://127.0.0.1:$ADMIN_PORT"
i=0
until [ "$(curl -s -o /dev/null -w '%{http_code}' "$base/heron.v1.PublicService/GetSite?connect=v1&encoding=json&message=%7B%7D")" = 200 ]; do
  i=$((i + 1)); [ "$i" -lt 100 ] || { echo "FAIL: hub did not come up"; docker logs "$p-hub"; exit 1; }
  sleep 0.2
done
hub_ip=$(docker inspect "$p-hub" --format "{{(index .NetworkSettings.Networks \"$p\").IPAddress}}")
echo "base_image=$BASE_IMAGE id=$(docker inspect "$p-hub" --format '{{.Image}}') debian_version=$(docker exec "$p-hub" cat /etc/debian_version)" >> "$OUT/env.txt"

run_ctr tap -v "$BIN_DIR:/wb:ro" -v "$OUT/logs:/out" "$BASE_IMAGE" \
  /wb/wirebench relay -listen :18603 -target "$p-hub:18600" -log /out/tap.jsonl -http
case "$PROXY" in
  caddy)
    sed "s/@TAP@/$p-tap:18603/" "$SCRIPTS/Caddyfile" > "$OUT/Caddyfile"
    run_ctr proxy -v "$OUT/Caddyfile:/etc/caddy/Caddyfile:ro" -v "$OUT/certs:/certs:ro" "$CADDY_IMAGE"
    ;;
  nginx)
    sed "s/@TAP@/$p-tap:18603/" "$SCRIPTS/nginx.conf" > "$OUT/nginx.conf"
    run_ctr proxy -v "$OUT/nginx.conf:/etc/nginx/nginx.conf:ro" -v "$OUT/certs:/certs:ro" "$NGINX_IMAGE"
    ;;
esac
# relay 占用 hub 的对外名字：容器里的 agent 解析 hub.wirebench.test 得到的是它，它再连反代。
relay_publish=""
case "$AGENT" in machine|systemd) relay_publish="-p 127.0.0.1:$RELAY_PORT:18601";; esac
# shellcheck disable=SC2086 # relay_publish 是空或一组 -p 参数，按空白分词是有意的
run_ctr relay --network-alias "$HUB_NAME" $relay_publish -v "$BIN_DIR:/wb:ro" -v "$OUT/logs:/out" "$BASE_IMAGE" \
  /wb/wirebench relay -listen :18601 -target "$p-proxy:443" -log /out/relay.jsonl -tls

# 管理密码与会话只在本轮的 secret 目录里；密码每轮随机。
admin_pw=$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')
printf '%s\n' "$admin_pw" | docker exec -i "$p-hub" /wb/heron-hub passwd --db /data/hub.db > /dev/null
jar="$OUT/secret/jar"
rpc() {
  name=$1; body=$2
  curl -sS -o "$OUT/secret/$name.json" -w '%{http_code}' -H 'Content-Type: application/json' \
    -b "$jar" -c "$jar" --data "$body" "$base/heron.v1.AdminService/$name"
}
[ "$(rpc Login "$(jq -nc --arg password "$admin_pw" '{password: $password}')")" = 200 ] || { echo "FAIL: login"; exit 1; }

key=$(docker exec "$p-hub" /wb/heron-hub window open --db /data/hub.db --ttl 10m --max 1 | sed -n 's/^key: //p')
[ -n "$key" ] || { echo "FAIL: no registration key"; exit 1; }

# 各形态的探测目标都是 agent 能连到的 hub 管理端口；target 是本形态下 agent 看到的地址。
case "$AGENT" in
container)
  target="$hub_ip:18600"
  # agent 以 nobody 运行，与安装脚本建的专用非 root 用户同样没有特权。
  agent_env="-e SSL_CERT_FILE=/certs/ca.pem"
  if [ -n "$AGENT_GOMAXPROCS" ]; then agent_env="$agent_env -e GOMAXPROCS=$AGENT_GOMAXPROCS"; fi
  # shellcheck disable=SC2086 # agent_env 是 -e 参数列表，按空白分词是有意的
  run_ctr agent --user 65534:65534 $agent_env -v "$BIN_DIR:/wb:ro" -v "$OUT/certs:/certs:ro" -v "$OUT/logs:/out" -v "$SCRIPTS:/scripts:ro" \
    "$BASE_IMAGE" sleep infinity
  docker exec "$p-agent" /wb/heron-agent register --hub "https://$HUB_NAME:18601" --key "$key" --config /tmp/agent.json --name "$p-agent" > "$OUT/logs/register.log" 2>&1 ||
    { echo "FAIL: register"; cat "$OUT/logs/register.log"; exit 1; }
  docker exec "$p-agent" /wb/wirebench gomaxprocs >> "$OUT/env.txt"
  docker exec "$p-agent" sh -c 'echo "agent_env nproc=$(nproc) pagesize=$(getconf PAGESIZE) kernel=$(uname -r)"' >> "$OUT/env.txt"
  phase start
  # 写下的 $$ 就是 exec 之后 agent 的 pid（exec 不换进程号）。
  docker exec -d "$p-agent" sh -c 'echo $$ > /tmp/agent.pid && exec /wb/heron-agent run --config /tmp/agent.json > /out/agent.log 2>&1'
  if [ "$RSS_EVERY" != 0 ]; then
    i=0
    until agent_pid=$(docker exec "$p-agent" cat /tmp/agent.pid 2> /dev/null) && [ -n "$agent_pid" ]; do
      i=$((i + 1)); [ "$i" -lt 50 ] || { echo "FAIL: agent did not record its pid"; exit 1; }
      sleep 0.2
    done
    echo "rss_pids heron-agent=$agent_pid" >> "$OUT/env.txt"
    # shellcheck disable=SC2016 # $1、$2 在容器里的 sh 展开
    docker exec -d "$p-agent" sh -c 'exec /scripts/rss.sh "$1" "$2" > /out/rss.txt 2>&1' _ "$RSS_EVERY" "$agent_pid"
  fi
  ;;
machine)
  machine=$p
  [ -f "$DIST/install.sh" ] || { echo "DIST=$DIST has no install.sh" >&2; exit 1; }
  # 下载服务只监听回环；机器经 host.orb.internal 访问本机回环。exec 让 $dist_pid 就是 python 本身。
  (cd "$DIST" && exec python3 -m http.server "$DIST_PORT" --bind 127.0.0.1) > "$OUT/logs/dist.log" 2>&1 &
  dist_pid=$!
  orb create -a "$arch" "$MACHINE_IMAGE" "$machine" > "$OUT/logs/machine-create.log" 2>&1 ||
    { echo "FAIL: orb create"; cat "$OUT/logs/machine-create.log"; exit 1; }
  # 机器把 macOS 的 /Users 挂在同一路径下：证书与脚本直接按本机路径读。机器经 host.orb.internal 访问本机回环，
  # 那个地址就是 relay 与下载服务在机器里看到的地址。
  host_ip=$(orb -m "$machine" sh -c "getent ahostsv4 host.orb.internal | awk '{print \$1; exit}'")
  [ -n "$host_ip" ] || { echo "FAIL: host.orb.internal does not resolve in the machine"; exit 1; }
  orb -m "$machine" -u root sh "$SCRIPTS/install-agent.sh" "$OUT/certs/ca.pem" "http://host.orb.internal:$DIST_PORT" "$host_ip" "$RELAY_PORT" \
    "$key" "$p-agent" "$host_ip/32" "$AGENT_GOMAXPROCS" > "$OUT/logs/install.log" 2>&1 ||
    { echo "FAIL: machine install"; cat "$OUT/logs/install.log"; exit 1; }
  target="$host_ip:$ADMIN_PORT"
  orb -m "$machine" env "GOMAXPROCS=$AGENT_GOMAXPROCS" "$BIN_DIR/wirebench" gomaxprocs >> "$OUT/env.txt"
  # shellcheck disable=SC2016 # 命令替换在机器里展开
  orb -m "$machine" sh -c 'echo "agent_env nproc=$(nproc) pagesize=$(getconf PAGESIZE) kernel=$(uname -r) systemd=$(systemctl --version | head -n 1)"' >> "$OUT/env.txt"
  phase start
  if [ "$RSS_EVERY" != 0 ]; then
    pids=$(unit_pids orb -m "$machine") || exit 1
    # shellcheck disable=SC2086 # pids 是空格分隔的进程号列表，按空白分词是有意的
    orb -m "$machine" -u root sh "$SCRIPTS/rss.sh" "$RSS_EVERY" $pids > "$OUT/logs/rss.txt" 2>&1 &
    rss_pid=$!
  fi
  ;;
systemd)
  [ -f "$DIST/install.sh" ] || { echo "DIST=$DIST has no install.sh" >&2; exit 1; }
  DIST=$(cd "$DIST" && pwd)
  (cd "$DIST" && exec python3 -m http.server "$DIST_PORT" --bind 127.0.0.1) > "$OUT/logs/dist.log" 2>&1 &
  dist_pid=$!
  i=0
  until curl -fsS -o /dev/null "http://127.0.0.1:$DIST_PORT/install.sh"; do
    i=$((i + 1)); [ "$i" -lt 50 ] || { echo "FAIL: download server"; cat "$OUT/logs/dist.log"; exit 1; }
    sleep 0.2
  done
  systemd_installed=1
  # 重定向以调用者身份写本轮的 OUT（属调用者），只有安装本身需要 root。
  # shellcheck disable=SC2024
  sudo sh "$SCRIPTS/install-agent.sh" "$OUT/certs/ca.pem" "http://127.0.0.1:$DIST_PORT" 127.0.0.1 "$RELAY_PORT" \
    "$key" "$p-agent" 127.0.0.0/8 "$AGENT_GOMAXPROCS" > "$OUT/logs/install.log" 2>&1 ||
    { echo "FAIL: install"; cat "$OUT/logs/install.log"; exit 1; }
  target="127.0.0.1:$ADMIN_PORT"
  env "GOMAXPROCS=$AGENT_GOMAXPROCS" "$BIN_DIR/wirebench" gomaxprocs >> "$OUT/env.txt"
  echo "agent_env nproc=$(nproc) pagesize=$(getconf PAGESIZE) kernel=$(uname -r) systemd=$(systemctl --version | head -n 1)" >> "$OUT/env.txt"
  phase start
  if [ "$RSS_EVERY" != 0 ]; then
    pids=$(unit_pids) || exit 1
    # 读更新器（root）的 /proc/<pid>/status 要 root；输出照样写调用者的 OUT。kill 给 sudo 的信号由 sudo 转给 rss.sh。
    # pids 是空格分隔的进程号列表，按空白分词是有意的。
    # shellcheck disable=SC2024,SC2086
    sudo sh "$SCRIPTS/rss.sh" "$RSS_EVERY" $pids > "$OUT/logs/rss.txt" 2>&1 &
    rss_pid=$!
  fi
  ;;
host)
  target="127.0.0.1:$ADMIN_PORT"
  agent_cfg=$OUT/secret/agent.json
  "$BIN_DIR/heron-agent-host" register --hub "$base" --key "$key" --config "$agent_cfg" --name "$p-agent" --insecure-http > "$OUT/logs/register.log" 2>&1 ||
    { echo "FAIL: register"; cat "$OUT/logs/register.log"; exit 1; }
  # 目标是回环上的 hub：回环在默认拒绝集里，由宿主机本地策略放行（规则见 prober.Policy）。
  "$BIN_DIR/heron-agent-host" configure --config "$agent_cfg" --probe-allow 127.0.0.0/8 > /dev/null
  if [ -n "$AGENT_GOMAXPROCS" ]; then
    GOMAXPROCS=$AGENT_GOMAXPROCS "$BIN_DIR/wirebench-host" gomaxprocs >> "$OUT/env.txt"
  else
    "$BIN_DIR/wirebench-host" gomaxprocs >> "$OUT/env.txt"
  fi
  echo "agent_env ncpu=$(sysctl -n hw.ncpu) perf_cores=$(sysctl -n hw.perflevel0.physicalcpu) efficiency_cores=$(sysctl -n hw.perflevel1.physicalcpu) pagesize=$(sysctl -n vm.pagesize) macos=$(sw_vers -productVersion)" >> "$OUT/env.txt"
  phase start
  if [ -n "$AGENT_GOMAXPROCS" ]; then
    GOMAXPROCS=$AGENT_GOMAXPROCS "$BIN_DIR/heron-agent-host" run --config "$agent_cfg" > "$OUT/logs/agent.log" 2>&1 &
  else
    "$BIN_DIR/heron-agent-host" run --config "$agent_cfg" > "$OUT/logs/agent.log" 2>&1 &
  fi
  host_agent=$!
  if [ "$RSS_EVERY" != 0 ]; then
    echo "rss_pids heron-agent=$host_agent" >> "$OUT/env.txt"
    "$SCRIPTS/rss-darwin.sh" "$RSS_EVERY" "$host_agent" > "$OUT/logs/rss.txt" 2>&1 &
    rss_pid=$!
  fi
  ;;
esac

# 等节点上线并取到它的 id。
i=0
until [ "$(rpc ListNodes '{}')" = 200 ] && node=$(jq -er '.nodes[0].id' "$OUT/secret/ListNodes.json" 2>/dev/null); do
  i=$((i + 1)); [ "$i" -lt 60 ] || { echo "FAIL: node did not appear"; cat "$OUT/logs/agent.log" 2>/dev/null || true; exit 1; }
  sleep 1
done

# set_tasks n：把本节点的任务调到 n 个。只增不减（阶段按任务数递增排列），每个任务都是对 hub 管理端口的 TCP
# 连接，结果都是成功的 rtt。
tasks=0
set_tasks() {
  want=$1
  [ "$want" -ge "$tasks" ] || { echo "phases must not reduce the task count ($tasks -> $want)" >&2; exit 2; }
  body=$(jq -nc --arg node "$node" --arg target "$target" '{task: {kind: "PROBE_KIND_TCP", target: $target, intervalS: 5, timeoutMs: 1000}, nodeIds: [$node]}')
  while [ "$tasks" -lt "$want" ]; do
    [ "$(rpc SaveProbeTask "$body")" = 200 ] || { echo "FAIL: SaveProbeTask"; cat "$OUT/secret/SaveProbeTask.json"; exit 1; }
    tasks=$((tasks + 1))
  done
}

# traffic_snapshot <标签>：hub 此刻对本节点累计记下的字节，一行 "<unix 纳秒> <标签> <rx> <tx>"。proto3 JSON 省略
# 零值，没有的字段按 0 记。
traffic_snapshot() {
  [ "$(rpc GetTraffic '{}')" = 200 ] || { echo "FAIL: GetTraffic"; cat "$OUT/secret/GetTraffic.json"; exit 1; }
  totals=$(jq -er --arg node "$node" '.nodes[] | select(.nodeId == $node) | "\(.traffic.totalRx // "0") \(.traffic.totalTx // "0")"' "$OUT/secret/GetTraffic.json") ||
    { echo "FAIL: GetTraffic lists no node $node"; exit 1; }
  echo "$(now_ns) $1 $totals" >> "$OUT/traffic.txt"
}

for step in $PHASES; do
  what=${step%%:*}; secs=${step#*:}
  case "$what" in
    hold) label="hold-T$tasks" ;;
    nic)
      [ "$AGENT" = container ] || { echo "nic phases need AGENT=container" >&2; exit 2; }
      label="nic-T$tasks"
      ;;
    *) label="T$what" ;;
  esac
  phase "$label"
  traffic_snapshot "$label"
  case "$what" in
    hold) ;;
    nic) docker network connect "$p-aux" "$p-agent" ;;
    *) set_tasks "$what" ;;
  esac
  sleep "$secs"
done
phase end

# 先停 agent：连接由 agent 一侧关闭，relay 记下 close；再停两个中继，日志落盘。
case "$AGENT" in
  container) docker stop -t 5 "$p-agent" > /dev/null ;;
  machine) orb -m "$machine" -u root systemctl stop heron-agent heron-updater-agent ;;
  systemd) sudo systemctl stop heron-agent heron-updater-agent ;;
  host) kill "$host_agent"; wait "$host_agent" || true; host_agent="" ;;
esac
if [ -n "$rss_pid" ]; then kill "$rss_pid" 2> /dev/null || true; wait "$rss_pid" 2> /dev/null || true; rss_pid=""; fi
case "$AGENT" in
  machine) orb -m "$machine" -u root journalctl -u heron-agent -u heron-updater-agent --no-pager > "$OUT/logs/agent.log" 2>&1 || true ;;
  systemd)
    # shellcheck disable=SC2024 # 输出写调用者的 OUT
    sudo journalctl -u heron-agent -u heron-updater-agent --no-pager > "$OUT/logs/agent.log" 2>&1 || true ;;
esac
sleep 2
# agent 已停：最后一次上报已入账，这一行是本轮的终值。
traffic_snapshot stopped
docker stop -t 5 "$p-relay" "$p-tap" > /dev/null
docker logs "$p-proxy" > "$OUT/proxy.log" 2>&1
docker logs "$p-hub" > "$OUT/hub.log" 2>&1
cp "$OUT/logs/relay.jsonl" "$OUT/logs/tap.jsonl" "$OUT/"
for f in agent.log rss.txt install.log; do
  if [ -f "$OUT/logs/$f" ]; then cp "$OUT/logs/$f" "$OUT/"; fi
done
"$BIN_DIR/wirebench-host" analyze -relay "$OUT/relay.jsonl" -tap "$OUT/tap.jsonl" -phases "$OUT/phases.txt" -detail "$OUT/exchanges.jsonl" -traffic "$OUT/traffic.txt" > "$OUT/analysis.md"
echo "analysis: $OUT/analysis.md"
