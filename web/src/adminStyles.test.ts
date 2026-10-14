import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, it } from "vitest";

const admin = readFileSync(join(import.meta.dirname, "admin.css"), "utf8");
const shared = readFileSync(join(import.meta.dirname, "styles.css"), "utf8");
const publicCss = readFileSync(join(import.meta.dirname, "public/public.css"), "utf8");

// 设计 token 只有 styles.css :root 一处来源；admin.css 重定义同名变量会使管理端使用另一套配色。
it("admin.css 不重定义 styles.css 的 token，也不另设字体", () => {
  const tokens = [...shared.matchAll(/^\s+(--[a-z-]+):/gm)].map((m) => m[1]);
  expect(tokens.length).toBeGreaterThan(10);
  for (const token of tokens) expect(admin, token).not.toMatch(new RegExp(`${token}\\s*:`));
  expect(admin).not.toMatch(/font-family\s*:/);
});

// 筛选行是两端共用的部件：只在 styles.css 定义，public.css 不再有自己的一份。
it("筛选行样式只在 styles.css", () => {
  expect(shared).toMatch(/^\.filter-row \{/m);
  expect(publicCss).not.toMatch(/\.filter-row/);
});

// 地区 / 标签入口（components/Facet.tsx）由公开页总览与管理端节点页共用：样式只在两端都加载的 styles.css，任一端另写一份
// 都会让两端的面板与胶囊各走各的。
it("地区 / 标签入口的样式只在 styles.css", () => {
  expect(shared).toMatch(/^\.facet-panel \{/m);
  expect(shared).toMatch(/^\.facet-chip \{/m);
  expect(publicCss).not.toMatch(/\.facet-/);
  expect(admin).not.toMatch(/\.facet-/);
});
