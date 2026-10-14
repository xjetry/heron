// @vitest-environment node
import { readFileSync } from "node:fs";
import { expect, it } from "vitest";
import { SERIES_PALETTE, seriesColors } from "./seriesPalette";

const css = readFileSync(new URL("../styles.css", import.meta.url), "utf8");
const statusColors = [...css.matchAll(/--status-[a-z]+:\s*(#[0-9a-f]{6});/g)].map((m) => m[1]);
const rgb = (hex: string): number[] => [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16));
const distance = (a: string, b: string): number => Math.hypot(...rgb(a).map((v, i) => v - rgb(b)[i]));

// 系列色不与六个状态色混淆：按 RGB 欧氏距离至少 70。先冒烟状态色真的读到了六个，空集合会让下面的 every 恒真。
it("六个状态色都读到了", () => {
  expect(statusColors).toHaveLength(6);
});

it.each(SERIES_PALETTE)("%s 与每个状态色的距离都 ≥ 70", (series) => {
  for (const status of statusColors) expect(distance(series, status)).toBeGreaterThanOrEqual(70);
});

it("浅线取它前面最近一条实线的颜色；实线按顺序取色、用完循环", () => {
  expect(seriesColors([false, true, false, true])).toEqual([SERIES_PALETTE[0], SERIES_PALETTE[0], SERIES_PALETTE[1], SERIES_PALETTE[1]]);
  expect(seriesColors([true, false])).toEqual([SERIES_PALETTE[0], SERIES_PALETTE[0]]);
  expect(seriesColors(Array(7).fill(false)).at(-1)).toBe(SERIES_PALETTE[0]);
});
