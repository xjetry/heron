import type { PublicNode } from "../gen/heron/v1/public_pb";
import { bytes } from "../lib/format";
import { STATUS_LABEL, STATUS_ORDER } from "../lib/status";
import { summarize } from "./filters";

// 只汇总传入节点；调用方应传入与墙、卡片相同的筛选结果，保证计数与可见节点一致。
export function StatusSummary({ nodes }: { nodes: readonly PublicNode[] }) {
  const s = summarize(nodes);
  const segments = STATUS_ORDER.map((status) => ({ status, count: s.counts[status] }));
  return (
    <section className="summary" aria-label="汇总">
      <strong className="summary-count num">{s.counts.online} / {s.total} 在线</strong>
      <div className="segments" role="img" aria-label={segments.map(({ status, count }) => `${STATUS_LABEL[status]} ${count}`).join("、")}>
        {/* 空快照不计算比例，非空快照也不生成零计数段。 */}
        {s.total > 0 && segments.filter((seg) => seg.count > 0).map(({ status, count }) => (
          <span key={status} data-status={status} style={{ width: `${(count / s.total) * 100}%` }} />
        ))}
      </div>
      <span className="summary-rates num" role="group" aria-label="实时合计">↓ {bytes(s.rxBps)}/s ↑ {bytes(s.txBps)}/s</span>
      <span className="summary-period num">本周期 {bytes(s.periodBytes)}</span>
    </section>
  );
}
