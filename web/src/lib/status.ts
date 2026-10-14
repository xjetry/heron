// 一个节点只属于一种状态，在线计数、汇总条、状态墙与徽章共用此判定。
// 已出租表示机器租给了别人、不会有上报，离线是预期；维护中表示站长主动摘出节点。两者都由站长设置，
// 互斥由 hub 的保存入口保证；已出租排在最前，即使还有上报也不计入在线。
// proto 以 lastSeenAt 缺失表示从未上报；hub 的 liveState 对这类节点返回 online=false。
export type NodeStatus = "online" | "offline" | "never" | "maintenance" | "rented";
export type StatusInput = { online: boolean; maintenance: boolean; rented: boolean; lastSeenAt?: bigint };

export function nodeStatus(n: StatusInput): NodeStatus {
  if (n.rented) return "rented";
  if (n.maintenance) return "maintenance";
  if (n.lastSeenAt === undefined) return "never";
  return n.online ? "online" : "offline";
}

export const STATUS_LABEL: Record<NodeStatus, string> = { online: "在线", offline: "离线", never: "从未上报", maintenance: "维护中", rented: "已出租" };
// 汇总条分段与图例共用固定顺序。
export const STATUS_ORDER: readonly NodeStatus[] = ["online", "maintenance", "rented", "never", "offline"];

export type Level = "neutral" | "attention" | "critical";

// 进度条两个边界都归需关注：<70 中性、70 至 90 琥珀、>90 玫红。
export function usageLevel(percent: number): Level {
  if (percent > 90) return "critical";
  if (percent >= 70) return "attention";
  return "neutral";
}

// 「N 天内到期」的 N：到期上色的需关注上边界、总览卡片与节点筛选的「N 天内到期」都从这里取，
// 三处必须是同一个数，否则卡上的计数与筛出的行数对不上。
export const EXPIRING_DAYS = 15;

// 缺失到期日不表示已过期：只有负数归严重，0 至 EXPIRING_DAYS 天归需关注。已出租恒为中性：出租期间的续费是
// 租约的事，hub 不替站长标；rented 必填，让每个读到期的地方都显式带上它。
export function expiryLevel(daysLeft: number | undefined, rented: boolean): Level {
  if (rented || daysLeft === undefined) return "neutral";
  if (daysLeft < 0) return "critical";
  return daysLeft <= EXPIRING_DAYS ? "attention" : "neutral";
}
