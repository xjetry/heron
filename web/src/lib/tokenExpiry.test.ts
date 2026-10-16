import { expect, it } from "vitest";
import { expiryText, isExpired, resolveExpiry } from "./tokenExpiry";

it("失效时刻本身即过期，与 hub 的边界相同", () => {
  expect(isExpired(undefined, 100n)).toBe(false);
  expect(isExpired(101n, 100n)).toBe(false);
  expect(isExpired(100n, 100n)).toBe(true);
});

it("三种写法", () => {
  expect(expiryText(undefined, 100n)).toBe("不过期");
  expect(expiryText(1_700_003_600n, 1_700_000_000n)).toMatch(/^至 \d{4}-\d{2}-\d{2} /);
  expect(expiryText(1_700_000_000n, 1_700_000_000n)).toMatch(/^已过期（\d{4}-/);
});

it("预设按 hub 时刻加时长，保持当前原样带回，不过期即缺失", () => {
  expect(resolveExpiry("1h", 1_000n, "", undefined)).toBe(4_600n);
  expect(resolveExpiry("30d", 0n, "", undefined)).toBe(2_592_000n);
  expect(resolveExpiry("keep", 1_000n, "", 50n)).toBe(50n);
  expect(resolveExpiry("keep", 1_000n, "", undefined)).toBeUndefined();
  expect(resolveExpiry("never", 1_000n, "", 50n)).toBeUndefined();
});

it("自定义按浏览器时区解释，空串与无法解析的值返回 null", () => {
  expect(resolveExpiry("custom", 0n, "2030-01-02T03:04", undefined)).toBe(BigInt(Math.floor(new Date(2030, 0, 2, 3, 4).getTime() / 1000)));
  expect(resolveExpiry("custom", 0n, "", undefined)).toBeNull();
  expect(resolveExpiry("custom", 0n, "not a date", undefined)).toBeNull();
});
