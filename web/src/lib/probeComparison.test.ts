import { expect, it } from "vitest";
import type { ProbeSample } from "../gen/heron/v1/query_pb";
import { assembleComparison, chunkNodeIds, sortRows, summarizeComparison, taskIdParam, type ComparisonChunk } from "./probeComparison";

const names = new Map<bigint, string>([
  [1n, "edge"],
  [2n, "edge"],
  [3n, "other"],
  [4n, "gone"],
  [5n, "pending"],
]);

function sample(ts: number, sent: number, lost: number, errors: number, rttMeanUs?: number): ProbeSample {
  return { ts: BigInt(ts), sent, lost, errors, rttMeanUs } as ProbeSample;
}

const from = 100;
const to = 300;

it("按节点汇总：均值按成功数加权，极值、丢包与错误各按其口径累计，不可见节点标记", () => {
  const rows = summarizeComparison([1n, 2n, 3n], new Map([[1n, "a"], [2n, "b"]]), [chunk([
    { nodeId: 1n, samples: [
      { ...sample(60, 4, 1, 1, 10_000), rttMinUs: 8_000, rttMaxUs: 12_000 },
      { ...sample(120, 4, 0, 0, 40_000), rttMinUs: 5_000, rttMaxUs: 90_000 },
    ] },
    { nodeId: 2n, samples: [sample(60, 2, 2, 0)] },
  ], [3n])]);
  expect(rows).toEqual([
    { id: 1n, label: "a", mean: 30, min: 5, max: 90, lossPercent: 12.5, errors: 1, unavailable: false },
    { id: 2n, label: "b", mean: null, min: null, max: null, lossPercent: 100, errors: 0, unavailable: false },
    { id: 3n, label: "#3", mean: null, min: null, max: null, lossPercent: null, errors: 0, unavailable: true },
  ]);
});

it("排序：数值列按方向，null 永远最后；标签按本地比较，不修改输入", () => {
  const rows = [
    { id: 1n, label: "b", mean: 30, min: 5, max: 90, lossPercent: 12.5, errors: 1, unavailable: false },
    { id: 2n, label: "a", mean: null, min: null, max: null, lossPercent: 100, errors: 0, unavailable: false },
    { id: 3n, label: "c", mean: 10, min: 1, max: 20, lossPercent: 0, errors: 3, unavailable: false },
  ];
  expect(sortRows(rows, "mean", "asc").map((r) => r.id)).toEqual([3n, 1n, 2n]);
  expect(sortRows(rows, "mean", "desc").map((r) => r.id)).toEqual([1n, 3n, 2n]);
  expect(sortRows(rows, "lossPercent", "desc").map((r) => r.id)).toEqual([2n, 1n, 3n]);
  expect(sortRows(rows, "label", "asc").map((r) => r.label)).toEqual(["a", "b", "c"]);
  expect(rows.map((r) => r.id)).toEqual([1n, 2n, 3n]);
});

function chunk(series: ComparisonChunk["series"], unavailable: bigint[] = [], stepS = 60): ComparisonChunk {
  return { stepS, series, unavailableNodeIds: unavailable };
}

it("任务编号只接受十进制正整数", () => {
  expect(taskIdParam("12")).toBe(12n);
  expect(taskIdParam("0")).toBeUndefined();
  expect(taskIdParam("01")).toBeUndefined();
  expect(taskIdParam(undefined)).toBeUndefined();
});

it("块大小只取调用方传入的上限，0 与负数不是不限，也不退回固定块大小", () => {
  expect(chunkNodeIds([1n, 2n, 3n, 4n, 5n], 2)).toEqual({ ok: true, chunks: [[1n, 2n], [3n, 4n], [5n]] });
  expect(chunkNodeIds([1n, 2n, 3n, 4n, 5n], 4).ok && chunkNodeIds([1n, 2n, 3n, 4n, 5n], 4)).toEqual({
    ok: true, chunks: [[1n, 2n, 3n, 4n], [5n]],
  });
  expect(chunkNodeIds([1n, 2n, 3n], 0)).toEqual({ ok: false, reason: "non-positive-limit" });
  expect(chunkNodeIds([1n, 2n, 3n], -1)).toEqual({ ok: false, reason: "non-positive-limit" });
});

it("全超时 100%、全本地错误 0%、混合按 lost/sent；三者都没有 RTT，不把 null 画成 0", () => {
  const view = assembleComparison([1n, 2n, 3n], names, [chunk([
    { nodeId: 1n, samples: [sample(120, 10, 10, 0)] },
    { nodeId: 2n, samples: [sample(120, 5, 0, 5)] },
    { nodeId: 3n, samples: [sample(120, 10, 4, 3)] },
  ])], from, to);
  expect(view.complete).toBe(true);
  expect(view.loss.labels).toEqual(["edge #1", "edge #2", "other"]);
  expect(view.loss.data[0]).toEqual([60, 120, 180, 240]);
  expect(view.loss.data.slice(1).map((col) => col[1])).toEqual([100, 0, 40]);
  expect(view.loss.missing).toEqual([]);
  expect(view.rtt.labels).toEqual([]);
  expect(view.rtt.data.slice(1)).toEqual([]);
  expect(view.rtt.missing).toEqual(["edge #1", "edge #2", "other"]);
  expect(view.unavailable).toEqual([]);
});

it("有 RTT 的节点才进 RTT 图；不可见节点单列，不进两张图的线或无读数名单", () => {
  const view = assembleComparison([1n, 2n, 4n], names, [chunk([
    { nodeId: 1n, samples: [sample(120, 10, 1, 0, 2_500)] },
    { nodeId: 2n, samples: [] },
  ], [4n])], from, to);
  expect(view.complete).toBe(true);
  expect(view.loss.labels).toEqual(["edge #1"]);
  expect(view.loss.data[1]).toEqual([null, 10, null, null]);
  expect(view.loss.missing).toEqual(["edge #2"]);
  expect(view.rtt.labels).toEqual(["edge #1"]);
  expect(view.rtt.data[1]).toEqual([null, 2.5, null, null]);
  expect(view.rtt.missing).toEqual(["edge #2"]);
  expect(view.unavailable).toEqual(["gone"]);
});

it("还没返回的块里的节点不算无结果，整次对比也不算齐", () => {
  const view = assembleComparison([1n, 2n, 5n], names, [chunk([
    { nodeId: 1n, samples: [] },
    { nodeId: 2n, samples: [sample(120, 10, 10, 0)] },
  ])], from, to);
  expect(view.complete).toBe(false);
  expect(view.loss.missing).toEqual(["edge #1"]);
  expect(view.rtt.missing).toEqual(["edge #1", "edge #2"]);
  expect(view.loss.labels).toEqual(["edge #2"]);
  expect([...view.loss.missing, ...view.rtt.missing, ...view.loss.labels, ...view.unavailable].join(" ")).not.toContain("pending");
});
