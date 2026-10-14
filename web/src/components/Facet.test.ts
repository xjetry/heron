import { expect, it } from "vitest";
import { facetSummary, narrowToSingle, pickOption } from "./Facet";

const options = [{ value: "HK", label: "香港", count: 2 }, { value: "JP", label: "日本", count: 3 }, { value: "", label: "未知", count: 1 }];

it.each<[readonly string[], string, string[]]>([
  [[], "JP", ["JP"]],
  [["HK"], "JP", ["JP"]],
  [["JP"], "JP", []],
  [["HK", "JP"], "JP", ["JP"]],
])("单选：已选 %j 时点 %s 得到 %j，点当前选中的那一个回到全部", (selected, value, expected) => {
  expect(pickOption("single", selected, value)).toEqual(expected);
});

it.each<[readonly string[], string, string[]]>([
  [[], "JP", ["JP"]],
  [["HK"], "JP", ["HK", "JP"]],
  [["HK", "JP"], "HK", ["JP"]],
  [["JP"], "JP", []],
])("多选：已选 %j 时点 %s 逐个翻转得到 %j", (selected, value, expected) => {
  expect(pickOption("multi", selected, value)).toEqual(expected);
});

it.each<[readonly string[], string[]]>([
  [["", "JP"], ["JP"]],
  [["JP", "HK"], ["HK"]],
  [[""], [""]],
  [[], []],
])("切回单选只留按选项顺序最前的已选项：%j → %j", (selected, expected) => {
  expect(narrowToSingle(options, selected)).toEqual(expected);
});

it.each<[readonly string[], string]>([
  [[], "全部"],
  [["JP"], "日本"],
  [[""], "未知"],
  [["HK", "JP"], "已选 2 个"],
])("入口摘要：%j → %s", (selected, expected) => {
  expect(facetSummary(options, selected)).toBe(expected);
});
