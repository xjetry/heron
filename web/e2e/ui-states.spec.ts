import { type Page } from "@playwright/test";
import { AdminService } from "../src/gen/heron/v1/admin_pb";
import { boxOf, expect, login, must, mustField, rpc, test } from "./fixtures";

async function tokenColor(page: Page, token: string): Promise<string> {
  return page.evaluate((name) => {
    const shell = document.querySelector(".admin-shell");
    if (shell === null) throw new Error("no admin shell for color probe");
    const probe = document.createElement("span");
    probe.style.color = `var(${name})`;
    shell.appendChild(probe);
    const color = getComputedStyle(probe).color;
    probe.remove();
    return color;
  }, token);
}

test("跳到主要内容时页头不被吸顶顶栏遮挡", async ({ page }) => {
  await page.goto("/admin/login");
  await login(page);
  for (const viewport of [{ width: 1440, height: 360 }, { width: 375, height: 500 }]) {
    await page.setViewportSize(viewport);
    await page.goto("/admin/");
    const heading = page.getByRole("heading", { name: "总览", exact: true });
    await expect(heading).toBeVisible();
    const skip = page.getByRole("link", { name: "跳到主要内容" });
    await skip.focus();
    await skip.press("Enter");
    await expect(page).toHaveURL(/#admin-main$/);
    await expect(page.locator("#admin-main")).toBeFocused();
    const [topbar, title] = await Promise.all([boxOf(page.locator(".admin-topbar")), boxOf(heading)]);
    expect(title.y, JSON.stringify({ viewport, topbar, title })).toBeGreaterThanOrEqual(topbar.y + topbar.height);
  }
});

test("中性悬停不覆盖主按钮与展开下拉的强调边框", async ({ page }) => {
  await page.goto("/admin/login");
  await login(page);
  await page.goto("/admin/nodes");
  await expect(page.getByRole("heading", { name: "节点", exact: true })).toBeVisible();
  const accent = await tokenColor(page, "--accent");
  const primary = page.getByRole("button", { name: "添加节点", exact: true });
  await primary.hover();
  expect(await primary.evaluate((el) => getComputedStyle(el).borderTopColor)).toBe(accent);
  const status = page.getByRole("button", { name: /^状态 / });
  await status.click();
  await expect(status).toHaveAttribute("aria-expanded", "true");
  await status.hover();
  expect(await status.evaluate((el) => getComputedStyle(el).borderTopColor)).toBe(accent);
});

test("危险确认按钮悬停时保持危险色边框", async ({ page, browserName, hub }) => {
  await page.goto("/admin/login");
  await login(page);
  const tag = `hover-danger-${browserName}`;
  const node = mustField(await rpc(page, AdminService.method.createNode, { name: tag }), "node");
  hub.deleteNodeAtEnd(node.id);
  hub.deleteAtEnd(AdminService.method.deleteTag, { name: tag });
  must(await rpc(page, AdminService.method.updateNode, { id: node.id, name: tag, tags: [tag], trafficResetDay: 1, offlineGraceS: 0 }));
  await page.goto("/admin/nodes");
  await page.getByRole("button", { name: `删除标签 ${tag}`, exact: true }).click();
  const confirm = page.getByRole("button", { name: `确认删除标签 ${tag}`, exact: true });
  await confirm.hover();
  expect(await confirm.evaluate((el) => getComputedStyle(el).borderTopColor)).toBe(await tokenColor(page, "--bad"));
});
