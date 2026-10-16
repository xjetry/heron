import type { MessageInitShape } from "@bufbuild/protobuf";
import { anyUnpack } from "@bufbuild/protobuf/wkt";
import { AdminService, TokenPermission, UpdateNodeResponseSchema, type ExecuteChangeRequestSchema } from "../src/gen/heron/v1/admin_pb";
import { expect, login, must, mustField, rpc, test } from "./fixtures";

test("限定节点凭据的创建、预览、写入、重试、回执和吊销", async ({ page, browserName, hub }, testInfo) => {
  await page.goto("/admin/login");
  await login(page);
  const ids: bigint[] = [];
  const name = `scoped-${browserName}`;
  for (const suffix of ["allowed", "outside"]) {
    const node = mustField(await rpc(page, AdminService.method.createNode, { name: `${name}-${suffix}` }), "node");
    ids.push(node.id);
    hub.deleteNodeAtEnd(node.id);
  }
  await page.goto("/admin/tokens");
  await page.getByRole("button", { name: "新建 API token", exact: true }).click();
  const form = page.getByRole("form", { name: "新建 API token" });
  await form.getByLabel("名称", { exact: true }).fill(name);
  await form.getByLabel("监控配置", { exact: true }).check();
  await form.getByLabel("节点范围").selectOption("selected");
  await form.getByLabel(`${name}-allowed（#${ids[0]}）`, { exact: true }).check();
  await form.getByRole("button", { name: "创建", exact: true }).click();
  const secret = page.locator("code.secret");
  await expect(secret).toBeVisible();
  const token = await secret.textContent();
  if (token === null) throw new Error("code.secret has no text content");
  const created = must(await rpc(page, AdminService.method.listApiTokens, {})).tokens.find((entry) => entry.name === name);
  if (!created) throw new Error(`API token ${name} not listed`);
  const tokenId = created.id;
  hub.deleteAtEnd(AdminService.method.deleteApiToken, { id: tokenId });
  expect(created.grant?.permissions).toEqual([TokenPermission.CONFIGURE]);
  expect(created.grant?.allNodes).toBe(false);
  expect(created.grant?.nodeIds).toEqual([ids[0]]);
  const scoped = must(await rpc(page, AdminService.method.listNodes, {}, token));
  expect(scoped.nodes.map((node) => node.id)).toEqual([ids[0]]);

  // 改动走 oneof 的 updateNode 分支，字段掩码只放开 note。
  const requestId = `browser-${browserName}`;
  const change: MessageInitShape<typeof ExecuteChangeRequestSchema> = { requestId, updateMask: { paths: ["note"] }, change: { case: "updateNode", value: { id: ids[0], note: "browser-verified" } } };
  const preview = must(await rpc(page, AdminService.method.executeChange, { ...change, preview: true }, token));
  expect(preview.expectedVersion).toBeTruthy();
  expect(preview.operation?.committedAt).toBe(0n);
  expect(preview.result).toBeUndefined();
  const before = must(await rpc(page, AdminService.method.listNodes, {}, token));
  expect(before.nodes[0].note).toBe("");
  const denied = await rpc(page, AdminService.method.executeChange, { ...change, preview: true, change: { case: "updateNode", value: { id: ids[1], note: "forbidden" } } }, token);
  expect(denied).toMatchObject({ ok: false, status: 403, error: { code: "permission_denied" } });
  const execution = { ...change, expectedVersion: preview.expectedVersion };
  const committed = must(await rpc(page, AdminService.method.executeChange, execution, token));
  const operationId = committed.operation?.id ?? "";
  expect(operationId).toBeTruthy();
  expect(committed.operation?.committedAt).toBeGreaterThan(0n);
  // 首次执行带回原业务响应（Any），按类型 URL 解出 UpdateNodeResponse。
  expect(committed.result && anyUnpack(committed.result, UpdateNodeResponseSchema)?.node?.note).toBe("browser-verified");
  const after = must(await rpc(page, AdminService.method.listNodes, {}, token));
  expect(after.nodes[0].note).toBe("browser-verified");
  expect(after.nodes[0].name).toBe(`${name}-allowed`);
  const replay = must(await rpc(page, AdminService.method.executeChange, execution, token));
  expect(replay.replayed).toBe(true);
  expect(replay.operation?.id).toBe(operationId);
  expect(replay.result).toBeUndefined();

  await page.getByRole("dialog").getByRole("button", { name: "关闭抽屉", exact: true }).click();
  await page.getByRole("button", { name: `更多操作 ${name}（#${tokenId}）`, exact: true }).click();
  await page.getByRole("menuitem", { name: `查看操作记录 ${name}（#${tokenId}）`, exact: true }).click();
  const operations = page.getByRole("region", { name: "操作记录" });
  await expect(operations.locator("details")).toHaveCount(1);
  await operations.locator("summary").click();
  await expect(operations.getByText(requestId, { exact: true })).toBeVisible();
  await expect(operations.getByText(operationId, { exact: true })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("token-audit-desktop.png"), fullPage: true });
  await page.setViewportSize({ width: 375, height: 812 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(375);
  await page.screenshot({ path: testInfo.outputPath("token-audit-mobile.png"), fullPage: true });
  await page.getByRole("button", { name: `更多操作 ${name}（#${tokenId}）`, exact: true }).click();
  await page.getByRole("menuitem", { name: `吊销 ${name}（#${tokenId}）`, exact: true }).click();
  await page.getByRole("menuitem", { name: `确认吊销 ${name}（#${tokenId}）`, exact: true }).click();
  await page.getByRole("button", { name: `更多操作 ${name}（#${tokenId}）`, exact: true }).waitFor({ state: "hidden" });
  await expect(secret).toHaveCount(0);
  const revoked = await rpc(page, AdminService.method.listNodes, {}, token);
  expect(revoked).toMatchObject({ ok: false, status: 401, error: { code: "unauthenticated" } });
});

test("编辑授权不换明文：条件读取看到新范围，过期后改有效期即恢复", async ({ page, browserName, hub }) => {
  await page.goto("/admin/login");
  await login(page);
  const name = `editable-${browserName}`;
  const node = mustField(await rpc(page, AdminService.method.createNode, { name: `${name}-node` }), "node");
  hub.deleteNodeAtEnd(node.id);
  await page.goto("/admin/tokens");
  await page.getByRole("button", { name: "新建 API token", exact: true }).click();
  const form = page.getByRole("form", { name: "新建 API token" });
  await form.getByLabel("名称", { exact: true }).fill(name);
  await form.getByLabel("节点范围").selectOption("selected");
  await form.getByRole("button", { name: "创建", exact: true }).click();
  const secret = page.locator("code.secret");
  await expect(secret).toBeVisible();
  const token = await secret.textContent();
  if (token === null) throw new Error("code.secret has no text content");
  await page.getByRole("dialog").getByRole("button", { name: "完成", exact: true }).click();
  const created = must(await rpc(page, AdminService.method.listApiTokens, {})).tokens.find((entry) => entry.name === name);
  if (!created) throw new Error(`API token ${name} not listed`);
  hub.deleteAtEnd(AdminService.method.deleteApiToken, { id: created.id });
  const first = must(await rpc(page, AdminService.method.listNodes, {}, token));
  expect(first.nodes).toEqual([]);

  // 指定节点、列表为空 → 全站：没有任何节点授权行变化，条件读取仍须看到新列表。
  const edit = async () => {
    await page.getByRole("button", { name: `更多操作 ${name}（#${created.id}）`, exact: true }).click();
    await page.getByRole("menuitem", { name: `编辑 ${name}（#${created.id}）`, exact: true }).click();
    return page.getByRole("form", { name: "编辑 API token" });
  };
  let editor = await edit();
  await editor.getByLabel("节点范围").selectOption("all");
  await editor.getByLabel("有效期").selectOption("1h");
  await editor.getByRole("button", { name: "保存", exact: true }).click();
  await expect(editor).toHaveCount(0);
  const second = must(await rpc(page, AdminService.method.listNodes, { knownRevision: first.revision }, token));
  expect(second.unchanged).toBe(false);
  expect(second.nodes.map((n) => n.id)).toContain(node.id);
  const caller = must(await rpc(page, AdminService.method.getCaller, {}, token));
  const listed = must(await rpc(page, AdminService.method.listApiTokens, {}));
  expect(caller.caller.case).toBe("apiToken");
  if (caller.caller.case === "apiToken") {
    expect(caller.caller.value.grant?.allNodes).toBe(true);
    expect(Number(caller.caller.value.expiresAt) - Number(listed.now)).toBeGreaterThan(3500);
  }

  // 改到过去即过期，同一明文得 401；再改到未来即恢复。datetime-local 按浏览器时区解释，hub 按 UTC 写出：本地
  // 2000-01-01T00:00 在 UTC−12 到 UTC+14 之间换成 UTC 后落在 1999-12-31 或 2000-01-01。
  editor = await edit();
  await editor.getByLabel("有效期").selectOption("custom");
  await editor.getByLabel("失效时间").fill("2000-01-01T00:00");
  await editor.getByRole("button", { name: "保存", exact: true }).click();
  await expect(editor).toHaveCount(0);
  await expect(page.getByRole("row", { name: new RegExp(name) })).toContainText("已过期");
  const expired = await rpc(page, AdminService.method.listNodes, {}, token);
  expect(expired).toMatchObject({ ok: false, status: 401, error: { code: "unauthenticated", message: expect.stringMatching(/^API token expired at (1999-12-31|2000-01-01)T/) } });
  editor = await edit();
  await editor.getByLabel("有效期").selectOption("1d");
  await editor.getByRole("button", { name: "保存", exact: true }).click();
  await expect(editor).toHaveCount(0);
  must(await rpc(page, AdminService.method.listNodes, {}, token));
});
