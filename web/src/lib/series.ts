import type { AlignedData } from "uplot";
import type { QueryMetricsResponse } from "../gen/heron/v1/query_pb";

// x 轴按 step 补全 [from, to) 的网格；起点向下对齐到 step 的整数倍，与 hub 的点对齐规则一致。
export function gridOf(step: number, from: number, to: number): number[] {
  const start = from - (from % step);
  const xs: number[] = [];
  for (let t = start; t < to; t += step) xs.push(t);
  return xs;
}

export type SeriesSelection =
  | { name: string; value: "mean" | "sum-rate" | "max"; label: string }
  // ratio 必须先求和再相除；hub 的 mean 是 sum / n，toAligned 用同一时间点的 mean × n 还原两列的和，再求百分比。
  // 分母为 0、任一侧 n = 0 或缺序列时是空洞：比值没有定义，不画成 0。
  | { name: string; value: "ratio"; over: string; label: string };

// 面板显式选择统计量，同一指标可同时画均值和峰值。缺桶、n = 0 或所选字段缺失均为空洞，
// 不借用其它统计量补齐；sum-rate 用整个桶宽换算速率，让不同聚合级别在同一根轴上可比。
export function toAligned(resp: QueryMetricsResponse, selections: SeriesSelection[], from: number, to: number): AlignedData {
  const step = resp.stepS;
  const xs = gridOf(step, from, to);
  const at = new Map<number, number>();
  resp.ts.forEach((t, i) => at.set(Number(t), i));
  const columns = selections.map((selection) => {
    const s = resp.series.find((x) => x.name === selection.name);
    const over = selection.value === "ratio" ? resp.series.find((x) => x.name === selection.over) : undefined;
    return xs.map((t) => {
      const i = at.get(t);
      const sample = i === undefined ? undefined : s?.samples[i];
      if (sample === undefined || sample.n === 0) return null;
      if (selection.value === "sum-rate") return sample.sum === undefined ? null : sample.sum / step;
      if (selection.value === "ratio") {
        const d = i === undefined ? undefined : over?.samples[i];
        if (d === undefined || d.n === 0 || sample.mean === undefined || d.mean === undefined) return null;
        const den = d.mean * d.n;
        return den > 0 ? (sample.mean * sample.n / den) * 100 : null;
      }
      return sample[selection.value] ?? null;
    });
  });
  return [xs, ...columns] as AlignedData;
}

export function unitOf(resp: QueryMetricsResponse, name: string): string {
  return resp.series.find((x) => x.name === name)?.unit ?? "";
}
