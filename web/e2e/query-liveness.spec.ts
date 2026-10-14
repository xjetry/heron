import type { Message } from "@bufbuild/protobuf";
import type { GenMessage } from "@bufbuild/protobuf/codegenv2";
import type { Request } from "@playwright/test";
import { AdminService } from "../src/gen/heron/v1/admin_pb";
import { PublicService } from "../src/gen/heron/v1/public_pb";
import { ProbeKind } from "../src/gen/heron/v1/types_pb";
import { expect, login, must, mustField, requestMessage, rpc, rpcPath, rpcRoute, test } from "./fixtures";
import { READ_DEADLINE_MESSAGE } from "../src/api/deadline";

// 三个查询方法的请求消息都带 hub 时钟下的 from、to（Unix 秒）。
type HubWindow = { from: bigint; to: bigint };
const windowOf = (request: Request, method: { name: string; input: GenMessage<Message & HubWindow> }): HubWindow => requestMessage(request, method);

for (const hours of [8, -8]) for (const [service, comparison] of [
  ["AdminService", false], ["PublicService", false], ["AdminService", true], ["PublicService", true],
] as const) test(`${service} ${comparison ? "对比" : "节点"} 浏览器偏移 ${hours} 小时仍使用 hub 窗口`, async ({ page, browserName, hub }, testInfo) => {
  await page.goto("/admin/login");
  await login(page);
  // 公开端用例的前提是公开页总闸打开；它是 hub 共用设置，不能依赖之前的 spec 留下的状态。
  if (service === "PublicService") must(await rpc(page, AdminService.method.updateSettings, { settings: { publicEnabled: true } }));
  const node = mustField(await rpc(page, AdminService.method.createNode, { name: `clock-${browserName}` }), "node");
  hub.deleteNodeAtEnd(node.id);
  must(await rpc(page, AdminService.method.updateNode, { id: node.id, name: node.name, public: true, trafficResetDay: 1, offlineGraceS: 0 }));
  const saved = await rpc(page, AdminService.method.saveProbeTask, {
    task: { kind: ProbeKind.TCP, target: "127.0.0.1:18987", intervalS: 60, timeoutMs: 1000 }, nodeIds: [node.id],
  });
  const taskId = mustField(saved, "task", "task").id;
  hub.deleteAtEnd(AdminService.method.deleteProbeTask, { id: taskId });
  const { now } = must(await rpc(page, AdminService.method.getSnapshot, {}));
  await page.clock.setFixedTime((Number(now) + hours * 3600) * 1000);
  const admin = service === "AdminService";
  const path = comparison ? (admin ? `/admin/probes/${taskId}/compare` : `/probes/${taskId}`) : `${admin ? "/admin" : ""}/nodes/${node.id}`;
  const queries = admin ? AdminService.method : PublicService.method;
  const methods = comparison ? [queries.queryProbeComparison] : [queries.queryMetrics, queries.queryProbes];
  const requests = methods.map((method) => page.waitForRequest((request) => request.url().includes(rpcPath(method))).then((request) => ({ request, window: windowOf(request, method) })));
  await page.goto(path);
  for (const { request, window } of await Promise.all(requests)) {
    expect(Number(window.to) - Number(window.from)).toBe(86400);
    expect(Math.abs(Number(window.to) - Number(now))).toBeLessThanOrEqual(120);
    expect(Math.abs(Number(window.to) - (Number(now) + hours * 3600))).toBeGreaterThan(28_000);
    expect(Number(window.to) % 60).toBe(0);
    expect(request.headers()["connect-timeout-ms"]).toBe("30000");
  }
  await expect(page.getByRole("navigation", { name: "时间窗口" })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("window-desktop.png"), fullPage: true });
  await page.setViewportSize({ width: 375, height: 812 });
  await page.screenshot({ path: testInfo.outputPath("window-mobile.png"), fullPage: true });
});

for (const [path, service] of [["/admin/", "AdminService"], ["/", "PublicService"]] as const) {
  const snapshot = service === "AdminService" ? AdminService.method.getSnapshot : PublicService.method.getSnapshot;
  test(`${service} 挂住轮询在预算内显示横幅，解除拦截后恢复`, async ({ page }) => {
    await page.goto("/admin/login");
    await login(page);
    if (service === "PublicService") must(await rpc(page, AdminService.method.updateSettings, { settings: { publicEnabled: true } }));
    // install 接管 setTimeout；pauseAt 停住自动走时，runFor 逐段执行真实页面的截止与重试计时器。
    const start = Date.now();
    await page.clock.install({ time: start });
    await page.clock.pauseAt(start);
    const initial = page.waitForResponse((response) => response.url().includes(rpcPath(snapshot)) && response.ok());
    await page.goto(path);
    await initial;
    await page.clock.runFor(10);
    let calls = 0;
    let hanging = true;
    const pattern = rpcRoute(snapshot);
    await page.route(pattern, async (route) => {
      calls += 1;
      if (!hanging) await route.continue();
    });
    await page.clock.runFor(1990);
    await expect.poll(() => calls).toBe(1);
    await page.clock.runFor(30_000);
    await page.clock.runFor(1000);
    await expect.poll(() => calls).toBe(2);
    await page.clock.runFor(30_000);
    await page.clock.runFor(2000);
    await expect.poll(() => calls).toBe(3);
    await page.clock.runFor(29_999);
    await expect(page.getByRole("alert")).toHaveCount(0);
    await page.clock.runFor(1);
    // 最终错误的订阅通知由 TanStack 排入下一轮任务；不是额外的网络等待预算。
    await page.clock.runFor(1);
    await expect(page.getByRole("alert")).toContainText(READ_DEADLINE_MESSAGE);
    hanging = false;
    const recovered = page.waitForResponse((response) => response.url().includes(rpcPath(snapshot)) && response.ok());
    await page.clock.runFor(2000);
    await recovered;
    await page.clock.runFor(1);
    await expect(page.getByRole("alert")).toHaveCount(0);
    expect(calls).toBe(4);
    await page.unroute(pattern);
  });
}
