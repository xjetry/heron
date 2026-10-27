import { type Page, type Route } from "@playwright/test";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { AdminService } from "../src/gen/heron/v1/admin_pb";
import { PublicService, type PublicNodeSchema } from "../src/gen/heron/v1/public_pb";
import { AddressDetectionState, BillingCycle } from "../src/gen/heron/v1/types_pb";
import { boxOf, expect, fulfillRpc, login, must, rpc, rpcRoute, test } from "./fixtures";
import { POLL_MS } from "../src/lib/poll";

// 公开页只消费 PublicService 的投影字段，不引入管理信息或无来源的统计；下面的禁用词在可见文本里出现即失败。IP 地址不在这里查：公开快照没有地址字段
// （PublicAddressDetection 只有 state，由 hub 的投影与 internal/hub/api 的测试钉住）；「IPv4」「IPv6」地址族标记是允许的。
const FORBIDDEN = ["可用率", "在线率", "SLA", "宕机", "不可达", "主机名", "内核", "agent 版本", "事件", "告警", "刷新"];
const AVAILABLE = { state: AddressDetectionState.AVAILABLE };

// e2e 的 hub 是每次新建的库，公开页总闸默认关闭（关闸时连 SPA 的静态资源都是 404），先登录打开；RPC 再由 route 拦截。
async function setPublicEnabled(page: Page, enabled: boolean) {
  await page.goto("/admin/login");
  await login(page);
  must(await rpc(page, AdminService.method.updateSettings, { settings: { publicEnabled: enabled } }));
}

test("公开总览：状态墙、详情、卡片、列表视图、手机布局与数据边界", async ({ page }, testInfo) => {
  await setPublicEnabled(page, true);
  const now = Math.floor(Date.now() / 1000);
  const minute = now - now % 60;
  const nodes: MessageInitShape<typeof PublicNodeSchema>[] = [
    { id: 1n, name: "tokyo-core", country: "JP", online: true, tags: ["机房"], lastSeenAt: BigInt(now - 2), facts: { os: "Debian 13", arch: "amd64", virtualization: "kvm", cpuModel: "EPYC", cpuCores: 4, network: { ipv4: AVAILABLE, ipv6: AVAILABLE } }, metrics: { cpuPct: 72, memUsed: BigInt(3 * 1024 ** 3), memTotal: BigInt(4 * 1024 ** 3), diskUsed: BigInt(8 * 1024 ** 3), diskTotal: BigInt(32 * 1024 ** 3), uptimeS: 720000n, load1: 0.3, load5: 0.2, load15: 0.1, netRxBps: BigInt(128 * 1024), netTxBps: BigInt(32 * 1024), tcpConns: 20, udpConns: 2, procs: 120 }, traffic: { totalRx: BigInt(1.5 * 1024 ** 4), totalTx: BigInt(640 * 1024 ** 3), periodRx: BigInt(12 * 1024 ** 3), periodTx: BigInt(8 * 1024 ** 3) }, billing: { price: "12", currency: "USD", billingCycle: BillingCycle.MONTHLY, expiresOn: "2027-10-01", daysLeft: 10 } },
    { id: 2n, name: "tokyo-home", country: "JP", online: false, tags: ["家宽"], lastSeenAt: BigInt(now - 7200), metrics: { cpuPct: 5 } },
    { id: 3n, name: "hk-edge", country: "HK", online: true, tags: ["机房"], lastSeenAt: BigInt(now - 1), maintenance: true, metrics: { cpuPct: 10, memUsed: BigInt(1024 ** 3), memTotal: BigInt(4 * 1024 ** 3) } },
    { id: 4n, name: "香港家宽-超长节点名称用于验证窄屏布局与完整可访问名称", country: "HK", online: true, tags: ["家宽"], lastSeenAt: BigInt(now - 1), facts: { network: { ipv4: AVAILABLE, ipv6: { state: AddressDetectionState.FAILED } } }, metrics: { cpuPct: 95, memUsed: BigInt(Math.floor(3.9 * 1024 ** 3)), memTotal: BigInt(4 * 1024 ** 3) } },
    { id: 5n, name: "等待首次接入", country: "", online: false, tags: [] },
  ];
  await page.route(rpcRoute(PublicService.method.getSite), (route) => fulfillRpc(route, PublicService.method.getSite, () => ({ title: "Heron · 基础设施", theme: "dark", adminPath: "/admin/" })));
  await page.route(rpcRoute(PublicService.method.getSnapshot), (route) => fulfillRpc(route, PublicService.method.getSnapshot, () => ({ now: BigInt(now), nodes, tags: ["家宽", "机房"] })));
  await page.route(rpcRoute(PublicService.method.queryMetrics), (route) => fulfillRpc(route, PublicService.method.queryMetrics, (request) => ({
    level: "1m", stepS: 60, ts: [BigInt(minute - 120), BigInt(minute - 60)],
    series: request.nodeId === 1n ? [{ name: "rx_bytes", unit: "bytes", samples: [{ n: 1, sum: 6000 }, { n: 1, sum: 3000 }] }, { name: "tx_bytes", unit: "bytes", samples: [{ n: 1, sum: 600 }, { n: 1, sum: 300 }] }] : [],
  })));
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.setViewportSize({ width: 1440, height: 960 });
  await page.goto("/");

  // 顶栏只有标题、实时说明、GitHub 仓库、明暗切换、登录；没有导航。
  const header = page.locator("header.public-header");
  await expect(header.getByRole("link")).toHaveCount(3);
  await expect(header.getByRole("link").nth(0)).toHaveText("Heron · 基础设施");
  await expect(header.getByRole("link").nth(1)).toHaveAccessibleName("GitHub 仓库");
  await expect(header.getByRole("link").nth(1)).toHaveAttribute("href", "https://github.com/xjetry/heron");
  await expect(header.getByRole("link").nth(2)).toHaveText("登录");
  await expect(header.getByText(`实时 · 每 ${POLL_MS / 1000} 秒`)).toBeVisible();
  await expect(page.getByRole("navigation")).toHaveCount(0);
  await expect(page.locator("footer.site-footer")).toHaveCount(0);

  // 汇总与分组：维护中不算在线；组按在线数降序，未知最后。
  await expect(page.getByText("2 / 5 在线")).toBeVisible();
  // 搜索框：空时显示「输入 / 搜索」提示；/ 与 ⌘K / Ctrl+K 聚焦，输入后提示消失。
  const searchBox = page.getByRole("searchbox", { name: "搜索节点" });
  const searchHint = page.locator(".search-field .search-hint");
  await expect(searchHint).toHaveText("输入 / 搜索名称、标签、备注");
  await page.keyboard.press("/");
  await expect(searchBox).toBeFocused();
  await page.keyboard.type("tokyo");
  await expect(searchHint).toHaveCount(0);
  await searchBox.fill("");
  await searchBox.blur();
  await page.keyboard.press("ControlOrMeta+k");
  await expect(searchBox).toBeFocused();
  await searchBox.blur();
  // 没选过时默认卡片；切到状态墙后记在浏览器里，刷新沿用。
  const views = page.getByRole("group", { name: "视图" });
  await expect(views.getByRole("button", { name: "卡片" })).toHaveAttribute("aria-pressed", "true");
  await expect(page.getByRole("article")).toHaveCount(3);
  await views.getByRole("button", { name: "状态墙" }).click();
  await page.reload();
  await expect(views.getByRole("button", { name: "状态墙" })).toHaveAttribute("aria-pressed", "true");
  // 日本与香港都是 1 / 2：同在线数、同总数时按代码排，HK 在 JP 前。
  await expect(page.locator(".wall-group > summary")).toHaveText(["香港 · 1 / 2 在线", "日本 · 1 / 2 在线", "未知 · 0 / 1 在线"]);
  await expect(page.locator(".tile[data-status='offline']").getByText("离线 · 2 小时前")).toBeVisible();
  // 分组切到标签：节点进它的每个标签组，同计数按 hub 的标签顺序，无标签最后；再切回地区。
  // 分组是自绘下拉：触发按钮写当前值，点开是自绘列表，不弹系统菜单。
  const grouping = page.getByRole("button", { name: /^分组 / });
  await expect(grouping).toHaveAccessibleName("分组 地区");
  await grouping.click();
  await page.getByRole("listbox", { name: "分组" }).getByRole("option", { name: "标签", exact: true }).click();
  await expect(page.getByRole("listbox")).toHaveCount(0);
  await expect(page.locator(".wall-group > summary")).toHaveText(["家宽 · 1 / 2 在线", "机房 · 1 / 2 在线", "无标签 · 0 / 1 在线"]);
  // 分组选择记在浏览器里，刷新沿用。
  await page.reload();
  await expect(grouping).toHaveAccessibleName("分组 标签");
  await expect(page.locator(".wall-group > summary")).toHaveText(["家宽 · 1 / 2 在线", "机房 · 1 / 2 在线", "无标签 · 0 / 1 在线"]);
  // 键盘：下键打开并聚焦当前项，上键移动，Enter 选中、收起并把焦点还给触发按钮。
  await grouping.press("ArrowDown");
  await expect(page.getByRole("option", { name: "标签", exact: true })).toBeFocused();
  await page.keyboard.press("ArrowUp");
  await expect(page.getByRole("option", { name: "地区", exact: true })).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("listbox")).toHaveCount(0);
  await expect(grouping).toBeFocused();
  await expect(grouping).toHaveAccessibleName("分组 地区");
  await expect(page.locator(".wall-group > summary")).toHaveText(["香港 · 1 / 2 在线", "日本 · 1 / 2 在线", "未知 · 0 / 1 在线"]);
  // Tab 收起列表，焦点落到下拉之后的控件，与原生 select 一致。
  await grouping.press("ArrowDown");
  await expect(page.getByRole("option", { name: "地区", exact: true })).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(page.getByRole("listbox")).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^着色依据 / })).toBeFocused();

  // 详情面板默认选中第一个节点；点另一个方块只切换，不导航。
  const panel = page.getByRole("complementary", { name: "节点详情" });
  await expect(panel.getByRole("heading", { name: "hk-edge" })).toBeVisible();
  await page.getByRole("link", { name: "tokyo-core", exact: true }).click();
  await expect(panel.getByRole("heading", { name: "tokyo-core" })).toBeVisible();
  await expect(panel.getByRole("group", { name: "公网出口" })).toHaveText("IPv4IPv6");
  await expect(panel.getByRole("img", { name: "最近 1 小时网络速率", exact: true })).toBeVisible();
  await page.getByRole("link", { name: "hk-edge" }).click();
  await expect(page).toHaveURL(/\/$/);
  await expect(panel.getByRole("heading", { name: "hk-edge" })).toBeVisible();
  await expect(panel.getByText("维护中 · 最近上报 刚刚")).toBeVisible();
  await expect(panel.getByRole("img", { name: "最近 1 小时网络速率：无读数", exact: true })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(1440);
  await page.screenshot({ path: testInfo.outputPath("wall-dark.png"), fullPage: true });

  // 筛选：地区入口（默认单选，点一个只看它，再点回到全部）+ 节点状态。离线是在线与已出租以外的全部：维护中的 hk-edge 虽仍在
  // 上报，也计入离线，所以不加筛选时是 5 = 2 + 3；快照里没有已出租节点，不出已出租那一档。三档计数跟随地区筛选，不跟随状态筛选本身。
  const statuses = page.getByRole("group", { name: "节点状态" });
  const status = (name: "全部" | "在线" | "离线") => statuses.getByRole("button", { name: new RegExp(`^${name} \\d+$`) });
  await expect(statuses.getByRole("button")).toHaveText(["全部 5", "在线 2", "离线 3"]);
  const regionFacet = page.getByRole("button", { name: /^地区 / });
  const japan = page.getByRole("group", { name: "地区" }).getByRole("button", { name: /^日本 \d+$/ });
  await regionFacet.click();
  await japan.click();
  await expect(regionFacet).toHaveAccessibleName("地区 日本");
  await expect(page.locator(".summary-count")).toHaveText("1 / 2 在线");
  await expect(statuses.getByRole("button")).toHaveText(["全部 2", "在线 1", "离线 1"]);
  await status("在线").click();
  await expect(page.locator(".tile")).toHaveCount(1);
  await expect(page.locator(".tile-name")).toHaveText(["tokyo-core"]);
  await status("离线").click();
  await expect(page.locator(".tile-name")).toHaveText(["tokyo-home"]);
  await expect(statuses.getByRole("button")).toHaveText(["全部 2", "在线 1", "离线 1"]);
  await japan.click();
  await expect(regionFacet).toHaveAccessibleName("地区 全部");
  await expect(page.locator(".tile")).toHaveCount(3);
  await status("全部").click();
  await regionFacet.click();

  // 卡片视图：只有在线与维护中出卡片，离线折叠；卡片有费用与到期。
  await views.getByRole("button", { name: "卡片" }).click();
  await expect(page.getByRole("article")).toHaveCount(3);
  // 排序同样是自绘下拉，列表里当前项打勾。
  const sorting = page.getByRole("button", { name: /^排序 / });
  await sorting.click();
  await expect(page.getByRole("listbox", { name: "排序" }).getByRole("option")).toHaveText(["默认", "到期", "CPU", "流量"]);
  await expect(page.getByRole("option", { name: "默认" })).toHaveAttribute("aria-selected", "true");
  await page.screenshot({ path: testInfo.outputPath("cards-sort-open.png") });
  await page.keyboard.press("Escape");
  await expect(page.getByRole("listbox")).toHaveCount(0);
  const card = page.getByRole("article", { name: "tokyo-core" });
  await expect(card.getByText("US$12 / 月")).toBeVisible();
  await expect(card.locator(".expiry")).toHaveAttribute("data-level", "attention");
  await expect(card.locator(".expiry")).toContainText("剩 10 天");
  // 内存与磁盘在用量 / 总量后附百分比；底部本周期之后是累计流量（total_rx / total_tx，不是本周期的 12 GiB / 8.0 GiB）。
  // CPU 在条下附总核心数（facts.cpu_cores）。
  await expect(card.getByRole("meter", { name: "内存 3.0 GiB / 4.0 GiB (75%)" })).toBeVisible();
  await expect(card.getByRole("meter", { name: "磁盘 8.0 GiB / 32 GiB (25%)" })).toBeVisible();
  await expect(card.locator(".node-meter > .num:not(.node-meter-detail)")).toHaveText(["72%", "75%", "25%"]);
  await expect(card.locator(".node-meter-detail")).toHaveText(["4 核", "3.0 GiB / 4.0 GiB", "8.0 GiB / 32 GiB"]);
  // 用量 / 总量在条下另起一行，百分比列定宽：三条等长，读数再长也不会把条挤短。
  const barWidths = await card.locator(".node-meter .bar").evaluateAll((bars) => bars.map((bar) => Math.round(bar.getBoundingClientRect().width)));
  expect(new Set(barWidths).size, barWidths.join()).toBe(1);
  expect(barWidths[0]).toBeGreaterThan(100);
  await expect(card.locator(".node-card-foot > dt")).toHaveText(["本周期", "累计流量", "费用", "到期", "运行时长"]);
  await expect(card.locator(".node-card-foot > dd").nth(1)).toHaveText("↓ 1.5 TiB ↑ 640 GiB");
  // 运行时长是卡片底部的最后一行，系统信息行不再带它。
  await expect(card.locator(".node-card-foot > dd").last()).toHaveText("8d 8h");
  await expect(card.locator(".node-card-foot > dd").last()).toBeVisible();
  await expect(card.locator(".node-card-meta")).not.toContainText("8d 8h");
  // 双栈标记在系统信息行尾，只画有公网出口的族（探测失败的 IPv6 不画）；标题行不因它变挤，短名称不截断。
  await expect(card.locator(".node-card-meta").getByRole("group", { name: "公网出口" })).toHaveText("IPv4IPv6");
  expect(await card.getByRole("link", { name: "tokyo-core" }).evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);
  const longCard = page.getByRole("article", { name: nodes[3].name });
  await expect(longCard.locator(".node-card-meta").getByRole("group", { name: "公网出口" })).toHaveText("IPv4");
  // 离线与从未上报默认展开；访客收起后，轮询带来新数据也不会再展开。
  const folded = page.locator("details.folded-nodes");
  await expect(folded.locator("summary")).toHaveText("离线与从未上报 · 2");
  await expect(folded).toHaveAttribute("open");
  await expect(folded.getByRole("row")).toHaveCount(3);
  await expect(folded.getByRole("link", { name: "tokyo-home" })).toBeVisible();
  // 标题前的箭头随展开状态转向：flex 标题没有浏览器自带的三角，靠它看出能收起。
  const chevron = () => folded.locator("summary").evaluate((el) => getComputedStyle(el, "::before").transform);
  const expanded = await chevron();
  expect(expanded).not.toBe("none");
  await folded.locator("summary").click();
  await expect(folded).not.toHaveAttribute("open");
  expect(await chevron()).not.toBe(expanded);
  const withExtra = (route: Route) => fulfillRpc(route, PublicService.method.getSnapshot, () => ({ now: BigInt(now), nodes: [...nodes, { id: 6n, name: "新离线节点", country: "", online: false, tags: [], lastSeenAt: BigInt(now - 600) }], tags: ["家宽", "机房"] }));
  // 换上与撤掉路由后的变化都要等下一次快照轮询：一个周期加请求耗时可能超过断言默认的 5 秒，等两个周期。
  await page.route(rpcRoute(PublicService.method.getSnapshot), withExtra);
  await expect(folded.locator("summary")).toHaveText("离线与从未上报 · 3", { timeout: 2 * POLL_MS });
  await expect(folded).not.toHaveAttribute("open");
  await page.unroute(rpcRoute(PublicService.method.getSnapshot), withExtra);
  await expect(folded.locator("summary")).toHaveText("离线与从未上报 · 2", { timeout: 2 * POLL_MS });
  await folded.locator("summary").click();
  await expect(folded.getByRole("link", { name: "tokyo-home" })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("cards-dark.png"), fullPage: true });

  // 浅色：访客切换压过站点的 dark。
  await page.getByRole("button", { name: "明暗切换" }).click();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  await page.screenshot({ path: testInfo.outputPath("cards-light.png"), fullPage: true });

  // 数据边界：页面文字里没有禁止字段。
  const text = await page.evaluate(() => document.body.innerText);
  for (const word of FORBIDDEN) expect(text, word).not.toContain(word);

  // 列表视图：与管理端总览同一组列（含运行时长）另加到期；全部节点一张表，离线与从未上报不折叠；同样不出现禁止字段。
  await views.getByRole("button", { name: "列表" }).click();
  const list = page.getByRole("region", { name: "节点列表" });
  await expect(list.getByRole("columnheader")).toHaveText(["状态", "节点", "CPU", "内存", "磁盘", "负载", "网络", "本周期", "运行时长", "到期", "最近上报"]);
  await expect(list.locator("tbody tr")).toHaveCount(5);
  const coreRow = list.getByRole("row", { name: "tokyo-core", exact: true });
  await expect(coreRow.getByRole("meter", { name: "CPU 72%" })).toBeVisible();
  await expect(coreRow.getByRole("group", { name: "公网出口" })).toHaveText("IPv4IPv6");
  await expect(coreRow.locator(".expiry")).toContainText("剩 10 天");
  await expect(coreRow.locator('td[data-label="运行时长"]')).toHaveText("8d 8h");
  await expect(list.getByRole("row", { name: "等待首次接入", exact: true }).getByRole("img", { name: "从未上报" })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(1440);
  await page.screenshot({ path: testInfo.outputPath("list-light.png"), fullPage: true });
  const listText = await page.evaluate(() => document.body.innerText);
  for (const word of FORBIDDEN) expect(listText, word).not.toContain(word);
  // 视图记在浏览器里，刷新沿用列表；名称链到节点页。
  await page.reload();
  await expect(list.locator("tbody tr")).toHaveCount(5);
  await expect(coreRow.getByRole("link", { name: "tokyo-core" })).toHaveAttribute("href", "/nodes/1");
  await views.getByRole("button", { name: "卡片" }).click();

  // 手机：卡片视图没有横向溢出；离线表的名称折行显示全名，不截断。
  await page.setViewportSize({ width: 390, height: 844 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(390);
  for (const cell of await folded.locator("tbody td:first-child").all()) expect(await cell.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);
  await page.screenshot({ path: testInfo.outputPath("cards-mobile.png"), fullPage: true });
  // 手机：列表每行折成一张卡（状态点、名称、最近上报；三项用量；网速），负载、本周期、运行时长与到期不显示，没有横向溢出。
  await views.getByRole("button", { name: "列表" }).click();
  await expect(coreRow.locator('td[data-label="CPU"]')).toBeVisible();
  for (const label of ["负载", "本周期", "运行时长", "到期"]) await expect(coreRow.locator(`td[data-label="${label}"]`)).toBeHidden();
  const [dot, cpu] = [await boxOf(coreRow.locator('td[data-label="状态"]')), await boxOf(coreRow.locator('td[data-label="CPU"]'))];
  expect(cpu.y).toBeGreaterThan(dot.y);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(390);
  await page.screenshot({ path: testInfo.outputPath("list-mobile.png"), fullPage: true });
  // 手机：墙是单列列表，点行直接进节点页，没有横向溢出。
  await views.getByRole("button", { name: "状态墙" }).click();
  await expect(panel).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(390);
  await page.screenshot({ path: testInfo.outputPath("wall-mobile.png"), fullPage: true });
  await page.getByRole("link", { name: nodes[3].name }).click();
  await expect(page).toHaveURL(/\/nodes\/4$/);
});

test("公开页关闭时分享链接得到说明页而不是 404", async ({ page, hub }) => {
  // 总闸是整个 hub 共用的设置，后续 spec 共用同一个 hub：关闸只在本用例内有效，收尾时恢复（fixtures.ts）。
  hub.atEnd("恢复公开页总闸", async () => must(await hub.rpc(AdminService.method.updateSettings, { settings: { publicEnabled: true } })));
  await setPublicEnabled(page, false);
  const response = await page.goto("/nodes/7");
  expect(response?.status()).toBe(200);
  await expect(page.getByRole("heading", { name: "公开页已关闭" })).toBeVisible();
  await expect(page.getByRole("link", { name: "管理员登录" })).toHaveAttribute("href", "/admin/");
});
