import { AdminService } from "../src/gen/heron/v1/admin_pb";
import { expect, login, must, mustField, rpc, test } from "./fixtures";

// 节点页、编辑抽屉与批量添加页上不再有浏览器按系统画的控件：下拉与标签候选是自绘列表，勾选框与单选框自绘外观，
// 数字框没有微调箭头。这些只能在真实浏览器里看：jsdom 不算样式，也不做焦点与默认动作。
test("管理端节点页的控件自绘：下拉、标签候选、勾选框、数字框", async ({ page, browserName, hub }, testInfo) => {
  await page.goto("/admin/login");
  await login(page);
  const name = `ctl-${browserName}`;
  const known = `ctl-known-${browserName}`;
  const id = mustField(await rpc(page, AdminService.method.createNode, { name }), "node").id;
  hub.deleteNodeAtEnd(id);
  const other = mustField(await rpc(page, AdminService.method.createNode, { name: `${name}-b` }), "node").id;
  hub.deleteNodeAtEnd(other);
  must(await rpc(page, AdminService.method.updateNode, { id: other, name: `${name}-b`, tags: [known], trafficResetDay: 1, offlineGraceS: 0 }));
  hub.deleteAtEnd(AdminService.method.deleteTag, { name: known });
  const label = `${name}（#${id}）`;
  await page.setViewportSize({ width: 1440, height: 960 });
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto("/admin/nodes");

  // 折叠标题的箭头自绘：浏览器自带的三角（list-item 标记）不画，箭头随展开转向。
  const summary = page.locator("details.tag-management > summary");
  expect(await summary.evaluate((el) => getComputedStyle(el).listStyleType)).toBe("none");
  const turned = () => summary.evaluate((el) => getComputedStyle(el, "::before").transform);
  const opened = await turned();
  expect(opened).not.toBe("none");
  await summary.click();
  expect(await turned()).not.toBe(opened);
  await summary.click();

  // 状态筛选：自绘下拉写进 URL。
  const status = page.getByRole("button", { name: /^状态 / });
  await expect(status).toHaveAccessibleName("状态 全部");
  await status.click();
  await page.getByRole("listbox", { name: "状态" }).getByRole("option", { name: "从未上报", exact: true }).click();
  await expect(page).toHaveURL(/status=never/);
  await expect(status).toHaveAccessibleName("状态 从未上报");
  await status.click();
  await page.getByRole("option", { name: "全部", exact: true }).click();
  await expect(page).not.toHaveURL(/status=/);

  // 勾选框自绘：appearance 为 none，勾上后铺强调色。
  const box = page.getByRole("checkbox", { name: "只看 15 天内到期" });
  expect(await box.evaluate((el) => getComputedStyle(el).appearance)).toBe("none");
  const accent = await page.evaluate(() => {
    const probe = document.createElement("span");
    probe.style.color = "var(--accent)";
    const shell = document.querySelector(".admin-shell");
    if (shell === null) throw new Error("no .admin-shell to host the accent probe");
    shell.appendChild(probe);
    const color = getComputedStyle(probe).color;
    probe.remove();
    return color;
  });
  await box.check();
  expect(await box.evaluate((el) => getComputedStyle(el).backgroundColor)).toBe(accent);
  await box.uncheck();

  // 编辑抽屉：数字框没有微调箭头；下拉在抽屉里按 Esc 只收起列表，抽屉不关，焦点回到下拉按钮。
  await page.getByRole("button", { name: `更多操作 ${label}`, exact: true }).click();
  await page.getByRole("menuitem", { name: `编辑 ${label}`, exact: true }).click();
  const drawer = page.getByRole("dialog");
  expect(await drawer.getByLabel(`重置日 ${label}`).evaluate((el) => getComputedStyle(el).appearance)).toBe("textfield");
  const unit = drawer.getByRole("button", { name: new RegExp(`^配额单位 ${name}`) });
  await unit.click();
  await expect(drawer.getByRole("listbox", { name: `配额单位 ${label}` })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(drawer.getByRole("listbox")).toHaveCount(0);
  await expect(drawer).toBeVisible();
  await expect(unit).toBeFocused();
  await unit.click();
  await drawer.getByRole("option", { name: "TiB", exact: true }).click();
  await expect(unit).toHaveAccessibleName(`配额单位 ${label} TiB`);

  // 标签候选：点一下直接加上，输入框清空。
  const tagInput = drawer.getByRole("combobox", { name: `新标签 ${label}` });
  await tagInput.fill(known);
  await drawer.getByRole("option", { name: known, exact: true }).click();
  await expect(drawer.getByRole("button", { name: `移除标签 ${known} ${label}` })).toBeVisible();
  await expect(tagInput).toHaveValue("");
  await expect(tagInput).toBeFocused();
  await page.screenshot({ path: testInfo.outputPath("node-editor-controls.png") });
  await drawer.getByRole("button", { name: "保存" }).click();
  await expect(drawer).toHaveCount(0);
  const { nodes } = must(await rpc(page, AdminService.method.listNodes, {}));
  expect(nodes.find((n) => n.id === id)?.tags).toEqual([known]);

  // 批量添加页：有效期是自绘下拉。
  await page.goto("/admin/register");
  await expect(page.getByRole("heading", { name: "批量添加节点" })).toBeVisible();
  await expect(page.getByText("当前没有开启的接入窗口。")).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("register-guide.png"), fullPage: true });
});
