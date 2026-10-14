import { afterEach, expect, it, vi } from "vitest";
import { readFacetMode, writeFacetMode } from "../lib/facets";
import { PUBLIC_FACET_MODE_KEYS, PUBLIC_TAG_MATCH_KEY, PUBLIC_VIEW_KEY, PUBLIC_WALL_GROUP_KEY, readPublicView, readTagMatch, readWallGroupBy, writePublicView, writeTagMatch, writeWallGroupBy } from "./prefs";

afterEach(() => { vi.restoreAllMocks(); localStorage.clear(); });

it("没选过时是卡片；写入后按记住的视图读出", () => {
  expect(readPublicView()).toBe("cards");
  writePublicView("wall");
  expect(localStorage.getItem(PUBLIC_VIEW_KEY)).toBe("wall");
  expect(readPublicView()).toBe("wall");
  writePublicView("list");
  expect(localStorage.getItem(PUBLIC_VIEW_KEY)).toBe("list");
  expect(readPublicView()).toBe("list");
  writePublicView("cards");
  expect(localStorage.getItem(PUBLIC_VIEW_KEY)).toBe("cards");
  expect(readPublicView()).toBe("cards");
});

it.each(["table", "", "WALL"])("记住的值 %j 不认识时按卡片", (value) => {
  localStorage.setItem(PUBLIC_VIEW_KEY, value);
  expect(readPublicView()).toBe("cards");
});

it("存储被禁用时读得到卡片、写入不抛", () => {
  vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("denied"); });
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("denied"); });
  expect(readPublicView()).toBe("cards");
  expect(() => writePublicView("wall")).not.toThrow();
});

it("状态墙分组没选过时是地区；写入后按记住的分组读出；不认识的值按地区", () => {
  expect(readWallGroupBy()).toBe("region");
  writeWallGroupBy("tag");
  expect(localStorage.getItem(PUBLIC_WALL_GROUP_KEY)).toBe("tag");
  expect(readWallGroupBy()).toBe("tag");
  localStorage.setItem(PUBLIC_WALL_GROUP_KEY, "country");
  expect(readWallGroupBy()).toBe("region");
});

it("存储被禁用时分组读得到地区、写入不抛", () => {
  vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("denied"); });
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("denied"); });
  expect(readWallGroupBy()).toBe("region");
  expect(() => writeWallGroupBy("tag")).not.toThrow();
});

it("选择方式按入口分开记：没选过时单选；写一个不影响另一个；不认识的值按单选", () => {
  expect([readFacetMode(PUBLIC_FACET_MODE_KEYS, "region"), readFacetMode(PUBLIC_FACET_MODE_KEYS, "tag")]).toEqual(["single", "single"]);
  writeFacetMode(PUBLIC_FACET_MODE_KEYS, "tag", "multi");
  expect(localStorage.getItem(PUBLIC_FACET_MODE_KEYS.tag)).toBe("multi");
  expect([readFacetMode(PUBLIC_FACET_MODE_KEYS, "region"), readFacetMode(PUBLIC_FACET_MODE_KEYS, "tag")]).toEqual(["single", "multi"]);
  localStorage.setItem(PUBLIC_FACET_MODE_KEYS.region, "MULTI");
  expect(readFacetMode(PUBLIC_FACET_MODE_KEYS, "region")).toBe("single");
});

it("标签匹配方式没选过时同时满足；写入后按记住的读出；不认识的值按同时满足", () => {
  expect(readTagMatch()).toBe("all");
  writeTagMatch("any");
  expect(localStorage.getItem(PUBLIC_TAG_MATCH_KEY)).toBe("any");
  expect(readTagMatch()).toBe("any");
  localStorage.setItem(PUBLIC_TAG_MATCH_KEY, "or");
  expect(readTagMatch()).toBe("all");
});

it("存储被禁用时选择方式读得到单选、匹配方式读得到同时满足，写入不抛", () => {
  vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("denied"); });
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("denied"); });
  expect([readFacetMode(PUBLIC_FACET_MODE_KEYS, "region"), readTagMatch()]).toEqual(["single", "all"]);
  expect(() => { writeFacetMode(PUBLIC_FACET_MODE_KEYS, "region", "multi"); writeTagMatch("any"); }).not.toThrow();
});
