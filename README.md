<img src="web/src/assets/heron.svg" alt="" width="64" height="64" />

# Heron

轻量自托管主机监控。agent 采集主机指标并上报，hub 存储、展示并对外提供查询与预授权的自动化管理。关注状态，不接管系统。

## 组件

- **hub**：静态链接的单二进制，数据在一个 SQLite 文件里，提供管理面板、公开页、告警通知与 API。支持 Linux（amd64、arm64）与 Docker 镜像。
- **agent**：装在每台被监控主机上，只读采集、周期上报。支持 Linux（amd64、arm64、armv7、386、riscv64；systemd 与 OpenRC）、Docker 镜像与 macOS（amd64、arm64）。

## 特性

- **指标与历史图**：CPU、按核负载、内存、swap、磁盘、连接数、网络速率与 TCP 建连/重传；历史图同时给出桶内均值与 agent 采样峰值。
- **探测任务**：ICMP、TCP、HTTP、DNS，按节点、标签或动态标签交集分配。HTTPS 任务可钉住叶证书公钥指纹；同一任务可跨节点并排对比。
- **流量与费用**：周期流量配额，收+发、只收、只发、收发取大者四种口径；记录价格、币种、付款周期与到期日，到期前提醒，可自动续期。
- **告警**：离线、探测失败、资源用量、流量配额、到期与证书到期等规则；Telegram 与 Webhook 通知渠道。
- **公开页与主题**：选中的节点可对外公开，访客无需登录；公开页可换第三方主题，主题在沙箱中运行，拿不到管理会话。
- **在线更新**：在面板里批量更新节点、或更新 hub 自身；只接受带官方发行签名的正式版本，新版本没能向 hub 确认就绪时自动回滚，回滚本身失败需人工处理。
- **自动化**：预授权 API token 让 AI 与脚本在授权范围内自主查询与修改；附给 AI agent 的技能文件与一键建节点的油猴脚本。

agent 不照单执行 hub 的指令。hub 每次应答能影响 agent 的只有四项：上报节奏（限在合法范围内）、探测任务（目标受节点本地策略约束）、出口地址探测的逐族开关（只能让 agent 停报某一族的出口地址，不影响上报、探测任务与鉴权）、在线更新任务（交给本机受限的更新器，更新器只接受带官方发行签名的正式版本并自行验签，不执行 hub 给的下载地址或命令）。hub 不能下发任意程序或修改节点本地配置。

## 快速开始

### 1. 安装 hub（Linux，systemd）

以 root 执行：

```sh
curl -fsSL https://github.com/xjetry/heron/releases/latest/download/install-hub.sh | sh
heron-hub passwd --db /var/lib/heron/heron.db
```

hub 默认只监听 `127.0.0.1:8080`，管理面板在 `http://127.0.0.1:8080/admin/`，TLS 交给反向代理。用 Docker 运行：

```sh
docker volume create heron-data
docker run -d --name heron --restart unless-stopped --stop-timeout 30 \
  -p 127.0.0.1:8080:8080 -v heron-data:/data -e TZ=Asia/Shanghai \
  ghcr.io/xjetry/heron-hub:v1.0.0
docker exec -it heron heron-hub passwd --db /data/heron.db
```

两种方式的分步说明与参数见 [docs/install.md](docs/install.md)。

### 2. 安装 agent

在面板「批量添加节点」开一个接入窗口拿到 key，然后在被监控主机上以 root 执行：

```sh
curl -fsSL https://github.com/xjetry/heron/releases/latest/download/install.sh | sh -s -- --hub https://heron.example.com --key <key>
```

Linux、Docker 与 macOS 的各种形态见 [docs/install.md](docs/install.md)。

### 3. 使用

节点上线后在面板操作：

- 「总览」与「节点」看实时状态，点进节点看历史图；「批量添加节点」一次接入多台。
- 「探测任务」建任务并跨节点对比结果；「告警规则」与「通知渠道」配告警。
- 账单、流量配额与到期日在节点的编辑抽屉里设置；「续费日历」集中看到期与续费。
- 「外观」开关公开页并设置样式，「主题」给公开页换第三方主题；「在线更新」批量升级节点。
- 「API token」给 AI 与脚本发凭据，见 [docs/automation.md](docs/automation.md)。

每个 hub 版本绑定一个 agent 版本，hub 只保证与绑定版本的 agent 配合。升级 hub 后，在面板「在线更新」把节点也更新到同一版本，见 [docs/operations.md](docs/operations.md)。

## 平台支持

| 形态 | 平台 |
|---|---|
| hub 二进制、hub 镜像 | Linux amd64、arm64 |
| agent 二进制 | Linux amd64、arm64、armv7、386、riscv64；macOS amd64、arm64 |
| agent 镜像 | Linux amd64、arm64 |
| 服务管理 | Linux：systemd 与 OpenRC；macOS：launchd |

rootless Docker 不支持；Podman 可用。限制与替代见 [docs/install.md](docs/install.md) 的支持平台一节。

## 常用命令

| 命令 | 作用 |
|---|---|
| `heron-hub serve` | 启动 hub（安装脚本已配成服务，一般不用直接执行） |
| `heron-hub passwd` | 设置或重设管理员密码 |
| `heron-hub security-reset --yes` | 清除 TOTP、恢复码与全部 Passkey |
| `heron-hub node\|window\|token` | 在 hub 主机上管理节点、注册窗口与 API token |
| `heron-hub stats` / `health` / `version` | 存储统计、健康检查、版本 |
| `heron-hub restore` | 离线恢复快照（先停 hub） |
| `heron-agent register` / `configure` / `run` | 注册、改本地策略、运行（安装脚本已配成服务） |

每条命令的参数见 `<命令> -h`；安装脚本的参数见 `<脚本> --help`。

## 文档

| 文档 | 内容 |
|---|---|
| [docs/install.md](docs/install.md) | 安装 hub 与 agent：Linux systemd、Docker、macOS |
| [docs/operations.md](docs/operations.md) | 升级与回退、备份与恢复、反向代理、账户安全、环境变量、时区、宿主机本地策略、排查、验证发布产物 |
| [docs/automation.md](docs/automation.md) | API token 与权限范围、给 AI agent 的技能文件、油猴脚本 |
| [docs/theme-guide.md](docs/theme-guide.md) | 公开页主题的安装、隔离与备份 |
| [proto/SKILL.md](proto/SKILL.md) | 给 AI agent 的 hub API 技能文件（面板里可下载同版本） |

## 许可

MIT，见 [LICENSE](LICENSE)。
