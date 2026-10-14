import { usageLevel } from "../lib/status";

// 无读数与 0 是两个事实：缺失的字段显示为破折号，不画成 0。
export function Missing() {
  return <span className="muted" aria-label="无读数">–</span>;
}

export function Bar({ value, label, thin = false }: { value: number; label: string; thin?: boolean }) {
  const v = Math.max(0, Math.min(100, value));
  return (
    <div className={thin ? "bar thin" : "bar"} data-level={usageLevel(v)} role="meter" aria-valuenow={Math.round(v)} aria-valuemin={0} aria-valuemax={100} aria-label={label}>
      <div className="fill" style={{ width: `${v}%` }} />
      <span>{label}</span>
    </div>
  );
}

export type Usage = { used: bigint; total: bigint; pct: number };

// 容量类读数（内存、交换、磁盘）的使用率只在这里算：公开页卡片、状态墙方块与详情面板、着色档位、节点页现值格、
// 列表与管理端总览都调它，"有没有读数"的判定不会各写一份而走散。
// total 缺失或为 0 时没有分母，used 缺失时没有分子，都返回 undefined 让调用方按无读数显示，而不是算成 0%。
// 有读数时把已确认存在的 used / total 一并带回，调用方写"用量 / 总量"不必再判一次缺失。
export function usage(used: bigint | undefined, total: bigint | undefined): Usage | undefined {
  if (used === undefined || !total) return undefined;
  return { used, total, pct: (Number(used) / Number(total)) * 100 };
}
