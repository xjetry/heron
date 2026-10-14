import { type Page } from "@playwright/test";
import { AdminService, AlertKind, ChannelKind, ProbeMetric, SilenceKind, TokenPermission } from "../src/gen/heron/v1/admin_pb";
import { ProbeKind } from "../src/gen/heron/v1/types_pb";
import { expect, login, must, mustField, test } from "./fixtures";

// 管理端表格的单元格一律不折行（styles.css 的 table.nodes td）：长度不受用户约束的内容若不收住，会把整张表撑到几千
// 像素，后面的列与 ⋯ 要横向滚动才看得到。这里把每类这样的内容都造到最长量级：标识类的值（探测目标、渠道目标）单行
// 截断、全文在 title；句子与拼起来的列表（告警条件、作用域、渠道名、令牌的节点范围）折行。只有真实浏览器排版才看得出。
test("长内容不撑破管理端表格：截断标识、折行句子与列表", async ({ page, hub, browserName }) => {
  test.setTimeout(90_000);
  const tag = (i: number) => `${browserName}-长标签-用于验证作用域折行-${"x".repeat(30)}-${i}`;
  const tags = [0, 1, 2, 3, 4, 5].map(tag);
  const nodeIds: bigint[] = [];
  for (let i = 0; i < 60; i++) {
    const name = `${browserName}-wide-${i}`;
    const id = mustField(await hub.rpc(AdminService.method.createNode, { name }), "node").id;
    hub.deleteNodeAtEnd(id);
    nodeIds.push(id);
    must(await hub.rpc(AdminService.method.updateNode, { id, name, trafficResetDay: 1, offlineGraceS: 0, tags: i < 2 ? tags : [] }));
  }
  for (const t of tags) hub.deleteAtEnd(AdminService.method.deleteTag, { name: t });
  const channelIds: bigint[] = [];
  for (let i = 0; i < 8; i++) {
    const name = `${browserName}-通知渠道名称用于验证列表折行-${"y".repeat(16)}-${i}`;
    const url = i === 0 ? `https://${"a".repeat(60)}.${"b".repeat(60)}.${"c".repeat(60)}.example.com/hook` : `https://hooks.example.com/${browserName}/${i}`;
    const id = mustField(await hub.rpc(AdminService.method.saveNotifyChannel, { channel: { name, kind: ChannelKind.WEBHOOK, webhook: { url, method: "POST" } } }), "channel").id;
    hub.deleteAtEnd(AdminService.method.deleteNotifyChannel, { id });
    channelIds.push(id);
  }
  const target = (`https://status.example-monitoring.com/${browserName}/` + "x".repeat(400) + "?region=ap-east-1").slice(0, 500);
  const save = async (task: { kind: ProbeKind; target: string }, scope: { nodeIds: bigint[] } | { selectorTags: string[] }) => {
    const id = mustField(await hub.rpc(AdminService.method.saveProbeTask, { task: { intervalS: 60, timeoutMs: 1000, ...task }, ...scope }), "task", "task").id;
    hub.deleteAtEnd(AdminService.method.deleteProbeTask, { id });
    return id;
  };
  const longTask = await save({ kind: ProbeKind.HTTP, target }, { nodeIds: nodeIds.slice(0, 2) });
  await save({ kind: ProbeKind.ICMP, target: "1.1.1.1" }, { selectorTags: tags });
  const rule = mustField(await hub.rpc(AdminService.method.saveAlertRule, { rule: { name: `${browserName} 长目标丢包`, kind: AlertKind.PROBE, enabled: true, selectorTags: tags, taskId: longTask, metric: ProbeMetric.LOSS_PCT, threshold: 20, forMinutes: 5, channelIds } }), "rule").id;
  hub.deleteAtEnd(AdminService.method.deleteAlertRule, { id: rule });
  const silence = mustField(await hub.rpc(AdminService.method.saveSilence, { silence: { name: `${browserName} 长作用域静默`, enabled: true, selectorTags: tags, kind: SilenceKind.DAILY, startHhmm: "01:00", endHhmm: "02:00", reason: "原因".repeat(60) } }), "silence").id;
  hub.deleteAtEnd(AdminService.method.deleteSilence, { id: silence });
  const token = mustField(await hub.rpc(AdminService.method.createApiToken, { name: `${browserName}-wide-token`, grant: { permissions: [TokenPermission.CREATE], allNodes: false, nodeIds } }), "apiToken").id;
  hub.deleteAtEnd(AdminService.method.deleteApiToken, { id: token });

  await page.goto("/admin/login");
  await login(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.emulateMedia({ reducedMotion: "reduce" });
  const fits = (p: Page) => p.locator(".table-scroll").first().evaluate((el) => [el.clientWidth, el.scrollWidth]);
  // 截断的值：元素确实被截（内容比框宽），title 是全文。
  const clipped = async (cell: ReturnType<Page["locator"]>, full: string) => {
    const clip = cell.locator(".clip-text");
    await expect(clip).toHaveAttribute("title", full);
    expect(await clip.evaluate((el) => el.scrollWidth > el.clientWidth)).toBe(true);
  };

  await page.goto("/admin/probes");
  const longRow = page.getByRole("row", { name: target, exact: true });
  await expect(longRow).toBeVisible();
  await expect(page.getByText(`标签：${tags.join(" ∩ ")}（当前 2）`)).toBeVisible();
  let [client, scroll] = await fits(page);
  expect(scroll, "探测任务表").toBeLessThanOrEqual(client + 1);
  await clipped(longRow.locator('td[data-label="目标"]'), target);

  await page.goto("/admin/alerts");
  const ruleRow = page.getByRole("row", { name: `${browserName} 长目标丢包`, exact: true });
  await expect(ruleRow.locator('td[data-label="条件"]')).toContainText("丢包率 ≥ 20%，连续 5 分钟");
  [client, scroll] = await fits(page);
  expect(scroll, "告警规则表").toBeLessThanOrEqual(client + 1);

  await page.goto("/admin/channels");
  const hostRow = page.getByRole("row").filter({ hasText: `${browserName}-通知渠道名称用于验证列表折行-${"y".repeat(16)}-0` });
  await expect(hostRow).toBeVisible();
  [client, scroll] = await fits(page);
  expect(scroll, "通知渠道表").toBeLessThanOrEqual(client + 1);
  await clipped(hostRow.locator('td[data-label="目标"]'), `POST https://${"a".repeat(60)}.${"b".repeat(60)}.${"c".repeat(60)}.example.com`);

  await page.goto("/admin/silences");
  await expect(page.getByRole("row").filter({ hasText: `${browserName} 长作用域静默` })).toBeVisible();
  [client, scroll] = await fits(page);
  expect(scroll, "维护静默表").toBeLessThanOrEqual(client + 1);

  await page.goto("/admin/tokens");
  await expect(page.getByText(`${browserName}-wide-token`)).toBeVisible();
  [client, scroll] = await fits(page);
  expect(scroll, "API token 表").toBeLessThanOrEqual(client + 1);
});
