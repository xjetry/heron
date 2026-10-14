#!/bin/sh
# hub 镜像冒烟：以默认参数起容器，/admin/ 返回嵌入的面板，docker exec 经 stdin
# 设密码后能登录，docker stop 后以 0 退出。另外钉住部署时最容易踩到的几条：运行用户与卷属主、
# passwd 没收到输入时的报错、库目录不可写时的退出、非 loopback 监听与时区回退的告警、docs/operations.md「排查」里
# 不依赖镜像内 shell 的排查办法。创建的容器与卷全部带本次运行的前缀，由 cleanup 删除；工件目录保留，
# 路径在开头打印。
set -eu
: "${IMAGE:?IMAGE is required, e.g. ghcr.io/xjetry/heron-hub:v0.1.0}"
: "${VERSION:?VERSION is required: the image must report exactly this version}"
# 空表示 docker 的默认平台；发布后回读时逐个架构给出。
platform=${SMOKE_PLATFORM:-}
# 带 shell 的工具镜像：读卷里的属主、预置不可写的卷、在 hub 的网络命名空间里发请求。不进入产物。
# 按 digest 固定，由 Makefile 的 ALPINE_IMAGE 经环境变量传入，与 Dockerfile 的基础镜像同一处定义。
# 工具容器不指定平台：本机已有这个 digest 的镜像就直接用，没有才拉取，每次冒烟不必访问 registry
# （Docker Hub 对匿名请求限流）。经典镜像存储里一个索引 digest 只对应一个本地镜像，它可能是别的架构、
# 经模拟运行并在 stderr 告警；这里用到的 busybox stat、wget、sh 与架构无关，读结果时只取 stdout。
tool=${TOOL_IMAGE:?TOOL_IMAGE is required: the pinned alpine image, see ALPINE_IMAGE in the Makefile}
# 每个等待的上限：发布后回读在 QEMU 模拟的 arm64 上跑同一段，比本机慢。上限按轮询间累计的 sleep
# 计（每次 0.5s，至多 wait_s×2 次），不按墙钟：宿主休眠时 docker 与本脚本一起停摆，一次休眠至多落在
# 一次 sleep 里；墙钟截止会把整段休眠算进上限，醒来后第一次检查就失败。
wait_s=30
# 与 docs/install.md「用 Docker 运行 hub」的部署一致：大于 hub 关停时排空请求的上限（cmd/hub/serve.go 的 drainTimeout，10 秒）。
# docker stop 的默认宽限期也是 10 秒，与排空上限相等时最后一批写可能被 SIGKILL 截断。
stop_timeout=30

work=$(mktemp -d)
echo "docker smoke artifacts: $work (image $IMAGE${platform:+, platform $platform})"
run_id=heron-smoke-$(basename "$work")
hub=$run_id-hub
denied=$run_id-denied
data=$run_id-data
denied_data=$run_id-denied-data
containers="$run_id-version $hub $run_id-owner $run_id-netns $run_id-prepare $denied"

cleanup() {
  # 容器先于卷删除：卷还挂在容器上时删不掉。
  # shellcheck disable=SC2086 # 容器名不含空白，按词拆开
  docker rm -f $containers > /dev/null 2>&1 || true
  docker volume rm "$data" "$denied_data" > /dev/null 2>&1 || true
}
trap cleanup EXIT
# dash 与 busybox ash 被信号终止时不执行 EXIT trap；转成 exit，清理照常发生。
trap 'exit 1' INT TERM HUP

# fail MESSAGE [FILE…]：先打印诊断文件（不存在或为空的跳过），最后一行是 FAIL。诊断输出不能抢在
# 结论行之前中止脚本：set -e 下 cat 一个不存在的文件会先让脚本退出，FAIL 行就打不出来了。
fail() {
  msg=$1
  shift
  for f in "$@"; do
    if [ -s "$f" ]; then
      echo "--- $f" >&2
      cat "$f" >&2
      # 响应体之类不以换行结尾的文件后面补一个，FAIL 行总是独占一行。
      [ -z "$(tail -c 1 "$f")" ] || echo >&2
    fi
  done
  echo "FAIL: $msg" >&2
  exit 1
}

# save_logs NAME：容器的 stdout 与 stderr 分别存到 $work/NAME.out、$work/NAME.err。
save_logs() {
  if ! docker logs "$1" > "$work/$1.out" 2> "$work/$1.err"; then
    echo "(docker logs $1 failed)" >> "$work/$1.err"
  fi
}

# 运行被测镜像的唯一入口：带上指定的平台（未指定时由 docker 取默认平台），且只用本机已有的镜像。
# docker run 默认 --pull=missing，本机没有这个 tag 时会去 registry 拉已发布的同名镜像来冒烟，版本断言
# 照样通过；回读脚本在冒烟之前按平台显式拉取。
drun() {
  if [ -n "$platform" ]; then
    docker run --pull=never --platform "$platform" "$@"
  else
    docker run --pull=never "$@"
  fi
}

# run_to_exit NAME TIMEOUT_MESSAGE ARGS…：起容器并等它退出，至多轮询 wait_s 秒；退出码存入 code，
# 日志经 save_logs 存档。
run_to_exit() {
  name=$1
  timeout_message=$2
  shift 2
  drun -d --name "$name" "$@" > /dev/null || fail "cannot start $name from $IMAGE"
  polls=0
  until [ "$(docker inspect -f '{{.State.Status}}' "$name")" = exited ]; do
    if [ "$polls" -ge "$((wait_s * 2))" ]; then
      save_logs "$name"
      fail "$timeout_message (still running after ${wait_s}s of polling)" "$work/$name.out" "$work/$name.err"
    fi
    sleep 0.5
    polls=$((polls + 1))
  done
  code=$(docker inspect -f '{{.State.ExitCode}}' "$name")
  save_logs "$name"
}

# 有时限：ENTRYPOINT 若被改成带 serve 的形式，version 会被当作 serve 的多余参数，hub 起来后不退出。
run_to_exit "$run_id-version" "heron-hub version did not exit" "$IMAGE" version
[ "$code" = 0 ] || fail "heron-hub version exited $code" "$work/$run_id-version.out" "$work/$run_id-version.err"
got=$(cat "$work/$run_id-version.out")
[ "$got" = "$VERSION" ] || fail "image reports version '$got', want '$VERSION'"
echo "version ok: $got"

# 三种失败分开报：容器已退出（没起来）、到上限仍无 HTTP 应答（起了但没在监听）、有应答但不是面板。
hub_running() {
  state=$(docker inspect -f '{{.State.Status}} {{.State.ExitCode}}' "$hub")
  case $state in
    running*) ;;
    *)
      save_logs "$hub"
      fail "hub container is not running ($state)" "$work/$hub.out" "$work/$hub.err"
      ;;
  esac
}

docker volume create "$data" > /dev/null
drun -d --name "$hub" --stop-timeout "$stop_timeout" -p 127.0.0.1::8080 -v "$data:/data" "$IMAGE" > /dev/null ||
  fail "cannot start $hub from $IMAGE"
# 容器已经退出时 docker port 同样失败；先按没起来报并附上 hub 的日志，端口确实没发布才报端口。
hostport=$(docker port "$hub" 8080/tcp) || {
  hub_running
  fail "port 8080 of $hub is not published"
}
base="http://127.0.0.1:${hostport##*:}"

polls=0
while :; do
  hub_running
  rc=0
  status=$(curl -sS --max-time 2 -o "$work/admin.html" -w '%{http_code}' "$base/admin/" 2> "$work/admin.curl") || rc=$?
  [ "$rc" = 0 ] && break
  if [ "$polls" -ge "$((wait_s * 2))" ]; then
    save_logs "$hub"
    fail "no HTTP answer on $base/admin/ after ${wait_s}s of polling (curl exit $rc)" "$work/$hub.out" "$work/$hub.err" "$work/admin.curl"
  fi
  sleep 0.5
  polls=$((polls + 1))
done
# 503 是 internal/hub/web 的"面板没有构建进二进制"说明页：镜像里的 hub 缺了 make web 的产物。
[ "$status" = 200 ] || fail "/admin/ returned $status, want 200" "$work/admin.html"
grep -q 'id="root"' "$work/admin.html" || fail "/admin/ is not the panel index" "$work/admin.html"
echo "admin ok: $base/admin/"

# 镜像自带的 HEALTHCHECK 由 Dockerfile 的 exec 形式 heron-hub health 驱动：容器在库打开、索引加载并挂载
# 全部服务之前不报 healthy（探针只在 Serve 之后才被应答）。起始宽限 5s 加一次间隔 30s，45s 上限足够；
# 超时把最后一次完整 health 状态落文件再失败——探针的 stderr 在 docker inspect 的 Log 里。
polls=0
while :; do
  health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}' "$hub")
  [ "$health" = healthy ] && break
  if [ "$polls" -ge "$((45 * 2))" ]; then
    docker inspect -f '{{json .State.Health}}' "$hub" > "$work/health.json" 2>&1 || true
    fail "container did not become healthy after 45s of polling (status '$health')" "$work/health.json"
  fi
  sleep 0.5
  polls=$((polls + 1))
done
echo "healthcheck ok: healthy"

# 根路径是公开页：它的产物 dist-public 与面板一样随 make web 进二进制，漏掉时 / 是"公开页没有构建进
# 二进制"的 503 说明页。公开页的 index 以 / 为 base，脚本路径是 /assets/…；面板的是 /admin/assets/…，据此分辨两者。
status=$(curl -sS --max-time 2 -o "$work/root.html" -w '%{http_code}' "$base/" 2> "$work/root.curl") ||
  fail "no HTTP answer on $base/" "$work/root.curl"
[ "$status" = 200 ] || fail "/ returned $status, want 200" "$work/root.html"
grep -q 'src="/assets/' "$work/root.html" || fail "/ is not the public page index" "$work/root.html"
echo "public ok: $base/"

docker logs "$hub" > "$work/hub.log" 2>&1
# 容器里监听 0.0.0.0 是镜像的默认参数。hub 发现监听地址不是回环时仍写启动告警：能直连这个端口的人都能绕过反代并自带转发头。
grep -q 'listening on a non-loopback address' "$work/hub.log" ||
  fail "startup log lacks the non-loopback listen warning" "$work/hub.log"
# 镜像里没有 /etc/localtime、默认不设 TZ。hub 解析不到宿主时区时按 UTC 运行并在启动日志告警；docs/operations.md「时区」因此要求显式给出时区。
grep -q 'host time zone could not be resolved; using UTC' "$work/hub.log" ||
  fail "startup log lacks the UTC fallback warning" "$work/hub.log"
echo "startup warnings ok"

# docs/install.md「用 Docker 运行 hub」的命令按名字执行 heron-hub：二进制必须在容器的默认 PATH 里。
# 两个流都落到同一个文件：exec 本身失败时（如 executable file not found in $PATH），docker CLI 把
# OCI 运行时的报错写在 stdout 上，只收 stderr 会让诊断丢失。
docker exec "$hub" heron-hub version > "$work/exec.log" 2>&1 ||
  fail "docker exec $hub heron-hub version failed: heron-hub is not runnable by name" "$work/exec.log"
got=$(cat "$work/exec.log")
[ "$got" = "$VERSION" ] || fail "docker exec heron-hub version printed '$got', want '$VERSION'"

# 不带 -i 时容器里的 stdin 是 /dev/null：passwd 必须报没有收到密码，且不改管理员表。
rc=0
docker exec "$hub" heron-hub passwd --db /data/heron.db > "$work/passwd-noinput.log" 2>&1 || rc=$?
[ "$rc" != 0 ] || fail "passwd without -i succeeded" "$work/passwd-noinput.log"
grep -q 'no password on stdin' "$work/passwd-noinput.log" ||
  fail "passwd without -i did not report the missing input" "$work/passwd-noinput.log"
docker exec "$hub" heron-hub stats --db /data/heron.db > "$work/stats.log" 2>&1 ||
  fail "heron-hub stats" "$work/stats.log"
grep -qx 'admin: 0' "$work/stats.log" || fail "passwd without input changed the admin table" "$work/stats.log"
echo "passwd without -i ok"

pw='smoke admin password 2026'
printf '%s\n' "$pw" | docker exec -i "$hub" heron-hub passwd --db /data/heron.db > "$work/passwd.log" 2>&1 ||
  fail "passwd via docker exec -i" "$work/passwd.log"
# login NAME PASSWORD：打印 HTTP 状态码（连不上时 curl 打印 000）；每次调用有自己的响应文件，
# 失败时看到的不会是上一次调用留下的内容。
login() {
  rm -f "$work/login-$1.json" "$work/login-$1.headers" "$work/login-$1.curl"
  curl -sS --max-time 10 -o "$work/login-$1.json" -D "$work/login-$1.headers" -w '%{http_code}' \
    -H 'Content-Type: application/json' --data "{\"password\":\"$2\"}" "$base/heron.v1.AdminService/Login" \
    2> "$work/login-$1.curl"
}
rc=0
status=$(login wrong 'not the admin password') || rc=$?
[ "$status" = 401 ] ||
  fail "a wrong password was not rejected with 401 (got $status, curl exit $rc)" "$work/login-wrong.json" "$work/login-wrong.curl"
rc=0
status=$(login right "$pw") || rc=$?
[ "$status" = 200 ] ||
  fail "login with the password set through docker exec (got $status, curl exit $rc)" "$work/login-right.json" "$work/login-right.curl"
grep -qi '^set-cookie: heron_session=' "$work/login-right.headers" ||
  fail "login did not set the session cookie" "$work/login-right.headers"
echo "passwd and login ok"

# 两个属主各有来源。/data 属 65532：Dockerfile 把镜像里的 /data 交给 65532，空卷挂上时 Docker 沿用镜像里
# 该目录的属主。heron.db 属 65532：文件属于创建它的进程的 uid，即以 USER 65532:65532 运行的 hub——
# 镜像配置里的 USER 在这里得到核对（checkimage 看的导出 tar 里没有镜像配置）；USER 为 root 时后面"库目录
# 不可写"一段同样会红。
docker run --rm --name "$run_id-owner" -v "$data:/data" "$tool" \
  stat -c '%u:%g %n' /data /data/heron.db > "$work/owner.log" 2> "$work/owner.err" ||
  fail "stat the data volume" "$work/owner.log" "$work/owner.err"
awk '$1 != "65532:65532" { bad = 1 } END { exit bad || NR != 2 }' "$work/owner.log" ||
  fail "data volume entries are not owned by 65532:65532" "$work/owner.log"
# docs/operations.md「排查」：镜像里没有 shell，用工具镜像进入 hub 的网络命名空间发请求。
docker run --rm --name "$run_id-netns" --network "container:$hub" "$tool" \
  wget -q -T 10 -O /dev/null http://127.0.0.1:8080/admin/ > "$work/netns.log" 2> "$work/netns.err" ||
  fail "request from the hub's network namespace" "$work/netns.log" "$work/netns.err"
echo "volume owner and debugging recipes ok"

# docker stop 先发 SIGTERM，等 --stop-timeout 秒后发 SIGKILL。hub 订阅了 SIGTERM（cmd/hub/serve.go 的
# runServe），排空请求、停后台循环、关库后返回 nil，以 0 退出。其余退出码逐条对应：143 是进程被 SIGTERM
# 的默认处置杀死，即没有订阅；137 是关停超过宽限期被 SIGKILL；1 是关停路径返回了错误。任一非 0，
# 退出前的落盘都没有得到确认。
docker stop "$hub" > /dev/null
code=$(docker inspect -f '{{.State.ExitCode}}' "$hub")
save_logs "$hub"
case $code in
  0) ;;
  143) fail "hub was killed by SIGTERM on docker stop (exit 143): SIGTERM is not handled" "$work/$hub.err" ;;
  137) fail "hub was SIGKILLed after the ${stop_timeout}s stop timeout (exit 137): shutdown did not finish" "$work/$hub.err" ;;
  1) fail "hub's shutdown path returned an error on docker stop (exit 1)" "$work/$hub.err" ;;
  *) fail "hub exited $code on docker stop, want 0" "$work/$hub.err" ;;
esac
echo "stop ok"

# 库目录不可写（绑定了属主为 root 的宿主目录）：卷里已有内容时 Docker 不改它的属主，
# 用预置了 root 文件的卷模拟。hub 必须退出，报错里写出库路径。
docker volume create "$denied_data" > /dev/null
docker run --rm --name "$run_id-prepare" -v "$denied_data:/data" "$tool" \
  sh -c 'touch /data/placeholder && chown 0:0 /data /data/placeholder && chmod 755 /data' ||
  fail "prepare the root-owned volume"
run_to_exit "$denied" "hub kept running on an unwritable /data" -v "$denied_data:/data" "$IMAGE"
[ "$code" = 1 ] || fail "hub on an unwritable /data exited $code, want 1" "$work/$denied.out" "$work/$denied.err"
grep -q 'open database /data/heron.db' "$work/$denied.err" ||
  fail "error on an unwritable /data does not name the database path" "$work/$denied.out" "$work/$denied.err"
echo "unwritable data dir ok"
echo "docker smoke passed: $IMAGE${platform:+ ($platform)}"
