// @vitest-environment node
import { readFileSync } from "node:fs";
import { expect, it } from "vitest";
import { OS_ICON_KINDS, type OsIconKind } from "../components/OsIcon";

const css = readFileSync(new URL("./public.css", import.meta.url), "utf8");
const tokens = readFileSync(new URL("../styles.css", import.meta.url), "utf8");

// 卡片底色取 styles.css 的 --card，与 styles.test 核对的是同一份声明。
const card = /--card:\s*light-dark\((#[0-9a-f]{6}),\s*(#[0-9a-f]{6})\)/i.exec(tokens);
if (card === null) throw new Error("styles.css 里没有 --card 的 light-dark 声明");
const [, cardLight, cardDark] = card;

// 品牌色是黑的（Apple、AlmaLinux）与自绘的通用主机图标跟随文字色，不单独上色。
const UNCOLORED: readonly OsIconKind[] = ["macos", "almalinux", "host"];
const colored = OS_ICON_KINDS.filter((kind) => !UNCOLORED.includes(kind));

// 只认该 kind 规则块里的 color 声明，位置不限；[\\s{;] 前缀排除 background-color 一类。
const colorOf = (kind: OsIconKind): string | undefined => new RegExp(`\\.os-icon\\[data-os="${kind}"\\]\\s*\\{[^}]*?[\\s{;]color:\\s*([^;]+);`).exec(css)?.[1].trim();

// WCAG 相对亮度与对比度；图形元素的下限是 3:1。
const luminance = (hex: string): number => {
  const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255).map((c) => c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
};
const contrast = (a: string, b: string): number => {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
};
// 单个 hex 两种主题共用；light-dark(a, b) 浅色取 a、深色取 b。
const pair = (value: string): [light: string, dark: string] => {
  const ld = /^light-dark\((#[0-9a-f]{6}),\s*(#[0-9a-f]{6})\)$/i.exec(value);
  if (ld) return [ld[1], ld[2]];
  if (/^#[0-9a-f]{6}$/i.test(value)) return [value, value];
  throw new Error(`不认识的颜色写法：${value}`);
};

it.each(colored)("%s 有品牌色，且在明暗两种卡片底上都不低于 3:1", (kind) => {
  const value = colorOf(kind);
  expect(value, `public.css 缺 .os-icon[data-os="${kind}"] 的 color`).toBeDefined();
  const [light, dark] = pair(value ?? "");
  expect(contrast(light, cardLight), `${kind} 浅色 ${light} 对 ${cardLight}`).toBeGreaterThanOrEqual(3);
  expect(contrast(dark, cardDark), `${kind} 深色 ${dark} 对 ${cardDark}`).toBeGreaterThanOrEqual(3);
});

it.each(UNCOLORED)("%s 不单独上色，跟随文字色", (kind) => {
  expect(colorOf(kind)).toBeUndefined();
});
