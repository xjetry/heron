import type { CoverageSummary } from "../gen/heron/v1/query_pb";

// 上报覆盖率的显示口径（与 proto CoverageSummary 的注释一致）：
// QueryMetrics 的成功响应总是带 coverage_summary（internal/hub/api/history.go 的 metrics 在读到桶之后填充）。
// 缺席不是覆盖率为零，调用方不得把它当成“不显示”。
// - coverage_start 缺席：hub 没有留存的覆盖起点，可能是从未上报，也可能是删除后经配置层恢复、
//   历史已丢失，两种情形无法区分，都不能说成"从未上报"；
// - observed_minutes = 0：窗口内没有 hub 能观测的分钟，分母为零，算出 NaN/Infinity 没有意义；
// - 否则覆盖率 = observed_reported ÷ observed，未知分钟 = eligible − observed（守恒式
//   0 ≤ observed_reported ≤ observed ≤ eligible 由 hub 汇总保证），未知不计入分母。
export type CoverageView =
  | { kind: "no-start" }
  | { kind: "no-observed" }
  | { kind: "rate"; percent: string; unknown: string | null };

export function coverageView(summary: CoverageSummary | undefined): CoverageView {
  if (!summary) throw new Error("QueryMetrics.coverage_summary is always set");
  if (summary.coverageStart === undefined) return { kind: "no-start" };
  if (summary.observedMinutes === 0n) return { kind: "no-observed" };
  // 显示值不超过真实值：按千分比整数向下截断到 0.1%。四舍五入会把 99.96% 显示成 100.0%，把漏报说成没有漏报。
  // 用 bigint 整数除法是因为它按定义精确，截断结果不取决于浮点运算的次序（先乘 100 再截断会把恰好 57% 算成 56.9%）。
  const permille = (summary.observedReportedMinutes * 1000n) / summary.observedMinutes;
  const percent = `${permille / 10n}.${permille % 10n}`;
  const unknownMinutes = summary.eligibleMinutes - summary.observedMinutes;
  return { kind: "rate", percent, unknown: unknownMinutes > 0n ? spanText(unknownMinutes) : null };
}

// 未知时长按量级选单位：满 2 天按天、满 2 小时按小时、其余按分钟；非整除保留一位小数。
function spanText(minutes: bigint): string {
  const m = Number(minutes);
  if (m >= 2 * 1440) return `${trim(m / 1440)} 天`;
  if (m >= 2 * 60) return `${trim(m / 60)} 小时`;
  return `${m} 分钟`;
}

const trim = (v: number) => (Number.isInteger(v) ? String(v) : v.toFixed(1));
