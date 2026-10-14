import { expect, it } from "vitest";
import { axisValues } from "./axis";

it("ms 轴带后缀且精度足以区分刻度", () => {
  expect(axisValues([0, 0.5, 1], "ms")).toEqual(["0.0 ms", "0.5 ms", "1.0 ms"]);
});

it("count/s 轴带 /s 后缀且精度足以区分刻度", () => {
  expect(axisValues([0, 0.5, 1], "count/s")).toEqual(["0.0/s", "0.5/s", "1.0/s"]);
});

it.each([
  { unit: "bytes", values: [40, 40.2, 40.4].map((v) => v * 1024 ** 3) },
  { unit: "bytes/s", values: [40, 40.2, 40.4].map((v) => v * 1024 ** 2) },
  { unit: "count", values: [0, 0.5, 1] },
  { unit: "percent", values: [40, 40.2, 40.4] },
])("$unit 的相邻轴刻度不重名", ({ unit, values }) => {
  expect(new Set(axisValues(values, unit)).size).toBe(values.length);
});

it("无单位轴按刻度间隔保留小数且不添加后缀", () => {
  expect(axisValues([0, 0.2, 0.4], "")).toEqual(["0.0", "0.2", "0.4"]);
});

it("bytes/s 轴用同一倍率并带 /s 后缀", () => {
  expect(axisValues([0, 1024, 2048], "bytes/s")).toEqual(["0 KiB/s", "1 KiB/s", "2 KiB/s"]);
});

// 倍率按该轴的最大刻度选：轴上最小的一段是 512 B/s，其余刻度升到 MiB/s，全部使用同一单位。
it("bytes/s 轴按最大值统一选倍率，小刻度也随大刻度升位", () => {
  expect(axisValues([0, 512, 1024 ** 2], "bytes/s")).toEqual(["0.0000 MiB/s", "0.0005 MiB/s", "1.0000 MiB/s"]);
});
