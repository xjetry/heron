#!/bin/sh
# 端到端：hub 跑在宿主机，agent 跑在 Linux 容器里经 host.docker.internal 上报；
# 管理 API 用 curl + jq 走纯 HTTP + JSON——这是面向 agent 设计的验收方式，不用生成客户端。
# 两个平台都必须实际运行；依赖 Docker 多架构模拟，CI 显式安装，OrbStack / Docker Desktop 自带。
# 验收凭据是库里出现两个节点、facts 与分钟行，且管理 API 看到它们在线并查到历史。
set -eu
# 镜像与期望系统名成对给出：缺一个就无法证明容器真的换成了目标发行版，不能退化成不检查。
: "${AGENT_IMAGE:?AGENT_IMAGE is required, e.g. alpine:3.21}"
: "${EXPECT_OS:?EXPECT_OS is required, e.g. Alpine}"
echo "agent image: $AGENT_IMAGE (expect os containing \"$EXPECT_OS\")"
cd "$(cd "$(dirname "$0")/.." && pwd)"
# hub 始终来自当前源码；agent 可来自校验过的发布包，不覆盖当前源码构建的产物。
agent_bin=$(cd "${E2E_AGENT_BIN_DIR:-$PWD/bin}" && pwd)
for arch in amd64 arm64; do
  [ -x "$agent_bin/heron-agent-linux-$arch" ] || { echo "FAIL: missing executable agent for $arch" >&2; exit 1; }
done
work=$(mktemp -d)
echo "E2E artifacts: $work"
db="$work/e2e.db"
# 端口默认 18080（hub）/18081（webhook 接收器），与 install-accept 的 18085/18086、macos-accept 的 18087/18088 错开；
# 本机上别的进程占着默认端口时，用环境变量 E2E_HUB_PORT、E2E_HOOK_PORT 覆盖。
port=${E2E_HUB_PORT:-18080}
hook_port=${E2E_HOOK_PORT:-18081}
base="http://127.0.0.1:$port"
# 原生 Linux 上 host-gateway 指向网桥而非宿主回环；CI 使用一次性运行器，显式允许监听网桥。
# 开发机默认仍只监听回环，不因本地验收向其它网卡暴露管理端口。
listen_host=${E2E_LISTEN_HOST:-127.0.0.1}
admin_pw="e2e admin password 2026"
: > "$work/jar"
hookrecv=""
hub=""
# 每次运行只回收自己的容器；失败也必须停止上报，不能把流量带进下一次验收。
# 清理先于一切会留下副作用的步骤安装：容器准备中途失败时，已启动的容器也要回收。
cleanup() {
  if [ -n "$hookrecv" ]; then
    kill "$hookrecv" || true
    wait "$hookrecv" || true
    hookrecv=""
  fi
  for arch in amd64 arm64; do
    if [ -s "$work/cid-$arch" ]; then
      docker rm -f "$(cat "$work/cid-$arch")" || true
    fi
  done
  if [ -n "$hub" ]; then
    kill "$hub" 2>/dev/null || true
    wait "$hub" 2>/dev/null || true
  fi
}
# 被信号打断时 dash 不执行 EXIT trap（容器实测），macOS 的 /bin/sh 实测会执行但 sh 不保证：把 INT、TERM、HUP
# 转成 exit 1，Ctrl-C 时容器也会被回收、停止上报。
trap cleanup EXIT
trap 'exit 1' INT TERM HUP

# 迁移前备份：用与单元测试同源的冻结 v40 库（TestWriteSchemaFixture 经 frozenSchemaFixture 写出，脚本不另抄 DDL）
# 启动当前源码的 serve。库旁应出现 0600 的 .before-schema-40.1，日志先记备份、后记迁移；备份是升级前的库：当前
# 二进制的离线命令以"比本二进制旧"拒绝它（docs/operations.md「升级与回退」核对二进制与备份是否匹配，凭的就是这条报错）；
# 迁移后的库里 stats 列出这份备份、节点仍在。在主 hub 启动之前、同一端口上先起先停，不占别的端口。
migrate_db="$work/migrate.db"
migrate_backup="$migrate_db.before-schema-40.1"
HERON_SCHEMA_FIXTURE_OUT="$migrate_db" HERON_SCHEMA_FIXTURE_VERSION=40 \
  go test -count=1 -run '^TestWriteSchemaFixture$' ./internal/hub/store/ > "$work/migrate-fixture.log" 2>&1 ||
  { echo "FAIL: writing the schema 40 fixture"; cat "$work/migrate-fixture.log"; exit 1; }
# go test 在没收到环境变量时会跳过并照样成功：以文件是否写出为准。
[ -s "$migrate_db" ] || { echo "FAIL: schema 40 fixture was not written"; cat "$work/migrate-fixture.log"; exit 1; }
bin/heron-hub serve --db "$migrate_db" --listen "$listen_host:$port" --timezone UTC > "$work/migrate-hub.log" 2>&1 &
hub=$!
attempt=0
until grep -q 'msg="hub listening"' "$work/migrate-hub.log"; do
  kill -0 "$hub" 2> /dev/null || { echo "FAIL: hub exited while migrating the schema 40 fixture"; cat "$work/migrate-hub.log"; exit 1; }
  attempt=$((attempt + 1))
  [ "$attempt" -lt 300 ] || { echo "FAIL: hub did not start on the schema 40 fixture within 60s"; cat "$work/migrate-hub.log"; exit 1; }
  sleep 0.2
done
kill "$hub"
wait "$hub" || { echo "FAIL: hub on the migrated fixture did not exit cleanly on SIGTERM"; cat "$work/migrate-hub.log"; exit 1; }
hub=""
[ -f "$migrate_backup" ] || { echo "FAIL: no pre-migration backup at $migrate_backup"; ls -l "$work"; cat "$work/migrate-hub.log"; exit 1; }
[ -n "$(find "$migrate_backup" -prune -type f -perm 600)" ] || { echo "FAIL: pre-migration backup is not a 0600 regular file"; ls -l "$migrate_backup"; exit 1; }
backup_line=$(grep -n 'msg="database backed up before migration"' "$work/migrate-hub.log" | sed -n 1p)
migrated_line=$(grep -n 'msg="database schema migrated"' "$work/migrate-hub.log" | sed -n 1p)
case "$backup_line" in
  *" path=$migrate_backup schema_version=40 "*" seq=1"*) ;;
  *) echo "FAIL: hub log lacks the backup line for $migrate_backup"; cat "$work/migrate-hub.log"; exit 1 ;;
esac
case "$migrated_line" in
  *" from=40 to="*) ;;
  *) echo "FAIL: hub log lacks the migration line from 40"; cat "$work/migrate-hub.log"; exit 1 ;;
esac
[ "${backup_line%%:*}" -lt "${migrated_line%%:*}" ] || { echo "FAIL: migration was logged before the backup"; cat "$work/migrate-hub.log"; exit 1; }
cp "$migrate_backup" "$work/backup-copy.db"
if bin/heron-hub stats --db "$work/backup-copy.db" > "$work/backup-stats.out" 2>&1; then
  echo "FAIL: the current binary opened the pre-migration backup; it should be schema 40"; cat "$work/backup-stats.out"; exit 1
fi
grep -q 'database schema version 40 is older than this binary' "$work/backup-stats.out" ||
  { echo "FAIL: the backup was not rejected as schema 40"; cat "$work/backup-stats.out"; exit 1; }
bin/heron-hub stats --db "$migrate_db" > "$work/migrate-stats.out" 2>&1 ||
  { echo "FAIL: stats on the migrated fixture"; cat "$work/migrate-stats.out"; exit 1; }
if ! grep -qx 'schema_backups.state: listed' "$work/migrate-stats.out" ||
  ! grep -q "^schema_backup.1: path=\"$migrate_backup\" schema_version=40 " "$work/migrate-stats.out" ||
  ! grep -qx 'node: 2' "$work/migrate-stats.out"; then
  echo "FAIL: stats on the migrated fixture does not list the backup and both nodes"; cat "$work/migrate-stats.out"; exit 1
fi
echo "pre-migration backup ok: $migrate_backup"

# 镜像冷拉取可能远超注册窗口与上线等待的预算，它属于准备阶段，不能消耗这些预算；
# 任一架构准备失败就直接退出，不进入注册阶段。注册与上报复用这两个容器。
for arch in amd64 arm64; do
  docker run -d --cidfile "$work/cid-$arch" --platform "linux/$arch" --add-host=host.docker.internal:host-gateway \
    -v "$agent_bin:/heron:ro" "$AGENT_IMAGE" sleep infinity > /dev/null
  if [ -n "${E2E_AGENT_VERSION:-}" ]; then
    actual=$(docker exec "$(cat "$work/cid-$arch")" "/heron/heron-agent-linux-$arch" version)
    [ "$actual" = "$E2E_AGENT_VERSION" ] || { echo "FAIL: $arch agent version $actual, expected $E2E_AGENT_VERSION" >&2; exit 1; }
  fi
done
echo "agent containers ready: $AGENT_IMAGE"

bin/heron-hub window open --db "$db" --ttl 10m --max 2 > "$work/window.txt"
key=$(sed -n 's/^key: //p' "$work/window.txt")
[ -n "$key" ] || { echo "no key"; exit 1; }
echo "registration window: $(sed -n 's/^expires: //p' "$work/window.txt")"

hub_log_from=0
HERON_OFFLINE_AFTER=12s bin/heron-hub serve --db "$db" --listen "$listen_host:$port" --timezone UTC > "$work/hub.log" 2>&1 &
hub=$!

# wait_hub：等本次启动的 hub 就绪。三者同时成立才算：
#   - $hub 进程还在；已经退出（例如端口被占、绑定失败）就停下并打印 hub.log；
#   - hub.log 第 $hub_log_from 行之后出现了 "hub listening"：serve 在 net.Listen 成功之后才写这一行，重启时 hub.log
#     是追加写的，所以只看这次启动之后的内容；
#   - 匿名的 GetSite 返回 200：根路径的应答取决于公开页是否构建进二进制、是否换了 --public-dir，GetSite 两者都不取决。
# 只看应答不够：端口被别的进程占着时，hub 绑定失败退出，应答却来自那个进程，而且第一次探测往往早于 hub 退出，
# 进程检查也拦不住；之后的报错（读不到启动行）与真实原因无关。启动行只由绑定成功的 hub 写出，据它区分。
wait_hub() {
  attempt=0
  while [ "$attempt" -lt 30 ]; do
    kill -0 "$hub" 2> /dev/null || { echo "FAIL: hub exited during startup"; cat "$work/hub.log"; exit 1; }
    if tail -n "+$((hub_log_from + 1))" "$work/hub.log" | grep -q 'msg="hub listening"' &&
      status=$(curl -s -o /dev/null -w '%{http_code}' "$base/heron.v1.PublicService/GetSite?connect=v1&encoding=json&message=%7B%7D"); then
      [ "$status" = 200 ] && return 0
    fi
    attempt=$((attempt + 1))
    sleep 0.2
  done
  echo "FAIL: hub did not answer on $base within 6s (expected an anonymous GetSite to return 200)"
  exit 1
}
wait_hub
# hub 就绪后先钉住探活入口：/healthz 是给 HEALTHCHECK 与运维脚本用的最小应答，正文恰为 ok\n；
# heron-hub health 是镜像内探针，按退出码判活，两者对同一个 hub 必须一致。
[ "$(curl -sS -o "$work/healthz.body" -w '%{http_code}' "$base/healthz")" = 200 ] ||
  { echo "FAIL: /healthz did not return 200"; cat "$work/healthz.body"; exit 1; }
printf 'ok\n' > "$work/healthz.want"
cmp -s "$work/healthz.body" "$work/healthz.want" ||
  { echo "FAIL: /healthz body is not exactly the two bytes ok and newline"; cat "$work/healthz.body"; exit 1; }
bin/heron-hub health --url "$base" > "$work/health-cli.out" 2> "$work/health-cli.err" ||
  { echo "FAIL: heron-hub health --url $base exited non-zero"; cat "$work/health-cli.out" "$work/health-cli.err"; exit 1; }
[ "$(cat "$work/health-cli.out")" = ok ] ||
  { echo "FAIL: heron-hub health did not print ok"; cat "$work/health-cli.out"; exit 1; }
echo "hub health endpoint and command ok"
# 告警等待上限由 hub 与 agent 实际生效的参数推出（见 wait_alert 的调用处），参数读自两者的启动行，
# 脚本里不另抄一份。只认整秒写法：读不出来就停下，不能退回一个与实际参数无关的固定上限。
# startup_seconds 日志文件 启动行消息 字段名。internal/testlog.WholeSeconds 与这里同形（msg 过滤、字段前后
# 空格、纯整秒、取第一条能读出的），make ci 的启动行测试靠它钉住本脚本读得出的写法，改一处须同改另一处。
startup_seconds() {
  sed -n "s/.*msg=\"$2\".* $3=\([0-9][0-9]*\)s .*/\1/p" "$1" | sed -n 1p
}
ttl_s=$(startup_seconds "$work/hub.log" "hub listening" ttl)
sweep_s=$(startup_seconds "$work/hub.log" "hub listening" offline_sweep)
retry_wait_s=$(startup_seconds "$work/hub.log" "hub listening" delivery_retry_wait)
[ -n "$ttl_s" ] && [ -n "$sweep_s" ] && [ -n "$retry_wait_s" ] || { echo "FAIL: hub startup line does not state ttl, offline_sweep and delivery_retry_wait in whole seconds"; cat "$work/hub.log"; exit 1; }

# 运行中设密码：另一进程经 WAL 写库，登录路径每次读库，不需要重启 hub。
printf '%s\n' "$admin_pw" | bin/heron-hub passwd --db "$db" > "$work/passwd.log" 2>&1

# rpc 名字 请求体 [额外 curl 参数]：向 AdminService 发 JSON，打印 HTTP 状态码，响应体落 $work/<名字>.json。
rpc() {
  name=$1; body=$2; shift 2
  curl -sS -o "$work/$name.json" -w '%{http_code}' -H 'Content-Type: application/json' \
    -b "$work/jar" -c "$work/jar" "$@" --data "$body" "$base/heron.v1.AdminService/$name"
}

# bearer 名字 请求体：用 API token 调 AdminService，不带 cookie；打印状态码，响应体落 $work/bearer-<名字>.json。
bearer() {
  name=$1; body=$2
  curl -sS -o "$work/bearer-$name.json" -w '%{http_code}' -H 'Content-Type: application/json' \
    -H "Authorization: Bearer $api_token" --data "$body" "$base/heron.v1.AdminService/$name"
}

# pubget 名字 方法 请求消息：以 GET 匿名调用 PublicService，不带 cookie 与 token；打印状态码，
# 响应体落 $work/pub-<名字>.json，响应头落 $work/pub-<名字>.headers。
pubget() {
  name=$1; method=$2; msg=$3
  curl -sS -G -o "$work/pub-$name.json" -D "$work/pub-$name.headers" -w '%{http_code}' \
    --data-urlencode connect=v1 --data-urlencode encoding=json --data-urlencode "message=$msg" "$base/heron.v1.PublicService/$method"
}

# hdr 名字 头名：打印 $work/pub-<名字>.headers 里该头的值（头名不分大小写，去掉行尾 CR）；没有这个头时不打印。
hdr() {
  awk -v want="$2" 'BEGIN { want = tolower(want) } { sub(/\r$/, "") } tolower(substr($0, 1, length(want) + 2)) == want ": " { print substr($0, length(want) + 3) }' "$work/pub-$1.headers"
}

# themereq 名字 路径 [额外 curl 参数]：请求同域主题路径，正文落 $work/theme-<名字>.body，
# 响应头落 $work/theme-<名字>.headers。
themereq() {
  name=$1; path=$2; shift 2
  curl -sS -o "$work/theme-$name.body" -D "$work/theme-$name.headers" -w '%{http_code}' "$@" "$base$path"
}

# themehdr 名字 头名：同 hdr，读 $work/theme-<名字>.headers。
themehdr() {
  awk -v want="$2" 'BEGIN { want = tolower(want) } { sub(/\r$/, "") } tolower(substr($0, 1, length(want) + 2)) == want ": " { print substr($0, length(want) + 3) }' "$work/theme-$1.headers"
}

# 技能文件里的示例取自 hub 刚下发的那份。空列表由例子自己处理，空 hub 与有数据时用同一段。
# 第二个参数非空时，每个例子的顶层 JSON 必须非空：有数据时例 2 输出 {} 说明取 id 走错了分支。
run_skill_examples() {
  label=$1
  nonempty=$2
  jq -r '.guide' "$work/bearer-GetApiReference.json" > "$work/SKILL.md"
  rm -f "$work"/skill-example-*.sh "$work"/skill-example-*.sh.out "$work"/skill-example-*.sh.err
  awk -v dir="$work" '
    /^```sh example$/ { n++; file = sprintf("%s/skill-example-%d.sh", dir, n); inblock = 1; next }
    inblock && /^```$/ { inblock = 0; close(file); next }
    inblock { print > file }
  ' "$work/SKILL.md"
  examples=$(ls "$work"/skill-example-*.sh 2> /dev/null | wc -l | tr -d ' ')
  # 标记块数独立于抽取逻辑另数一次：抽取漏块或多切时两数不等；技能文件被删到只剩寥寥几例时下限挡住。
  marked=$(grep -c '^```sh example$' "$work/SKILL.md" || [ "$?" = 1 ])
  [ "$examples" = "$marked" ] || { echo "FAIL: extracted $examples skill examples but SKILL.md marks $marked"; exit 1; }
  [ "$examples" -ge 3 ] || { echo "FAIL: expected at least 3 skill examples, found $examples"; exit 1; }
  for ex in "$work"/skill-example-*.sh; do
    status=0
    HERON_HUB=$base HERON_TOKEN=$api_token sh -eu "$ex" > "$ex.out" 2> "$ex.err" || status=$?
    [ "$status" = 0 ] || { echo "FAIL: skill example $ex exited $status"; cat "$ex" "$ex.err"; exit 1; }
    [ -s "$ex.out" ] && jq -e . "$ex.out" > /dev/null || { echo "FAIL: skill example $ex did not print JSON"; cat "$ex" "$ex.out" "$ex.err"; exit 1; }
    # null 的 length 是 0，不能靠 length 单独把 null 当成有内容；空对象与空数组的 length 也是 0。
    if [ -n "$nonempty" ]; then
      jq -e 'if . == null then false else length > 0 end' "$ex.out" > /dev/null || { echo "FAIL: skill example $ex printed empty JSON"; cat "$ex" "$ex.out"; exit 1; }
    fi
  done
  if [ -n "$label" ]; then
    echo "skill examples ok ($label): $examples"
  else
    echo "skill examples ok: $examples"
  fi
}

# 根路径是内置公开页，面板在 /admin/；两者各有一份构建产物，资源分别引用 /assets/ 与 /admin/assets/。
[ "$(curl -sS -o "$work/pub-index.html" -D "$work/pub-index.headers" -w '%{http_code}' "$base/")" = 200 ] || { echo "FAIL: / did not serve the public page"; exit 1; }
grep -q 'src="/assets/' "$work/pub-index.html" || { echo "FAIL: public page does not load its own bundle"; cat "$work/pub-index.html"; exit 1; }
[ -n "$(hdr index Content-Security-Policy)" ] || { echo "FAIL: CSP header missing on the public page"; cat "$work/pub-index.headers"; exit 1; }
[ "$(curl -sS -o "$work/admin.html" -w '%{http_code}' "$base/admin/")" = 200 ] || { echo "FAIL: /admin/ not served"; exit 1; }
grep -q 'id="root"' "$work/admin.html" || { echo "FAIL: panel index missing root element"; exit 1; }
grep -q 'src="/admin/assets/' "$work/admin.html" || { echo "FAIL: panel does not load its own bundle"; cat "$work/admin.html"; exit 1; }
curl -sS -D "$work/admin.headers" -o /dev/null "$base/admin/" && grep -qi '^content-security-policy:' "$work/admin.headers" || { echo "FAIL: CSP header missing"; exit 1; }

[ "$(rpc GetSnapshot '{}')" = 401 ] || { echo "FAIL: anonymous GetSnapshot was not 401"; exit 1; }
# 公开服务匿名可达；从未保存过外观时明暗为 auto，其余为空（JSON 里省略）。
[ "$(pubget site-default GetSite '{}')" = 200 ] || { echo "FAIL: anonymous GetSite"; cat "$work/pub-site-default.json"; exit 1; }
jq -e '. == {theme: "auto", adminPath: "/admin/"}' "$work/pub-site-default.json" > /dev/null || { echo "FAIL: default site settings"; cat "$work/pub-site-default.json"; exit 1; }
[ "$(hdr site-default Cache-Control)" = "max-age=300" ] || { echo "FAIL: GetSite Cache-Control"; cat "$work/pub-site-default.headers"; exit 1; }
login_body=$(jq -nc --arg password "$admin_pw" '{password: $password}')
[ "$(rpc Login "$login_body")" = 200 ] || { echo "FAIL: login"; cat "$work/Login.json"; exit 1; }
# 外观整体替换并回显 hub 实际保存的值（主色转小写），公开页随即拿到；被拒的更新什么都不写。
settings_body='{"settings": {"title": "e2e 状态", "theme": "dark", "accentColor": "#FF5500", "customCss": ".card { border-width: 2px; }", "publicEnabled": true}}'
[ "$(rpc UpdateSettings "$settings_body")" = 200 ] || { echo "FAIL: UpdateSettings"; cat "$work/UpdateSettings.json"; exit 1; }
jq -e '.settings.accentColor == "#ff5500"' "$work/UpdateSettings.json" > /dev/null || { echo "FAIL: UpdateSettings echo"; cat "$work/UpdateSettings.json"; exit 1; }
[ "$(pubget site GetSite '{}')" = 200 ] || { echo "FAIL: GetSite after update"; exit 1; }
jq -e '. == {title: "e2e 状态", theme: "dark", accentColor: "#ff5500", customCss: ".card { border-width: 2px; }", adminPath: "/admin/"}' "$work/pub-site.json" > /dev/null || { echo "FAIL: GetSite does not serve the saved settings"; cat "$work/pub-site.json"; exit 1; }
[ "$(rpc UpdateSettings '{"settings": {"theme": "auto", "customCss": "a</style>"}}')" = 400 ] || { echo "FAIL: CSS containing </ was accepted"; cat "$work/UpdateSettings.json"; exit 1; }
grep -q 'settings.custom_css must not contain' "$work/UpdateSettings.json" || { echo "FAIL: error must name the field"; cat "$work/UpdateSettings.json"; exit 1; }
[ "$(pubget site-after-reject GetSite '{}')" = 200 ] && cmp -s "$work/pub-site.json" "$work/pub-site-after-reject.json" || { echo "FAIL: a rejected update changed the site"; cat "$work/pub-site-after-reject.json"; exit 1; }
# hub 上还没有节点：技能文件的例子必须在空库上也能跑完。
[ "$(rpc CreateApiToken '{"name":"e2e-empty"}')" = 200 ] || { echo "FAIL: CreateApiToken (empty hub)"; cat "$work/CreateApiToken.json"; exit 1; }
api_token=$(jq -r '.token' "$work/CreateApiToken.json")
api_token_id=$(jq -r '.apiToken.id' "$work/CreateApiToken.json")
[ "$(bearer GetApiReference '{}')" = 200 ] || { echo "FAIL: GetApiReference on empty hub"; cat "$work/bearer-GetApiReference.json"; exit 1; }
run_skill_examples "empty hub" ""
[ "$(rpc DeleteApiToken "$(jq -nc --arg id "$api_token_id" '{id: $id}')")" = 200 ] || { echo "FAIL: DeleteApiToken (empty hub)"; exit 1; }
[ "$(curl -sS -o /dev/null -w '%{http_code}' -b "$work/jar" -H 'Content-Type: text/plain' --data '{}' "$base/heron.v1.AdminService/CreateNode")" = 415 ] || { echo "FAIL: text/plain POST was not 415"; exit 1; }
[ "$(curl -sS -o /dev/null -w '%{http_code}' -b "$work/jar" "$base/heron.v1.AdminService/CreateNode?connect=v1&encoding=json&message=%7B%7D")" = 405 ] || { echo "FAIL: GET was not 405"; exit 1; }

# 同域主题经可信容器承载，包文件必须带文档沙箱，管理 API 拒绝不透明来源。
mkdir -p "$work/theme/assets"
printf '%s\n' '<!doctype html><title>e2e theme</title><script src="./assets/app.js"></script>' > "$work/theme/index.html"
printf '%s\n' 'console.log("e2e theme")' > "$work/theme/assets/app.js"
printf '%s\n' '{"id": "e2e-theme", "name": "e2e theme", "version": "1", "sdk": 1}' > "$work/theme/theme.json"
python3 - "$work/theme" "$work/theme.zip" << 'PY'
import os, sys, zipfile
src, out = sys.argv[1:]
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
    for root, _, files in os.walk(src):
        for f in files:
            path = os.path.join(root, f)
            z.write(path, os.path.relpath(path, src))
PY
[ "$(rpc UploadTheme "$(jq -nc --arg pkg "$(base64 < "$work/theme.zip" | tr -d '\n')" '{package: $pkg}')")" = 200 ] || { echo "FAIL: UploadTheme"; cat "$work/UploadTheme.json"; exit 1; }
digest=$(jq -r '.theme.digest' "$work/UploadTheme.json")
[ "$(rpc EnableTheme "$(jq -nc --arg digest "$digest" '{id: "e2e-theme", digest: $digest}')")" = 200 ] || { echo "FAIL: EnableTheme"; cat "$work/EnableTheme.json"; exit 1; }
[ "$(rpc ListThemes '{}')" = 200 ] || { echo "FAIL: ListThemes"; exit 1; }
jq -e '(.themes | length) == 1 and .themes[0].enabled == true' "$work/ListThemes.json" > /dev/null || { echo "FAIL: ListThemes content"; exit 1; }
theme_path="/_heron/themes/e2e-theme/$digest"
[ "$(themereq index /)" = 200 ] && grep -q 'sandbox="allow-scripts"' "$work/theme-index.body" || { echo "FAIL: trusted theme shell"; exit 1; }
[ "$(themereq deep /nodes/1)" = 200 ] && cmp -s "$work/theme-index.body" "$work/theme-deep.body" || { echo "FAIL: theme deep link"; exit 1; }
[ "$(themereq document "$theme_path/index.html")" = 200 ] && grep -q '<title>e2e theme</title>' "$work/theme-document.body" || { echo "FAIL: theme document"; exit 1; }
case "$(themehdr document Content-Security-Policy)" in *"sandbox allow-scripts;"*) ;; *) echo "FAIL: document sandbox"; exit 1 ;; esac
[ "$(themereq asset "$theme_path/assets/app.js")" = 200 ] && grep -q 'e2e theme' "$work/theme-asset.body" || { echo "FAIL: theme asset"; exit 1; }
[ "$(themereq missing "$theme_path/assets/missing.js")" = 404 ] || { echo "FAIL: missing theme asset"; exit 1; }
[ "$(themereq admin /admin/)" = 200 ] || { echo "FAIL: same-domain panel"; exit 1; }
[ "$(themereq admin-rpc /heron.v1.AdminService/ListNodes -b "$work/jar" -H 'Origin: null' -H 'Content-Type: application/json' --data '{}')" = 403 ] || { echo "FAIL: opaque origin reached admin API"; exit 1; }
[ "$(rpc DeleteTheme '{"id": "e2e-theme"}')" = 200 ] || { echo "FAIL: DeleteTheme"; exit 1; }
[ "$(themereq after-delete /)" = 200 ] && cmp -s "$work/pub-index.html" "$work/theme-after-delete.body" || { echo "FAIL: builtin fallback"; exit 1; }
echo "sandbox theme ok"

register_agent() {
  arch=$1
  cid=$(cat "$work/cid-$arch")
  agent="/heron/heron-agent-linux-$arch"
  # hub 经 host.docker.internal 以明文 http 访问，不是 loopback 字面量，注册时必须显式放行（规则见 agentconfig.CheckHub）。
  docker exec "$cid" "$agent" register \
    --hub "http://host.docker.internal:$port" --key "$key" --config /tmp/agent.json --name "e2e-$arch" --insecure-http
  # 两个探测任务的目标是容器回环（ICMP）与宿主上的 hub（TCP）。回环在默认拒绝集里；host.docker.internal 在
  # OrbStack 上解析到 0.250.250.254，落在默认拒绝的 0.0.0.0/8，其他运行时可能是私网地址。两者都由宿主机本地策略
  # 放行（规则见 prober.Policy），宿主地址取容器里实际解析出的那个，不写死某个运行时的约定。
  host_ip=$(docker exec "$cid" getent hosts host.docker.internal | awk '{print $1; exit}')
  [ -n "$host_ip" ] || { echo "FAIL: host.docker.internal does not resolve in the $arch container"; return 1; }
  case "$host_ip" in *:*) host_prefix="$host_ip/128";; *) host_prefix="$host_ip/32";; esac
  docker exec "$cid" "$agent" configure --config /tmp/agent.json --probe-allow "127.0.0.0/8,$host_prefix"
}
run_agent() {
  arch=$1
  if [ "$#" -gt 1 ]; then
    docker exec "$(cat "$work/cid-$arch")" timeout "$2" "/heron/heron-agent-linux-$arch" run --config /tmp/agent.json || [ "$?" = 124 ]
  else
    docker exec "$(cat "$work/cid-$arch")" "/heron/heron-agent-linux-$arch" run --config /tmp/agent.json
  fi
}
first_agent() {
  register_agent "$1"
  run_agent "$1" 75
}
first_agent amd64 > "$work/agent-amd64.log" 2>&1 &
amd64=$!
first_agent arm64 > "$work/agent-arm64.log" 2>&1 &
arm64=$!

# CPU 百分比需要两次采样差分；在线并不保证第一份上报已有 CPU 读数。
i=0
until [ "$(rpc GetSnapshot '{}')" = 200 ] && jq -e '[.nodes[]? | select(.online == true and .metrics.cpuPct != null)] | length == 2' "$work/GetSnapshot.json" > /dev/null; do
  i=$((i + 1)); [ "$i" -lt 60 ] || { echo "FAIL: nodes did not come online"; cat "$work/GetSnapshot.json"; exit 1; }
  sleep 1
done
# hub 绑定的 agent 版本经 hub-binary 的 ldflags 注入。变量改名或注入断开时链接器静默忽略 -X，
# hub 就没有绑定、节点在线更新全部被拒，编译与单元测试都照不出来，只有从真实二进制回读才看得见。
# 期望值取构建时导出的 AGENT_VERSION，不读仓库文件，也不读下载的已发布版本：命令行覆盖绑定时文件里仍是原值，
# 拿文件去比会把一次合法的覆盖判失败。
: "${AGENT_VERSION:?AGENT_VERSION is required: it is the version injected into the hub build}"
bound=$AGENT_VERSION
jq -e --arg bound "$bound" '.reportIntervalMs == 4000 and all(.nodes[]; .metrics.cpuPct != null) and .boundAgentVersion == $bound' "$work/GetSnapshot.json" > /dev/null || { echo "FAIL: snapshot shape or bound agent version (want $bound)"; cat "$work/GetSnapshot.json"; exit 1; }
[ "$(rpc ListNodes '{}')" = 200 ] || { echo "FAIL: ListNodes"; exit 1; }
jq -e '[.nodes[] | select(.facts.arch == "amd64" or .facts.arch == "arm64")] | length == 2' "$work/ListNodes.json" > /dev/null || { echo "FAIL: facts not reported"; cat "$work/ListNodes.json"; exit 1; }
jq -e --arg os "$EXPECT_OS" '(.nodes | length) == 2 and all(.nodes[]; (.facts.os // "") | contains($os))' "$work/ListNodes.json" > /dev/null || { echo "FAIL: nodes did not report an OS containing \"$EXPECT_OS\""; cat "$work/ListNodes.json"; exit 1; }
if [ -n "${E2E_AGENT_VERSION:-}" ]; then
  jq -e --arg version "$E2E_AGENT_VERSION" 'all(.nodes[]; .facts.agentVersion == $version)' "$work/ListNodes.json" > /dev/null || { echo "FAIL: reported agent version differs from the pinned published version"; cat "$work/ListNodes.json"; exit 1; }
fi
# 结构规则：没有显式网卡规则时按结构分类，容器里的 veth（eth0）计入。由 agent 采集的默认分类保证。
jq -e '(.nodes | length) == 2 and all(.nodes[]; .facts.diagnostics.netStructureRules == true and (.facts.diagnostics.netInterfaces | index("eth0")) != null)' "$work/ListNodes.json" > /dev/null || { echo "FAIL: structure rules diagnostics"; cat "$work/ListNodes.json"; exit 1; }
node1=$(jq -r '.nodes[] | select(.facts.arch == "amd64") | .id' "$work/ListNodes.json")
node2=$(jq -r '.nodes[] | select(.facts.arch == "arm64") | .id' "$work/ListNodes.json")
icmp_body=$(jq -nc --arg a "$node1" --arg b "$node2" '{task: {kind: "PROBE_KIND_ICMP", target: "127.0.0.1", intervalS: 5, timeoutMs: 1000}, nodeIds: [$a, $b]}')
[ "$(rpc SaveProbeTask "$icmp_body")" = 200 ] || { echo "FAIL: SaveProbeTask icmp"; cat "$work/SaveProbeTask.json"; exit 1; }
icmp_task=$(jq -r '.task.task.id' "$work/SaveProbeTask.json")
tcp_body=$(jq -nc --arg a "$node1" --arg b "$node2" --arg target "host.docker.internal:$port" '{task: {kind: "PROBE_KIND_TCP", target: $target, intervalS: 5, timeoutMs: 2000}, nodeIds: [$a, $b]}')
[ "$(rpc SaveProbeTask "$tcp_body")" = 200 ] || { echo "FAIL: SaveProbeTask tcp"; cat "$work/SaveProbeTask.json"; exit 1; }
tcp_task=$(jq -r '.task.task.id' "$work/SaveProbeTask.json")
[ "$(rpc SaveProbeTask '{"task": {"kind": "PROBE_KIND_ICMP", "target": "127.0.0.1", "intervalS": 1, "timeoutMs": 1000}}')" = 400 ] || { echo "FAIL: interval below the minimum must be rejected"; exit 1; }
grep -q 'interval_s must be between 5 and 3600' "$work/SaveProbeTask.json" || { echo "FAIL: error must name the field"; cat "$work/SaveProbeTask.json"; exit 1; }
[ "$(rpc ListProbeTasks '{}')" = 200 ] || { echo "FAIL: ListProbeTasks"; exit 1; }
jq -e '(.version | tonumber) > 0 and (.tasks | length) == 2 and all(.tasks[]; (.nodeIds | length) == 2)' "$work/ListProbeTasks.json" > /dev/null || { echo "FAIL: task list shape"; cat "$work/ListProbeTasks.json"; exit 1; }

agent_status=0
wait "$amd64" || agent_status=1
wait "$arm64" || agent_status=1

task_version=$(jq -r '.version' "$work/ListProbeTasks.json")
echo "probe task version before restart: $task_version"

# 跨过分钟边界后查最近一小时；首次冷采样所在桶可能没有 CPU 读数，后续样本不能因此被忽略。
now=$(date +%s)
query_body=$(jq -nc --arg nodeId "$node1" --argjson from "$((now - 3600))" --argjson to "$((now + 60))" \
  '{nodeId: $nodeId, from: $from, to: $to, maxPoints: 100}')
[ "$(rpc QueryMetrics "$query_body")" = 200 ] || { echo "FAIL: QueryMetrics"; cat "$work/QueryMetrics.json"; exit 1; }
jq -e '.level == "1m" and (.ts | length) >= 1 and any(.series[] | select(.name == "cpu") | .samples[]; .n > 0)' "$work/QueryMetrics.json" > /dev/null || { echo "FAIL: QueryMetrics shape"; cat "$work/QueryMetrics.json"; exit 1; }
# TCP 速率：agent 每次采样都读 /proc/net/snmp，第二次采样起四项都有读数。
jq -e '. as $root | all(["tcp_active_opens_ps", "tcp_passive_opens_ps", "tcp_out_segs_ps", "tcp_retrans_segs_ps"][]; . as $n | any($root.series[] | select(.name == $n) | .samples[]; (.n // 0) > 0)) and any(.series[] | select(.name == "tcp_out_segs_ps"); .unit == "count/s")' "$work/QueryMetrics.json" > /dev/null || { echo "FAIL: tcp rate series missing"; cat "$work/QueryMetrics.json"; exit 1; }
probe_body=$(jq -nc --arg nodeId "$node1" --argjson from "$((now - 3600))" --argjson to "$((now + 60))" '{nodeId: $nodeId, from: $from, to: $to, maxPoints: 100}')
# 两个任务都有成功的探测：容器里的 ICMP 探测回环，TCP 连接宿主上的 hub。
# hub 在分钟边界后 0.5s 刷出分钟桶；4s 上报间隔与最多 5s 首次偏移下，首条结果最晚在任务创建后约 13s 到达。
# agent 退出后立即查库，距首条结果最坏不足 60.5s，一次性查询的最坏余量为负，须等待常规刷出。
# 截止按轮询间累计的 sleep 秒数计（见 wait_alert 的说明）。分钟行按墙钟分钟边界刷出，但宿主休眠时墙钟照走，
# hub 的刷出定时器无论是否把休眠时长计入，醒来后还需的醒着时间都不超过不休眠时；醒来后的第一次刷出由
# live.Flush 取走全部已闭合的桶（起始早于当前墙钟分钟的，不只上一分钟），墙钟跳过多个分钟也一次交出。
# 所以这里也不用墙钟截止。
probe_started=$(date +%s)
probe_budget_s=75
probe_slept_s=0
while :; do
  [ "$(rpc QueryProbes "$probe_body")" = 200 ] || { echo "FAIL: QueryProbes"; cat "$work/QueryProbes.json"; exit 1; }
  if jq -e --arg icmp "$icmp_task" --arg tcp "$tcp_task" '.level == "1m" and ([.series[].taskId] | sort) == ([$icmp, $tcp] | sort) and all(.series[]; any(.samples[]; .sent > 0 and .rttMeanUs != null and (.errors // 0) == 0))' "$work/QueryProbes.json" > /dev/null; then
    echo "probe results ready after $(($(date +%s) - probe_started))s (${probe_slept_s}s of polling)"
    break
  fi
  if [ "$probe_slept_s" -ge "$probe_budget_s" ]; then
    echo "FAIL: probe results not ready after ${probe_slept_s}s of polling"; cat "$work/QueryProbes.json"; exit 1
  fi
  sleep 2
  probe_slept_s=$((probe_slept_s + 2))
done
jq -e 'all(.nodes[]; .facts.icmpAvailable == true)' "$work/ListNodes.json" > /dev/null || { echo "FAIL: icmp_available not reported"; cat "$work/ListNodes.json"; exit 1; }
# 流量：两个 agent 每 4 秒上报一次，上报本身就产生字节；首次上报只取基线，之后的差分进总量。
[ "$(rpc GetTraffic '{}')" = 200 ] || { echo "FAIL: GetTraffic"; cat "$work/GetTraffic.json"; exit 1; }
jq -e '.timezone == "UTC" and (.nodes | length) == 2 and all(.nodes[]; (.traffic.totalRx | tonumber) > 0 and (.traffic.totalTx | tonumber) > 0 and .traffic.resetDay == 1 and (.traffic.nextResetAt | tonumber) > (.traffic.periodStart | tonumber))' "$work/GetTraffic.json" > /dev/null || { echo "FAIL: traffic shape"; cat "$work/GetTraffic.json"; exit 1; }
tx_before=$(jq -r --arg id "$node1" '.nodes[] | select(.nodeId == $id) | .traffic.totalTx' "$work/GetTraffic.json")
adjust_body=$(jq -nc --arg nodeId "$node1" '{nodeId: $nodeId, periodRx: "1073741824", periodTx: "0"}')
[ "$(rpc AdjustTraffic "$adjust_body")" = 200 ] || { echo "FAIL: AdjustTraffic"; cat "$work/AdjustTraffic.json"; exit 1; }
# 首个周期里总量等于周期量，校正后两者同为 1 GiB；上行改成 0 后总量也随差值归零。
jq -e '.traffic.periodRx == "1073741824" and .traffic.totalRx == "1073741824" and (.traffic.periodTx // "0") == "0" and (.traffic.totalTx // "0") == "0"' "$work/AdjustTraffic.json" > /dev/null || { echo "FAIL: AdjustTraffic result"; cat "$work/AdjustTraffic.json"; exit 1; }
[ "$(rpc AdjustTraffic '{"nodeId": "999999", "periodRx": "1"}')" = 404 ] || { echo "FAIL: AdjustTraffic on an unknown node must be 404"; exit 1; }
# node1 标为公开：技能文件"公开数据"的例子只列公开节点，重启后那一轮 run_skill_examples 要求它输出非空。
update_body=$(jq -nc --arg id "$node1" --arg name "e2e-amd64" '{id: $id, name: $name, public: true, note: "", trafficResetDay: 15, offlineGraceS: 0}')
[ "$(rpc UpdateNode "$update_body")" = 200 ] || { echo "FAIL: UpdateNode reset day"; cat "$work/UpdateNode.json"; exit 1; }
jq -e '.node.trafficResetDay == 15 and .node.public == true' "$work/UpdateNode.json" > /dev/null || { echo "FAIL: reset day or public flag not echoed"; cat "$work/UpdateNode.json"; exit 1; }

# 公开服务只给 node1（公开）；node2 保持私有。快照响应缓存 1 秒，公开之后最迟 1 秒出现在公开快照里。
i=0
until [ "$(pubget snapshot GetSnapshot '{}')" = 200 ] && jq -e --arg id "$node1" '[(.nodes // [])[].id] == [$id]' "$work/pub-snapshot.json" > /dev/null; do
  i=$((i + 1)); [ "$i" -lt 10 ] || { echo "FAIL: public snapshot does not list exactly node1"; cat "$work/pub-snapshot.json"; exit 1; }; sleep 0.5
done
# 公开的主机信息只有这六项：主机名、内核、agent 版本、ICMP 可用性不出现在线上。双栈出口一定在：agent 的 facts
# 总带出口探测的状态，公开投影里每个地址族只有 state，出口地址与探测时间不出现。指标一定在：node1 早已上报，live
# 里的最近一次指标只在删节点时清掉；公开指标里没有 bootId。
jq -e --arg os "$EXPECT_OS" '.reportIntervalMs == 4000 and (.nodes[0].facts | (.os | contains($os)) and .arch == "amd64" and (keys - ["os", "arch", "virtualization", "cpuModel", "cpuCores", "network"]) == [] and (.network | type == "object" and ([.[] | keys[]] - ["state"]) == [])) and (.nodes[0].metrics | type == "object" and .memTotal != null and (has("bootId") | not))' "$work/pub-snapshot.json" > /dev/null || { echo "FAIL: public snapshot shape"; cat "$work/pub-snapshot.json"; exit 1; }
[ "$(hdr snapshot Cache-Control)" = "max-age=1" ] || { echo "FAIL: GetSnapshot Cache-Control"; cat "$work/pub-snapshot.headers"; exit 1; }
# 缓存头只给 GET：POST 的响应不进浏览器缓存，不带这个头。
[ "$(curl -sS -o /dev/null -D "$work/pub-post.headers" -w '%{http_code}' -H 'Content-Type: application/json' --data '{}' "$base/heron.v1.PublicService/GetSnapshot")" = 200 ] || { echo "FAIL: POST GetSnapshot"; exit 1; }
[ -z "$(hdr post Cache-Control)" ] || { echo "FAIL: POST response carries Cache-Control"; cat "$work/pub-post.headers"; exit 1; }
[ "$(pubget metrics QueryMetrics "$query_body")" = 200 ] || { echo "FAIL: public QueryMetrics"; cat "$work/pub-metrics.json"; exit 1; }
jq -e '.level == "1m" and any(.series[] | select(.name == "cpu") | .samples[]; .n > 0)' "$work/pub-metrics.json" > /dev/null || { echo "FAIL: public QueryMetrics shape"; cat "$work/pub-metrics.json"; exit 1; }
# 公开端与各管理端同一投影（规则见 proto/heron/v1/public.proto 的 PublicService 注释）：TCP 四条序列与单位一致。protojson 省略值为 0 的 n，写 (.n // 0)。
jq -e '. as $root | all(["tcp_active_opens_ps", "tcp_passive_opens_ps", "tcp_out_segs_ps", "tcp_retrans_segs_ps"][]; . as $n | any($root.series[] | select(.name == $n) | .samples[]; (.n // 0) > 0)) and any(.series[] | select(.name == "tcp_out_segs_ps"); .unit == "count/s")' "$work/pub-metrics.json" > /dev/null || { echo "FAIL: public tcp rate series missing"; cat "$work/pub-metrics.json"; exit 1; }
[ "$(hdr metrics Cache-Control)" = "max-age=60" ] || { echo "FAIL: QueryMetrics Cache-Control"; cat "$work/pub-metrics.headers"; exit 1; }
# 公开节点即公开它的探测目标：序列带任务当前的种类与目标。
[ "$(pubget probes QueryProbes "$probe_body")" = 200 ] || { echo "FAIL: public QueryProbes"; cat "$work/pub-probes.json"; exit 1; }
jq -e --arg icmp "$icmp_task" --arg tcp "$tcp_task" --arg target "host.docker.internal:$port" '[.series[] | {taskId, kind, target}] | sort_by(.taskId) == ([{taskId: $icmp, kind: "PROBE_KIND_ICMP", target: "127.0.0.1"}, {taskId: $tcp, kind: "PROBE_KIND_TCP", target: $target}] | sort_by(.taskId))' "$work/pub-probes.json" > /dev/null || { echo "FAIL: public probe series lack kind and target"; cat "$work/pub-probes.json"; exit 1; }
# 私有节点与不存在的节点逐字节同一个 NotFound，且不进缓存：节点改为公开后浏览器不会继续用它。
for method in QueryMetrics QueryProbes; do
  private_body=$(jq -nc --arg nodeId "$node2" --argjson from "$((now - 3600))" --argjson to "$((now + 60))" '{nodeId: $nodeId, from: $from, to: $to, maxPoints: 100}')
  missing_body=$(jq -nc --argjson from "$((now - 3600))" --argjson to "$((now + 60))" '{nodeId: "999999", from: $from, to: $to, maxPoints: 100}')
  [ "$(pubget "private-$method" "$method" "$private_body")" = 404 ] || { echo "FAIL: $method on a private node was not 404"; cat "$work/pub-private-$method.json"; exit 1; }
  [ "$(pubget "missing-$method" "$method" "$missing_body")" = 404 ] || { echo "FAIL: $method on a missing node was not 404"; cat "$work/pub-missing-$method.json"; exit 1; }
  cmp -s "$work/pub-private-$method.json" "$work/pub-missing-$method.json" || { echo "FAIL: $method tells private and missing nodes apart"; cat "$work/pub-private-$method.json" "$work/pub-missing-$method.json"; exit 1; }
  [ "$(hdr "private-$method" Cache-Control)" = no-store ] || { echo "FAIL: $method NotFound is cacheable"; cat "$work/pub-private-$method.headers"; exit 1; }
done

# 总闸整体关闭公开接口，重新打开仍使用原外观与逐节点公开范围。
closed_settings=$(printf '%s' "$settings_body" | jq '.settings.publicEnabled = false')
[ "$(rpc UpdateSettings "$closed_settings")" = 200 ] || { echo "FAIL: close public page"; exit 1; }
[ "$(pubget disabled-site GetSite '{}')" = 404 ] || { echo "FAIL: disabled GetSite must be 404"; cat "$work/pub-disabled-site.json"; exit 1; }
# 旧客户端仅改标题，保留其它外观字段但不认识总闸；缺席不能改变任一方向的状态。
title_settings=$(printf '%s' "$settings_body" | jq 'del(.settings.publicEnabled) | .settings.title = "e2e 标题更新"')
[ "$(rpc UpdateSettings "$title_settings")" = 200 ] || { echo "FAIL: title update while closed"; exit 1; }
jq -e '.settings.publicEnabled == false and .settings.title == "e2e 标题更新"' "$work/UpdateSettings.json" > /dev/null || { echo "FAIL: omitted gate must echo closed and new title"; exit 1; }
[ "$(pubget omitted-closed-site GetSite '{}')" = 404 ] || { echo "FAIL: omitted gate reopened public page"; exit 1; }
[ "$(rpc UpdateSettings "$settings_body")" = 200 ] || { echo "FAIL: reopen public page"; exit 1; }
[ "$(pubget reopened-site GetSite '{}')" = 200 ] || { echo "FAIL: reopened GetSite"; cat "$work/pub-reopened-site.json"; exit 1; }
[ "$(rpc UpdateSettings "$title_settings")" = 200 ] || { echo "FAIL: title update while open"; exit 1; }
jq -e '.settings.publicEnabled == true and .settings.title == "e2e 标题更新"' "$work/UpdateSettings.json" > /dev/null || { echo "FAIL: omitted gate must echo open and new title"; exit 1; }
[ "$(pubget omitted-open-site GetSite '{}')" = 200 ] || { echo "FAIL: omitted gate closed public page"; exit 1; }
[ "$(rpc UpdateSettings "$settings_body")" = 200 ] || { echo "FAIL: restore appearance"; exit 1; }

# 规则只覆盖 arm64；amd64 保持退出，node1 的流量精确复核不受后续上报影响。
run_agent arm64 >> "$work/agent-arm64.log" 2>&1 &
arm64=$!
i=0
until [ "$(rpc GetSnapshot '{}')" = 200 ] && jq -e --arg id "$node2" '[.nodes[] | select(.id == $id and .online == true)] | length == 1' "$work/GetSnapshot.json" > /dev/null; do
  i=$((i + 1)); [ "$i" -lt 40 ] || { echo "FAIL: arm64 did not resume"; cat "$work/GetSnapshot.json"; exit 1; }; sleep 1
done

# 接收器与 hub 同在宿主回环；每次请求体落一行，退出时一并回收。
python3 - "$work/hooks.txt" "$hook_port" <<'PY' > "$work/hookrecv.log" 2>&1 &
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
out = sys.argv[1]
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        with open(out, "ab") as f:
            f.write(body + b"\n")
        self.send_response(200)
        self.end_headers()
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
    def log_message(self, *a):
        pass
HTTPServer(("127.0.0.1", int(sys.argv[2])), H).serve_forever()
PY
hookrecv=$!
i=0
until curl -sf -o /dev/null "http://127.0.0.1:$hook_port/"; do
  i=$((i + 1)); [ "$i" -lt 30 ] || { echo "FAIL: webhook receiver did not listen"; cat "$work/hookrecv.log"; exit 1; }; sleep 0.2
done
[ "$(rpc SaveNotifyChannel "$(jq -nc --arg url "http://127.0.0.1:$hook_port/hook" '{channel: {name: "e2e hook", kind: "CHANNEL_KIND_WEBHOOK", webhook: {url: $url}}}')")" = 200 ] || { echo "FAIL: SaveNotifyChannel"; cat "$work/SaveNotifyChannel.json"; exit 1; }
channel=$(jq -r '.channel.id' "$work/SaveNotifyChannel.json")
[ "$(rpc TestNotifyChannel "$(jq -nc --arg id "$channel" '{id: $id}')")" = 200 ] || { echo "FAIL: TestNotifyChannel"; cat "$work/TestNotifyChannel.json"; exit 1; }
[ "$(rpc SaveAlertRule "$(jq -nc --arg c "$channel" --arg n "$node2" '{rule: {name: "e2e offline", kind: "ALERT_KIND_OFFLINE", enabled: true, allNodes: false, nodeIds: [$n], channelIds: [$c]}}')")" = 200 ] || { echo "FAIL: SaveAlertRule"; cat "$work/SaveAlertRule.json"; exit 1; }

# 截止按轮询间累计的 sleep 秒数计，不按墙钟。宿主休眠时 hub、docker VM 与本脚本一起停摆，一次休眠
# 至多落在一次 sleep 里，累计值至多多算这一次，其余每一秒都是被等的系统醒着运行的时间。墙钟截止会把
# 整段休眠算进预算，醒来后第一次检查就失败，而那段时间里被等的系统根本没有运行。
# wait_alert 节点 变化 预算秒数：等该节点恰有一条已送达的该变化事件。
wait_alert() {
  alert_node=$1
  transition=$2
  alert_budget_s=$3
  alert_started=$(date +%s)
  alert_slept_s=0
  while :; do
    [ "$(rpc ListAlertEvents '{}')" = 200 ] || { echo "FAIL: ListAlertEvents"; cat "$work/ListAlertEvents.json"; exit 1; }
    if jq -e --arg n "$alert_node" --arg tr "$transition" '[.events[]? | select(.transition == $tr and .nodeId == $n and any(.deliveries[]; (.ok // false) == true))] | length == 1' "$work/ListAlertEvents.json" > /dev/null; then
      echo "alert $transition delivered after $(($(date +%s) - alert_started))s (${alert_slept_s}s of polling, budget ${alert_budget_s}s)"
      return
    fi
    [ "$alert_slept_s" -lt "$alert_budget_s" ] || { echo "FAIL: $transition alert not delivered after ${alert_slept_s}s of polling"; cat "$work/ListAlertEvents.json"; exit 1; }
    sleep 1
    alert_slept_s=$((alert_slept_s + 1))
  done
}
# agent 的参数取自它首次运行的启动行（cmd/agent/main.go 的 requestTimeout、initialInterval）。
request_timeout_s=$(startup_seconds "$work/agent-arm64.log" "agent starting" request_timeout)
initial_interval_s=$(startup_seconds "$work/agent-arm64.log" "agent starting" initial_interval)
[ -n "$request_timeout_s" ] && [ -n "$initial_interval_s" ] || { echo "FAIL: agent startup line does not state request_timeout and initial_interval in whole seconds"; cat "$work/agent-arm64.log"; exit 1; }
# 两个上限都是"推出的最坏时长 + 投递 + 余量"。SweepOffline 只由 RunOfflineSweep 按 offline_sweep
# 定时调用，触发与恢复都只在巡检时判定。
# firing：最后一次上报早于 docker kill，kill 后至多 ttl 即满足离线（node2 的宽限未设置，取 TTL，
#   NextOffline 也以 TTL 为下限），之后至多再等一个 offline_sweep。agent 侧没有量进入这条链：
#   kill 前 agent 若正在退避，最后一次上报只会更早，离线只会更早成立。
# recovered：agent 一启动就上报（Runner.Run 在首次 Sleep 之前发出 Report）。最坏情形取首次上报挂满
#   request_timeout（main.go 的 http.Client 超时），按 client.Backoff(1, initial_interval) 退避——
#   attempt 为 1 时等待落在 [interval/2, interval)——第二次上报成功。此后按 hub 下发的 ttl/3 间隔
#   上报，任何一轮巡检看到的未上报时长都小于 TTL，首个成功上报之后的第一轮巡检就判恢复，至多一个
#   offline_sweep，与 ttl 无关。连续失败没有上限可推：hub 一直不应答时节点本来就没有恢复上报。
# 投递 delivery_retry_wait：渠道失败可重试且存储正常时，一个批次依次等过 internal/hub/alert/queue.go
#   的 backoff 各项，总和即 DeliveryRetryWait。429 的 Retry-After、渠道节奏排队与 not_before 的整秒取整（每次
#   重试多等不到 1 s）会等得更久，都不在预算内：接收器是 Webhook 渠道（保存时省略节奏上限，取默认 0 即不限），
#   总回 200，既不回 429 也不让批次重试。存储失败走 worker 级退避
#   （1s 起翻倍、上限 1 分钟），没有总量上界，不在预算内；e2e 的库在本机磁盘上，视为正常。渠道客户端总时限（serve
#   交给 outbound.NewClient 的 alert.NotifyTimeout）只在接收器挂住时才会用满；接收器在本机回环、已由 TestNotifyChannel 验证能应答，
#   上限不为此留量。
# 余量一个 offline_sweep：容纳 agent 进程启动与巡检本身的耗时推迟下一轮。docker kill/start 在计数
#   开始之前完成，不占预算。
wait_alert_firing_s=$((ttl_s + sweep_s + retry_wait_s + sweep_s))
wait_alert_recovered_s=$((request_timeout_s + initial_interval_s + sweep_s + retry_wait_s + sweep_s))
docker kill "$(cat "$work/cid-arm64")" > /dev/null
wait "$arm64" || true
wait_alert "$node2" firing "$wait_alert_firing_s"
grep -q '"transition":"firing"' "$work/hooks.txt" || { echo "FAIL: webhook body"; cat "$work/hooks.txt"; exit 1; }
docker start "$(cat "$work/cid-arm64")" > /dev/null
run_agent arm64 >> "$work/agent-arm64.log" 2>&1 &
arm64=$!
wait_alert "$node2" recovered "$wait_alert_recovered_s"

# 计费与到期（规则见 store.Billing）。UpdateNode 整体替换，billing 与 node1 其余可编辑字段（公开、重置日 15）一起给全。
# hub 以 --timezone UTC 运行，jq 的 now 与 strftime 按 UTC 取日历日。
# node_body 到期日 自动续期：node1 的整份可编辑字段，12.50 USD 按月。billing 里的 daysLeft 由 hub 计算，请求里的 999
# 不起作用：续期后的 0–31、公开快照里与 now 相符、重启后的 4–5、重启后续期的 59–60 四处断言都排除了 999。
node_body() {
  jq -nc --arg id "$node1" --arg exp "$1" --argjson renew "$2" \
    '{id: $id, name: "e2e-amd64", public: true, note: "", trafficResetDay: 15, offlineGraceS: 0,
      billing: {price: "12.50", currency: "USD", billingCycle: "BILLING_CYCLE_MONTHLY", expiresOn: $exp, autoRenew: $renew, daysLeft: 999}}'
}
[ "$(rpc UpdateNode "$(jq -nc --arg id "$node1" '{id: $id, name: "e2e-amd64", public: true, trafficResetDay: 15, offlineGraceS: 0, billing: {price: "12.345", currency: "USD"}}')")" = 400 ] || { echo "FAIL: a three-decimal price was accepted"; cat "$work/UpdateNode.json"; exit 1; }
grep -q 'billing.price: must match' "$work/UpdateNode.json" || { echo "FAIL: price error must name the field"; cat "$work/UpdateNode.json"; exit 1; }
# 自动续期：上月 1 日到期、按月续期。保存触发的扫描当即推后到不早于今天的某月 1 日，响应里已是推后的日期；
# 1 日在任何月份都不钳，跑在月底零点前后也只是推后到这个月或下个月的 1 日。
last_month=$(jq -rn 'now | strftime("%Y %m") | split(" ") | map(tonumber) | if .[1] == 1 then [.[0] - 1, 12] else [.[0], .[1] - 1] end | "\(.[0])-\(if .[1] < 10 then "0" else "" end)\(.[1])-01"')
[ "$(rpc UpdateNode "$(node_body "$last_month" true)")" = 200 ] || { echo "FAIL: UpdateNode with auto renew"; cat "$work/UpdateNode.json"; exit 1; }
jq -e --arg from "$last_month" '.node.billing | .autoRenew == true and .expiresOn != $from and (.expiresOn | endswith("-01")) and .daysLeft >= 0 and .daysLeft <= 31' "$work/UpdateNode.json" > /dev/null || { echo "FAIL: auto renew did not push the expiry date past today"; cat "$work/UpdateNode.json"; exit 1; }
renewed_to=$(jq -r '.node.billing.expiresOn' "$work/UpdateNode.json")
grep -q "msg=\"node expiry renewed\" node_id=$node1 node=e2e-amd64 cycle=monthly from=$last_month to=$renewed_to\$" "$work/hub.log" || { echo "FAIL: renewal not logged"; grep 'expiry' "$work/hub.log"; exit 1; }
# 5 天后到期、关掉自动续期；公开快照带价格、币种、周期、到期日与 days_left，不带自动续期。快照缓存 1 秒。
soon=$(jq -rn 'now + 5 * 86400 | strftime("%Y-%m-%d")')
[ "$(rpc UpdateNode "$(node_body "$soon" false)")" = 200 ] || { echo "FAIL: UpdateNode billing"; cat "$work/UpdateNode.json"; exit 1; }
i=0
until [ "$(pubget billing GetSnapshot '{}')" = 200 ] && jq -e --arg exp "$soon" '.nodes[0].billing.expiresOn == $exp' "$work/pub-billing.json" > /dev/null; do
  i=$((i + 1)); [ "$i" -lt 10 ] || { echo "FAIL: public snapshot lacks the expiry date"; cat "$work/pub-billing.json"; exit 1; }; sleep 0.5
done
jq -e '.nodes[0].billing | .price == "12.50" and .currency == "USD" and .billingCycle == "BILLING_CYCLE_MONTHLY" and (has("autoRenew") | not)' "$work/pub-billing.json" > /dev/null || { echo "FAIL: public billing fields"; cat "$work/pub-billing.json"; exit 1; }
# days_left 与同一响应的 now 出自同一时刻：到期日的 UTC 零点减 now 所在日的 UTC 零点，按天计。
jq -e '(.now | tonumber) as $now | .nodes[0].billing | .daysLeft == (((.expiresOn | strptime("%Y-%m-%d") | mktime) - ($now - $now % 86400)) / 86400)' "$work/pub-billing.json" > /dev/null || { echo "FAIL: public days_left does not match now"; cat "$work/pub-billing.json"; exit 1; }
[ "$(rpc SaveAlertRule '{"rule": {"name": "bad expiry", "kind": "ALERT_KIND_EXPIRY", "enabled": true, "allNodes": true, "daysBefore": 0}}')" = 400 ] || { echo "FAIL: days_before 0 was accepted"; cat "$work/SaveAlertRule.json"; exit 1; }
grep -q 'rule.days_before must be between 1 and 365' "$work/SaveAlertRule.json" || { echo "FAIL: days_before error must name the field"; cat "$work/SaveAlertRule.json"; exit 1; }
# 到期规则在 SaveAlertRule 与 UpdateNode 的请求里同步评估：响应返回时事件已落库并交给投递队列，等待的只是投递。
# 预算 delivery_retry_wait，余量一个 offline_sweep（与上面两处同一余量口径）。
wait_expiry_s=$((retry_wait_s + sweep_s))
# expiry_rule_body 规则 ID：node1 的到期规则，提前 10 天；新建时 ID 为 0。重启后按同一份载荷再保存一次。
expiry_rule_body() {
  jq -nc --arg id "$1" --arg c "$channel" --arg n "$node1" '{rule: {id: $id, name: "e2e expiry", kind: "ALERT_KIND_EXPIRY", enabled: true, allNodes: false, nodeIds: [$n], channelIds: [$c], daysBefore: 10}}'
}
[ "$(rpc SaveAlertRule "$(expiry_rule_body 0)")" = 200 ] || { echo "FAIL: SaveAlertRule expiry"; cat "$work/SaveAlertRule.json"; exit 1; }
jq -e '.rule.kind == "ALERT_KIND_EXPIRY" and .rule.daysBefore == 10' "$work/SaveAlertRule.json" > /dev/null || { echo "FAIL: expiry rule echo"; cat "$work/SaveAlertRule.json"; exit 1; }
expiry_rule=$(jq -r '.rule.id' "$work/SaveAlertRule.json")
wait_alert "$node1" firing "$wait_expiry_s"
jq -e --arg n "$node1" --arg exp "$soon" '[.events[] | select(.nodeId == $n and .transition == "firing")][0].summary | startswith("节点 e2e-amd64 将于 " + $exp + " 到期（剩 ") and endswith("天，规则 e2e expiry）")' "$work/ListAlertEvents.json" > /dev/null || { echo "FAIL: expiry firing summary"; cat "$work/ListAlertEvents.json"; exit 1; }
grep -q '"kind":"expiry","transition":"firing"' "$work/hooks.txt" || { echo "FAIL: expiry webhook body"; cat "$work/hooks.txt"; exit 1; }
# node1 停在 firing 进入重启；续期与恢复放到重启之后。
# node1 的 agent 已退出；先推进到新重置日对应的周期，再保存停机前状态。
[ "$(rpc GetTraffic '{}')" = 200 ] || { echo "FAIL: GetTraffic before restart"; exit 1; }
traffic_before=$(jq -c --arg id "$node1" '.nodes[] | select(.nodeId == $id) | .traffic' "$work/GetTraffic.json")

[ "$(rpc CreateApiToken '{"name":"e2e"}')" = 200 ] || { echo "FAIL: CreateApiToken"; cat "$work/CreateApiToken.json"; exit 1; }
api_token=$(jq -r '.token' "$work/CreateApiToken.json")
api_token_id=$(jq -r '.apiToken.id' "$work/CreateApiToken.json")
case "$api_token" in heron_at_*) ;; *) echo "FAIL: API token lacks the heron_at_ prefix"; exit 1 ;; esac

[ "$(rpc Logout '{}')" = 200 ] || { echo "FAIL: logout"; exit 1; }
[ "$(rpc GetSnapshot '{}')" = 401 ] || { echo "FAIL: session survived logout"; exit 1; }
# token 与会话是两条独立口径：登出不影响 token。
[ "$(bearer GetSnapshot '{}')" = 200 ] || { echo "FAIL: API token stopped working after logout"; cat "$work/bearer-GetSnapshot.json"; exit 1; }
# 节点更新也会推进任务版本；重启的不变式以全部写请求结束后的完整快照为准，包含展示顺序与覆盖范围。
[ "$(bearer ListProbeTasks '{}')" = 200 ] || { echo "FAIL: ListProbeTasks before restart"; exit 1; }
jq '{version, tasks}' "$work/bearer-ListProbeTasks.json" > "$work/tasks-before-restart.json"
task_version=$(jq -r .version "$work/tasks-before-restart.json")
kill "$hub"; wait "$hub"
hub=""

# 重启时换上替换目录：它接管 / 下除 /admin 与 RPC 之外的路径；指向目录外的符号链接拿不到目标内容。
mkdir -p "$work/site/assets"
printf '%s\n' '<!doctype html><title>e2e custom public page</title>' > "$work/site/index.html"
printf '%s\n' 'body { color: red }' > "$work/site/assets/app.css"
printf '%s\n' 'outside secret' > "$work/outside.txt"
ln -s ../outside.txt "$work/site/leak.txt"

# 重启：流量状态、重置日与被 Drain 出的分钟行都必须还在。
hub_log_from=$(wc -l < "$work/hub.log")
HERON_OFFLINE_AFTER=12s bin/heron-hub serve --db "$db" --listen "$listen_host:$port" --timezone UTC --public-dir "$work/site" >> "$work/hub.log" 2>&1 &
hub=$!
wait_hub
# 在登录、续期等写请求之前回读；比较整个有序任务清单，不只比较任务 ID 的集合。
[ "$(bearer ListProbeTasks '{}')" = 200 ] || { echo "FAIL: ListProbeTasks after restart"; exit 1; }
jq '{version, tasks}' "$work/bearer-ListProbeTasks.json" > "$work/tasks-after-restart.json"
cmp -s "$work/tasks-before-restart.json" "$work/tasks-after-restart.json" || { echo "FAIL: task version, order or configuration changed across restart"; cat "$work/tasks-before-restart.json" "$work/tasks-after-restart.json"; exit 1; }
echo "probe task version after restart: $task_version"
[ "$(curl -sS -o "$work/pub-dir-index.html" -D "$work/pub-dir-index.headers" -w '%{http_code}' "$base/")" = 200 ] || { echo "FAIL: --public-dir index not served"; exit 1; }
grep -q 'e2e custom public page' "$work/pub-dir-index.html" || { echo "FAIL: / is not the --public-dir index"; cat "$work/pub-dir-index.html"; exit 1; }
[ "$(hdr dir-index Content-Security-Policy)" = "frame-ancestors 'none'" ] || { echo "FAIL: --public-dir CSP"; cat "$work/pub-dir-index.headers"; exit 1; }
[ "$(hdr dir-index X-Content-Type-Options)" = nosniff ] || { echo "FAIL: --public-dir nosniff"; cat "$work/pub-dir-index.headers"; exit 1; }
[ "$(hdr dir-index Cache-Control)" = no-cache ] || { echo "FAIL: --public-dir Cache-Control"; cat "$work/pub-dir-index.headers"; exit 1; }
[ "$(curl -sS -o "$work/pub-dir-css" -w '%{http_code}' "$base/assets/app.css")" = 200 ] && grep -q 'color: red' "$work/pub-dir-css" || { echo "FAIL: --public-dir asset"; exit 1; }
[ "$(curl -sS -o /dev/null -w '%{http_code}' "$base/assets/missing.js")" = 404 ] || { echo "FAIL: a missing asset under --public-dir must be 404"; exit 1; }
# 两条路都要回落 index.html、读不到目录外的内容。/leak.txt 是指向目录外的符号链接，由 os.Root 拒绝；
# /%2e%2e/outside.txt 到得了替换目录的处理器，由三层各自挡住：relPath 的 path.Clean 先把它清成 /outside.txt；
# 不清理时 .. 段也被当作点段（hidden）而不打开；再往下 os.Root 拒绝越界。这一轮钉的是用户可见的结果，三层全失效才红。
# 字面的 /../ 不在这里测：它在分派之前就被 ServeMux 以 307 重定向清理掉，到不了替换目录。
for path in /leak.txt /%2e%2e/outside.txt; do
  curl -sS --path-as-is -o "$work/pub-dir-escape" "$base$path"
  if grep -q 'outside secret' "$work/pub-dir-escape"; then echo "FAIL: $path read a file outside --public-dir"; exit 1; fi
  grep -q 'e2e custom public page' "$work/pub-dir-escape" || { echo "FAIL: $path did not fall back to index.html"; cat "$work/pub-dir-escape"; exit 1; }
done
[ "$(curl -sS -o "$work/admin-after-dir.html" -w '%{http_code}' "$base/admin/")" = 200 ] && grep -q 'src="/admin/assets/' "$work/admin-after-dir.html" || { echo "FAIL: --public-dir shadowed the panel"; exit 1; }
# 替换目录对不是文件的路径一律回落 index.html（也是 200），只看状态码分不出 RPC 是否被遮蔽：比对正文。
# 与重启前保存的那份逐字节相同，也就钉住了外观跨重启保留（同一个二进制的 protojson 输出稳定，上面"a rejected update changed the site"那一处同样依赖这一点）。
[ "$(pubget site-after-dir GetSite '{}')" = 200 ] && cmp -s "$work/pub-site.json" "$work/pub-site-after-dir.json" || { echo "FAIL: --public-dir shadowed PublicService or the saved site settings did not survive the restart"; cat "$work/pub-site-after-dir.json"; exit 1; }
[ "$(rpc Login "$login_body")" = 200 ] || { echo "FAIL: login after restart"; exit 1; }
# token 跨重启存活，只读、不能写；技能文件取自 hub 实际下发的那份，其中的例子逐个在真实数据上跑。
[ "$(bearer ListNodes '{}')" = 200 ] || { echo "FAIL: API token lost across restart"; cat "$work/bearer-ListNodes.json"; exit 1; }
jq -e '(.nodes | length) == 2' "$work/bearer-ListNodes.json" > /dev/null || { echo "FAIL: ListNodes via token"; cat "$work/bearer-ListNodes.json"; exit 1; }
[ "$(bearer CreateNode '{"name":"via-token"}')" = 403 ] || { echo "FAIL: API token was allowed to write"; cat "$work/bearer-CreateNode.json"; exit 1; }
jq -e '.code == "permission_denied"' "$work/bearer-CreateNode.json" > /dev/null || { echo "FAIL: write via token not permission_denied"; exit 1; }
[ "$(bearer GetApiReference '{}')" = 200 ] || { echo "FAIL: GetApiReference via token"; exit 1; }
jq -e 'any(.files[]; .path == "heron/v1/admin.proto") and (.guide | contains("HERON_TOKEN"))' "$work/bearer-GetApiReference.json" > /dev/null || { echo "FAIL: GetApiReference content"; exit 1; }
run_skill_examples "" nonempty
[ "$(rpc DeleteApiToken "$(jq -nc --arg id "$api_token_id" '{id: $id}')")" = 200 ] || { echo "FAIL: DeleteApiToken"; exit 1; }
[ "$(bearer ListNodes '{}')" = 401 ] || { echo "FAIL: revoked API token still accepted"; exit 1; }
# 改密只清会话、不动 token；运行中由另一进程吊销，下一个请求即 401。
[ "$(rpc CreateApiToken '{"name":"e2e-cli"}')" = 200 ] || { echo "FAIL: CreateApiToken (cli)"; exit 1; }
api_token=$(jq -r '.token' "$work/CreateApiToken.json")
api_token_id=$(jq -r '.apiToken.id' "$work/CreateApiToken.json")
printf '%s\n' "$admin_pw" | bin/heron-hub passwd --db "$db" > "$work/passwd2.log" 2>&1
grep -q 'API tokens are not revoked' "$work/passwd2.log" || { echo "FAIL: passwd did not list API tokens"; cat "$work/passwd2.log"; exit 1; }
[ "$(rpc GetSnapshot '{}')" = 401 ] || { echo "FAIL: session survived password change"; exit 1; }
[ "$(bearer GetSnapshot '{}')" = 200 ] || { echo "FAIL: password change revoked the API token"; exit 1; }
bin/heron-hub token revoke --db "$db" --id "$api_token_id" > "$work/token-revoke.log" 2>&1 || { echo "FAIL: heron-hub token revoke"; cat "$work/token-revoke.log"; exit 1; }
[ "$(bearer GetSnapshot '{}')" = 401 ] || { echo "FAIL: CLI revocation not effective on a running hub"; exit 1; }
[ "$(rpc Login "$login_body")" = 200 ] || { echo "FAIL: login after password change"; exit 1; }
[ "$(rpc ListAlertRules '{}')" = 200 ] || { echo "FAIL: ListAlertRules after restart"; exit 1; }
jq -e '(.rules | length) == 2 and any(.rules[]; .kind == "ALERT_KIND_EXPIRY" and .daysBefore == 10)' "$work/ListAlertRules.json" > /dev/null || { echo "FAIL: alert rule lost"; cat "$work/ListAlertRules.json"; exit 1; }
[ "$(rpc ListNodes '{}')" = 200 ] || { echo "FAIL: ListNodes after restart"; exit 1; }
jq -e --arg id "$node1" --arg exp "$soon" '.nodes[] | select(.id == $id) | .billing | .price == "12.50" and .currency == "USD" and .billingCycle == "BILLING_CYCLE_MONTHLY" and .expiresOn == $exp and (.autoRenew // false) == false and .daysLeft >= 4 and .daysLeft <= 5' "$work/ListNodes.json" > /dev/null || { echo "FAIL: billing lost across restart"; cat "$work/ListNodes.json"; exit 1; }
# node1 在 firing 状态下重启：状态从 alert_state 读回，启动扫描不再发第二条触发。再保存一次同一条规则，让一次扫描
# 在响应之前同步做完，之后再数规则事件，不与启动扫描赛跑。审计事件没有 ruleId，不属于规则生命周期。
# 离线一对加到期的触发，共三条规则事件，都已送达。
[ "$(rpc SaveAlertRule "$(expiry_rule_body "$expiry_rule")")" = 200 ] || { echo "FAIL: SaveAlertRule expiry after restart"; cat "$work/SaveAlertRule.json"; exit 1; }
[ "$(rpc ListAlertEvents '{}')" = 200 ] || { echo "FAIL: ListAlertEvents after restart"; exit 1; }
jq -e '[.events[] | select(.ruleId != null)] | length == 3 and all(.[]; any(.deliveries[]; (.ok // false) == true))' "$work/ListAlertEvents.json" > /dev/null || { echo "FAIL: restart changed the alert events (lost, undelivered or fired again)"; cat "$work/ListAlertEvents.json"; exit 1; }
# 重启之后续期：60 天后到期，恢复事件送达，文案写新日期。
later=$(jq -rn 'now + 60 * 86400 | strftime("%Y-%m-%d")')
[ "$(rpc UpdateNode "$(node_body "$later" false)")" = 200 ] || { echo "FAIL: UpdateNode renewal"; cat "$work/UpdateNode.json"; exit 1; }
jq -e '.node.billing | .daysLeft >= 59 and .daysLeft <= 60' "$work/UpdateNode.json" > /dev/null || { echo "FAIL: renewed days_left"; cat "$work/UpdateNode.json"; exit 1; }
wait_alert "$node1" recovered "$wait_expiry_s"
jq -e --arg n "$node1" --arg exp "$later" '[.events[] | select(.nodeId == $n and .transition == "recovered")][0].summary == "节点 e2e-amd64 到期日已更新为 " + $exp + "（规则 e2e expiry）"' "$work/ListAlertEvents.json" > /dev/null || { echo "FAIL: expiry recovered summary"; cat "$work/ListAlertEvents.json"; exit 1; }
jq -e '[.events[] | select(.ruleId != null)] | length == 4 and all(.[]; any(.deliveries[]; (.ok // false) == true))' "$work/ListAlertEvents.json" > /dev/null || { echo "FAIL: alert events after renewal"; cat "$work/ListAlertEvents.json"; exit 1; }
[ "$(rpc ListProbeTasks '{}')" = 200 ] || { echo "FAIL: ListProbeTasks before deletion"; exit 1; }
task_version=$(jq -r '.version' "$work/ListProbeTasks.json")
[ "$(rpc DeleteProbeTask "$(jq -nc --arg id "$tcp_task" '{id: $id}')")" = 200 ] || { echo "FAIL: DeleteProbeTask"; exit 1; }
[ "$(rpc ListProbeTasks '{}')" = 200 ] || { echo "FAIL: ListProbeTasks after deletion"; exit 1; }
jq -e --arg version "$task_version" --arg icmp "$icmp_task" '(.version | tonumber) > ($version | tonumber) and (.tasks | length) == 1 and .tasks[0].task.id == $icmp' "$work/ListProbeTasks.json" > /dev/null || { echo "FAIL: task deletion not reflected"; cat "$work/ListProbeTasks.json"; exit 1; }
[ "$(rpc QueryProbes "$probe_body")" = 200 ] || { echo "FAIL: QueryProbes after deletion"; exit 1; }
echo "probe task version after deletion: $(jq -r '.version' "$work/ListProbeTasks.json")"
# 删除任务即对读者消失：它的历史行在清理作业完成前仍在库里，但不出现在查询结果里；留下的任务的历史照常可查。
jq -e --arg icmp "$icmp_task" '[.series[].taskId] == [$icmp]' "$work/QueryProbes.json" > /dev/null || { echo "FAIL: probe history after deleting a task"; cat "$work/QueryProbes.json"; exit 1; }
[ "$(rpc GetTraffic '{}')" = 200 ] || { echo "FAIL: GetTraffic after restart"; exit 1; }
# 总量不因周期滚动清零；agent 已退出，同周期的用量不再变化，跨周期则为零。
jq -e --arg id "$node1" --arg tx "$tx_before" --argjson before "$traffic_before" '.nodes[] | select(.nodeId == $id) |
  .traffic.resetDay == 15 and (.traffic.totalRx | tonumber) >= 1073741824 and (.traffic.totalTx // "0" | tonumber) <= ($tx | tonumber) and
  (if .traffic.periodStart == $before.periodStart then
    (.traffic.periodRx // "0") == ($before.periodRx // "0")
  else
    (.traffic.periodStart | tonumber) > ($before.periodStart | tonumber) and (.traffic.periodRx // "0") == "0"
  end)' "$work/GetTraffic.json" > /dev/null || { echo "FAIL: traffic state lost across restart"; cat "$work/GetTraffic.json"; exit 1; }
[ "$(rpc QueryMetrics "$query_body")" = 200 ] || { echo "FAIL: QueryMetrics after restart"; exit 1; }
jq -e 'any(.series[] | select(.name == "tx_bytes") | .samples[]; .n > 0 and .sum != null and .mean == null)' "$work/QueryMetrics.json" > /dev/null || { echo "FAIL: tx_bytes minute sums missing"; cat "$work/QueryMetrics.json"; exit 1; }
[ "$(rpc GetStorageStats '{}')" = 200 ] || { echo "FAIL: GetStorageStats"; cat "$work/GetStorageStats.json"; exit 1; }
jq -e '(.dbBytes | tonumber) > 0 and (.tables | length) > 0' "$work/GetStorageStats.json" > /dev/null || { echo "FAIL: GetStorageStats shape"; cat "$work/GetStorageStats.json"; exit 1; }
# 停机前最后一次读事件：规则事件仍是 4 条；总数（含登录、认证变更等系统事件）留给下面与库里的行数对照。
# Logout 不写事件，所以这之后到停机，alert_event 不再增长。
[ "$(rpc ListAlertEvents '{"limit": 500}')" = 200 ] || { echo "FAIL: ListAlertEvents before shutdown"; exit 1; }
jq -e '(.events | length) < 500 and ([.events[] | select(.ruleId != null)] | length == 4)' "$work/ListAlertEvents.json" > /dev/null || { echo "FAIL: rule events before shutdown"; cat "$work/ListAlertEvents.json"; exit 1; }
events_total=$(jq '.events | length' "$work/ListAlertEvents.json")
[ "$(rpc Logout '{}')" = 200 ] || { echo "FAIL: logout after restart"; exit 1; }
kill "$hub"; wait "$hub"
hub=""

cleanup
trap - EXIT

echo "--- hub.log ---"; cat "$work/hub.log"
echo "--- agent-amd64.log (tail) ---"; tail -5 "$work/agent-amd64.log"
echo "--- agent-arm64.log (tail) ---"; tail -5 "$work/agent-arm64.log"
[ "$agent_status" = 0 ] || { echo "FAIL: agent container failed"; exit 1; }
echo "--- stats ---"
bin/heron-hub stats --db "$db" > "$work/stats.txt"
cat "$work/stats.txt"
bin/heron-hub node list --db "$db" > "$work/nodes.txt"
cat "$work/nodes.txt"

get() { sed -n "s/^$1: //p" "$work/stats.txt"; }
# API 与 CLI 同一来源：两边列出同一组表（行数在两次读取之间会变，只比表名）。
jq -r '.tables[].name' "$work/GetStorageStats.json" > "$work/stats-api-tables.txt"
sed -n '/^db_bytes: /d; s/^\([a-z0-9_]*\): [0-9][0-9]*$/\1/p' "$work/stats.txt" > "$work/stats-cli-tables.txt"
[ -s "$work/stats-cli-tables.txt" ] && cmp -s "$work/stats-api-tables.txt" "$work/stats-cli-tables.txt" || { echo "FAIL: GetStorageStats and heron-hub stats list different tables"; cat "$work/stats-api-tables.txt" "$work/stats-cli-tables.txt"; exit 1; }
[ "$(get db_bytes)" -gt 0 ] || { echo "FAIL: db_bytes"; exit 1; }
# 已显式保存总闸与五项外观，空 logo 也是一行；表里目前只有公开页设置。
[ "$(get setting)" = 6 ] || { echo "FAIL: setting rows"; exit 1; }
[ "$(get node)" = 2 ] || { echo "FAIL: node count"; exit 1; }
[ "$(get theme)" = 0 ] && [ "$(get theme_file)" = 0 ] || { echo "FAIL: the deleted theme left rows"; exit 1; }
[ "$(get alert_rule)" = 2 ] || { echo "FAIL: alert rule count"; exit 1; }
[ "$(get alert_rule_node)" = 2 ] || { echo "FAIL: alert scope count"; exit 1; }
[ "$(get alert_event)" = "$events_total" ] || { echo "FAIL: alert event count (storage $(get alert_event), API $events_total)"; exit 1; }
[ "$(get alert_delivery)" = 4 ] || { echo "FAIL: alert delivery count"; exit 1; }
[ "$(get notify_channel)" = 1 ] || { echo "FAIL: channel count"; exit 1; }
[ "$(get probe_task)" = 1 ] || { echo "FAIL: task count"; exit 1; }
[ "$(get probe_task_node)" = 2 ] || { echo "FAIL: assignment count"; exit 1; }
[ "$(get probe_1m)" -ge 2 ] || { echo "FAIL: no probe minute rows"; exit 1; }
[ "$(get traffic)" = 2 ] || { echo "FAIL: traffic rows"; exit 1; }
[ "$(get node_facts)" = 2 ] || { echo "FAIL: facts count"; exit 1; }
[ "$(get metric_1m)" -ge 2 ] || { echo "FAIL: no minute rows"; exit 1; }
# 删掉的那个探测任务登记了一个清理作业；停机前维护循环可能已把它清完，也可能还没轮到，两种都对，多了不对。
case "$(get cleanup.pending)" in 0|1) ;; *) echo "FAIL: cleanup.pending"; exit 1 ;; esac
[ "$(get admin)" = 1 ] || { echo "FAIL: admin row"; exit 1; }
[ "$(get admin_session)" = 0 ] || { echo "FAIL: session not removed by logout"; exit 1; }
[ "$(grep -c 'node registered' "$work/hub.log")" = 2 ] || { echo "FAIL: registration log count"; exit 1; }
grep -Eq '^[0-9]+[[:space:]]+e2e-amd64[[:space:]]' "$work/nodes.txt" || { echo "FAIL: amd64 node missing"; exit 1; }
grep -Eq '^[0-9]+[[:space:]]+e2e-arm64[[:space:]]' "$work/nodes.txt" || { echo "FAIL: arm64 node missing"; exit 1; }
echo "E2E OK"
