import type { Node, NodeStatus as LiveNode } from "../gen/heron/v1/admin_pb";
import { liveStatus } from "./adminStatus";
import { matchesRegion, regionName, regionOptions, tagOptions, type FacetModeKeys, type FacetOption } from "./facets";
import { filterNodes } from "./nodeSearch";
import { laggingNode } from "./lagging";
import { expiryLevel, STATUS_LABEL, STATUS_ORDER, type NodeStatus } from "./status";
import { matchesTags, sameTag, type TagMatch } from "./tags";

export type ScopeFilters = { status: NodeStatus | null; expiring: boolean; lagging: boolean };

export const STATUS_OPTIONS: readonly { value: NodeStatus; label: string }[] = STATUS_ORDER.map((value) => ({ value, label: STATUS_LABEL[value] }));

// 「需要处理」的卡链到这里（lib/attention.ts 的 to）：谓词必须与那边逐个相同，否则卡上的数与列表行数对不上。
// URL 是外部输入：不认识的值当没写，列表不能因为一个错字变空且无法清除。
export function scopeFromParams(params: URLSearchParams): ScopeFilters {
  const status = params.get("status");
  return {
    status: STATUS_ORDER.includes(status as NodeStatus) ? (status as NodeStatus) : null,
    expiring: params.get("expiring") === "1",
    lagging: params.get("lagging") === "1",
  };
}

export function paramsWithScope(params: URLSearchParams, scope: ScopeFilters): URLSearchParams {
  const next = new URLSearchParams(params);
  if (scope.status) next.set("status", scope.status); else next.delete("status");
  if (scope.expiring) next.set("expiring", "1"); else next.delete("expiring");
  if (scope.lagging) next.set("lagging", "1"); else next.delete("lagging");
  return next;
}

export const NO_SCOPE: ScopeFilters = { status: null, expiring: false, lagging: false };
export const isScoped = (scope: ScopeFilters): boolean => scope.status !== null || scope.expiring || scope.lagging;

// 视图级排序（sort=expiry），同样由 URL 持有。它不是筛选：不收窄集合、不出现在「清除筛选」里，但显示顺序不再是
// 服务端全序，拖动与上下移（它们保存完整排列）与筛选时一样关闭。只认 expiry，其余当没写。
export type NodeSort = "expiry" | null;
export const sortFromParams = (params: URLSearchParams): NodeSort => (params.get("sort") === "expiry" ? "expiry" : null);

export function paramsWithSort(params: URLSearchParams, sort: NodeSort): URLSearchParams {
  const next = new URLSearchParams(params);
  if (sort) next.set("sort", sort); else next.delete("sort");
  return next;
}

// 按到期排序：剩余天数升序，已过期（负数）自然在最前；没有剩余天数的（没填到期日）排最后并保持传入顺序
// （Array.prototype.sort 是稳定的）。剩余天数取 hub 下发的 days_left，与到期上色同一个数。
export function applySort(nodes: readonly Node[], sort: NodeSort): Node[] {
  if (sort === null) return [...nodes];
  const days = (node: Node): number | undefined => node.billing?.daysLeft;
  return [...nodes].sort((a, b) => {
    const x = days(a), y = days(b);
    if (x === undefined) return y === undefined ? 0 : 1;
    if (y === undefined) return -1;
    return x - y;
  });
}

// 标签过滤二选一：按一组标签过滤（names 为空表示不过滤），或只要无标签节点。用判别式联合让"既选了标签又选了无标签"
// 在类型上不可表示：两者的交集必然为空，面板上它们互斥（components/Facet.tsx 的 FacetExclusive 依赖这一点——写标签
// 选择即取消无标签）。
export type TagFilter = { kind: "tags"; names: string[] } | { kind: "untagged" };
export const NO_TAG_FILTER: TagFilter = { kind: "tags", names: [] };

// 搜索词、地区与标签过滤同样由 URL 持有（q；region 与 tag 可重复，未知地区写空值 region=；untagged=1；match=any），
// 从详情返回列表时连同其余筛选一起还原。URL 是外部输入：untagged=1 与 tag 同时出现时取无标签（两者只能二选一），
// 空的 tag 丢弃。
export function tagFilterFromParams(params: URLSearchParams): TagFilter {
  if (params.get("untagged") === "1") return { kind: "untagged" };
  return { kind: "tags", names: params.getAll("tag").filter((name) => name !== "") };
}

export function paramsWithTagFilter(params: URLSearchParams, filter: TagFilter): URLSearchParams {
  const next = new URLSearchParams(params);
  next.delete("tag");
  next.delete("untagged");
  if (filter.kind === "untagged") next.set("untagged", "1");
  else for (const name of filter.names) next.append("tag", name);
  return next;
}

// 多选标签默认同时满足（与 ListNodes 的 tags 参数同一口径），缺省不写进 URL；只认 any，其余当没写。
export const tagMatchFromParams = (params: URLSearchParams): TagMatch => (params.get("match") === "any" ? "any" : "all");

export function paramsWithTagMatch(params: URLSearchParams, match: TagMatch): URLSearchParams {
  const next = new URLSearchParams(params);
  if (match === "any") next.set("match", "any"); else next.delete("match");
  return next;
}

// 地区是 Node.country 的写法：两个大写字母，空串表示未知。URL 里的小写换成大写，别的写法当没写（不能让一个错字变成
// 看不见的过滤条件），重复的去掉。
export function regionsFromParams(params: URLSearchParams): string[] {
  const regions: string[] = [];
  for (const raw of params.getAll("region")) {
    const code = raw.toUpperCase();
    if ((code === "" || /^[A-Z]{2}$/.test(code)) && !regions.includes(code)) regions.push(code);
  }
  return regions;
}

export function paramsWithRegions(params: URLSearchParams, regions: readonly string[]): URLSearchParams {
  const next = new URLSearchParams(params);
  next.delete("region");
  for (const code of regions) next.append("region", code);
  return next;
}

export const searchFromParams = (params: URLSearchParams): string => params.get("q") ?? "";

export function paramsWithSearch(params: URLSearchParams, search: string): URLSearchParams {
  const next = new URLSearchParams(params);
  if (search !== "") next.set("q", search); else next.delete("q");
  return next;
}

// URL 里的标签名按现有标签清单折叠比较、换成清单里的写法并去重：面板按写法精确匹配胶囊，换写法之前 URL 里的「DB」
// 既不算从清单消失的标签、又对不上选项「db」，会成为看不见的过滤条件。清单里没有的原样保留，由 nodeFacetOptions 作为
// 可取消的选项列出（与已选标签从清单消失同一处理）；清单未到或取不到时整体原样使用。
export function canonicalTagFilter(filter: TagFilter, known: readonly string[] | undefined): TagFilter {
  if (filter.kind === "untagged" || known === undefined) return filter;
  const names: string[] = [];
  for (const name of filter.names) {
    const canonical = known.find((tag) => sameTag(tag, name)) ?? name;
    if (!names.some((n) => sameTag(n, canonical))) names.push(canonical);
  }
  return { kind: "tags", names };
}

// 从节点列表进入详情时，列表把自己的查询串放进导航 state；详情页的「返回节点列表」据此回到同一组筛选。
// state 来自浏览器历史，按外部输入处理：只认以 "?" 开头的字符串，其余一律回到不带筛选的列表。
export const nodeListReturnState = (search: string) => ({ nodeListSearch: search });

export function nodeListReturnPath(state: unknown): string {
  const search = typeof state === "object" && state !== null ? (state as { nodeListSearch?: unknown }).nodeListSearch : undefined;
  return typeof search === "string" && search.startsWith("?") ? `/nodes${search}` : "/nodes";
}

// 节点页的地区与标签筛选入口（components/Facet.tsx）。选择方式是浏览器偏好，键与公开页的分开；选择本身在 URL。
export const NODE_FACET_MODE_KEYS: FacetModeKeys = { region: "heron-admin-nodes-region-mode", tag: "heron-admin-nodes-tag-mode" };

export type NodeFacets = { regions: readonly string[]; tags: TagFilter; tagMatch: TagMatch };
export const NO_FACETS: NodeFacets = { regions: [], tags: NO_TAG_FILTER, tagMatch: "all" };

export const facetsFromParams = (params: URLSearchParams, knownTags: readonly string[] | undefined): NodeFacets => ({
  regions: regionsFromParams(params), tags: canonicalTagFilter(tagFilterFromParams(params), knownTags), tagMatch: tagMatchFromParams(params),
});

export const paramsWithFacets = (params: URLSearchParams, facets: NodeFacets): URLSearchParams =>
  paramsWithTagMatch(paramsWithTagFilter(paramsWithRegions(params, facets.regions), facets.tags), facets.tagMatch);

// 地区或标签让列表成为子集。匹配方式单独不收窄：它只在选了至少两个标签时影响结果，而那时标签本身已经算数。
export const isFaceted = (facets: NodeFacets): boolean =>
  facets.regions.length > 0 || facets.tags.kind === "untagged" || facets.tags.names.length > 0;

// 面板的选项与计数按全部节点算（与公开页按快照算同一做法），不随其他筛选变化；标签的集合与顺序取 ListTags（清单未到时为空）。
// 已选却不在选项里的地区与标签（URL 带来的、在别处删掉或改掉的）仍列在最后、计数照算：否则它们成了看不见、取消不了的
// 过滤条件。标签已按清单换过写法（canonicalTagFilter），这里与清单逐字比较即可。
export function nodeFacetOptions(nodes: readonly Node[], knownTags: readonly string[] | undefined, facets: NodeFacets): { regions: FacetOption[]; tags: FacetOption[] } {
  const regions = regionOptions(nodes);
  const missingRegions = facets.regions.filter((code) => !regions.some((option) => option.value === code));
  const known = knownTags ?? [];
  const missingTags = facets.tags.kind === "tags" ? facets.tags.names.filter((name) => !known.includes(name)) : [];
  return {
    regions: [...regions, ...missingRegions.map((code) => ({ value: code, label: regionName(code), count: 0 }))],
    tags: tagOptions(nodes, [...known, ...missingTags]),
  };
}

// 过滤在浏览器里做：节点页本就轮询全部节点（ListNodes 不带条件），各筛选彼此取交集。地区之间取并集（matchesRegion），
// 标签按 tagMatch 取交集或并集（matchesTags，标签名按折叠比较），无标签只留没有任何标签的节点。
export function applyScope(nodes: readonly Node[], live: ReadonlyMap<bigint, LiveNode>, boundAgentVersion: string | undefined, scope: ScopeFilters, search: string, facets: NodeFacets): Node[] {
  return filterNodes(nodes, search).filter((node) => {
    if (!matchesRegion(node.country, facets.regions)) return false;
    if (facets.tags.kind === "untagged" ? node.tags.length > 0 : !matchesTags(node.tags, facets.tags.names, facets.tagMatch)) return false;
    if (scope.status && liveStatus(node, live.get(node.id)) !== scope.status) return false;
    if (scope.expiring && expiryLevel(node.billing?.daysLeft, node.rented) === "neutral") return false;
    // 绑定版本未知时谁都判不出落后：结果为空，调用方说明原因（与「需要处理」卡上的「—」同一含义）。已出租同样不落后。
    if (scope.lagging && !laggingNode(node, boundAgentVersion)) return false;
    return true;
  });
}
