#!/bin/sh
# agent 镜像冒烟：起一个 hub 容器，按 docs/install.md「安装 agent → Docker」的原样命令（一次性容器注册，镜像默认参数运行，不覆写
# 命令）起 agent 容器，经 hub 的管理接口确认它在线并上报。scripts/docker-image.sh 在 check（make docker-agent 装入
# 本机平台之后）与 verify（发布后回读按平台拉取之后）调用它。
#
# 分两层。受测平台是 docker 引擎的本机平台时（CI 的 amd64 运行器、发布后回读的 amd64）做完整验收：
#   - 对照组：从受测镜像本身提取同一份二进制，在 docker 宿主上以非 root（65532）作为普通宿主进程运行、注册为第二个
#     节点；容器节点报出的宿主读数必须与它一致（逐字段见 compare_nodes），执行环境的差别只能是容器节点多一条
#     "在容器中运行"的说明；
#   - 探测策略：以宿主的非回环地址为目标的 TCP 任务，两个节点都必须拒绝（hub 上只见 errors），同时以 hub 容器的
#     桥接地址为目标的对照任务两个节点都必须成功——拒绝因而来自本机地址策略，不是探测本身不通；
#   - 负面：去掉 --network host、去掉 -v /:/hostfs，agent 都必须以退出码 2 拒绝启动，日志带点名缺失参数的提示；
#   - ICMP：容器里的 agent 以 65532 运行，有效能力集含 CAP_NET_RAW（镜像二进制的文件能力，deploy/Dockerfile.agent），
#     容器节点报 icmp_available。
# 受测平台经模拟运行时（QEMU 下的 arm64）只核对镜像起得来、能注册、能上报：模拟器给不出宿主对照的意义。它的
# 有效能力集照样打印出来（模拟器经 binfmt 运行时文件能力是否生效取决于注册方式），不作断言。
#
# 宿主对照组的运行方式：脚本所在的机器不一定是 docker 宿主（Docker Desktop、OrbStack、DOCKER_HOST 指向别处），所以
# 对照 agent 不在脚本的机器上运行，而是经一个特权辅助容器（--privileged --pid host --cgroupns host，只给这个辅助
# 容器）以 nsenter -t 1 进入 docker 宿主 PID 1 的 mount、uts、ipc、net 命名空间，以 -S 65532 -G 65532 运行：
# 进程看到的根、/proc、/sys、主机名与网络都是宿主自己的，与 systemd 单元里的非 root agent 同一口径；pid 命名空间
# 由 --pid host 给出。busybox nsenter 的 -S/-G 清空附加组、去掉全部能力（实测 Groups 为空、CapEff 为 0）。二进制
# 经 stdin 写到宿主上本次运行私有的 /var/tmp/<运行 ID>/，cleanup 删除。宿主要有 POSIX sh、cat、chmod、mkdir、rm。
#
# 输入只取显式给出的，缺任一个即失败，不回退到本机缓存或 registry 上的 tag：
#   IMAGE       受测的 agent 镜像：check 装入的 <仓库>:<VERSION>，或 verify 按平台清单 digest 拉回的 <仓库>@sha256:…
#   HUB_IMAGE   hub 镜像的 ID（sha256:<64 位十六进制>）：scripts/docker-image.sh 按 hub 组的构建记录从它的布局载入，
#               ID 是内容寻址的，本机或 registry 上同名的旧镜像顶替不了它
#   VERSION     两个镜像都必须报告的版本
#   TOOL_IMAGE  带 shell 的工具镜像（按 digest 固定的 alpine，deploy/image.mk 的 ALPINE_IMAGE）：辅助容器与读宿主事实
#   SMOKE_PLATFORM  可空，受测镜像运行的平台；空表示 docker 的默认平台（本机平台）
#   SMOKE_EXPECT_SCOPE_KIND  可空，宿主的执行环境种类（HOST、CGROUP_NAMESPACE…，即 ScopeKind 去掉 SCOPE_KIND_ 前缀）；
#               给出时两个节点都必须报它。CI 的运行器挂载根是真根，ci.yml 与 release.yml 设为 HOST；本机的 docker 宿主
#               可能自己就在 cgroup 命名空间里（OrbStack、LXC 里的 Docker），不设，只断言两边一致
# 受测镜像与 hub 都以 --pull=never 运行：本机没有就失败。工具镜像不受测，照 scripts/docker-smoke.sh 的做法运行。
# 创建的容器与卷全部带本次运行的前缀，由 cleanup 删除；工件目录保留，路径在开头打印。
set -eu
: "${IMAGE:?IMAGE is required: the agent image under test (<repo>:<version> or <repo>@sha256:...)}"
: "${HUB_IMAGE:?HUB_IMAGE is required: the ID (sha256:...) of the hub image to register with}"
: "${VERSION:?VERSION is required: both images must report exactly this version}"
: "${TOOL_IMAGE:?TOOL_IMAGE is required: the pinned alpine image, see ALPINE_IMAGE in deploy/image.mk}"
platform=${SMOKE_PLATFORM:-}
expect_kind=${SMOKE_EXPECT_SCOPE_KIND:-}
tool=$TOOL_IMAGE
# 每个等待的上限：发布后回读在 QEMU 模拟的 arm64 上跑同一段，比本机慢。上限按轮询间累计的 sleep 计（每次 1s），
# 不按墙钟，理由同 scripts/docker-smoke.sh。
wait_s=90
# 探测结果要等 hub 把分钟桶刷出才查得到（QueryProbes 不含当前分钟）：任务在 agent 起来之前建好，最晚的情形是
# 首个结果落在一分钟的开头、要等这一分钟结束再刷出，所以比 wait_s 多给两分钟。
probe_wait_s=$((wait_s + 120))
# agent 对 TCP 探测的超时，毫秒；对照任务的目标在同一台宿主上，远小于它。
probe_timeout_ms=1000

fail() {
  msg=$1
  shift
  for f in "$@"; do
    if [ -s "$f" ]; then
      echo "--- $f" >&2
      cat "$f" >&2
      [ -z "$(tail -c 1 "$f")" ] || echo >&2
    fi
  done
  echo "FAIL: $msg" >&2
  exit 1
}

case $HUB_IMAGE in
  sha256:*[!0-9a-f]* | sha256:) fail "HUB_IMAGE '$HUB_IMAGE' is not an image ID (sha256:<64 hex digits>)" ;;
  sha256:*) [ ${#HUB_IMAGE} = 71 ] || fail "HUB_IMAGE '$HUB_IMAGE' is not an image ID (sha256:<64 hex digits>)" ;;
  *) fail "HUB_IMAGE '$HUB_IMAGE' is not an image ID (sha256:<64 hex digits>)" ;;
esac
# 只认 proto 里 ScopeKind 的取值：拼错的期望会让断言永远失败或（若改成宽松比较）永远通过。
case $expect_kind in
  '' | HOST | CGROUP_NAMESPACE | CGROUP_V1_LEGACY | IDENTIFY_FAILED) ;;
  *) fail "SMOKE_EXPECT_SCOPE_KIND '$expect_kind' is not a scope kind (HOST, CGROUP_NAMESPACE, CGROUP_V1_LEGACY, IDENTIFY_FAILED)" ;;
esac

# 受测的引用只能是两种局部引用之一，且版本 tag 不能是冒烟之前就在本机的旧东西：
#   <仓库>@sha256:…  verify 按 digest 拉回的；这时本机不应有 <仓库>:<VERSION>——回读的环境里不该有任何本地构建
#                    或旧 tag，有就说明回读不是在干净的环境里做的。
#   <仓库>:<VERSION>  check 刚按 digest 从布局装入的。
# 仓库名：去掉 @digest，再去掉最后一个 / 之后的 :tag（仓库主机可以带 :端口），与 image-platform-ref.sh 同一写法。
repo=${IMAGE%%@*}
case ${repo##*/} in
  *:*) repo=${repo%:*} ;;
esac
case $IMAGE in
  "$repo"@sha256:*)
    stale=$(docker image ls -q "$repo:$VERSION") || fail "cannot list local images of $repo"
    [ -z "$stale" ] || fail "$repo:$VERSION already exists locally ($stale); the readback smoke must see only the image verify pulled by digest"
    ;;
  "$repo:$VERSION") ;;
  *) fail "IMAGE '$IMAGE' is neither $repo:$VERSION (loaded by docker-image.sh check) nor $repo@sha256:... (pulled by docker-image.sh verify)" ;;
esac
docker image inspect "$IMAGE" > /dev/null 2>&1 || fail "$IMAGE is not in the local image store (nothing is pulled here)"
docker image inspect "$HUB_IMAGE" > /dev/null 2>&1 || fail "hub image $HUB_IMAGE is not in the local image store (nothing is pulled here)"

work=$(mktemp -d)
echo "docker agent smoke artifacts: $work (image $IMAGE${platform:+, platform $platform}, hub $HUB_IMAGE)"
run_id=heron-agent-smoke-$(basename "$work")
hub=$run_id-hub
agent=$run_id-agent
host_agent=$run_id-host-agent
data=$run_id-data
config=$run_id-config
# 宿主上对照 agent 的私有目录（宿主的 mount 命名空间里的路径）；run_id 由 mktemp 生成，只含 [A-Za-z0-9.-]。
host_dir=/var/tmp/$run_id
host_dir_made=
containers="$hub $agent $host_agent $run_id-register $run_id-extract $run_id-no-netns $run_id-no-root"

# host_sh SCRIPT [ARG…]：在 docker 宿主的命名空间里以 root 执行一段 sh（见文件头的运行方式），stdin 原样交给它。
host_sh() {
  docker run --rm -i --privileged --pid host --cgroupns host "$tool" \
    nsenter -t 1 -m -u -i -n sh -c "$@"
}

cleanup() {
  # 容器先于卷删除：卷还挂在容器上时删不掉。宿主对照 agent 是辅助容器的子进程、在它的 cgroup 里，随容器删除结束。
  # shellcheck disable=SC2086 # 容器名不含空白，按词拆开
  docker rm -f $containers > /dev/null 2>&1 || true
  docker volume rm "$data" "$config" > /dev/null 2>&1 || true
  if [ -n "$host_dir_made" ]; then
    # shellcheck disable=SC2016 # 这段 sh 在 docker 宿主上展开
    host_sh 'rm -rf "$1"' sh "$host_dir" < /dev/null > /dev/null 2>&1 || echo "warning: could not remove $host_dir on the docker host" >&2
  fi
}
trap cleanup EXIT
# dash 与 busybox ash 被信号终止时不执行 EXIT trap；转成 exit，清理照常发生。
trap 'exit 1' INT TERM HUP

# 运行受测镜像的唯一入口：带上指定的平台（未指定时由 docker 取默认平台），且只用本机已有的镜像。
drun() {
  if [ -n "$platform" ]; then
    docker run --pull=never --platform "$platform" "$@"
  else
    docker run --pull=never "$@"
  fi
}

# 期望的 GOARCH：受测平台的架构部分；默认平台取 docker 引擎的架构。hub 报告的节点 arch 必须等于它，否则跑的不是
# 受测的那个平台。
native=$(docker version -f '{{.Server.Os}}/{{.Server.Arch}}') || fail "cannot read the docker engine's platform"
if [ -n "$platform" ]; then
  want_arch=${platform#*/}
  want_arch=${want_arch%%/*}
else
  want_arch=${native#*/}
fi
# 完整验收只在本机平台：宿主对照组的二进制以宿主原生方式运行，模拟运行的平台没有对照。完整验收注册两个节点
# （容器与宿主对照），模拟平台只注册容器。
full=
nodes=1
if [ -z "$platform" ] || [ "$platform" = "$native" ]; then
  full=1
  nodes=2
fi

got=$(drun --rm "$IMAGE" version 2> "$work/agent-version.err") || fail "heron-agent version exited non-zero" "$work/agent-version.err"
[ "$got" = "$VERSION" ] || fail "agent image reports version '$got', want '$VERSION'"
got=$(docker run --rm --pull=never "$HUB_IMAGE" version 2> "$work/hub-version.err") || fail "heron-hub version exited non-zero" "$work/hub-version.err"
[ "$got" = "$VERSION" ] || fail "hub image reports version '$got', want '$VERSION'"
echo "versions ok: $VERSION"

if [ -n "$full" ]; then
  # 宿主对照用的二进制取自受测镜像本身，不从 build/ 取、不重编：对照的两边必须是同一份字节。
  mkdir "$work/host"
  docker create --pull=never ${platform:+--platform "$platform"} --name "$run_id-extract" "$IMAGE" > /dev/null ||
    fail "docker create $IMAGE for extracting the binary"
  docker cp "$run_id-extract:/usr/local/bin/heron-agent" "$work/host/heron-agent" > /dev/null ||
    fail "extract /usr/local/bin/heron-agent from $IMAGE"
  docker rm "$run_id-extract" > /dev/null
  [ -s "$work/host/heron-agent" ] && [ -x "$work/host/heron-agent" ] ||
    fail "the binary extracted from $IMAGE is empty or not executable"

  # 宿主事实：探测目标用的非回环地址（宿主默认路由的源地址；192.0.2.1 是 TEST-NET-1，route get 只查表不发包）、
  # 主机名、/etc/os-release 的 PRETTY_NAME。都在宿主的命名空间里读，与对照 agent 同一来源。
  # shellcheck disable=SC2016 # 这段 sh 在 docker 宿主上展开
  host_sh 'ip -4 route get 192.0.2.1 2>&1 | sed -n "s/.* src \([0-9.]*\).*/addr \1/p"
    printf "hostname %s\n" "$(cat /proc/sys/kernel/hostname)"
    sed -n "s/^PRETTY_NAME=//p" /etc/os-release | sed "s/^\"\(.*\)\"$/\1/; s/^/os /"' < /dev/null > "$work/host-facts.txt" 2> "$work/host-facts.err" ||
    fail "read the docker host's facts" "$work/host-facts.err"
  host_addr=$(sed -n 's/^addr //p' "$work/host-facts.txt")
  host_name=$(sed -n 's/^hostname //p' "$work/host-facts.txt")
  host_os=$(sed -n 's/^os //p' "$work/host-facts.txt")
  [ -n "$host_addr" ] && [ -n "$host_name" ] && [ -n "$host_os" ] ||
    fail "incomplete docker host facts (need a default route, a hostname and PRETTY_NAME in /etc/os-release)" "$work/host-facts.txt" "$work/host-facts.err"
  echo "docker host: $host_name, $host_os, address $host_addr"
fi

# hub：镜像默认参数（serve，监听容器内 0.0.0.0:8080），端口发布在宿主回环上；完整验收另发布在宿主的非回环地址上，
# 作为"以宿主地址为目标"的探测任务的目标——那里有监听，探测若没被拒绝就会成功，拒绝与连不上因而分得开。
publish_host=
[ -z "$full" ] || publish_host="-p $host_addr::8080"
docker volume create "$data" > /dev/null
# shellcheck disable=SC2086 # publish_host 是空或两个词（地址只含数字与点）
docker run -d --pull=never --name "$hub" -p 127.0.0.1::8080 $publish_host -v "$data:/data" "$HUB_IMAGE" > /dev/null ||
  fail "cannot start $hub from $HUB_IMAGE"
docker port "$hub" 8080/tcp > "$work/hub-ports.txt" 2>&1 || fail "port 8080 of $hub is not published" "$work/hub-ports.txt"
port=$(sed -n 's/^127\.0\.0\.1://p' "$work/hub-ports.txt")
[ -n "$port" ] || fail "port 8080 of $hub is not published on 127.0.0.1" "$work/hub-ports.txt"
base="http://127.0.0.1:$port"
polls=0
until [ "$(curl -sS --max-time 2 -o /dev/null -w '%{http_code}' "$base/healthz" 2> /dev/null)" = 200 ]; do
  if [ "$polls" -ge "$wait_s" ] || [ "$(docker inspect -f '{{.State.Status}}' "$hub")" != running ]; then
    docker logs "$hub" > "$work/hub.log" 2>&1 || true
    fail "hub did not become ready on $base" "$work/hub.log"
  fi
  sleep 1
  polls=$((polls + 1))
done
echo "hub ok: $base"

pw='agent smoke admin password 2026'
printf '%s\n' "$pw" | docker exec -i "$hub" heron-hub passwd --db /data/heron.db > "$work/passwd.log" 2>&1 ||
  fail "set the admin password" "$work/passwd.log"
docker exec "$hub" heron-hub window open --db /data/heron.db --ttl 10m --max "$nodes" > "$work/window.txt" 2>&1 ||
  fail "open a registration window" "$work/window.txt"
key=$(sed -n 's/^key: //p' "$work/window.txt")
[ -n "$key" ] || fail "no key in the registration window output" "$work/window.txt"

curl -sS --max-time 10 -c "$work/jar" -o "$work/login.json" -H 'Content-Type: application/json' \
  --data "{\"password\":\"$pw\"}" "$base/heron.v1.AdminService/Login" 2> "$work/login.curl" ||
  fail "admin login" "$work/login.json" "$work/login.curl"

# api METHOD BODY OUT：调用 AdminService 的一个方法，响应存到 OUT；HTTP 非 200 即失败。
api() {
  code=$(curl -sS --max-time 10 -b "$work/jar" -o "$3" -w '%{http_code}' -H 'Content-Type: application/json' \
    --data "$2" "$base/heron.v1.AdminService/$1" 2> "$3.curl") || fail "AdminService/$1" "$3" "$3.curl"
  [ "$code" = 200 ] || fail "AdminService/$1 answered HTTP $code" "$3"
}

if [ -n "$full" ]; then
  # 探测任务先于 agent 建好，两个节点第一次上报就领到它们（all_nodes）。
  hub_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$hub")
  [ -n "$hub_ip" ] || fail "$hub has no bridge address for the reachable probe target"
  deny_target=$host_addr:$(sed -n "s/^$host_addr://p" "$work/hub-ports.txt")
  [ "$deny_target" != "$host_addr:" ] || fail "port 8080 of $hub is not published on $host_addr" "$work/hub-ports.txt"
  reach_target=$hub_ip:8080
  for t in deny="$deny_target" reach="$reach_target"; do
    api SaveProbeTask "{\"task\":{\"kind\":\"PROBE_KIND_TCP\",\"target\":\"${t#*=}\",\"intervalS\":5,\"timeoutMs\":$probe_timeout_ms},\"allNodes\":true}" "$work/task-${t%%=*}.json"
  done
  api ListProbeTasks '{}' "$work/tasks.json"
  deny_task=$(jq -r --arg t "$deny_target" '.tasks[] | select(.task.target == $t) | .task.id' "$work/tasks.json")
  reach_task=$(jq -r --arg t "$reach_target" '.tasks[] | select(.task.target == $t) | .task.id' "$work/tasks.json")
  [ -n "$deny_task" ] && [ -n "$reach_task" ] || fail "the probe tasks were not saved" "$work/tasks.json"
  probes_from=$(($(date +%s) - 60))
fi

# 注册：docs/install.md「安装 agent → Docker」的原样命令——一次性容器，与运行的容器共用同一个配置卷。卷首次挂上时继承镜像里 /etc/heron-agent 的
# 属主（65532），register 以镜像的运行用户写配置。
name=heron-smoke-$want_arch
docker volume create "$config" > /dev/null
drun --rm --name "$run_id-register" --network host -v "$config:/etc/heron-agent" "$IMAGE" \
  register --hub "$base" --key "$key" --name "$name" > "$work/register.log" 2>&1 ||
  fail "register from a one-off container" "$work/register.log"
echo "register ok: $name"

# 运行：docs/install.md「安装 agent → Docker」的原样命令，镜像默认参数（run --host-root /hostfs），不覆写。
drun -d --name "$agent" --network host --read-only -v /:/hostfs:ro -v "$config:/etc/heron-agent" "$IMAGE" > /dev/null ||
  fail "cannot start $agent from $IMAGE"

if [ -n "$full" ]; then
  host_name_node=$name-host
  host_dir_made=1
  # shellcheck disable=SC2016 # 这段 sh 在 docker 宿主上展开
  host_sh 'set -e; umask 022; mkdir "$1" "$1/etc"; cat > "$1/heron-agent"; chmod 0755 "$1/heron-agent"; chown 65532:65532 "$1/etc"; chmod 0700 "$1/etc"' \
    sh "$host_dir" < "$work/host/heron-agent" > "$work/host-install.log" 2>&1 ||
    fail "put the comparison binary at $host_dir on the docker host" "$work/host-install.log"
  docker run --rm --privileged --pid host --cgroupns host "$tool" \
    nsenter -t 1 -m -u -i -n -S 65532 -G 65532 "$host_dir/heron-agent" register --hub "$base" --key "$key" \
    --name "$host_name_node" --config "$host_dir/etc/config.json" > "$work/host-register.log" 2>&1 ||
    fail "register the comparison agent on the docker host" "$work/host-register.log"
  docker run -d --name "$host_agent" --privileged --pid host --cgroupns host "$tool" \
    nsenter -t 1 -m -u -i -n -S 65532 -G 65532 "$host_dir/heron-agent" run --config "$host_dir/etc/config.json" > /dev/null ||
    fail "cannot start the comparison agent on the docker host"
  echo "comparison agent on the docker host: $host_name_node (uid 65532)"
fi

# snapshot：GetSnapshot 与 ListNodes 各取一次。
snapshot() {
  api GetSnapshot '{}' "$work/snapshot.json"
  api ListNodes '{}' "$work/nodes.json"
}

# 在线且带着指标（内存总量是第一份上报就有的读数）：这一份上报来自受测镜像（与对照 agent）的运行。完整验收还等到
# cpuPct 出现：它是两次 /proc/stat 采样的差分，第一份上报没有，对照要比较它。
online_names="$name${full:+ $host_name_node}"
want_metric=memTotal
[ -z "$full" ] || want_metric=cpuPct
polls=0
while :; do
  snapshot
  missing=
  for n in $online_names; do
    jq -e --arg name "$n" --arg m "$want_metric" \
      '[.nodes[]? | select(.name == $name and .online == true and .metrics.memTotal != null and .metrics[$m] != null)] | length == 1' \
      "$work/snapshot.json" > /dev/null || missing="$missing $n"
  done
  [ -n "$missing" ] || break
  if [ "$polls" -ge "$wait_s" ] || [ "$(docker inspect -f '{{.State.Status}}' "$agent")" != running ] ||
    { [ -n "$full" ] && [ "$(docker inspect -f '{{.State.Status}}' "$host_agent")" != running ]; }; then
    docker logs "$agent" > "$work/agent.log" 2>&1 || true
    [ -z "$full" ] || docker logs "$host_agent" > "$work/host-agent.log" 2>&1 || true
    fail "node(s)$missing did not come online with metrics" "$work/snapshot.json" "$work/agent.log" "$work/host-agent.log"
  fi
  sleep 1
  polls=$((polls + 1))
done
jq -e --arg name "$name" --arg arch "$want_arch" --arg version "$VERSION" \
  '[.nodes[] | select(.name == $name and .facts.arch == $arch and .facts.agentVersion == $version)] | length == 1' \
  "$work/nodes.json" > /dev/null ||
  fail "node $name does not report arch $want_arch and agent version $VERSION" "$work/nodes.json"
echo "online with metrics ok: $name ($want_arch)"

# 容器里 agent 进程的身份与有效能力集：按 docker 记下的宿主 pid 读 /proc/<pid>/status（宿主的 pid 命名空间，
# 辅助容器 --pid host；读别的 uid 的进程状态要特权）。CapEff 是十六进制位图，CAP_NET_RAW 是第 13 位。
agent_pid=$(docker inspect -f '{{.State.Pid}}' "$agent") || fail "cannot read the pid of $agent"
docker run --rm --privileged --pid host "$tool" cat "/proc/$agent_pid/status" > "$work/agent-status.txt" 2> "$work/agent-status.err" ||
  fail "read /proc/$agent_pid/status of $agent" "$work/agent-status.err"
agent_uid=$(sed -n 's/^Uid:[[:space:]]*\([0-9]*\).*/\1/p' "$work/agent-status.txt")
agent_capeff=$(sed -n 's/^CapEff:[[:space:]]*\([0-9a-f]*\)$/\1/p' "$work/agent-status.txt")
[ -n "$agent_uid" ] && [ -n "$agent_capeff" ] || fail "no Uid or CapEff in /proc/$agent_pid/status" "$work/agent-status.txt"
echo "agent process in the container: uid $agent_uid, CapEff $agent_capeff"
[ "$agent_uid" = 65532 ] || fail "the agent in $agent runs as uid $agent_uid, want 65532" "$work/agent-status.txt"

if [ -z "$full" ]; then
  echo "docker agent smoke passed: $IMAGE${platform:+ ($platform)} (start, register and report; no host comparison on an emulated platform)"
  exit 0
fi

# 两个节点逐字段对照。容器节点与宿主进程读的是同一台宿主：
#   相等：Facts 的 hostname（另须等于宿主当前的主机名）、kernel、os（另须等于宿主 /etc/os-release 的 PRETTY_NAME）、
#     arch、cpuCores、virtualization（读者镜像不改变宿主虚拟化）、网卡集合（diagnostics.netInterfaces）；Metrics 的
#     bootId、memTotal、swapTotal、diskTotal（宿主根所在文件系统）。
#   执行环境：kind 相等（给了 SMOKE_EXPECT_SCOPE_KIND 时还须等于它）；kind 为 HOST 时容器节点的说明 = 宿主节点的
#     说明加上 CONTAINER_SIGNAL_ON_HOST_ROOT、宿主节点不带它；其余 kind 下 agent 不给这条说明，两边说明完全相同。
#   同量级（动态量，两次读数的时刻不同）：netRxTotal、netTxTotal、cpuPct、load1 两边都在，且大者不超过
#     10 × max(小者, 下限)；下限吸收空闲宿主上接近 0 的读数（cpuPct 5 个百分点、load1 1.0、网卡累计 1 MiB）。
# 输出每个不符项一行，空输出即一致。
compare_nodes() {
  jq -r -n --slurpfile nodes "$work/nodes.json" --slurpfile snap "$work/snapshot.json" \
    --arg c "$name" --arg h "$host_name_node" --arg hostname "$host_name" --arg os "$host_os" --arg kind "$expect_kind" '
    def node($n): $nodes[0].nodes[] | select(.name == $n);
    def metrics($n): $snap[0].nodes[] | select(.name == $n) | .metrics;
    def num: if . == null then null else tonumber end;
    def same_mag($a; $b; $floor): ($a != null and $b != null) and ([$a, $b] | max) <= 10 * ([([$a, $b] | min), $floor] | max);
    (node($c).facts) as $cf | (node($h).facts) as $hf | metrics($c) as $cm | metrics($h) as $hm |
    ( ["hostname", "kernel", "os", "arch", "cpuCores", "virtualization"][] as $f
      | select($cf[$f] != $hf[$f]) | "facts.\($f): container \($cf[$f] | tojson), host \($hf[$f] | tojson)" ),
    ( select(($cf.diagnostics.netInterfaces // [] | sort) != ($hf.diagnostics.netInterfaces // [] | sort))
      | "facts.diagnostics.netInterfaces: container \($cf.diagnostics.netInterfaces | tojson), host \($hf.diagnostics.netInterfaces | tojson)" ),
    ( select($hf.hostname != $hostname) | "facts.hostname: host node \($hf.hostname | tojson), docker host \($hostname | tojson)" ),
    ( select($hf.os != $os) | "facts.os: host node \($hf.os | tojson), /etc/os-release PRETTY_NAME \($os | tojson)" ),
    ( ["bootId", "memTotal", "swapTotal", "diskTotal"][] as $f
      | select($cm[$f] == null or $cm[$f] != $hm[$f]) | "metrics.\($f): container \($cm[$f] | tojson), host \($hm[$f] | tojson)" ),
    ( [["netRxTotal", 1048576], ["netTxTotal", 1048576], ["cpuPct", 5], ["load1", 1]][] as [$f, $floor]
      | select(same_mag($cm[$f] | num; $hm[$f] | num; $floor) | not)
      | "metrics.\($f): container \($cm[$f] | tojson), host \($hm[$f] | tojson) (not the same order of magnitude)" ),
    ( ($cf.execution.kind) as $ck | ($hf.execution.kind) as $hk
      | ($cf.execution.notes // [] | sort) as $cn | ($hf.execution.notes // [] | sort) as $hn
      | "SCOPE_NOTE_CONTAINER_SIGNAL_ON_HOST_ROOT" as $signal
      | ( select($ck != $hk) | "facts.execution.kind: container \($ck | tojson), host \($hk | tojson)" ),
        ( select($kind != "" and ($ck != "SCOPE_KIND_\($kind)" or $hk != "SCOPE_KIND_\($kind)"))
          | "facts.execution.kind: container \($ck | tojson), host \($hk | tojson), SMOKE_EXPECT_SCOPE_KIND \($kind)" ),
        ( select($hn | index($signal)) | "facts.execution.notes: the host node carries \($signal)" ),
        ( if $ck == "SCOPE_KIND_HOST" then
            select($cn != ($hn + [$signal] | sort))
            | "facts.execution.notes: container \($cn | tojson), want the host node'"'"'s \($hn | tojson) plus \($signal)"
          else
            select($cn != $hn) | "facts.execution.notes: container \($cn | tojson), host \($hn | tojson) (kind \($ck))"
          end ) )
  ' > "$work/compare.txt" 2> "$work/compare.err" || fail "compare the two nodes" "$work/compare.err"
}
compare_nodes
[ ! -s "$work/compare.txt" ] ||
  fail "the container node and the host-process node disagree ($(wc -l < "$work/compare.txt" | tr -d ' ') item(s), first: $(head -n 1 "$work/compare.txt"))" \
    "$work/compare.txt" "$work/nodes.json" "$work/snapshot.json"
# 文件能力生效：有效能力集含 CAP_NET_RAW，容器节点据此报 ICMP 可用（宿主的 ping_group_range 是否放行 65532 都成立：
# 数据报 socket 不可用时 agent 退到 raw socket）。宿主对照 agent 由 nsenter -S 降权、不带能力，两边的
# icmp_available 不比较。
[ $((0x$agent_capeff & 0x2000)) != 0 ] ||
  fail "the agent in $agent does not hold CAP_NET_RAW (CapEff $agent_capeff); the binary's file capability did not take effect" "$work/agent-status.txt"
jq -e --arg n "$name" '[.nodes[] | select(.name == $n and .facts.icmpAvailable == true)] | length == 1' "$work/nodes.json" > /dev/null ||
  fail "node $name does not report icmp_available" "$work/nodes.json"
echo "host comparison ok: facts, totals and execution scope ($(jq -r --arg n "$name" '.nodes[] | select(.name == $n) | .facts.execution.kind' "$work/nodes.json"))"

# 进程数：两个 agent 各自按自己的周期采样，两次读数之间宿主上生灭的进程造成差异。受控窗口：从这里到比较结束，
# 脚本在 docker 宿主上不起任何容器、不调用 docker，只有 curl、jq、sleep 依次运行（docker 宿主就是脚本所在的机器时，
# 任一时刻至多脚本的 shell 加一个子进程，shell 本身两边都数到）。容差 = 两个采样时刻各至多 1 个脚本子进程 = 2，
# 另加宿主上与脚本无关的生灭，所以取连续 procs_pairs 对新上报，至少一对的差不超过容差。容器读自己的 pid 命名空间
# 这类缺陷差的是宿主上全部进程数，不会落进容差。
procs_tolerance=2
procs_pairs=6
pairs=0
# 判定只看是否出现过一对落在容差内的读数，不看循环结束时 pairs 的值：第 procs_pairs 对恰好落在容差内时 pairs 也到了上限，
# 按计数判会把它当成"全部超差"。
procs_within=
seen=
: > "$work/procs.txt"
polls=0
while [ "$pairs" -lt "$procs_pairs" ]; do
  api GetSnapshot '{}' "$work/procs-snapshot.json"
  # 两个节点都要有 procs 读数：缺失不是 0（Metrics.procs 是 optional），按缺失失败。
  jq -r --arg c "$name" --arg h "$host_name_node" \
    '[(.nodes[] | select(.name == $c)), (.nodes[] | select(.name == $h))] |
     if (.[0].metrics.procs | type) == "number" and (.[1].metrics.procs | type) == "number"
     then "\(.[0].lastSeenAt) \(.[1].lastSeenAt) \(.[0].metrics.procs) \(.[1].metrics.procs)"
     else error("procs missing: container \(.[0].metrics.procs), host \(.[1].metrics.procs)") end' \
    "$work/procs-snapshot.json" > "$work/procs-pair.txt" 2>&1 || fail "read the process counts" "$work/procs-pair.txt"
  read -r c_seen h_seen c_procs h_procs < "$work/procs-pair.txt"
  set -- "$c_seen" "$h_seen" "$c_procs" "$h_procs"
  # 两个节点都比上一对有了新上报才算一对新的读数。
  if [ -z "$seen" ] || { [ "$1" != "${seen% *}" ] && [ "$2" != "${seen#* }" ]; }; then
    [ -z "$seen" ] || {
      pairs=$((pairs + 1))
      d=$(($3 - $4))
      echo "container $3 (at $1), host $4 (at $2), difference $d" >> "$work/procs.txt"
      [ "${d#-}" -gt "$procs_tolerance" ] || { procs_within=1; break; }
    }
    seen="$1 $2"
  fi
  [ "$polls" -lt "$((wait_s * 2))" ] || fail "fewer than $procs_pairs fresh report pairs in $((wait_s * 2))s" "$work/procs.txt"
  sleep 1
  polls=$((polls + 1))
done
[ -n "$procs_within" ] ||
  fail "process counts differ by more than $procs_tolerance in all $procs_pairs report pairs" "$work/procs.txt"
echo "process count ok: $(tail -n 1 "$work/procs.txt")"

# 探测策略：以宿主非回环地址为目标的任务被本机地址策略拒绝（internal/agent/prober/policy.go：宿主接口上的地址并入
# 默认拒绝），agent 回报 ProbeError，hub 只计 errors、不存消息——QueryProbes 的样本里 errors == sent 且没有 rtt；
# 对照任务（hub 容器的桥接地址，不是宿主的地址）必须有成功的 rtt。两个节点各自满足，容器节点与宿主进程行为相同。
# probe_outcome NODE_ID TASK_ID：把 "<sent> <lost> <errors> <有 rtt 的样本数>" 读进 sent lost errors rtt；窗口里没有
# 样本时都是 0。不经命令替换调用：api 与 jq 失败时 fail 要中止整个脚本，而不是只退出一个子 shell。
probe_outcome() {
  api QueryProbes "{\"nodeId\":$1,\"from\":$probes_from,\"to\":$(($(date +%s) + 120))}" "$work/probes-$1.json"
  jq -r --argjson t "$2" '[.series[]? | select((.taskId | tonumber) == $t) | .samples[]] |
    "\(map(.sent // 0) | add // 0) \(map(.lost // 0) | add // 0) \(map(.errors // 0) | add // 0) \(map(select(.rttMeanUs != null)) | length)"' \
    "$work/probes-$1.json" > "$work/probe-outcome.txt" 2>&1 || fail "read the probe results of node $1" "$work/probe-outcome.txt"
  read -r sent lost errors rtt < "$work/probe-outcome.txt"
}
container_id=$(jq -r --arg n "$name" '.nodes[] | select(.name == $n) | .id' "$work/nodes.json")
host_id=$(jq -r --arg n "$host_name_node" '.nodes[] | select(.name == $n) | .id' "$work/nodes.json")
polls=0
while :; do
  denied=1 reached=1
  : > "$work/probes.txt"
  for node in "$name=$container_id" "$host_name_node=$host_id"; do
    probe_outcome "${node#*=}" "$deny_task"
    echo "${node%%=*} deny $deny_target: sent $sent lost $lost errors $errors rtt-samples $rtt" >> "$work/probes.txt"
    # 被拒绝：有结果、全是 errors、没有 rtt 也没有超时。
    { [ "$sent" -gt 0 ] && [ "$errors" = "$sent" ] && [ "$rtt" = 0 ]; } || denied=
    probe_outcome "${node#*=}" "$reach_task"
    echo "${node%%=*} reach $reach_target: sent $sent lost $lost errors $errors rtt-samples $rtt" >> "$work/probes.txt"
    [ "$rtt" -gt 0 ] || reached=
  done
  [ -z "$denied" ] || [ -z "$reached" ] || break
  if [ "$polls" -ge "$probe_wait_s" ]; then
    [ -n "$denied" ] || fail "the probe to the host address $deny_target was not rejected on both nodes (want results that are all errors)" "$work/probes.txt"
    fail "the reachable control $reach_target did not succeed on both nodes, so a rejection could not be told from an unreachable target" "$work/probes.txt"
  fi
  sleep 1
  polls=$((polls + 1))
done
echo "probe policy ok: $deny_target rejected and $reach_target reached on both nodes"

# 负面：缺任一必要参数，agent 以退出码 2 拒绝启动，日志带点名缺失参数的提示（internal/agent/collect/root.go 的
# hintHostNetwork、hintMountRoot）。配置卷只读挂上：拒绝发生在任何上报之前，不写配置。运行至多 wait_s 秒：
# 缺陷让它照常运行时按"仍在运行"失败，而不是挂住冒烟。
# refused NAME HINT DOCKER_RUN_ARGS…
refused() {
  rname=$1 hint=$2
  shift 2
  drun -d --name "$rname" --read-only -v "$config:/etc/heron-agent:ro" "$@" "$IMAGE" > /dev/null ||
    fail "cannot start $rname from $IMAGE"
  polls=0
  until [ "$(docker inspect -f '{{.State.Status}}' "$rname")" = exited ]; do
    if [ "$polls" -ge "$wait_s" ]; then
      docker logs "$rname" > "$work/$rname.log" 2>&1 || true
      fail "$rname: want exit code 2, still running after ${wait_s}s" "$work/$rname.log"
    fi
    sleep 1
    polls=$((polls + 1))
  done
  code=$(docker inspect -f '{{.State.ExitCode}}' "$rname")
  docker logs "$rname" > "$work/$rname.log" 2>&1 || true
  [ "$code" = 2 ] || fail "$rname: want exit code 2, got $code" "$work/$rname.log"
  grep -qF -- "$hint" "$work/$rname.log" || fail "$rname: the log does not say '$hint'" "$work/$rname.log"
}
refused "$run_id-no-netns" 'run the container in the host network namespace (docker run --network host)' -v /:/hostfs:ro
refused "$run_id-no-root" 'mount the host root there read-only (docker run -v /:/hostfs:ro)' --network host
echo "refusals ok: without --network host and without -v /:/hostfs both exit 2"

echo "docker agent smoke passed: $IMAGE${platform:+ ($platform)}"
