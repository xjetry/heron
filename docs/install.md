# 安装

- hub：[Linux systemd](#安装-hublinuxsystemd)、[Docker](#用-docker-运行-hub)
- agent：[Linux](#linuxsystemd-与-openrc)、[Docker](#docker)、[macOS](#macos)

## 安装 hub（Linux，systemd）

支持 amd64、arm64，以 root 执行：

```sh
curl -fsSL https://github.com/xjetry/heron/releases/latest/download/install-hub.sh | sh
heron-hub passwd --db /var/lib/heron/heron.db
```

安装器下载本版 hub 并按脚本内嵌的 SHA-256 校验，创建静态系统用户 `heron-hub` 与同名 systemd 服务，确认进程持续存活后提示设置密码。管理员密码由你设置，脚本不生成、不打印。主机没有 CA 证书包时安装器会装上 `ca-certificates`：hub 发往 Telegram 的告警走 HTTPS。

数据库固定为 `/var/lib/heron/heron.db`。默认只监听 `127.0.0.1:8080`，TLS 交给反向代理（见 [operations.md](operations.md#反向代理与---trusted-proxies)）。

serve 参数在安装时随命令给出：`--listen`、`--timezone`、`--offline-after`、`--trusted-proxies`、`--public-dir`、`--geo-mmdb`、`--retention-1m`、`--retention-5m`、`--retention-1h`、`--retention-alert-events`；`--admin-origin` 仅用于迁移已有的 Passkey 凭据，新安装不需要。各参数的含义见 `heron-hub serve -h`。例如：

```sh
curl -fsSL https://github.com/xjetry/heron/releases/latest/download/install-hub.sh | sh -s -- \
  --timezone Asia/Taipei --trusted-proxies 127.0.0.1/32
```

### 升级与卸载（hub）

- 重跑同一条安装命令即升级：沿用 `/etc/systemd/system/heron-hub.service` 里 `ExecStart` 的参数，命令行显式给出的值按参数名替换旧值。
- 脚本只装自己所属的版本，没有 `--version`：`releases/latest/download/install-hub.sh` 装最新正式版；装指定版本取该版本的脚本，`https://github.com/xjetry/heron/releases/download/vX.Y.Z/install-hub.sh`。
- 升级前先看 [operations.md 的升级与回退](operations.md#升级与回退)：新版本的 `serve` 会迁移数据库，迁移后旧版本打不开。
- 手工定制（例如 `Environment=HERON_OFFLINE_AFTER=60s`）放进 drop-in（`systemctl edit heron-hub`）：每次安装都用发行包的单元覆盖主单元、只保留其中 `ExecStart` 的参数，不设 `ExecStart` 的 drop-in 升级时保留。普通卸载保留 drop-in，`--purge` 删除。
- 卸载（保留数据与账户）：

```sh
curl -fsSL https://github.com/xjetry/heron/releases/latest/download/install-hub.sh | sh -s -- --uninstall --yes
```

  加 `--purge` 连同 `/var/lib/heron`、服务用户与组一起删除，不可恢复：

```sh
curl -fsSL https://github.com/xjetry/heron/releases/latest/download/install-hub.sh | sh -s -- --uninstall --purge --yes
```

  两者都需确认；无终端时必须显式 `--yes`。

- 安装、升级都会先核对目标端口的监听进程（排除现有 hub 自身），冲突时不停止旧服务。查看状态与日志：`systemctl status heron-hub`、`journalctl -u heron-hub`。

## 用 Docker 运行 hub

镜像 `ghcr.io/xjetry/heron-hub:<版本>`，含 linux/amd64 与 linux/arm64；正式版本同时成为 `latest`。镜像基于空镜像构建，只有静态链接的 `heron-hub`、CA 证书与非 root 用户，没有 shell。

```sh
docker volume create heron-data
docker run -d --name heron --restart unless-stopped --stop-timeout 30 \
  -p 127.0.0.1:8080:8080 \
  -v heron-data:/data \
  -e TZ=Asia/Shanghai \
  ghcr.io/xjetry/heron-hub:v1.0.0
```

- 默认参数是 `serve --db /data/heron.db --listen 0.0.0.0:8080`。镜像名之后写的任何参数都会替换这一整组默认参数，要加参数时连同默认的一起写全：

```sh
docker run -d --name heron --restart unless-stopped --stop-timeout 30 -p 127.0.0.1:8080:8080 -v heron-data:/data \
  ghcr.io/xjetry/heron-hub:v1.0.0 \
  serve --db /data/heron.db --listen 0.0.0.0:8080 --timezone Asia/Shanghai
```

- `--stop-timeout 30` 给关停留出余量：默认宽限 10 秒与 hub 排空在途请求的上限相等，排空用满时最后一批写入可能被截断。
- 镜像自带 `HEALTHCHECK`：`healthy` 表示 hub 已能接受请求，不只是进程还在。
- 容器里监听 `0.0.0.0` 是预期的。端口只发布到本机回环（`-p 127.0.0.1:8080:8080`，反代在宿主上），或者不发布端口、让反代容器与 hub 在同一个 Docker 网络里转发到 `http://heron:8080`。反代与 `--trusted-proxies` 的配置见 [operations.md](operations.md#反向代理与---trusted-proxies)。
- 绑定宿主目录（`-v /srv/heron:/data`）代替命名卷时，目录须可被 uid 65532 写入（Linux 宿主上 `chown 65532:65532 /srv/heron`），否则 hub 启动即退出。

### 设置管理员密码（Docker）

hub 没有经网络的首次设置页，密码在容器里设置；hub 运行中设置即生效，不需要重启：

```sh
docker exec -it heron heron-hub passwd --db /data/heron.db
```

从脚本设置时经 stdin 传一行，必须带 `-i`（不带 `-i` 时容器里的 stdin 是空的，`passwd` 报 `no password on stdin` 且不做任何修改）：

```sh
printf '%s\n' "$ADMIN_PASSWORD" | docker exec -i heron heron-hub passwd --db /data/heron.db
```

### 升级（Docker）

换镜像 tag，删掉旧容器、用同样参数起新容器（compose：改 `image:` 后 `docker compose up -d`），数据卷沿用。升级注意事项同 [operations.md 的升级与回退](operations.md#升级与回退)。

## 安装 agent

### 先拿注册凭据

两种凭据都能用来注册，都只能用一次：

- **接入窗口 key**：面板「批量添加节点」开一个窗口；或在 hub 主机上 `heron-hub window open --db /var/lib/heron/heron.db`（`--max` 限名额、`--ttl` 限时长）。
- **节点安装凭据**：面板「节点」页直接添加节点，创建时返回 `heron_install_` 开头的凭据；`heron-hub node create --db /var/lib/heron/heron.db --name <名称>` 同样返回。

hub 认领节点后使该凭据失效，返回只用于上报的运行 token：安装凭据不能上报，运行 token 不能再次注册。在面板「换 token」后，必须用弹窗中带 `--re-register` 的命令重新注册，普通升级不消费 key。

安装命令以本仓库 README 与 GitHub Release 为准，不以 hub 面板为准：面板由 hub 提供，hub 失守时面板上的命令可以被整条换掉。复制前核对脚本地址是 `https://github.com/xjetry/heron/releases/…`。

### Linux（systemd 与 OpenRC）

以 root 执行（面板「批量添加节点」给出的命令形同这条）：

```sh
curl -fsSL https://github.com/xjetry/heron/releases/latest/download/install.sh | sh -s -- --hub https://heron.example.com --key <key>
```

参数：`--hub`、`--key`（首次安装必需）、`--name`、`--insecure-http`、`--base-url`、`--update-source github|hub`（在线更新从哪里取产物，见 [operations.md](operations.md#在线更新)）。

- 普通重跑即升级：已有配置时沿用现有注册，不消费 `--key`。
- `--update-source` 只对 Linux systemd 有效，OpenRC 主机的安装器会拒绝它。
- 卸载：同一条命令把参数换成 `--uninstall`；加 `--purge` 一并删除配置、日志目录、`heron-agent` 用户与同名组：

```sh
curl -fsSL https://github.com/xjetry/heron/releases/latest/download/install.sh | sh -s -- --uninstall
```

### Docker

镜像 `ghcr.io/xjetry/heron-agent:<版本>`，含 linux/amd64 与 linux/arm64。容器里的 agent 读的是宿主的指标，与装在宿主上的 agent 同一口径；读不到的读数缺失而不是 0。先注册一次，再长期运行，两条命令用同一个配置卷：

```sh
docker run --rm --network host -v heron-agent:/etc/heron-agent \
  ghcr.io/xjetry/heron-agent:v1.0.0 register --hub https://heron.example.com --key <key>
docker run -d --name heron-agent --restart unless-stopped \
  --network host --read-only -v /:/hostfs:ro -v heron-agent:/etc/heron-agent \
  ghcr.io/xjetry/heron-agent:v1.0.0
```

- 两个必要参数：`-v /:/hostfs:ro` 把宿主根（连同 `/proc`、`/sys`、`/etc/os-release`）交给 agent，CPU、内存、磁盘、系统信息都从这里读；`--network host` 让连接数、网卡与本机地址是宿主的。缺任一个 agent 拒绝启动。镜像默认参数是 `run --host-root /hostfs`，第二条命令不要在镜像名后面加参数。
- `register` 的参数与安装脚本相同：`--hub`、`--key`、`--name`（默认宿主主机名）、`--insecure-http`。节点 token 写在卷里的 `config.json`，长期运行的容器定义里没有任何凭据；key 只出现在那条一次性容器的命令行里，容器退出即删。
- 改用绑定挂载代替配置卷时，先把宿主目录交给容器用户：`mkdir -p /etc/heron-agent && chown 65532:65532 /etc/heron-agent && chmod 0700 /etc/heron-agent`。
- 升级：换镜像 tag，删掉旧容器、用同样参数起新容器，配置卷沿用。容器形态不支持在线更新。
- rootless Docker 不支持：`--network host` 的容器里没有 sysfs，agent 无法确认自己在宿主的网络命名空间，拒绝启动。Podman（rootful 与 rootless）用同样的两条命令，`docker` 换成 `podman`，两条都加 `--cap-add NET_RAW`；rootless 下 ICMP 探测还取决于宿主的 `net.ipv4.ping_group_range` 是否放行 agent 的组。
- 本地策略：`docker run --rm -v heron-agent:/etc/heron-agent ghcr.io/xjetry/heron-agent:v1.0.0 configure --probe-allow …`，参数见 [operations.md 的宿主机本地策略](operations.md#宿主机本地策略)，改完 `docker restart heron-agent`。

compose 写法（注册仍是一次性的 `run --rm`，不写进 compose 文件）：

```yaml
services:
  agent:
    image: ghcr.io/xjetry/heron-agent:v1.0.0
    restart: unless-stopped
    network_mode: host
    read_only: true
    volumes:
      - /:/hostfs:ro
      - heron-agent:/etc/heron-agent
volumes:
  heron-agent:
```

```sh
docker compose run --rm agent register --hub https://heron.example.com --key <key>
docker compose up -d
```

### macOS

Apple Silicon 与 Intel 通用，脚本按硬件选包：

```sh
curl -fsSL https://github.com/xjetry/heron/releases/latest/download/install-macos.sh | sudo sh -s -- --hub https://heron.example.com --key <key>
```

- agent 装成 LaunchDaemon `xyz.heron.agent`，以隐藏的系统用户 `_heron-agent` 运行；二进制在 `/usr/local/bin/heron-agent`，配置在 `/etc/heron-agent/`，日志在 `/Library/Logs/heron-agent/`。
- 参数与 Linux 脚本相同：`--hub`、`--key`、`--name`、`--insecure-http`、`--base-url`；没有 `--update-source`（macOS 不支持在线更新）。
- 卸载：`… | sudo sh -s -- --uninstall`；加 `--purge` 一并删除配置、日志、用户与同名组。

### `--insecure-http`

hub 地址是 `http://` 且主机不是回环地址（`127.0.0.0/8`、`[::1]`；`localhost` 不算）时必须给出，agent 否则拒绝注册与启动。它把接受明文 http 这一许可写进 agent 的本地配置，意味着节点 token 与指标明文传输；能用 https 就用 https。面板在这种地址上给出的命令会自动带上它。不重跑安装脚本、只修正已部署节点时：

```sh
sudo heron-agent configure --insecure-http=true
sudo systemctl restart heron-agent
```

### 安装脚本共同的规则

- 脚本内嵌本版全部 tar 包的 SHA-256，只按它校验下载的包。`--base-url URL` 只改从哪个目录下载（镜像站、本地归档），不改变校验依据。
- `install-hub.sh` 装自己所属的 hub 版本；`install.sh` 与 `install-macos.sh` 装该 release 的 hub 绑定的 agent 版本，`releases/latest/download/` 下的因而是最新 hub 绑定的版本。要装指定版本，取那个版本 release 里的脚本：`https://github.com/xjetry/heron/releases/download/vX.Y.Z/<脚本名>`。
- 仓库里的源码脚本没有内嵌哈希，只能卸载；安装请用 release 里的脚本。
- 安装凭据只能消费一次。脚本已显示注册成功并写好配置、但后续步骤失败时，去掉 `--re-register` 按普通升级重跑，沿用已写入的运行 token。注册响应丢失或配置写入失败时，在面板为原节点重新换发凭据：不重复创建节点，也不重用已消费的凭据。

## 支持平台

| 组件 | 平台 |
|---|---|
| hub | Linux amd64、arm64；Docker 镜像 linux/amd64、linux/arm64 |
| agent | Linux amd64、arm64、armv7、386、riscv64；macOS amd64、arm64；Docker 镜像 linux/amd64、linux/arm64 |

不支持 Windows 与 FreeBSD；rootless Docker 不支持（见上）；Podman 需要 `--cap-add NET_RAW`。
