# 运维

## 升级与回退

### 版本配合

每个 hub 版本绑定一个 agent 版本（release 里的 `AGENT_VERSION` 资产，一行）。hub 只保证与它绑定版本的 agent 配合；面板按绑定版本给落后的节点打 `agent 低于 <版本>` 徽标，在线更新节点也只到绑定版本。更新 hub 之后，要在面板里把节点也更新到同一版本。

### 升级 hub

升级 hub 就是换上新的二进制（或新镜像）后启动一次 `serve`：systemd 安装重跑安装脚本，Docker 换镜像 tag 重建容器。新版本发现库的格式比自己旧时，先在库旁备份，再迁移：

- 备份文件名为 `<库路径>.before-schema-<升级前的版本号>.<序号>`，权限 0600，单个文件即完整的库。只保留序号最大的两份，更早的在新备份写好之后删除。面板的「存储」页与 `heron-hub stats` 列出现有的迁移前备份。
- 备份任何一步失败（目录不可写、磁盘满、库被锁住等）都不迁移；迁移本身是一个事务，失败时库保持原样，直接换回旧二进制就能启动。
- 迁移成功后旧版本不能再打开这个库。离线子命令（`passwd`、`stats` 等）遇到更新的库会拒绝操作，提示先用新版本 `serve` 升级。
- 磁盘至少留出库大小的一倍（备份），跨多个版本升级大库时另加迁移期间的日志空间。

### 回退

迁移成功后要回退到升级前的版本（从升级到回退之间写入的指标与配置改动会丢失）：

1. 停 hub：`systemctl stop heron-hub`（Docker：`docker stop heron`）。
2. 选备份：备份名里的版本号对应升级前的库版本，升级前运行的那一版正好能打开它。不确定时复制一份备份，用候选二进制对副本运行 `heron-hub stats --db <副本>`：能输出读数即匹配；报 `newer than this binary` 说明二进制太旧，报 `older than this binary` 说明太新。
3. 用备份替换库，并删掉库旁的 `-wal` 与 `-shm`（它们属于迁移后的库，留下的 `-wal` 会在打开时被当作这份库的日志重放）。systemd 安装：

```sh
cd /var/lib/heron
rm -f heron.db-wal heron.db-shm
install -o heron-hub -g heron-hub -m 0600 heron.db.before-schema-<旧版本号>.<序号> heron.db
```

   Docker（卷里的文件属主是 uid 65532，`cp -p` 保留属主与权限）：

```sh
docker run --rm -v heron-data:/data alpine:3.21 sh -c 'cd /data && rm -f heron.db-wal heron.db-shm && cp -p heron.db.before-schema-<旧版本号>.<序号> heron.db'
```

4. 换回旧二进制（Docker 换回旧镜像 tag）后启动。

### 在线更新

管理后台的「在线更新」提供官方版本检查、hub 更新与选定节点批量更新。

- 前提：在对应机器重跑一次官方安装器，装上独立的 `heron-updater-hub` / `heron-updater-agent` root 服务。主服务仍以原来的非 root 用户运行。Docker、OpenRC、macOS 和不符合固定程序/配置路径的自定义服务不支持在线更新。
- 更新只接受官方仓库已发布的正式版本，拒绝预发布、同版和降级。每台机器串行更新；节点任务离线等待最多 24 小时，仅尚未下发的任务可以取消。安装器与在线更新互斥，正在更新时重跑安装器会拒绝。
- 下载与校验期间旧服务继续运行。agent 必须以新版本成功向 hub 上报才算完成：更新器最多等 90 秒，未确认就自动回滚。回滚本身失败时保留备份并报告错误，需要人工处理，事务与回滚备份在 `/var/lib/heron-update-hub` 与 `/var/lib/heron-update-agent`。
- 更新器只替换主程序，不更新自身、systemd 单元或启动参数；更新器本身通过重跑新版官方安装器升级。
- 节点默认从 GitHub 取产物；出站受限、只能连到 hub 的节点在安装时给 `install.sh` 加 `--update-source hub`，改由 hub 中转：hub 从 GitHub 取回、验签后转发，节点上的更新器照样验签。重跑安装器不带这个参数时沿用原设置，给 `--update-source github` 改回直连。面板生成安装命令时勾选「在线更新经 hub 中转」会带上这个参数。
- 更新器日志：节点上 `journalctl -u heron-updater-agent`，hub 主机上 `journalctl -u heron-updater-hub`。

## 备份与恢复

### 在线快照（S3 兼容）

在面板配置 S3 兼容 endpoint、bucket、区域、access key、secret 与前缀后，hub 自动生成分层快照：配置层默认每 5 分钟一次、保留 48 份；指标层默认每天一次、保留 14 份。周期与份数可配置，两层状态从面板读取。

- bucket 必须私有，并使用 HTTPS；快照不额外加密。快照包含认证密钥与节点凭据，按凭据管理，不应公开 bucket 或分享快照。
- 配置层故障与恢复按设置的渠道通知；周期成功只记事件。

### 恢复

恢复前停止 hub，把配置快照、可选的指标快照与快照引用的主题包下载到本地：

```sh
heron-hub restore --db /var/lib/heron/heron.db --config config.db --metrics metrics.db --themes ./themes --yes
```

- `--config` 必需；省略 `--metrics` 保留库里的现有指标；`--themes` 指向按摘要命名的 `<SHA-256>.zip` 目录，省略时恢复配置但停用全部主题。
- 恢复不复活登录会话、注册窗口或节点更新任务。恢复记录（两层各自的快照时刻、恢复时刻等）在面板备份区列出。
- Docker 部署先 `docker stop heron`，挂同一个卷在临时容器里执行 `restore`，完成后 `docker start heron`。

### 停机整卷备份

不使用在线快照时，停 hub 后复制整个数据目录。库由 `heron.db` 与同名的 `heron.db-wal`、`heron.db-shm` 三个文件组成，已提交但尚未写回 `heron.db` 的数据只在 `-wal` 里：运行中只复制 `heron.db` 会丢数据。systemd 安装复制 `/var/lib/heron/`；Docker：

```sh
docker stop heron
docker run --rm -v heron-data:/data -v "$PWD":/backup alpine:3.21 tar -C /data -czf /backup/heron-data.tgz .
docker start heron
```

## 反向代理与 `--trusted-proxies`

hub 只提供明文 HTTP，TLS 由反代（Caddy、nginx、CDN）终止。

- `--trusted-proxies` 写 hub 看到的反代地址（TCP 对端；CIDR 列表，逗号分隔）。只有来自这些地址的 `X-Forwarded-For`、`X-Forwarded-Proto` 才被采信；为空则不信任任何转发头。
- HTTPS 反代必须保留浏览器访问的 Host（含非默认端口），并设置 `X-Forwarded-Proto: https`。否则管理接口的同源检查会拒绝浏览器请求（包括密码登录），返回 403。不要通过改写或删除浏览器的 Origin 头绕过检查。
- 不信任转发头时，公开页与注册的限流按 TCP 对端地址计，反代后的访客共用代理地址的一个桶，登录锁定也按代理地址计；节点来源 IP 同样记成代理地址。
- 反代与 CDN 必须对 `/heron.v1.AdminService/` 路径禁用响应压缩，并保留 hub 的 `Cache-Control: no-store, no-transform`；公开 API 与静态资源可继续压缩。部署后用带 `Accept-Encoding: gzip` 的管理请求检查最终响应没有 `Content-Encoding`、缓存头完整。
- 公开服务按来源限流（IPv4 一个地址、IPv6 一个 /64 算一个来源）：每个来源瞬时 60 次，此后每秒 10 次，超出返回 429。
- 经过 CDN 时在反代验证 CDN 来源后解析真实客户端地址，再转发给 hub；不要无条件信任客户端自带的地址头。

## 账户安全与恢复命令

在面板「安全」页管理认证方式：TOTP、一次性恢复码、Passkey。

- 启用 TOTP 后，密码登录必须同时提供六位动态验证码或一个恢复码；恢复码只显示一次，使用后失效，重新生成会作废旧码。
- 修改认证方式需要重新证明身份，并撤销全部旧会话。
- Passkey 支持无密码登录：通过当前 HTTPS 域名进入「安全」页添加，首次注册把该域名与凭据绑定。换域名后用密码及现有第二因素登录，在「安全」页重新绑定并注册新 Passkey；改绑会撤销旧 Passkey 和旧会话。管理界面要求 HTTPS。

认证器丢失时先用恢复码登录。密码或全部认证器丢失时，登录 hub 服务器，用对数据库有读写权限的账号执行（不需要旧密码或认证器证明；两条都撤销全部登录会话，无需重启 hub）：

```sh
# 重设密码，终端交互输入且不回显；不会清除 TOTP 或 Passkey
heron-hub passwd --db /var/lib/heron/heron.db
# 清除 TOTP、恢复码和全部 Passkey；不改变密码或 API token
heron-hub security-reset --db /var/lib/heron/heron.db --yes
```

- 密码和认证器全部丢失时执行两条。
- `passwd` 会列出现有 API token，并在交互终端询问是否一并撤销；非交互时默认保留。
- Docker 部署：

```sh
docker exec -it heron heron-hub passwd --db /data/heron.db
docker exec heron heron-hub security-reset --db /data/heron.db --yes
```

## 环境变量

`serve` 的每个 flag 都可以用环境变量 `HERON_<FLAG>` 给出：flag 名的横线换成下划线、转大写，例如 `--offline-after` ↔ `HERON_OFFLINE_AFTER`、`--trusted-proxies` ↔ `HERON_TRUSTED_PROXIES`。同一项两处都给时以显式 flag 为准。环境变量设为空串等于显式给出空值，空值的含义与该 flag 写成空值时相同（`HERON_OFFLINE_AFTER=` 取默认值）。

| 变量 | 作用 |
|---|---|
| `TZ` | IANA 时区名；`--timezone` 与 `HERON_TIMEZONE` 都没有给出时使用 |
| `HERON_OFFLINE_AFTER` | 节点离线判定时长（`10s` 到 `3m`，默认 `30s`）；agent 上报间隔、重试退避上限与告警宽限下限都由它推出 |

systemd 安装把这类定制放进 drop-in（`systemctl edit heron-hub`）：主单元里的手工改动会在升级时丢失。

## 时区

hub 按 `--timezone` 判定流量周期的重置日与节点到期日的天边界。不指定时取宿主时区（`TZ` 或 `/etc/localtime`），都解析不到时按 UTC 并在启动日志里告警。Docker 镜像里没有 `/etc/localtime`，用 `-e TZ=Asia/Shanghai`（不替换默认参数），或在写全的参数里加 `--timezone`；两者都给时以 `--timezone` 为准。

## 宿主机本地策略

agent 不照单执行 hub 的指令。hub 每次应答能影响 agent 的只有四项：上报节奏（限在合法范围内）、探测任务（目标受节点本地策略约束）、出口地址探测的逐族开关（只能让 agent 停报某一族的出口地址，不影响上报、探测任务与鉴权）、在线更新任务（交给本机受限的更新器，更新器只接受带官方发行签名的正式版本并自行验签，不执行 hub 给的下载地址或命令）。hub 不能下发任意程序或修改节点本地配置。本地探测策略写在 `/etc/heron-agent/config.json`，用 `heron-agent configure` 修改，改完重启服务生效：

```sh
# 放行对本机回环与某个内网段的探测（默认拒绝本机回环）
sudo heron-agent configure --probe-allow 127.0.0.0/8,10.20.0.0/16
# 另外拒绝整个私网（私网默认允许）
sudo heron-agent configure --probe-deny 10.0.0.0/8,172.16.0.0/12,192.168.0.0/16
# 清空某个列表
sudo heron-agent configure --probe-deny ""
sudo systemctl restart heron-agent   # OpenRC：rc-service heron-agent restart；macOS：sudo launchctl kickstart -k system/xyz.heron.agent
```

- 默认拒绝的目标：本机回环、本机网卡上的全部地址（每次探测前重新枚举）、链路本地（含云厂商的 metadata 地址 `169.254.169.254`）、组播与广播。检查的是域名解析之后实际要连的地址。被拒的任务在面板上显示为错误，写明地址和命中的前缀。
- 节点探测自己网卡上的地址会被拒：要放行就写出该地址的 `/32`（IPv6 为 `/128`）；回环与链路本地地址按整段放行（如 `127.0.0.0/8`）。
- 规则按最长前缀匹配；前缀一样长时本地规则优先于默认规则。前缀必须写成规范形式（`10.0.0.0/8`，不能写 `10.1.2.3/8`）；同一前缀不能同时出现在两个列表里。配置文件里有不认识的字段时 agent 拒绝启动，免得拼错的规则被静默忽略。
- 计入流量与速率的网卡由 `heron-agent run` 的 `--net-include` / `--net-exclude` 控制（逗号分隔的 glob，给出任一个即整个替换平台默认规则；默认规则与各平台默认值见 `heron-agent run -h`）。安装脚本不暴露这两个参数：systemd 用 drop-in 覆盖 `ExecStart`，OpenRC 改 `/etc/init.d/heron-agent`，macOS 改 plist 的 `ProgramArguments`；升级会重写发行包自带的服务定义，drop-in 之外的改动要重放。
- agent 日志有总量上限（突发之后被压掉的行数记在下一条日志里）。systemd 下日志进 journald；OpenRC 与 launchd 的日志文件不自动轮转，由宿主机的日志管理处理。

## 排查

- hub 日志：`journalctl -u heron-hub`（Docker：`docker logs heron`）。
- hub 状态与存储：`heron-hub stats --db /var/lib/heron/heron.db`（库大小、各表行数、存储健康与迁移前备份）、`heron-hub version`、`heron-hub health`（请求 `/healthz`，2xx 时退出码 0）。
- 在 hub 主机上离线管理：`heron-hub node create|list|delete|rotate-token`、`heron-hub window open|close|show`、`heron-hub token list|revoke`，都带 `--db /var/lib/heron/heron.db`。hub 运行中执行也生效，hub 约 1 秒内感知变化；生效之前 hub 仍按旧状态裁决，稍后重试即可。
- agent 日志：`journalctl -u heron-agent`（OpenRC 与 macOS 在各自的日志文件，macOS 为 `/Library/Logs/heron-agent/`）。
- Docker 镜像里没有 shell，`docker exec heron sh` 不可用。可以：
  - `docker exec heron heron-hub stats --db /data/heron.db`
  - 挂同一个卷起一个带 shell 的临时容器：`docker run --rm -v heron-data:/data alpine:3.21 ls -l /data`
  - 从 hub 自己的网络里发请求：`docker run --rm --network container:heron alpine:3.21 wget -qO /dev/null http://127.0.0.1:8080/admin/ && echo ok`

## 验证发布产物

每个 release 带 `SHA256SUMS` 与它的签名 `SHA256SUMS.sig`，另有两个清单资产：

- `AGENT_VERSION`：这一版 hub 绑定的 agent 版本，一行。
- `IMAGES`：这一版发布的镜像，每行 `<仓库>:<tag>@sha256:<索引 digest>`。

签名用 Ed25519 覆盖固定前缀、tag 与 `SHA256SUMS` 原文；镜像不另签名，它的 digest 写在 `IMAGES` 里，随 `SHA256SUMS` 一起被签名覆盖。

1. 取清单并验签（OpenSSL 3 或更新；macOS 自带的 `/usr/bin/openssl` 是 LibreSSL，读不了 Ed25519 公钥，改用 Homebrew 的 `openssl`）：

```sh
v=vX.Y.Z
base=https://github.com/xjetry/heron/releases/download/$v
mkdir "heron-$v" && cd "heron-$v"
for f in SHA256SUMS SHA256SUMS.sig AGENT_VERSION IMAGES; do curl -fsSLO "$base/$f"; done
printf '%s\n' '-----BEGIN PUBLIC KEY-----' 'MCowBQYDK2VwAyEAx5bcmXXnQ0kdpXfLfl+YsHVHgbivdOr/YHdrVb/5n4Q=' '-----END PUBLIC KEY-----' > heron-release.pem
{ printf 'heron-release-v1\n%s\n' "$v"; cat SHA256SUMS; } > signed-message
openssl base64 -d -A -in SHA256SUMS.sig -out SHA256SUMS.sig.bin
openssl pkeyutl -verify -pubin -inkey heron-release.pem -rawin -in signed-message -sigfile SHA256SUMS.sig.bin
```

   通过时打印 `Signature Verified Successfully`；版本写错、清单被改过都会打印 `Signature Verification Failure` 并以非 0 退出。

2. 核对下载的文件：`sha256sum -c --ignore-missing SHA256SUMS`（macOS 用 `shasum -a 256 -c --ignore-missing SHA256SUMS`）。`--ignore-missing` 跳过没下载的资产，所以要看输出里 `AGENT_VERSION`、`IMAGES` 与你下载的每个包、脚本都是 `OK`。

3. 核对镜像：`IMAGES` 每一行 `@` 之前的部分是 tag 引用，之后是这一版发布的索引 digest：

```sh
cat IMAGES
docker buildx imagetools inspect "ghcr.io/xjetry/heron-hub:$v" --format '{{.Manifest.Digest}}'
```

   打印的 digest 应与 `IMAGES` 里 hub 那一行 `@` 之后的部分相同（agent 镜像同理换成 `heron-agent`）。运行时把整行当镜像引用，例如 `ghcr.io/xjetry/heron-hub:vX.Y.Z@sha256:<digest>`：带 digest 的引用按 digest 拉取，tag 之后被移到别的索引也拉不到别的内容。`latest` 不在 `IMAGES` 里。
