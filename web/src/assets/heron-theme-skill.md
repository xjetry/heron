---
name: heron-theme
description: 为 Heron 自托管探针的公开页构建第三方主题：主题是静态前端 ZIP 包，经 hub 提供的 SDK 读取公开节点、指标与探测数据。说明数据有哪些字段、SDK 怎么调用、包格式与限制、怎样构建与预览。用户要求做、改或排查 Heron 公开页主题时使用。
---

# Heron 公开页主题

主题替换 Heron 的公开状态页（访客在 `__HERON_HUB__/` 看到的页面）。它是**构建好的静态前端**：hub 只接收 ZIP 产物、不执行构建脚本；管理员在面板 `__HERON_HUB__/admin/` 的「主题」页上传或从 GitHub Release 安装，先预览再启用。

## 运行环境与硬约束

- 主题页面在 `sandbox="allow-scripts"` 的 iframe 里运行，**没有** `allow-same-origin`：脚本来源是不透明来源，读不到父页面；`localStorage`、`sessionStorage`、`IndexedDB` 一访问就抛 `SecurityError`，cookie 也不可用。**主题记不住访客的选择**（视图、筛选、明暗），需要的状态放在内存里，刷新即复位；能放进路由的只有下面 `navigate` 认的三种路径。
- 主题的每个响应都带这条 CSP：`sandbox allow-scripts; default-src 'none'; script-src http: https: 'unsafe-inline'; style-src http: https: 'unsafe-inline'; img-src http: https: data:; font-src http: https: data:; connect-src 'none'; worker-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'`。也就是：
  - **不能直接 `fetch` / XHR / WebSocket / EventSource**（`connect-src 'none'`）。所有数据只能经 SDK（下文）由 hub 的可信容器代为请求，只能调用六个只读公开方法，请求不带任何凭据。
  - 没有 Web Worker、内嵌 iframe、`<object>`、表单提交；没有放行 `unsafe-eval`，依赖 `eval` / `new Function` 的库（如运行时编译模板）要换成预编译构建；`blob:` 地址的脚本与图片不能用。
  - 脚本与样式可以来自包内、内联或任意 http(s) 地址，图片与字体还可以用 `data:`。第三方 CDN 属于作者自己的供应链，沙箱只隔离权限，不担保内容；能打进包里的依赖就打进包里。
- 不能用 WebAuthn、摄像头、麦克风、定位。
- 只能拿到**公开数据**：只有站长标为公开的节点，且不含主机名、IP、内核、agent 版本等字段。不要设计依赖这些字段的界面。
- 站点的公开页总闸关闭时，主题与公开数据一起不可用。

## 最小主题

包根两个文件：

`theme.json`

```json
{ "id": "my-theme", "name": "My theme", "version": "1.0.0", "sdk": 1 }
```

`index.html`

```html
<!doctype html>
<html lang="zh">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Heron</title>
<h1 id="title"></h1>
<ul id="nodes"></ul>
<script type="module">
import { getSite, getSnapshot, onRoute } from '/_heron/theme-sdk.js';

const site = await getSite();
document.querySelector('#title').textContent = site.title || 'Heron';
async function render() {
  const snap = await getSnapshot();
  document.querySelector('#nodes').replaceChildren(...(snap.nodes ?? []).map((n) => {
    const li = document.createElement('li');
    li.textContent = `${n.name} ${n.online ? '在线' : '离线'}`;
    return li;
  }));
}
await render();
setInterval(render, 2000);
await onRoute((path) => console.log('当前路由', path));
</script>
</html>
```

SDK 模块的地址固定是 `/_heron/theme-sdk.js`（由 hub 提供，不要打进包里，也不要改写成相对路径）。

## SDK

```js
import { getSite, getSnapshot, queryMetrics, queryProbes, listProbeComparisonNodes, queryProbeComparison, navigate, onRoute } from '/_heron/theme-sdk.js';
```

| 函数 | 参数 | 返回（Promise） | 对应公开接口 |
|---|---|---|---|
| `getSite()` | 无 | `PublicSite` | `PublicService.GetSite` |
| `getSnapshot()` | 无 | `PublicSnapshot` | `PublicService.GetSnapshot` |
| `queryMetrics(args)` | `{ nodeId, from, to, maxPoints }` | `QueryMetricsResponse` | `PublicService.QueryMetrics` |
| `queryProbes(args)` | `{ nodeId, from, to, maxPoints }` | `QueryProbesResponse` | `PublicService.QueryProbes` |
| `listProbeComparisonNodes(args)` | `{ taskId }` | `ListProbeComparisonNodesResponse` | `PublicService.ListProbeComparisonNodes` |
| `queryProbeComparison(args)` | `{ taskId, nodeIds, from, to, maxPoints }` | `QueryProbeComparisonResponse` | `PublicService.QueryProbeComparison` |
| `navigate(path, replace?)` | `/`、`/nodes/<正整数>`、`/probes/<正整数>`（可带尾部斜杠）；`replace` 为真时替换历史记录 | 无 | 更新公开页地址并通知路由订阅者 |
| `onRoute(listener)` | `(path) => void` | 取消订阅的函数 | 订阅后立即收到当前路径 |

调用约定：

- 返回的是响应数据本身（已解析的 JSON 对象），不是 `Response`。失败时 Promise 拒绝，错误对象带 `message` 与 `code`（Connect 错误码，如 `not_found`、`resource_exhausted`、`failed_precondition`、`invalid_argument`）。
- 单次调用 15 秒没有结果即以「公开接口调用超时」拒绝。
- 容器对消息通道限流：同时在飞的调用至多 8 个，每秒至多 30 条消息，单条消息序列化后至多 16384 个字符。**超出的消息被静默丢弃**，对应调用只会在 15 秒后超时——不要并发刷请求，串行或小批量即可。
- SDK 一律经 POST 请求，不经浏览器 HTTP 缓存；同一节点同一窗口的历史在主题里自己缓存，别每次重绘都重新查。
- 公开接口另有按来源 IP 的限流（桶 60、每秒补 10），超出返回 `resource_exhausted`。实时状态约每 5 秒轮询一次 `getSnapshot()` 就够（agent 上报间隔见 `reportIntervalMs`，再快也拿不到更新的数据）；历史查询在用户换窗口或换节点时才发。
- 路由：主题文档实际放在带摘要的资源路径下，**不能**把 iframe 的 `location.pathname` 当作站点路由；用 `onRoute` 取当前路径、用 `navigate` 跳转。访客打开 `/nodes/3` 这样的深链接时，`onRoute` 第一次回调就给出 `/nodes/3`。

## JSON 口径

- 字段名是 lowerCamelCase（`lastSeenAt`、`reportIntervalMs`）。
- `int64` / `uint64` 是**字符串**（`"id": "12"`、`"now": "1791500000"`、字节数）；`uint32`、`int32`、`double` 是数字。请求参数里的 `nodeId`、`taskId`、`nodeIds`、`from`、`to` 也按字符串传最稳。
- 枚举是名字字符串，如 `"BILLING_CYCLE_MONTHLY"`、`"PROBE_KIND_TCP"`。
- 值为默认值（0、空串、false、空数组）的普通字段**不出现**在 JSON 里，读的时候要给默认值（`snap.nodes ?? []`）。
- 标为「可缺失」的字段缺失表示**没有读数**，与读数为 0 是两件事：缺失画「–」或断线，0 照常显示。

## 数据

### PublicSite（`getSite()`）

| 字段 | 说明 |
|---|---|
| `title` | 站点标题；空表示站长没设（主题自定） |
| `theme` | `auto`、`light` 或 `dark`：站长选的明暗；`auto` 跟随访客系统 |
| `accentColor` | 主色 `#rrggbb`；空表示主题自定 |
| `logo` | `data:image/…;base64,…` 或空 |
| `customCss` | 站长为内置公开页写的附加 CSS；主题可自行决定是否套用（套用时放在自己的样式之后） |

### PublicSnapshot（`getSnapshot()`）

| 字段 | 说明 |
|---|---|
| `now` | hub 墙钟（Unix 秒，字符串）。显示「多久之前」用它减 `lastSeenAt`，不要用访客本机时钟 |
| `reportIntervalMs` | agent 上报间隔（毫秒） |
| `nodes` | 公开节点，已按站长定的顺序排好，直接按数组顺序显示 |
| `tags` | 所有公开节点标签的并集，已按 hub 的折叠规则排好序；做标签筛选时照这个顺序列 |

### PublicNode（`nodes` 的元素）

| 字段 | 说明 |
|---|---|
| `id` | 节点 id（字符串），用于 `queryMetrics` 与 `/nodes/<id>` |
| `name` | 名称 |
| `online` | hub 判定的在线 |
| `lastSeenAt` | 最近一次上报（Unix 秒）；**可缺失**：从未上报 |
| `sortOrder` | 站长定的顺序（`nodes` 已按它排好） |
| `maintenance` | 站长设为维护中 |
| `rented` | 站长设为已出租：不会有上报，离线是预期 |
| `country` | 国家 / 地区代码（如 `JP`）；空表示未知 |
| `tags` | 标签数组 |
| `publicRemark` | 站长写给访客的一行备注 |
| `facts` | 主机信息，**可缺失**（从未上报）：`os`、`arch`、`virtualization`、`cpuModel`、`cpuCores`，以及双栈出口 `network`（见下） |
| `metrics` | 最近一次读数，**可缺失**（从未上报；离线节点保留最后一次读数），见下表 |
| `traffic` | hub 累计的流量，见下表 |
| `billing` | 计费与到期，**可缺失**（站长没填）：`price`（十进制文本）、`currency`（ISO 4217）、`billingCycle`（`BILLING_CYCLE_MONTHLY` / `QUARTERLY` / `SEMIANNUAL` / `YEARLY` / `BIENNIAL` / `TRIENNIAL` / `QUINQUENNIAL`）、`expiresOn`（`YYYY-MM-DD`）、`daysLeft`（到期剩余天数，负数为已过期；可缺失） |

`facts.network` 只给每个地址族的显示状态，不给地址：`network.ipv4.state` 与 `network.ipv6.state` 取自 hub 的显示值（与管理端 `Node.network` 同一判定），`ADDRESS_DETECTION_STATE_AVAILABLE` 有三个来源——agent 探测到该族公网地址、站长手填、hub 看到它以该族公网地址上报；`ADDRESS_DETECTION_STATE_UNSUPPORTED`（本机没有该族可用的接口地址或路由）、`ADDRESS_DETECTION_STATE_FAILED`（这一轮探测没成功，不能据此判断有没有出口）保留原释义。`network` 缺失表示所有适用显示来源都没有结果，不要据此反推 agent 版本；某个族缺失同理（还没探测过、没有手填，hub 也没记到该族公网来源）。内置页只为 `AVAILABLE` 的族画「IPv4」「IPv6」小标。

`name`、`publicRemark`、`tags` 与 `facts` 里的 `os`、`arch`、`virtualization`、`cpuModel` 是站长或节点上的 agent 写入的文本：hub 只保证它们不含控制字符，不做 HTML 转义，`<`、`>` 与引号原样下发。请按文本渲染（`textContent`、框架的文本插值），不要拼进 `innerHTML` 或 `insertAdjacentHTML`。沙箱让主题脚本拿不到面板会话，但一台节点的 token 持有者仍能借 `innerHTML` 改写你给访客看的整个页面。

内置公开页的五态判定（建议主题沿用，计数才与内置页一致）：`rented` 为真 → 已出租；否则 `maintenance` 为真 → 维护中；否则没有 `lastSeenAt` → 从未上报；否则 `online` 为真 → 在线，为假 → 离线。

### metrics（PublicMetrics，每一项都可缺失）

| 字段 | 单位 / 说明 |
|---|---|
| `cpuPct` | CPU 使用率，百分比 |
| `cpuStealPct`、`cpuIowaitPct` | steal、iowait 占比，百分比 |
| `load1`、`load5`、`load15` | 负载 |
| `load1PerCore` | 按核负载 |
| `memUsed`、`memTotal` | 内存字节（字符串）；使用率 = memUsed / memTotal。`memTotal` 缺失或为 `"0"`、`memUsed` 缺失时没有使用率，按无读数显示，不要算成 0%（内置页同此口径） |
| `swapUsed`、`swapTotal` | 交换字节；使用率与缺失口径同内存 |
| `diskUsed`、`diskTotal` | 磁盘字节；使用率与缺失口径同内存 |
| `netRxBps`、`netTxBps` | 实时下行 / 上行速率，字节/秒 |
| `netRxTotal`、`netTxTotal` | 网卡累计字节（内核计数器） |
| `diskReadBps`、`diskWriteBps` | 磁盘读写速率，字节/秒 |
| `tcpConns`、`udpConns`、`procs` | 连接数、进程数 |
| `uptimeS` | 运行秒数 |

### traffic（Traffic）

| 字段 | 说明 |
|---|---|
| `periodRx`、`periodTx` | 本周期收、发字节 |
| `totalRx`、`totalTx` | 自首次上报累计字节 |
| `periodStart`、`nextResetAt` | 本周期起点、下次重置（Unix 秒） |
| `resetDay` | 每月重置日 1–28 |
| `quotaBytes` | 周期配额字节；缺失或 0 表示未设配额 |
| `quotaMode` | 配额口径：`TRAFFIC_QUOTA_MODE_SUM`（收+发）、`RX`、`TX`、`MAX`（取大者） |
| `quotaUsedBytes` | 按配额口径算的本周期用量（没设配额也有值） |
| `quotaUsedPct` | 用量占配额的百分比；没设配额时缺失 |

### 指标历史（`queryMetrics`）

请求 `{ nodeId, from, to, maxPoints }`：窗口 `[from, to)`，Unix 秒，跨度最长 400 天；`maxPoints` 省略或 0 取 720，最大 2000，hub 据此选步长。

响应：

- `stepS`：每个点覆盖的秒数；画点间隔以它为准。`level` 是选定的聚合级别（`1m`、`5m`、`1h`）。
- `ts`：各点起始（Unix 秒）。**只含有数据的点**，中间缺的时间段就是没有数据，画成断开。
- `series`：每个指标一条，按固定顺序；新增指标只会追加在末尾，按 `name` 取，不要按下标取。`unit` 为 `percent`、`bytes`、`bytes/s`、`count` 或空（负载类）。`samples` 与 `ts` 一一对应，每个样本有 `n`（这一点里的样本数）与下面三者之一或两者：`mean`（均值）、`max`（采样峰值）、`sum`（字节总和）。`n` 为 0 时这一点该指标没有读数，留空洞。

| name | unit | 样本里的值 |
|---|---|---|
| `cpu` | percent | mean、max |
| `mem_used` | bytes | mean、max |
| `swap_used` | bytes | mean |
| `disk_used` | bytes | mean |
| `load1` | （空） | mean |
| `tcp` | count | mean |
| `udp` | count | mean |
| `procs` | count | mean |
| `rx_bytes` | bytes | sum |
| `tx_bytes` | bytes | sum |
| `memory_used_pct` | percent | mean |
| `disk_used_pct` | percent | mean |
| `net_rx_bps` | bytes/s | mean、max |
| `net_tx_bps` | bytes/s | mean、max |
| `disk_read_bps` | bytes/s | mean、max |
| `disk_write_bps` | bytes/s | mean、max |
| `cpu_steal_pct` | percent | mean、max |
| `cpu_iowait_pct` | percent | mean、max |
| `load1_per_core` | （空） | mean |
| `tcp_active_opens_ps` | count/s | mean、max |
| `tcp_passive_opens_ps` | count/s | mean、max |
| `tcp_out_segs_ps` | count/s | mean |
| `tcp_retrans_segs_ps` | count/s | mean |

- 平均网速 = `rx_bytes` / `tx_bytes` 的 `sum / stepS`（字节/秒）；采样峰值取 `net_rx_bps` / `net_tx_bps` 的 `max`。
- 重传率不是一条序列，取同一个点上 `tcp_retrans_segs_ps` 与 `tcp_out_segs_ps` 的 `mean × n` 之比再乘 100，分母为 0 或任一侧 n=0 时没有重传率。
- 最后一个点可能是未完成的桶。
- `coverage`、`coverageSummary` 是上报覆盖率的统计，不是在线率；不做覆盖率展示可以忽略。要展示时：`coverage` 与 `ts` 一一对应，`minutes` 是留存的上报分钟，`observed` 是 hub 观测到的分钟，`observedReported` 是二者交集，三者各自可缺失；纯观测点的所有指标 `n` 为 0，按空值画。`coverageSummary` 汇总整个请求窗口、与 `maxPoints` 无关：覆盖率 = `observedReportedMinutes / observedMinutes`，未知分钟 = `eligibleMinutes − observedMinutes`；`coverageStart` 缺失表示还没有覆盖记录。

### 探测历史（`queryProbes`）

请求同 `queryMetrics`。响应 `series` 每个探测任务一条：`taskId`、`kind`（`PROBE_KIND_ICMP` / `TCP` / `HTTP` / `DNS`；不标注时缺失）、`target`（目标；不标注时为空，用 `taskId` 称呼）、`samples`。样本 `{ ts, sent, lost, errors, rttMeanUs, rttMinUs, rttMaxUs }`：丢包率 = `lost / sent`（`errors` 是本地错误，不计入丢包）；RTT 单位微秒，只有有成功探测的点才带。没有样本的时间段就是没有结果，不是 0。按服务端给的顺序显示，不要按 id 重排。

### 跨节点探测对比（`/probes/<taskId>`）

1. `listProbeComparisonNodes({ taskId })` → `{ kind, target, nodeIds, maxNodesPerQuery }`：当前分配了该任务的公开节点（已排序）与每次查询最多可带的节点数。`maxNodesPerQuery` 恒为正，收到 0 当作协议错误处理。任务不存在、或没分配给任何公开节点，都返回 `not_found`。
2. 按 `maxNodesPerQuery` 把 `nodeIds` 分块，逐块 `queryProbeComparison({ taskId, nodeIds, from, to, maxPoints })` → `{ stepS, level, series: [{ nodeId, samples }], unavailableNodeIds }`。`series` 与请求的 `nodeIds` 同序，没有样本的节点也在（`samples` 为空）；`unavailableNodeIds` 里的节点不画。同时在飞的块不超过两块；某块失败就显示错误让用户重试，不要自动循环；用户改了窗口或任务就丢掉还没返回的块。

历史查询超过额度时返回 `failed_precondition`，`message` 里带建议（缩小窗口、增大 `maxPoints` 或稍后再试），把它显示给用户即可。

## 包格式与清单

- ZIP 包根必须直接是 `index.html` 和 `theme.json`，不能再套一层 `dist/` 或仓库目录。
- 包内资源用相对地址（`./assets/app.js`）。用 Vite 构建时设 `base: './'`；不要写成 `/assets/...`，也不要用 `<base>` 改基址。
- `theme.json` 只认这几个字段，未知字段会被拒绝：

| 字段 | 约束 |
|---|---|
| `id` | 必填，`[a-z0-9-]{1,32}`，不能是 `builtin`；同一主题的新版本 `id` 必须不变 |
| `name`、`version` | 必填，去掉首尾空白后非空，最多 64 字符，无控制字符；`version` 只是展示文字 |
| `sdk` | 必填，`1` |
| `preview` | 可选，包内 PNG / JPEG / WebP 预览图路径，内容须与扩展名一致 |

- 限制：ZIP 最多 8 MiB、2000 个条目；单文件展开后最多 16 MiB，总计 64 MiB；只接受 store 与 deflate 压缩、普通文件和目录。绝对路径、`..`、反斜杠、空路径段、控制字符、以 `.` 开头的路径段（含 `.DS_Store`、`__MACOSX/._*`）、重复路径与链接都会让整包被拒。
- 打包：在构建产物目录里执行 `zip -r -X ../theme.zip .`。不要用 macOS Finder 的「压缩」（会带上 `__MACOSX`）。
- 从 GitHub 安装时，要发布 Release 并把这个 ZIP 作为资产上传；GitHub 自动生成的源码压缩包不能安装。

## 开发与验证

SDK 依赖 hub 可信容器的消息通道，框架开发服务器（`vite dev`）直接打开主题时 SDK 不会连上。建议：

1. **先拿真实数据样本**：公开接口支持 GET，可以直接取到当前 hub 的数据形状：

   ```sh
   HUB=__HERON_HUB__
   curl -fsS "$HUB/heron.v1.PublicService/GetSnapshot?connect=v1&encoding=json&message=%7B%7D" | jq .
   curl -fsS "$HUB/heron.v1.PublicService/GetSite?connect=v1&encoding=json&message=%7B%7D" | jq .
   # 指标历史：把 nodeId 换成快照里的某个 id，from / to 是 Unix 秒
   msg=$(jq -cn --arg id 1 --argjson to "$(date +%s)" '{nodeId:$id, from:(($to - 3600)|tostring), to:($to|tostring), maxPoints:120}')
   curl -fsS -G "$HUB/heron.v1.PublicService/QueryMetrics" --data-urlencode connect=v1 --data-urlencode encoding=json --data-urlencode "message=$msg" | jq '.series[].name'
   ```

   公开页总闸关闭时这些接口返回 `not_found`。
2. **本地开发用替身**：写一个与 SDK 同名导出的开发用模块（在开发构建里替换 `/_heron/theme-sdk.js` 的导入），用上面存下的 JSON 返回数据；`navigate` / `onRoute` 用 `history` 简单模拟。hub 不下发 CORS 允许头，开发服务器里直接跨域 `fetch` hub 读不到响应，要实时数据就用开发服务器的代理（如 Vite `server.proxy` 把 `/heron.v1.PublicService` 转到 hub）。生产构建里必须仍从 `/_heron/theme-sdk.js` 导入。
3. **在 hub 里预览**：在面板「主题」页上传 ZIP，用「预览」打开（预览不影响访客看到的页面）。逐项检查：相对资源都加载到了；`/`、`/nodes/<id>`、`/probes/<taskId>` 深链接与前进后退正常；窄屏（约 375px 宽）没有横向滚动；明暗两种 `theme` 都能看；从未上报、离线、维护中、已出租、没有读数的节点都能正常显示。
4. 确认无误后在「主题」页启用。安装新版本不会自动启用，也不会覆盖当前版本；启用后可以切回上一个版本或内置公开页。
