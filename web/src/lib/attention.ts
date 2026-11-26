import type { AlertStateEntry, Node, NodeStatus as LiveNode } from "../gen/heron/v1/admin_pb";
import { liveStatus } from "./adminStatus";
import { laggingNode } from "./lagging";
import { EXPIRING_DAYS, expiryLevel } from "./status";

export type AttentionCard = { key: "offline" | "expiring" | "lagging" | "firing"; label: string; count: number | null; note?: string; to: string };

// 与 web/index.html 的默认标题一致；原标题为空（如测试环境未预置 document.title）时作为有告警前缀的兜底标题。
export const DEFAULT_ADMIN_TITLE = "Heron";

// 触发中告警条数：总览待办卡与管理端标签页标题共用这一个判定，避免两处口径漂移。
// states 为 undefined 表示尚未取得或请求失败，返回 null（与「确认为 0 条」区分）。
export function firingCount(states: readonly AlertStateEntry[] | undefined): number | null {
  return states === undefined ? null : states.filter((s) => s.state === "firing").length;
}

// 管理端标签页标题：仅在已知存在触发中告警时加数量前缀，无触发或状态未知时保持原标题。
export function alertTabTitle(baseTitle: string, firing: number | null): string {
  if (firing === null || firing <= 0) return baseTitle;
  return `(${firing}) ${baseTitle || DEFAULT_ADMIN_TITLE}`;
}

// 总览待办卡与筛选同源的规则见 web/src/pages/Overview.tsx 的 Overview 注释；到期卡另带按到期排序，
// 最紧的排最前。
// count 为 null 表示依赖的数据没拿到（绑定版本、告警状态），显示为「—」；0 才是真的没有。
export function attentionCards({ nodes, live, boundAgentVersion, states }: {
  nodes: readonly Node[]; live: ReadonlyMap<bigint, LiveNode>; boundAgentVersion: string | undefined; states: readonly AlertStateEntry[] | undefined;
}): AttentionCard[] {
  const statuses = nodes.map((n) => liveStatus(n, live.get(n.id)));
  const offline = statuses.filter((s) => s === "offline").length;
  const never = statuses.filter((s) => s === "never").length;
  // 卡片计数包含 expiryLevel 的需关注与严重两档，不能漏掉已过期节点。已出租被判为中性，不进这张卡。
  const expiring = nodes.filter((n) => expiryLevel(n.billing?.daysLeft, n.rented) !== "neutral");
  const expired = expiring.filter((n) => (n.billing?.daysLeft ?? 0) < 0).length;
  // 绑定版本未知时计数是 null（「—」），不是 0：laggingNode 在绑定未知时为 false，直接数会把「没拿到绑定」说成「没有落后」。
  const lagging = boundAgentVersion === undefined ? null : nodes.filter((n) => laggingNode(n, boundAgentVersion)).length;
  return [
    { key: "offline", label: "离线", count: offline, note: `从未上报 ${never}`, to: "/nodes?status=offline" },
    { key: "expiring", label: `${EXPIRING_DAYS} 天内到期`, count: expiring.length, note: `已过期 ${expired}`, to: "/nodes?expiring=1&sort=expiry" },
    { key: "lagging", label: "agent 版本落后", count: lagging, note: boundAgentVersion === undefined ? "无法取得 hub 绑定的 agent 版本" : `低于 ${boundAgentVersion}`, to: "/nodes?lagging=1" },
    { key: "firing", label: "触发中告警", count: firingCount(states), to: "/alerts?state=firing" },
  ];
}
