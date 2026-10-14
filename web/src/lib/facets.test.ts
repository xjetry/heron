import { expect, it } from "vitest";
import { matchesRegion, regionName, regionOptions, tagOptions } from "./facets";

// 只给 country 与 tags：选项与计数只读这两个字段，公开节点与管理端节点经结构类型共用同一份函数。
const nodes = [
  { country: "JP", tags: ["prod"] },
  { country: "JP", tags: ["prod", "DB"] },
  { country: "HK", tags: [] },
  { country: "", tags: [] },
  { country: "HK", tags: [] },
];

it.each([
  ["JP", "日本"], ["HK", "香港"], ["", "未知"], ["ZZ", "未知地区"], ["XX", "XX"], ["invalid", "invalid"],
])("地区中文短名 %s，空代码未知、无名称或无效代码回退", (code, expected) => {
  expect(regionName(code)).toBe(expected);
});

it("标签选项按 hub 给的顺序，计数按折叠比较，带上没有节点的标签", () => {
  expect(tagOptions(nodes, ["db", "prod", "gone"])).toEqual([
    { value: "db", label: "db", count: 1 },
    { value: "prod", label: "prod", count: 2 },
    { value: "gone", label: "gone", count: 0 },
  ]);
});

it("地区选项按代码排序，未知最后，带中文名与计数", () => {
  expect(regionOptions(nodes)).toEqual([
    { value: "HK", label: "香港", count: 2 },
    { value: "JP", label: "日本", count: 2 },
    { value: "", label: "未知", count: 1 },
  ]);
});

it("地区取并集：命中任一所选地区即保留，未知地区是空码；空选择匹配一切", () => {
  const kept = (regions: string[]) => nodes.filter((n) => matchesRegion(n.country, regions)).map((n) => n.country);
  expect(kept(["JP", "HK"])).toEqual(["JP", "JP", "HK", "HK"]);
  expect(kept([""])).toEqual([""]);
  expect(kept([])).toEqual(["JP", "JP", "HK", "", "HK"]);
  expect(kept(["US"])).toEqual([]);
});
