import { olderThan } from "./version";

// 「agent 版本落后」只有这一处判定：总览卡、节点筛选、节点行与详情页徽章、更新页都用它。已出租节点没有 agent，
// 不算落后；绑定版本未知时谁都判不出；agent 版本缺失或不是正式版按 olderThan 的口径不算落后。
// 只读这两个字段：更新页版本列比的是更新状态里的版本，调用方把它放进同一形状，不另写一套比较。
export function laggingNode(node: { rented: boolean; facts?: { agentVersion?: string } }, bound: string | undefined): boolean {
  return bound !== undefined && !node.rented && olderThan(node.facts?.agentVersion, bound);
}
