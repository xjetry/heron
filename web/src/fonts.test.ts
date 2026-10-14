// @vitest-environment node
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { expect, it } from "vitest";

const require = createRequire(import.meta.url);
const entry = readFileSync(new URL("./fonts.ts", import.meta.url), "utf8");
const styles = readFileSync(new URL("./styles.css", import.meta.url), "utf8");
const imported = [...entry.matchAll(/^import "([^"]+\.css)";$/gm)].map((m) => m[1]);
const faces = imported.flatMap((spec) => [...readFileSync(require.resolve(spec), "utf8").matchAll(/@font-face\s*\{([^}]*)\}/g)].map((m) => m[1]));
const viteConfig = readFileSync(new URL("../vite.config.ts", import.meta.url), "utf8");
const licensedJson: unknown = JSON.parse(/const FONT_PACKAGES = (\[[^\]]*\]);/.exec(viteConfig)?.[1] ?? "null");
// 读不出或不是字符串数组时当作空表，下面与内嵌字体包的比对随之变红。
const licensed = Array.isArray(licensedJson) && licensedJson.every((p): p is string => typeof p === "string") ? licensedJson : [];
const leading = (name: string): string | undefined => new RegExp(`${name}:\\s*"([^"]+)"`).exec(styles)?.[1];

it("内嵌字体的每个字面都从包内相对路径加载 woff2，不指向远程地址", () => {
  expect(imported).toHaveLength(2);
  expect(faces.length).toBeGreaterThan(0);
  for (const face of faces) {
    const src = /src:\s*([^;]+);/.exec(face)?.[1] ?? "";
    expect(src).toMatch(/^url\(\.\/files\/[\w-]+\.woff2\)/);
    expect(src).not.toMatch(/https?:|\/\//);
  }
});

// 字体栈首位的家族名必须正是内嵌 CSS 声明的家族名：上游改名或换包而 token 没跟上时，页面会静默退回系统字体。
it("--font-ui / --font-mono 首位与内嵌字体的家族名一致", () => {
  const families = new Set(faces.map((face) => /font-family:\s*'([^']+)'/.exec(face)?.[1]));
  expect(families).toEqual(new Set([leading("--font-ui"), leading("--font-mono")]));
});

// OFL 要求随字体附上许可证全文：vite.config.ts 的 FONT_PACKAGES 把每个字体包的 LICENSE 产出到 licenses/。
// 两边不一致时，要么有字体没带许可证，要么产出了已不再内嵌的字体的许可证。
it("产出许可证的字体包正是内嵌的字体包，且每个包都带 OFL 全文", () => {
  const packages = imported.map((spec) => spec.split("/").slice(0, 2).join("/"));
  expect(new Set(licensed)).toEqual(new Set(packages));
  for (const pkg of licensed) expect(readFileSync(new URL(`../node_modules/${pkg}/LICENSE`, import.meta.url), "utf8")).toMatch(/SIL OPEN FONT LICENSE Version 1\.1/);
});
