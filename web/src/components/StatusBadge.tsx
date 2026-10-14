import { STATUS_LABEL, type NodeStatus } from "../lib/status";

// 状态文字承载含义，色点只是视觉辅助，对读屏隐藏以免重复播报。
export function StatusBadge({ status, detail }: { status: NodeStatus; detail?: string }) {
  return (
    <span className={`status-badge is-${status}`} data-status={status}>
      <span className="status-dot" aria-hidden="true" />
      {detail ? `${STATUS_LABEL[status]} · ${detail}` : STATUS_LABEL[status]}
    </span>
  );
}
