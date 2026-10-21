import { AdminService } from "../src/gen/heron/v1/admin_pb";
import { boxOf, expect, fulfillRpc, login, mustField, requestMessage, rpcRoute, test } from "./fixtures";

const MiB = 1024 ** 2;
const at = (iso: string) => Date.parse(iso) / 1000;
const periodStart = at("2025-09-01T00:00:00Z");
const now = at("2025-09-10T12:00:00Z");
// 09-03 是峰值，09-05 恰好一半（只有下行），09-07 没有点；其余日子各 64 MiB。
const usage = new Map([["2025-09-03", { rx: 768 * MiB, tx: 256 * MiB }], ["2025-09-05", { rx: 512 * MiB, tx: 0 }]]);

// 柱高、柱宽与卡片溢出只有真实浏览器排版才量得出：jsdom 没有布局。
test("每日流量柱高与用量成比例、柱宽封顶 24px，窄屏不撑出卡片", async ({ page, hub, browserName }) => {
  const id = mustField(await hub.rpc(AdminService.method.createNode, { name: `${browserName}-daily` }), "node").id;
  hub.deleteNodeAtEnd(id);
  await page.route(rpcRoute(AdminService.method.getTraffic), (route) => fulfillRpc(route, AdminService.method.getTraffic, () => ({
    timezone: "UTC", now: BigInt(now),
    nodes: [{ nodeId: id, name: `${browserName}-daily`, traffic: { periodStart: BigInt(periodStart), nextResetAt: BigInt(at("2025-10-01T00:00:00Z")), resetDay: 1, periodRx: 2n * 1024n ** 3n } }],
  })));
  await page.route(rpcRoute(AdminService.method.queryMetrics), (route) => {
    if (requestMessage(route.request(), AdminService.method.queryMetrics).maxPoints !== 2000) return route.continue();
    const points: { ts: number; rx: number; tx: number }[] = [];
    for (let d = 1; d <= 10; d++) {
      const day = `2025-09-${String(d).padStart(2, "0")}`;
      if (day !== "2025-09-07") points.push({ ts: at(`${day}T06:00:00Z`), ...(usage.get(day) ?? { rx: 48 * MiB, tx: 16 * MiB }) });
    }
    return fulfillRpc(route, AdminService.method.queryMetrics, () => ({
      level: "1h", stepS: 3600, ts: points.map((p) => BigInt(p.ts)),
      series: [
        { name: "rx_bytes", unit: "bytes", samples: points.map((p) => ({ n: 1, sum: p.rx })) },
        { name: "tx_bytes", unit: "bytes", samples: points.map((p) => ({ n: 1, sum: p.tx })) },
      ],
      coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n },
    }));
  });
  await page.goto("/admin/login");
  await login(page);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(`/admin/nodes/${id}?tab=traffic`);
  const card = page.getByRole("region", { name: "每日流量" });
  const bars = card.getByRole("list", { name: "每日用量" });
  await expect(bars.getByRole("listitem")).toHaveCount(10);

  const column = async (day: string) => {
    const item = bars.getByRole("listitem", { name: new RegExp(`^${day} `) });
    const col = await boxOf(item.locator(".daily-column"));
    const segs = await item.locator(".daily-segment").evaluateAll((els) => els.map((el) => { const r = el.getBoundingClientRect(); return { top: r.top, height: r.height, width: r.width }; }));
    return { col, segs, height: segs.reduce((h, s) => h + s.height, 0) + Math.max(0, segs.length - 1) * 2 };
  };
  const peak = await column("2025-09-03");
  const half = await column("2025-09-05");
  const missing = await column("2025-09-07");
  // 峰值那天两段加中间 2px 间隔正好占满柱区（允许 2px 的取整与间隔误差），一半用量的那天约为一半高。
  expect(Math.abs(peak.height - peak.col.height)).toBeLessThanOrEqual(2);
  expect(peak.segs).toHaveLength(2);
  expect(Math.abs(half.height / peak.col.height - 0.5)).toBeLessThan(0.03);
  expect(missing.segs).toHaveLength(0);
  // 无数据与用量为 0 都没有柱段，图上只靠占位区分：只有缺点的那天有，且看得见。
  await expect(bars.locator(".daily-missing")).toHaveCount(1);
  await expect(bars.getByRole("listitem", { name: /^2025-09-07 / }).locator(".daily-missing")).toBeVisible();
  for (const s of peak.segs) expect(s.width).toBeLessThanOrEqual(24);
  await expect(card.getByText("用量最高（下载 + 上传）：09-03 1.0 GiB、09-05 512 MiB")).toBeVisible();
  if (process.env.DAILY_SHOT_DIR) await card.screenshot({ path: `${process.env.DAILY_SHOT_DIR}/daily-${browserName}-wide.png` });

  await page.setViewportSize({ width: 375, height: 800 });
  await expect(bars.getByRole("listitem")).toHaveCount(10);
  const overflow = await card.evaluate((el) => el.scrollWidth - el.clientWidth);
  expect(overflow).toBeLessThanOrEqual(0);
  if (process.env.DAILY_SHOT_DIR) await card.screenshot({ path: `${process.env.DAILY_SHOT_DIR}/daily-${browserName}-narrow.png` });
});
