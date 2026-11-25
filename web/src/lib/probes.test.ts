import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";
import { ProbeSampleSchema, ProbeSeriesSchema, QueryProbesResponseSchema } from "../gen/heron/v1/query_pb";
import { ProbeKind } from "../gen/heron/v1/types_pb";
import { PROBE_KINDS, isHTTPSTarget, kindLabel, lossPercent, rttMeanMs, rttMinMs, rttMaxMs, seriesLabels, summarizeProbes, targetRule, taskIdsOf, toProbeAligned, toProbeTaskAligned } from "./probes";

it.each([
  {
    series: [{ taskId: 7n, kind: ProbeKind.ICMP, target: "host" }, { taskId: 3n, kind: ProbeKind.ICMP, target: "host" }, { taskId: 9n }],
    labels: ["ICMP host #7", "ICMP host #3", "任务 #9"],
  },
  { series: [{ taskId: 3n, kind: ProbeKind.TCP, target: "1.1.1.1:443" }], labels: ["TCP 1.1.1.1:443"] },
])("序列标签取序列自带的种类与目标，未标注的用编号 $labels", ({ series, labels }) => {
  expect(seriesLabels(series.map((s) => create(ProbeSeriesSchema, s)))).toEqual(labels);
});

it("类型表与图例使用同一组标签", () => {
  expect(PROBE_KINDS).toEqual([
    { kind: ProbeKind.ICMP, label: "ICMP" },
    { kind: ProbeKind.TCP, label: "TCP" },
    { kind: ProbeKind.HTTP, label: "HTTP" },
    { kind: ProbeKind.DNS, label: "DNS" },
  ]);
  for (const { kind, label } of PROBE_KINDS) {
    expect(kindLabel(kind)).toBe(label);
    expect(seriesLabels([create(ProbeSeriesSchema, { taskId: 1n, kind, target: "host" })])).toEqual([`${label} host`]);
  }
});

it("目标约束按种类：与 probelimit 的上限同源", () => {
  expect(targetRule(ProbeKind.ICMP)).toEqual({ placeholder: "IP 或主机名", maxLength: 253 });
  expect(targetRule(ProbeKind.TCP)).toEqual({ placeholder: "host:port", maxLength: 253 });
  expect(targetRule(ProbeKind.HTTP)).toEqual({ placeholder: "https://example.com/path", maxLength: 512 });
  expect(targetRule(ProbeKind.DNS)).toEqual({ placeholder: "要解析的 DNS 名", maxLength: 253 });
});

const resp = create(QueryProbesResponseSchema, {
  level: "1m", stepS: 60,
  series: [
    { taskId: 3n, samples: [
      { ts: 120n, sent: 10, lost: 2, errors: 0, rttMeanUs: 12_500 },
      { ts: 240n, sent: 10, lost: 10, errors: 0 },
    ] },
    { taskId: 7n, samples: [{ ts: 180n, sent: 5, lost: 0, errors: 5 }] },
  ],
});

it("每任务对齐：均值 / 最小 / 最大三列同网格，没有成功探测的点三列都是 null", () => {
  const response = create(QueryProbesResponseSchema, { level: "1m", stepS: 60, series: [
    { taskId: 3n, samples: [{ ts: 60n, sent: 4, lost: 0, errors: 0, rttMeanUs: 20_000, rttMinUs: 10_000, rttMaxUs: 30_000 }, { ts: 120n, sent: 4, lost: 4, errors: 0 }] },
  ] });
  expect(toProbeTaskAligned(response, 3n, 60, 240, [rttMeanMs, rttMinMs, rttMaxMs])).toEqual([[60, 120, 180], [20, null, null], [10, null, null], [30, null, null]]);
  expect(toProbeTaskAligned(response, 9n, 60, 180, [rttMeanMs])).toEqual([[60, 120], [null, null]]);
});

describe("toProbeAligned", () => {
  it("每个任务一列，缺 ts 与缺 rtt 都是 null", () => {
    const loss = toProbeAligned(resp, [3n, 7n], 100, 300, lossPercent);
    expect(loss[0]).toEqual([60, 120, 180, 240]);
    expect(loss[1]).toEqual([null, 20, null, 100]);
    expect(loss[2]).toEqual([null, null, null, null]);
    const rtt = toProbeAligned(resp, [3n, 7n], 100, 300, rttMeanMs);
    expect(rtt[1]).toEqual([null, 12.5, null, null]);
    expect(rtt[2]).toEqual([null, null, null, null]);
  });
  it("全是错误的点没有丢包率、rtt 为空：error 是本机没能发起测量，不是可达性读数，不能画成 0%", () => {
    const s = resp.series[1].samples[0];
    expect(lossPercent(s)).toBeNull();
    expect(rttMeanMs(s)).toBeNull();
    // 有一次有效测量就有读数，分母仍是全部结果。
    expect(lossPercent({ ...s, sent: 6, lost: 1, errors: 4 })).toBeCloseTo(100 / 6);
  });
  it("taskIdsOf 保持响应顺序", () => {
    expect(taskIdsOf(resp)).toEqual([3n, 7n]);
  });
});

// 与 hub 的 probelimit.IsHTTPSTarget 同一判据：scheme 不分大小写，种类必须是 HTTP。
it.each([
  { kind: ProbeKind.HTTP, target: "https://example.com/", want: true },
  { kind: ProbeKind.HTTP, target: "HTTPS://example.com/", want: true },
  { kind: ProbeKind.HTTP, target: " Https://example.com/", want: true },
  { kind: ProbeKind.HTTP, target: "http://example.com/", want: false },
  { kind: ProbeKind.TCP, target: "https://example.com/", want: false },
])("isHTTPSTarget($kind, $target) = $want", ({ kind, target, want }) => {
  expect(isHTTPSTarget(kind, target)).toBe(want);
});

// 单节点探测卡片与跨节点对比表的汇总都经 summarizeProbes：均值按成功数加权，丢包率与单点同口径——全是本地错误的
// 窗口没有丢包率，不是 0%。
it.each([
  {
    name: "加权均值与最值、丢包按 lost/sent",
    samples: [
      { ts: 0n, sent: 100, lost: 0, errors: 0, rttMeanUs: 10_000, rttMinUs: 5_000, rttMaxUs: 15_000 },
      { ts: 60n, sent: 100, lost: 10, errors: 0, rttMeanUs: 50_000, rttMinUs: 40_000, rttMaxUs: 90_000 },
    ],
    want: { mean: 5500 / 190, min: 5, max: 90, lossPercent: 5, errors: 0 },
  },
  { name: "全是本地错误：没有丢包率", samples: [{ ts: 0n, sent: 3, lost: 0, errors: 3 }], want: { mean: null, min: null, max: null, lossPercent: null, errors: 3 } },
  { name: "没有样本", samples: [], want: { mean: null, min: null, max: null, lossPercent: null, errors: 0 } },
])("窗口汇总：$name", ({ samples, want }) => {
  const got = summarizeProbes(samples.map((s) => create(ProbeSampleSchema, s)));
  expect({ ...got, mean: got.mean === null ? null : Number(got.mean.toFixed(9)) }).toEqual({ ...want, mean: want.mean === null ? null : Number(want.mean.toFixed(9)) });
});
