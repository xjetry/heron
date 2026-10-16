# 自动化

hub 的管理接口面向 AI agent 与脚本设计：预授权 API token，HTTP+JSON 调用，不需要生成客户端。

## API token 与权限范围

在面板「API token」页创建。token 形如 `heron_at_` 加 64 位十六进制，创建后只显示一次，按凭据保管。

- 默认全站只读。可按操作勾选预授权：监控配置、创建节点、注册入口、轮换节点凭据、删除节点及历史、官方节点更新；范围选全站或指定节点。
- 指定节点的授权同时约束读写；普通标签不扩权。凭据自己创建或经自己的注册窗口接入的节点自动纳入范围。
- 有效期：新建时可选 1 小时、1 天、7 天、30 天或自定义时刻，默认不过期。到期后用它的请求返回 401，message 写明过期时刻；在面板改有效期即恢复，明文不变。
- 编辑：行菜单「编辑」整体替换名称、权限、节点范围与有效期，明文与操作记录不变，下一个请求即按新授权鉴权。去掉「注册入口」会关闭它开着的注册窗口。
- agent 用 `GetCaller` 读取自己的权限、范围与有效期。`heron-hub token list` 的 EXPIRES 列显示有效期，过期的标 `(expired)`。
- 不开放：远程命令、云主机操作、hub 升级、管理员与 API 凭据管理、会话管理、通知密钥修改。
- 写入经 `ExecuteChange` 进行：先 `preview: true` 预览改动，再带上预览返回的 `expectedVersion` 与唯一 `requestId` 执行；响应丢失时原样重试同一 `requestId`，不会重复执行。每次写入都留审计回执。
- 吊销在面板操作，或在 hub 主机上：

```sh
heron-hub token list --db /var/lib/heron/heron.db
heron-hub token revoke --id <id> --db /var/lib/heron/heron.db    # --all 吊销全部
```

  吊销立即生效，hub 运行中执行也一样。

## 调用方式

每个方法都是 `POST <hub>/heron.v1.AdminService/<方法>`，请求体与响应体都是 JSON，每个请求带两个头：`Authorization: Bearer <token>` 与 `Content-Type: application/json`。curl 加 jq 即可。

方法、字段与语义都写在 proto 注释里。用 `GetApiReference` 在线取回与 hub 同版本的全部 proto：

```sh
curl -fsS -H "Authorization: Bearer $HERON_TOKEN" -H 'Content-Type: application/json' \
  --data '{}' "$HERON_HUB/heron.v1.AdminService/GetApiReference" | jq '[.files[].path]'
```

基本约定：int64/uint64 在 JSON 里是字符串；时间是 Unix 秒；字段名是 proto 字段名的小驼峰；取默认值的字段在响应里省略，读不到就按默认值理解；出错时 HTTP 状态非 200，响应体是 `{"code": "...", "message": "..."}`。

## 给 AI agent 的技能文件

[proto/SKILL.md](../proto/SKILL.md) 是给 AI agent 用的 hub 技能文件：调用约定、预授权写入的完整流程、常用查询（节点、指标、流量、探测对比、费用、告警事件）的 curl 示例都在里面。面板「API token」页可下载与 hub 同版本的这份文件。

token 经环境变量交给 agent，不写进技能文件：

- `HERON_HUB`：hub 的对外地址，如 `https://heron.example.com`，不带末尾斜杠。
- `HERON_TOKEN`：API token。

Claude Code 写进 `~/.claude/settings.json` 的 env 段：

```json
{"env": {"HERON_HUB": "https://heron.example.com", "HERON_TOKEN": "heron_at_…"}}
```

其他 agent 在启动它的 shell 里 export 这两个变量。

## 油猴脚本：快速建节点

面板「API token」页可复制油猴脚本（适配 Tampermonkey / Violentmonkey）。粘贴进脚本管理器后，任意站点右下角出现悬浮按钮：在 IDC 页面看着价格与到期，一键把正在浏览的机器添加为监控节点，创建后直接给出安装凭据与安装命令。

- 从面板复制的版本已填好 hub 地址与 token；token 需勾选「创建节点」预授权。
- 设置存在脚本管理器里，之后可随时用油猴菜单命令修改。
