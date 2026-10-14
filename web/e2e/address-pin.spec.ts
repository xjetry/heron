import { type Page } from "@playwright/test";
import { AdminService } from "../src/gen/heron/v1/admin_pb";
import { AgentService } from "../src/gen/heron/v1/agent_pb";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { AddressDetectionState, type AddressDetectionSchema } from "../src/gen/heron/v1/types_pb";
import { expect, login, must, mustField, rpc, test } from "./fixtures";

async function openEditor(page: Page, label: string) {
  await page.getByRole("button", { name: `更多操作 ${label}`, exact: true }).click();
  await page.getByRole("menuitem", { name: `编辑 ${label}`, exact: true }).click();
  return page.getByRole("dialog");
}

// 出站 HTTPS 受限的主机 IPv4 每轮探测失败。管理员在编辑器里手填 IPv4 之后：列表显示手填地址与标记、公开页画出 IPv4
// 标记、agent 下一次上报的应答要求停用 IPv4 探测；清空手填之后，agent 还报着停用时列表写"等待探测"、公开标记消失，
// agent 恢复探测后回到探测结果。三次上报都走 hub 的真实 Report，状态全部来自 hub 的显示值。
test("手填出口地址：编辑、列表与公开标记、清空恢复探测", async ({ page, hub, browserName }) => {
  await page.goto("/admin/login");
  await login(page);
  must(await rpc(page, AdminService.method.updateSettings, { settings: { publicEnabled: true } }));
  const name = `pinned-${browserName}`;
  const created = await rpc(page, AdminService.method.createNode, { name });
  const id = mustField(created, "node").id;
  hub.deleteNodeAtEnd(id);
  const label = `${name}（#${id}）`;
  must(await rpc(page, AdminService.method.updateNode, { id, name, public: true, trafficResetDay: 1, offlineGraceS: 0 }));
  const { token } = must(await rpc(page, AgentService.method.register, { key: must(created).token }));
  const checkedAt = BigInt(Math.floor(Date.now() / 1000));
  let factsHash = 0n;
  const report = async (ipv4: MessageInitShape<typeof AddressDetectionSchema>) => must(await rpc(page, AgentService.method.report, {
    factsHash: ++factsHash, metrics: { bootId: "0b7c3a1e-5d2f-4e6a-9c8b-1a2b3c4d5e6f", cpuPct: 5 },
    facts: { hostname: "blocked-egress", agentVersion: "dev", network: { ipv4, ipv6: { state: AddressDetectionState.UNSUPPORTED, checkedAt } } },
  }, token));
  const failed: MessageInitShape<typeof AddressDetectionSchema> = { state: AddressDetectionState.FAILED, checkedAt };
  expect((await report(failed)).detection?.skipIpv4).toBe(false);

  await page.goto("/admin/nodes");
  const row = page.getByRole("row").filter({ has: page.getByRole("link", { name: label, exact: true }) });
  await expect(row.getByLabel("IPv4", { exact: true })).toHaveText("IPv4探测失败");
  let dialog = await openEditor(page, label);
  await dialog.getByLabel(`手填 IPv4 地址 ${label}`, { exact: true }).fill("8.8.8.8");
  await dialog.getByRole("button", { name: "保存", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(row.getByLabel("IPv4", { exact: true })).toHaveText(/^IPv48\.8\.8\.8.*手填$/);
  // agent 的下一次上报：应答要求停用 IPv4，agent 随后报 DISABLED；显示值仍是手填。
  const pinned = await report({ state: AddressDetectionState.DISABLED, checkedAt });
  expect(pinned.detection?.skipIpv4).toBe(true);
  expect(pinned.detection?.skipIpv6).toBe(false);
  await expect(row.getByLabel("IPv4", { exact: true })).toHaveText(/^IPv48\.8\.8\.8.*手填$/);

  const publicPage = await page.context().newPage();
  await publicPage.goto("/");
  await publicPage.getByRole("group", { name: "视图" }).getByRole("button", { name: "列表" }).click();
  const publicRow = publicPage.getByRole("region", { name: "节点列表" }).getByRole("row", { name, exact: true });
  await expect(publicRow.getByRole("group", { name: "公网出口" })).toHaveText("IPv4");
  await expect(publicPage.getByText("8.8.8.8")).toHaveCount(0);

  dialog = await openEditor(page, label);
  await dialog.getByLabel(`手填 IPv4 地址 ${label}`, { exact: true }).fill("");
  await dialog.getByRole("button", { name: "保存", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  // agent 还没收到恢复探测的应答，库里仍是它报的 DISABLED：列表写"等待探测"，公开页不再画 IPv4。
  await expect(row.getByLabel("IPv4", { exact: true })).toHaveText("IPv4等待探测");
  await expect(publicRow.getByRole("group", { name: "公网出口" })).toHaveCount(0);
  // 清空之后的应答不再要求停用；agent 恢复探测，报回探测结果，列表回到探测状态。facts 经写协程异步落库，节点列表每 10 秒
  // 才重取一次（Nodes.tsx 的 refetchInterval），长于断言的等待：先确认 hub 已给出新显示值，再重新进入页面。
  expect((await report(failed)).detection?.skipIpv4).toBe(false);
  await expect.poll(async () => must(await rpc(page, AdminService.method.listNodes, {})).nodes.find((n) => n.id === id)?.network?.ipv4?.state).toBe(AddressDetectionState.FAILED);
  await page.reload();
  await expect(row.getByLabel("IPv4", { exact: true })).toHaveText("IPv4探测失败");
  await publicPage.close();
});
