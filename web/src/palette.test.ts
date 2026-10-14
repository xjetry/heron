// @vitest-environment node
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, it } from "vitest";
import { BUILT_IN_ACCENT } from "./lib/palette";

const css = readFileSync(resolve(dirname(fileURLToPath(import.meta.url)), "styles.css"), "utf8");

// 明暗只经 color-scheme 切换：公开页把站点设置写在 html 的 data-theme 上，它要能压过系统设置。
// 按系统明暗的媒体查询改写变量会绕过 data-theme，所以一处都不能有。浏览器里的实际效果由公开页的浏览器验收核对。
it("调色板随 color-scheme 取值，data-theme 能强制明暗", () => {
  expect(css).not.toMatch(/prefers-color-scheme/);
  const root = /:root\s*\{([^}]*)\}/.exec(css)?.[1];
  expect(root).toMatch(/color-scheme:\s*light dark/);
  for (const name of ["--bg", "--card", "--fg", "--muted", "--line", "--accent"]) {
    expect(root, name).toMatch(new RegExp(`${name}:\\s*light-dark\\(`));
  }
  expect(css).toMatch(/:root\[data-theme="light"\]\s*\{\s*color-scheme:\s*light;?\s*\}/);
  expect(css).toMatch(/:root\[data-theme="dark"\]\s*\{\s*color-scheme:\s*dark;?\s*\}/);
});

// 外观页取色器的兜底色就是内置浅色主题的主色：不另存一份，改了 styles.css 这里跟着变。
it("内置主色取自 styles.css 的 --accent 浅色值", () => {
  const light = /--accent:\s*light-dark\(\s*(#[0-9a-fA-F]{6})/.exec(css)?.[1].toLowerCase();
  expect(BUILT_IN_ACCENT).toBe(light);
});
