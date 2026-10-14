import { expect, test } from "@playwright/test";
import { login } from "./fixtures";

// 字体随前端产物嵌进 hub：要证明的是浏览器真的从 hub 同源取到了 woff2、类型对（hub 回 nosniff，类型错了浏览器会拒用），
// 而且页面用上了它，不是样式表里写了名字就算。
test("内嵌字体从 hub 同源加载为 font/woff2，正文与等宽读数都用上", async ({ page }) => {
  const fonts: { url: string; status: number; type: string }[] = [];
  page.on("response", (r) => { if (new URL(r.url()).pathname.endsWith(".woff2")) fonts.push({ url: r.url(), status: r.status(), type: r.headers()["content-type"] ?? "" }); });
  await page.goto("/admin/login");
  await login(page);
  await page.goto("/admin/");
  await expect(page.getByRole("heading", { level: 1, name: "总览" })).toBeVisible();
  const state = await page.evaluate(async () => {
    await document.fonts.load('14px "JetBrains Mono Variable"', "0123456789");
    await document.fonts.ready;
    return {
      loaded: [...document.fonts].filter((f) => f.status === "loaded").map((f) => f.family.replace(/"/g, "")),
      body: getComputedStyle(document.body).fontFamily,
      mono: document.fonts.check('14px "JetBrains Mono Variable"', "0123456789"),
    };
  });
  expect(state.loaded).toContain("Inter Variable");
  expect(state.loaded).toContain("JetBrains Mono Variable");
  expect(state.mono).toBe(true);
  expect(state.body).toMatch(/^"?Inter Variable"?,/);
  expect(fonts.length).toBeGreaterThanOrEqual(2);
  const origin = new URL(page.url()).origin;
  for (const font of fonts) {
    expect(new URL(font.url).origin, font.url).toBe(origin);
    expect(font.status, font.url).toBe(200);
    expect(font.type, font.url).toBe("font/woff2");
  }
  // OFL 全文随产物嵌进 hub（vite.config.ts 的 fontLicenses），与字体同源可取。
  for (const name of ["inter", "jetbrains-mono"]) {
    const license = await page.request.get(`/admin/licenses/${name}-OFL.txt`);
    expect(license.status(), name).toBe(200);
    expect(await license.text(), name).toContain("SIL OPEN FONT LICENSE Version 1.1");
  }
});

test("自绘日期控件：未填完的日期不进 URL，月历选日写入筛选；抽屉里 Esc 只关月历", async ({ page }) => {
  await page.goto("/admin/login");
  await login(page);
  await page.goto("/admin/events");
  const from = page.getByRole("group", { name: "起始日期" });
  await from.getByRole("textbox", { name: "起始日期 年" }).pressSequentially("2031");
  await expect(from.getByRole("textbox", { name: "起始日期 月" })).toBeFocused();
  expect(new URL(page.url()).searchParams.get("from")).toBeNull();
  await from.getByRole("textbox", { name: "起始日期 月" }).pressSequentially("02");
  await from.getByRole("button", { name: "选择起始日期" }).click();
  const calendar = page.getByRole("dialog", { name: "选择起始日期" });
  await expect(calendar.getByText(/^\d+ 年 \d+ 月$/)).toBeVisible();
  await calendar.getByRole("button", { name: "今天" }).click();
  await expect(calendar).toHaveCount(0);
  await expect(page).toHaveURL(/[?&]from=\d{4}-\d{2}-\d{2}(&|$)/);
  await expect(from.getByRole("button", { name: "选择起始日期" })).toBeFocused();

  await page.goto("/admin/silences");
  await page.getByRole("button", { name: "新建维护静默" }).click();
  const drawer = page.getByRole("dialog", { name: "新建维护静默" });
  await drawer.getByLabel("类型").selectOption({ label: "一次性" });
  await drawer.getByRole("button", { name: "选择开始" }).click();
  await expect(page.getByRole("dialog", { name: "选择开始" })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog", { name: "选择开始" })).toHaveCount(0);
  await expect(drawer).toBeVisible();
  await expect(drawer.getByRole("button", { name: "选择开始" })).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(drawer).toHaveCount(0);
});
