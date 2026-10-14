import type { AlignedData } from "uplot";
import type { ProbeSample } from "../gen/heron/v1/query_pb";
import { disambiguate, lossPercent, rttMeanMs, type ProbeValue } from "./probes";
import { gridOf } from "./series";

// 一次对比先 List 再按 List 给出的上限切块。上限缺席即 0（proto3 标量），0 不是“不限制”：
// 没有可以退回的固定块大小，调用方必须停下来报协议错误，而不是自己挑一个数继续请求。
export type ChunkPlan =
  | { ok: true; chunks: bigint[][] }
  | { ok: false; reason: "non-positive-limit" };

// 路由里的任务编号：只接受十进制正整数。0 与前导零不是任务 id 的写法，直接当无效路径。
export function taskIdParam(param: string | undefined): bigint | undefined {
  if (param === undefined || !/^[1-9][0-9]*$/.test(param)) return undefined;
  return BigInt(param);
}

export function chunkNodeIds(nodeIds: readonly bigint[], maxNodesPerQuery: number): ChunkPlan {
  if (!Number.isSafeInteger(maxNodesPerQuery) || maxNodesPerQuery < 1) {
    return { ok: false, reason: "non-positive-limit" };
  }
  const chunks: bigint[][] = [];
  for (let i = 0; i < nodeIds.length; i += maxNodesPerQuery) chunks.push(nodeIds.slice(i, i + maxNodesPerQuery));
  return { ok: true, chunks };
}

// 一块 QueryProbeComparison 的可见序列与不可见节点。stepS 由这一次窗口的选级决定，同一窗口的各块应当相同。
export type ComparisonChunk = {
  stepS: number;
  series: readonly { nodeId: bigint; samples: readonly ProbeSample[] }[];
  unavailableNodeIds: readonly bigint[];
};

export type ComparisonChart = {
  // 只含该图至少有一个读数的节点。整列没有读数的序列不放进来：Chart 会把它们的名字拼进一条提示，节点多时不可读。
  // ids 与 labels 同序同长。线的位置随每次窗口刷新变化（有读数的节点集合会变），调用方按 id 记偏好
  // （显隐、悬停），只在交给 Chart 时换算成当前索引；按索引记偏好会在集合变化后指到另一个节点。
  ids: bigint[];
  labels: string[];
  data: AlignedData;
  // 已经返回、且该图在窗口网格上没有任何读数的节点。还没返回的块里的节点不在这里——没取到不是“无结果”。
  missing: string[];
};

export type ComparisonAssembly = {
  // 每个候选都出现在某块的 series 或 unavailable 里，且各块 stepS 一致、为正。否则不能画：缺的块不是空序列。
  complete: boolean;
  stepS: number;
  loss: ComparisonChart;
  rtt: ComparisonChart;
  unavailable: string[];
};

function labelOf(id: bigint, names: ReadonlyMap<bigint, string>): string {
  const name = names.get(id);
  return name ? name : `#${id}`;
}

function column(samples: readonly ProbeSample[], xs: readonly number[], value: ProbeValue): (number | null)[] {
  const at = new Map<number, ProbeSample>();
  for (const sample of samples) at.set(Number(sample.ts), sample);
  return xs.map((t) => {
    const sample = at.get(t);
    return sample === undefined ? null : value(sample);
  });
}

// 0 是读数（全部本地错误的丢包率就是 0）。null 是没有读数，不能当成 0 画上去。
function isPlotted(v: number | null): v is number {
  return v !== null && Number.isFinite(v);
}

// nodeIds 是 List 的全序。chunks 只含已经返回的块：没出现在任何块里的节点既不画线，也不进“没有读数”。
// 丢包率只用 lossPercent（lost/sent），不因缺少 RTT 另记成 100%。RTT 图只保留至少有一个 RTT 的节点。
export function assembleComparison(
  nodeIds: readonly bigint[],
  names: ReadonlyMap<bigint, string>,
  chunks: readonly ComparisonChunk[],
  from: number,
  to: number,
): ComparisonAssembly {
  const seriesById = new Map<bigint, readonly ProbeSample[]>();
  const unavailable = new Set<bigint>();
  for (const chunk of chunks) {
    for (const series of chunk.series) {
      if (!seriesById.has(series.nodeId)) seriesById.set(series.nodeId, series.samples);
    }
    for (const id of chunk.unavailableNodeIds) unavailable.add(id);
  }
  const accounted = (id: bigint) => seriesById.has(id) || unavailable.has(id);
  const stepS = chunks[0]?.stepS ?? 0;
  const canGrid = stepS > 0 && chunks.every((chunk) => chunk.stepS === stepS);
  const complete = canGrid && nodeIds.every(accounted);
  const extraUnavailable = [...unavailable].filter((id) => !nodeIds.includes(id));
  const labelIds = [...nodeIds.filter(accounted), ...extraUnavailable];
  const labels = disambiguate(labelIds.map((id) => labelOf(id, names)), labelIds);
  const labelById = new Map(labelIds.map((id, i) => [id, labels[i]]));
  const xs = canGrid ? gridOf(stepS, from, to) : [];
  const chartFor = (value: ProbeValue): ComparisonChart => {
    const plotted: bigint[] = [];
    const cols: (number | null)[][] = [];
    const missing: string[] = [];
    for (const id of nodeIds) {
      if (!seriesById.has(id) || unavailable.has(id)) continue;
      const col = column(seriesById.get(id)!, xs, value);
      if (col.some(isPlotted)) {
        plotted.push(id);
        cols.push(col);
      } else if (canGrid) {
        missing.push(labelById.get(id)!);
      }
    }
    return { ids: plotted, labels: plotted.map((id) => labelById.get(id)!), data: [xs, ...cols] as AlignedData, missing };
  };
  return {
    complete,
    stepS,
    loss: chartFor(lossPercent),
    rtt: chartFor(rttMeanMs),
    unavailable: [...nodeIds.filter((id) => unavailable.has(id)), ...extraUnavailable].map((id) => labelById.get(id)!),
  };
}

export type ComparisonRow = {
  id: bigint; label: string;
  mean: number | null; min: number | null; max: number | null;
  lossPercent: number | null; errors: number;
  unavailable: boolean;
};

// 每个桶的成功数可能不同，窗口均值按成功数加权，不能简单平均桶均值。
// ProbeSample 的 sent 包含超时与本地错误；丢包只计 lost，错误单独累计。
export function summarizeComparison(nodeIds: readonly bigint[], names: ReadonlyMap<bigint, string>, chunks: readonly ComparisonChunk[]): ComparisonRow[] {
  const seriesById = new Map<bigint, readonly ProbeSample[]>();
  const unavailable = new Set<bigint>();
  for (const chunk of chunks) {
    for (const series of chunk.series) if (!seriesById.has(series.nodeId)) seriesById.set(series.nodeId, series.samples);
    for (const id of chunk.unavailableNodeIds) unavailable.add(id);
  }
  const labels = disambiguate(nodeIds.map((id) => labelOf(id, names)), nodeIds);
  return nodeIds.map((id, i) => {
    const samples = seriesById.get(id) ?? [];
    let sent = 0, lost = 0, errors = 0, weighted = 0, ok = 0;
    let min: number | null = null, max: number | null = null;
    for (const s of samples) {
      sent += s.sent;
      lost += s.lost;
      errors += s.errors;
      const succeeded = s.sent - s.lost - s.errors;
      if (s.rttMeanUs !== undefined && succeeded > 0) {
        weighted += (s.rttMeanUs / 1000) * succeeded;
        ok += succeeded;
      }
      if (s.rttMinUs !== undefined) min = min === null ? s.rttMinUs / 1000 : Math.min(min, s.rttMinUs / 1000);
      if (s.rttMaxUs !== undefined) max = max === null ? s.rttMaxUs / 1000 : Math.max(max, s.rttMaxUs / 1000);
    }
    return {
      id, label: labels[i],
      mean: ok > 0 ? weighted / ok : null, min, max,
      lossPercent: sent > 0 ? (lost / sent) * 100 : null, errors,
      unavailable: unavailable.has(id),
    };
  });
}

export type SortKey = "label" | "mean" | "min" | "max" | "lossPercent" | "errors";

// 缺读数不是最小值或最大值，两种排序方向都排最后；不修改调用方的数组。
export function sortRows(rows: readonly ComparisonRow[], key: SortKey, dir: "asc" | "desc"): ComparisonRow[] {
  const sign = dir === "asc" ? 1 : -1;
  return [...rows].sort((a, b) => {
    if (key === "label") return sign * a.label.localeCompare(b.label);
    const va = a[key], vb = b[key];
    if (va === null && vb === null) return 0;
    if (va === null) return 1;
    if (vb === null) return -1;
    return sign * (va - vb);
  });
}
