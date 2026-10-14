import type { Facet } from "../lib/facets";
import { readChoice, writeChoice } from "../lib/storedChoice";
import type { TagMatch } from "../lib/tags";
import { GROUP_BYS, type GroupBy } from "./filters";

export type View = "wall" | "cards" | "list";
export const PUBLIC_VIEW_KEY = "heron-public-view";
export const PUBLIC_WALL_GROUP_KEY = "heron-public-wall-group";
const VIEWS: readonly View[] = ["wall", "cards", "list"];

// 地区与标签筛选各自的选择方式（lib/facets.ts 的 useFacetModes 按这两个键读写）。
export const PUBLIC_FACET_MODE_KEYS: Readonly<Record<Facet, string>> = { region: "heron-public-region-mode", tag: "heron-public-tag-mode" };
export const PUBLIC_TAG_MATCH_KEY = "heron-public-tag-match";
const TAG_MATCHES: readonly TagMatch[] = ["all", "any"];

// 访客的视图、状态墙分组与标签多选的匹配方式按浏览器记住（lib/storedChoice），不认识的值用默认值：视图默认卡片（卡片视图
// 只放在线与维护中节点，离线、从未上报与已出租列在网格下方），分组默认地区，标签匹配默认同时满足（与管理端一致）。
export const readPublicView = (): View => readChoice(PUBLIC_VIEW_KEY, VIEWS, "cards");
export const writePublicView = (view: View): void => writeChoice(PUBLIC_VIEW_KEY, view);
export const readWallGroupBy = (): GroupBy => readChoice(PUBLIC_WALL_GROUP_KEY, GROUP_BYS.map((option) => option.value), "region");
export const writeWallGroupBy = (by: GroupBy): void => writeChoice(PUBLIC_WALL_GROUP_KEY, by);
export const readTagMatch = (): TagMatch => readChoice(PUBLIC_TAG_MATCH_KEY, TAG_MATCHES, "all");
export const writeTagMatch = (match: TagMatch): void => writeChoice(PUBLIC_TAG_MATCH_KEY, match);
