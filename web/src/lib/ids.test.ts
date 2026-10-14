import { expect, it } from "vitest";
import { ascending, toggled, withId } from "./ids";

it("ascending 按数值而不是字典序排 bigint", () => {
  expect(ascending(new Set([10n, 2n, 1n]))).toEqual([1n, 2n, 10n]);
});

it("toggled 返回新集合，不改原集合", () => {
  const before = new Set([1n]);
  expect([...toggled(before, 2n)]).toEqual([1n, 2n]);
  expect([...toggled(before, 1n)]).toEqual([]);
  expect([...before]).toEqual([1n]);
});

it("withId 为同名行生成稳定可区分的身份", () => {
  expect(withId("ci", 2n)).toBe("ci（#2）");
});
