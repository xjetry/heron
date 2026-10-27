import { usage } from "../components/Bar";
import type { PublicNode } from "../gen/heron/v1/public_pb";
import { sortByExpiry } from "../lib/billing";
import { matchesRegion, regionName } from "../lib/facets";
import { literalPattern } from "../lib/fold";
import { expiryLevel, nodeStatus, STATUS_ORDER, usageLevel, type Level, type NodeStatus } from "../lib/status";
import { matchesTags, sameTag, type TagMatch } from "../lib/tags";

// 节点状态筛选把五态分成互斥且互补的三档：在线就是 nodeStatus 的「在线」；已出租单独一档——机器租出去了、不会有上报，
// 离线是它的常态而不是故障，即使仍在上报也归这一档（与 nodeStatus 先判已出租的次序一致）；离线是其余三态（离线、
// 从未上报、维护中）。互补保证「在线 + 离线 + 已出租 = 全部」，四个计数对得上。
export type StatusFilter = "all" | "online" | "offline" | "rented";
export const STATUS_FILTERS: readonly { value: StatusFilter; label: string }[] = [
  { value: "all", label: "全部" }, { value: "online", label: "在线" }, { value: "offline", label: "离线" }, { value: "rented", label: "已出租" },
];

function statusTier(node: PublicNode): Exclude<StatusFilter, "all"> {
  const status = nodeStatus(node);
  return status === "online" || status === "rented" ? status : "offline";
}

function matchesStatus(node: PublicNode, status: StatusFilter): boolean {
  return status === "all" || statusTier(node) === status;
}

// tagMatch 只在选了至少两个标签时影响结果；默认交集，与管理端的标签过滤同一口径。
export type PublicFilters = { search: string; regions: readonly string[]; tags: readonly string[]; tagMatch: TagMatch; status: StatusFilter };
export const NO_FILTERS: PublicFilters = { search: "", regions: [], tags: [], tagMatch: "all", status: "all" };

// 名称、标签与公开备注共用 lib/fold 的折叠字面匹配，不把访客输入当成正则表达式。
export function matchesSearch(node: PublicNode, search: string): boolean {
  if (search === "") return true;
  const pattern = literalPattern(search, false);
  return pattern.test(node.name) || node.tags.some((tag) => pattern.test(tag)) || pattern.test(node.publicRemark);
}

// 地区取并集（matchesRegion），标签按 tagMatch 取交集或并集，再与搜索及节点状态取交集。两种空选择与状态「全部」都匹配一切，
// 由各自的谓词显式处理。
export function filterPublicNodes(nodes: readonly PublicNode[], f: PublicFilters): PublicNode[] {
  return nodes.filter((n) =>
    matchesSearch(n, f.search)
    && matchesRegion(n.country, f.regions)
    && matchesTags(n.tags, f.tags, f.tagMatch)
    && matchesStatus(n, f.status));
}

// 各档计数按其余筛选（搜索、地区、标签）算，不看状态筛选本身：切换状态时数字不变，访客能先看到每一档有多少台再选。
export function statusCounts(nodes: readonly PublicNode[], f: PublicFilters): Record<StatusFilter, number> {
  const scope = filterPublicNodes(nodes, { ...f, status: "all" });
  const counts: Record<StatusFilter, number> = { all: scope.length, online: 0, offline: 0, rented: 0 };
  for (const n of scope) counts[statusTier(n)]++;
  return counts;
}

export type NodeSort = "default" | "expiry" | "cpu" | "traffic";
export const NODE_SORTS: readonly { value: NodeSort; label: string }[] = [
  { value: "default", label: "默认" }, { value: "expiry", label: "到期" }, { value: "cpu", label: "CPU" }, { value: "traffic", label: "流量" },
];

// 缺读数排最后，相等保持传入顺序；直接比较 bigint，避免字节数超过安全整数后丢失顺序。
function byDesc(key: (n: PublicNode) => number | bigint | undefined) {
  return (a: PublicNode, b: PublicNode) => {
    const ka = key(a), kb = key(b);
    if (ka === kb) return 0;
    if (ka === undefined) return 1;
    if (kb === undefined) return -1;
    return ka < kb ? 1 : -1;
  };
}

export function sortNodes(nodes: readonly PublicNode[], sort: NodeSort): PublicNode[] {
  switch (sort) {
    case "expiry": return sortByExpiry(nodes);
    case "cpu": return [...nodes].sort(byDesc((n) => n.metrics?.cpuPct));
    // 按卡片与列表上显示的那个数排序：本周期用量按节点自己的配额口径计（与 trafficText 同源）。
    case "traffic": return [...nodes].sort(byDesc((n) => n.traffic?.quotaUsedBytes));
    default: return [...nodes];
  }
}

export type ColorBy = "status" | "cpu" | "memory" | "expiry";
export const COLOR_BYS: readonly { value: ColorBy; label: string }[] = [
  { value: "status", label: "状态" }, { value: "cpu", label: "CPU" }, { value: "memory", label: "内存" }, { value: "expiry", label: "到期" },
];

// 状态着色来自五态，不给使用量档位；缺使用量读数不给档位，缺到期日与已出租由 expiryLevel 判为中性。
export function tileLevel(node: PublicNode, by: ColorBy): Level | undefined {
  const m = node.metrics;
  switch (by) {
    case "cpu": return m?.cpuPct === undefined ? undefined : usageLevel(m.cpuPct);
    case "memory": {
      const mem = usage(m?.memUsed, m?.memTotal);
      return mem ? usageLevel(mem.pct) : undefined;
    }
    case "expiry": return expiryLevel(node.billing?.daysLeft, node.rented);
    default: return undefined;
  }
}

export type GroupBy = "region" | "tag";
export const GROUP_BYS: readonly { value: GroupBy; label: string }[] = [{ value: "region", label: "地区" }, { value: "tag", label: "标签" }];

// online 与 rented 是组内按五态数出的在线数与已出租数；组头写「在线 / 未出租的总数」，已出租另计，
// 租出去的机器不会有上报，不能让它看起来像离线。
export type WallGroup = { key: string; name: string; nodes: PublicNode[]; online: number; rented: number };

const countOnline = (nodes: readonly PublicNode[]) => nodes.filter((n) => nodeStatus(n) === "online").length;
const countRented = (nodes: readonly PublicNode[]) => nodes.filter((n) => nodeStatus(n) === "rented").length;

// 组头文案与可访问名共用：在线 / 未出租总数，有已出租时再带上它的数。
export function groupHead(g: WallGroup): string {
  return `${g.online} / ${g.nodes.length - g.rented} 在线${g.rented > 0 ? ` · 已出租 ${g.rented}` : ""}`;
}

// 两种分组同一排序：兜底组（未知地区、无标签）固定最后；其余按五态在线数降序、总数降序，再按各自的 tiebreak。
function ordered(groups: WallGroup[], fallbackKey: string, tiebreak: (a: WallGroup, b: WallGroup) => number): WallGroup[] {
  return groups.sort((a, b) => {
    if ((a.key === fallbackKey) !== (b.key === fallbackKey)) return a.key === fallbackKey ? 1 : -1;
    return b.online - a.online || b.nodes.length - a.nodes.length || tiebreak(a, b);
  });
}

// 组键是国家代码，未知地区为空串；同在线数、同总数时按代码排。
export function groupByRegion(nodes: readonly PublicNode[]): WallGroup[] {
  const groups = new Map<string, WallGroup>();
  for (const n of nodes) {
    const g = groups.get(n.country) ?? { key: n.country, name: regionName(n.country), nodes: [], online: 0, rented: 0 };
    g.nodes.push(n);
    groups.set(n.country, g);
  }
  for (const g of groups.values()) {
    g.online = countOnline(g.nodes);
    g.rented = countRented(g.nodes);
  }
  return ordered([...groups.values()], "", (a, b) => a.key.localeCompare(b.key));
}

const UNTAGGED = "untagged";

// 组的集合、写法与 tiebreak 次序取 hub 下发的标签并集（按折叠键排序），与标签筛选同源；节点标签按 sameTag 折叠比较归组。
// 一个节点出现在它的每个标签组里，各组计数独立，组计数之和可以大于节点数。并集覆盖全部公开节点的标签，
// 落不进任何组的只有没有标签的节点，归入「无标签」。筛选后没有节点的标签不成组。
export function groupByTag(nodes: readonly PublicNode[], tags: readonly string[]): WallGroup[] {
  const groups: WallGroup[] = [];
  const placed = new Set<PublicNode>();
  for (const tag of tags) {
    const members = nodes.filter((n) => n.tags.some((t) => sameTag(t, tag)));
    for (const n of members) placed.add(n);
    if (members.length > 0) groups.push({ key: `tag:${tag}`, name: tag, nodes: members, online: countOnline(members), rented: countRented(members) });
  }
  const rest = nodes.filter((n) => !placed.has(n));
  if (rest.length > 0) groups.push({ key: UNTAGGED, name: "无标签", nodes: rest, online: countOnline(rest), rented: countRented(rest) });
  const order = new Map(tags.map((tag, i) => [`tag:${tag}`, i]));
  return ordered(groups, UNTAGGED, (a, b) => order.get(a.key)! - order.get(b.key)!);
}

export type Summary = { total: number; counts: Record<NodeStatus, number>; rxBps: bigint; txBps: bigint; periodBytes: bigint };

// 五态互斥，计数之和等于总数。实时速率只含在线与维护中：离线的最后读数是旧值，已出租不参与合计；
// 本周期流量是 hub 累计值，离线与已出租节点也保留其贡献。
export function summarize(nodes: readonly PublicNode[]): Summary {
  const counts = Object.fromEntries(STATUS_ORDER.map((s) => [s, 0])) as Record<NodeStatus, number>;
  let rxBps = 0n, txBps = 0n, periodBytes = 0n;
  for (const n of nodes) {
    const status = nodeStatus(n);
    counts[status]++;
    if (status === "online" || status === "maintenance") {
      rxBps += n.metrics?.netRxBps ?? 0n;
      txBps += n.metrics?.netTxBps ?? 0n;
    }
    // 全站合计是实际收发的字节数，不是各节点配额口径的分子：只收、只发、取大者的分子相加没有物理含义。
    if (n.traffic) periodBytes += n.traffic.periodRx + n.traffic.periodTx;
  }
  return { total: nodes.length, counts, rxBps, txBps, periodBytes };
}
