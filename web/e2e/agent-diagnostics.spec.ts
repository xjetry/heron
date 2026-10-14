import { toJsonString } from "@bufbuild/protobuf";
import { AdminService } from "../src/gen/heron/v1/admin_pb";
import { AgentService } from "../src/gen/heron/v1/agent_pb";
import { PublicService, PublicSnapshotSchema } from "../src/gen/heron/v1/public_pb";
import { CollectionComponent } from "../src/gen/heron/v1/types_pb";
import { expect, login, must, mustField, rpc, test } from "./fixtures";

test("采集诊断从真实上报进入管理详情且不进入公开页", async ({ page, browserName, hub }, testInfo) => {
  await page.goto("/admin/login");
  await login(page);
  const created = await rpc(page, AdminService.method.createNode, { name: `agent-health-${browserName}` });
  const node = mustField(created, "node");
  hub.deleteNodeAtEnd(node.id);
  const { token } = must(await rpc(page, AgentService.method.register, { key: must(created).token }));
  must(await rpc(page, AdminService.method.updateNode, { id: node.id, name: node.name, public: true, trafficResetDay: 1, offlineGraceS: 0 }));
  await page.goto(`/admin/nodes/${node.id}`);
  await page.getByRole("tab", { name: "Agent 诊断", exact: true }).click();
  const card = page.getByRole("region", { name: "Agent 运行诊断" });
  await expect(card.getByText("Agent 未提供诊断信息，请更新 Agent 后等待上报。")).toBeVisible();
  const diagnostics = {
    netInclude: ["private-uplink-*"], netInterfaces: ["private-uplink-0"], netInterfacesTotal: 1,
    failedCollectors: [CollectionComponent.DISK], reportIntervalMs: 10000,
  };
  must(await rpc(page, AgentService.method.report, {
    metrics: { bootId: "0b7c3a1e-5d2f-4e6a-9c8b-1a2b3c4d5e6f", netCounterEpoch: "a".repeat(64), netRxTotal: 100n, netTxTotal: 200n },
    factsHash: 101n, facts: { agentVersion: "browser-diagnostic-version", diagnostics },
  }, token));
  await expect(card.getByText("private-uplink-0", { exact: true })).toBeVisible({ timeout: 15000 });
  await expect(card.getByText("磁盘", { exact: true })).toBeVisible();
  await expect(card.getByText("10000 ms", { exact: false })).toBeVisible();
  await expect(card.getByText("最近保存的诊断，不保证实时健康。生效参数只读。")).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("diagnostics-desktop.png"), fullPage: true });
  await page.setViewportSize({ width: 375, height: 812 });
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth)).toBe(375);
  await page.screenshot({ path: testInfo.outputPath("diagnostics-mobile.png"), fullPage: true });
  must(await rpc(page, AgentService.method.report, {
    metrics: { bootId: "0b7c3a1e-5d2f-4e6a-9c8b-1a2b3c4d5e6f", netCounterEpoch: "a".repeat(64) },
    factsHash: 102n, facts: { diagnostics: { ...diagnostics, failedCollectors: [] } },
  }, token));
  await expect(card.getByText("最近采集未报告失败", { exact: true })).toBeVisible({ timeout: 15000 });
  await page.goto(`/admin/nodes/${node.id}?tab=diagnostics`);
  await expect(card).toBeVisible();
  // 解码不放过未知字段，诊断若以新字段漏进公开快照会在这里失败；已知字段的取值再按序列化后的全文核对。
  const snapshot = must(await rpc(page, PublicService.method.getSnapshot, {}));
  expect(snapshot.nodes.some((entry) => entry.id === node.id)).toBe(true);
  const json = toJsonString(PublicSnapshotSchema, snapshot);
  for (const value of ["private-uplink", "diagnostics", "netCounterEpoch", token]) expect(json).not.toContain(value);
});
