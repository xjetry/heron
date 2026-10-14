import { useState } from "react";
import { readChoice, writeChoice } from "./storedChoice";
import { sameTag } from "./tags";

// 地区与标签两个筛选入口（components/Facet.tsx）的数据面，公开页与管理端节点页共用。两端的节点类型不同（PublicNode、Node），
// 这里只读 country 与 tags 两个字段，用结构类型接住两者，选项、计数与地区匹配只有这一份。
export type Facet = "region" | "tag";
export type FacetMode = "single" | "multi";
const FACET_MODES: readonly FacetMode[] = ["single", "multi"];

// 两个入口的选择方式是浏览器偏好，各端用自己的一组键（公开页与管理端互不影响），按入口分开记；没选过或记住的值不认识时
// 为单选。选择本身不在这里：公开页的选择只在内存，管理端的在 URL。
export type FacetModeKeys = Readonly<Record<Facet, string>>;
export const readFacetMode = (keys: FacetModeKeys, facet: Facet): FacetMode => readChoice(keys[facet], FACET_MODES, "single");
export const writeFacetMode = (keys: FacetModeKeys, facet: Facet, mode: FacetMode): void => writeChoice(keys[facet], mode);

export function useFacetModes(keys: FacetModeKeys): [Record<Facet, FacetMode>, (facet: Facet, mode: FacetMode) => void] {
  const [modes, setModes] = useState<Record<Facet, FacetMode>>(() => ({ region: readFacetMode(keys, "region"), tag: readFacetMode(keys, "tag") }));
  const choose = (facet: Facet, mode: FacetMode) => { setModes((current) => ({ ...current, [facet]: mode })); writeFacetMode(keys, facet, mode); };
  return [modes, choose];
}

// 计数是调用方给的节点集合（公开页是当前快照、管理端是全部节点）里的节点数，不随其他筛选变化。
export type FacetOption = { value: string; label: string; count: number };

// 中文短名适合组头与胶囊，不维护另一份国家名表；空串会被 DisplayNames 拒绝，先归入「未知」。
export function regionName(code: string): string {
  if (code === "") return "未知";
  try {
    return new Intl.DisplayNames(["zh-CN"], { type: "region", style: "short", fallback: "code" }).of(code) ?? code;
  } catch {
    return code;
  }
}

const byCodeUnknownLast = (a: string, b: string) => (a === b ? 0 : a === "" ? 1 : b === "" ? -1 : a.localeCompare(b));

export function regionOptions(nodes: readonly { country: string }[]): FacetOption[] {
  const counts = new Map<string, number>();
  for (const n of nodes) counts.set(n.country, (counts.get(n.country) ?? 0) + 1);
  return [...counts.keys()].sort(byCodeUnknownLast).map((code) => ({ value: code, label: regionName(code), count: counts.get(code)! }));
}

// 标签的集合与顺序取 hub 下发的清单（公开页是快照的标签并集，管理端是 ListTags，两者都按折叠键排序），页面不自己汇总、
// 排序：折叠规则只在 hub 一处。节点上的写法与清单不同（db / DB）也按 sameTag 计进同一个选项。
export function tagOptions(nodes: readonly { tags: readonly string[] }[], tags: readonly string[]): FacetOption[] {
  return tags.map((tag) => ({ value: tag, label: tag, count: nodes.filter((n) => n.tags.some((t) => sameTag(t, tag))).length }));
}

// 地区取并集：一个节点只有一个地区，选了至少两个地区时交集必然为空，所以地区没有匹配方式。
// 空选择表示不过滤、匹配一切——与 some 对空数组恒假的方向相反，这一放宽分支显式写出。
export function matchesRegion(country: string, regions: readonly string[]): boolean {
  return regions.length === 0 || regions.includes(country);
}
