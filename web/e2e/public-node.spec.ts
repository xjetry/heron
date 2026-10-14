import { expect, test } from "@playwright/test";
import { AdminService } from "../src/gen/heron/v1/admin_pb";
import { PublicService } from "../src/gen/heron/v1/public_pb";
import { ProbeKind } from "../src/gen/heron/v1/types_pb";
import { fulfillRpc, login, must, rpc, rpcRoute } from "./fixtures";

// 横轴间距规则见 web/src/components/Chart.tsx 的 Chart 注释与 X_TICK_SPACE；刻度数由容器宽度决定。刻度文字画在 canvas 上，Chart 把刻度数写在
// data-x-ticks；这里从用户入口在两个宽度下核对，并确认画布不超出容器。
test("节点页：现值头、两列图表与横轴刻度随宽度变化", async ({ page }, testInfo) => {
  // 总闸默认关闭，先打开（同 public-overview.spec）。
  await page.goto("/admin/login");
  await login(page);
  must(await rpc(page, AdminService.method.updateSettings, { settings: { publicEnabled: true } }));
  const now = Math.floor(Date.now() / 1000);
  const from = now - 86_400;
  const ts = Array.from({ length: 24 }, (_, i) => BigInt(from - (from % 3600) + i * 3600));
  await page.route(rpcRoute(PublicService.method.getSite), (route) => fulfillRpc(route, PublicService.method.getSite, () => ({ title: "状态" })));
  await page.route(rpcRoute(PublicService.method.getSnapshot), (route) => fulfillRpc(route, PublicService.method.getSnapshot, () => ({ now: BigInt(now), nodes: [{ id: 1n, name: "tokyo-core", online: true, lastSeenAt: BigInt(now - 2), facts: { os: "Debian 13", arch: "amd64", cpuModel: "EPYC", cpuCores: 4 }, metrics: { cpuPct: 42, memUsed: BigInt(1024 ** 3), memTotal: BigInt(4 * 1024 ** 3), uptimeS: 7200n } }], tags: [] })));
  await page.route(rpcRoute(PublicService.method.queryMetrics), (route) => fulfillRpc(route, PublicService.method.queryMetrics, () => ({ level: "1h", stepS: 3600, ts, series: [{ name: "cpu", unit: "percent", samples: ts.map((_, i) => ({ n: 60, mean: 20 + i, max: 40 + i })) }] })));
  const probes = [[3n, ProbeKind.ICMP, "1.1.1.1"], [4n, ProbeKind.TCP, "8.8.8.8:53"], [5n, ProbeKind.HTTP, "https://example.com/health"]] as const;
  await page.route(rpcRoute(PublicService.method.queryProbes), (route) => fulfillRpc(route, PublicService.method.queryProbes, () => ({ level: "1h", stepS: 3600, series: probes.map(([taskId, kind, target]) => ({ taskId, kind, target, samples: ts.map((t) => ({ ts: t, sent: 60, lost: 1, errors: 0, rttMeanUs: 20000, rttMinUs: 10000, rttMaxUs: 50000 })) })) })));
  await page.setViewportSize({ width: 1440, height: 960 });
  await page.goto("/nodes/1");
  await expect(page.getByRole("group", { name: "CPU" })).toContainText("42%");
  const cpu = page.locator(".card", { has: page.getByRole("heading", { name: "CPU", exact: true }) });
  await expect(cpu.locator("[data-x-ticks]")).toHaveAttribute("data-x-ticks", /^\d+$/);
  const wideTicks = Number(await cpu.locator("[data-x-ticks]").getAttribute("data-x-ticks"));
  const wideWidth = await cpu.locator("canvas").evaluate((c) => c.getBoundingClientRect().width);
  expect(wideTicks).toBeGreaterThanOrEqual(4);
  // 图例显示最新值而不是破折号。
  await expect(cpu.getByRole("list", { name: "图例" }).getByRole("button", { name: /CPU 均值/ })).toContainText("43%");
  await expect(page.getByRole("heading", { name: "ICMP 1.1.1.1" }).getByRole("link")).toHaveAttribute("href", "/probes/3");
  // 图表每张至少 480px（styles.css 的 .chart-grid），1440 宽下与管理端节点详情一样排两列；三张探测图里最后一张撑满整行，不留空格。
  const grids = page.locator(".chart-grid");
  await expect(grids).toHaveCount(2);
  expect(await grids.first().evaluate((g) => getComputedStyle(g).gridTemplateColumns.split(" ").length)).toBe(2);
  const probeWidths = await grids.nth(1).evaluate((g) => [g.getBoundingClientRect().width, ...[...g.children].map((c) => c.getBoundingClientRect().width)]);
  expect(probeWidths).toHaveLength(4);
  const [gridWidth, first, second, last] = probeWidths;
  expect(first).toBeCloseTo(second, 0);
  expect(first).toBeLessThan(gridWidth / 2);
  expect(last).toBeCloseTo(gridWidth, 0);
  await page.screenshot({ path: testInfo.outputPath("node-desktop.png"), fullPage: true });

  await page.setViewportSize({ width: 390, height: 844 });
  // 视口改变后由 ResizeObserver 调整画布，先等尺寸落定再读同一布局下的刻度。
  await expect.poll(async () => cpu.locator("canvas").evaluate((c) => c.getBoundingClientRect().width)).toBeLessThanOrEqual(390);
  const mobileTicks = Number(await cpu.locator("[data-x-ticks]").getAttribute("data-x-ticks"));
  const mobileWidth = await cpu.locator("canvas").evaluate((c) => c.getBoundingClientRect().width);
  expect(mobileTicks).toBeGreaterThanOrEqual(2);
  expect(mobileWidth / mobileTicks).toBeGreaterThanOrEqual(80);
  expect(wideWidth / wideTicks).toBeGreaterThanOrEqual(80);
  expect(mobileWidth).toBeLessThanOrEqual(390);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(390);
  await page.screenshot({ path: testInfo.outputPath("node-mobile.png"), fullPage: true });
});
