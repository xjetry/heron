import { expect, it } from "vitest";
import { usage } from "./Bar";

it("使用率是 used / total 的百分比，并带回用量与总量", () => {
  expect(usage(1n, 4n)).toEqual({ used: 1n, total: 4n, pct: 25 });
  expect(usage(0n, 4n)?.pct).toBe(0);
  expect(usage(4n, 4n)?.pct).toBe(100);
});

it("total 为 0 或缺失、used 缺失时没有读数，不是 0", () => {
  expect(usage(1n, 0n)).toBeUndefined();
  expect(usage(1n, undefined)).toBeUndefined();
  expect(usage(undefined, 4n)).toBeUndefined();
  expect(usage(undefined, undefined)).toBeUndefined();
});
