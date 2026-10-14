import { afterEach, expect, it, vi } from "vitest";
import { forcedTheme, nextSchemeChoice, readSchemeChoice, writeSchemeChoice } from "./scheme";

afterEach(() => { vi.restoreAllMocks(); localStorage.clear(); });

it.each([
  ["dark", "light", "light"],
  ["dark", "auto", "dark"],
  ["auto", "auto", undefined],
  ["", "dark", "dark"],
  ["sepia", "auto", undefined],
] as const)("站点 %s 与访客 %s 合成为 %s", (site, choice, expected) => {
  expect(forcedTheme(site, choice)).toBe(expected);
});

it.each([["auto", "light"], ["light", "dark"], ["dark", "auto"]] as const)("选择从 %s 循环到 %s", (choice, next) => {
  expect(nextSchemeChoice(choice)).toBe(next);
});

it("读写 localStorage；auto 表示删除键；非法值当 auto", () => {
  writeSchemeChoice("k", "dark");
  expect(localStorage.getItem("k")).toBe("dark");
  expect(readSchemeChoice("k")).toBe("dark");
  writeSchemeChoice("k", "auto");
  expect(localStorage.getItem("k")).toBeNull();
  localStorage.setItem("k", "sepia");
  expect(readSchemeChoice("k")).toBe("auto");
});

it("存储被禁用时读得到 auto、写入和删除都不抛", () => {
  vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("denied"); });
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("denied"); });
  vi.spyOn(Storage.prototype, "removeItem").mockImplementation(() => { throw new Error("denied"); });
  expect(readSchemeChoice("k")).toBe("auto");
  expect(() => writeSchemeChoice("k", "dark")).not.toThrow();
  expect(() => writeSchemeChoice("k", "auto")).not.toThrow();
});
