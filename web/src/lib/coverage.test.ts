import { create } from "@bufbuild/protobuf";
import { expect, it } from "vitest";
import { CoverageSummarySchema } from "../gen/heron/v1/query_pb";
import { coverageView } from "./coverage";

it("coverage_summary 由 QueryMetrics 总是填充：缺席不是不显示", () => {
  expect(() => coverageView(undefined)).toThrow(/coverage_summary/);
});

it("coverageStart 缺席 → 尚无覆盖记录（eligible 恒为 0，不据此猜原因）", () => {
  expect(coverageView(create(CoverageSummarySchema)).kind).toBe("no-start");
});

it("observedMinutes 为 0 → 无可观测区间，不做除零", () => {
  const view = coverageView(create(CoverageSummarySchema, { coverageStart: 1_700_000_000n, eligibleMinutes: 60n }));
  expect(view.kind).toBe("no-observed");
});

it.each([
  { eligible: 200n, observed: 150n, reported: 100n, percent: "66.6", unknown: "50 分钟" },
  { eligible: 150n, observed: 150n, reported: 150n, percent: "100.0", unknown: null },
  { eligible: 151n, observed: 151n, reported: 100n, percent: "66.2", unknown: null },
  // 一位小数向下截断：显示值不超过真实值。
  { eligible: 3n, observed: 3n, reported: 1n, percent: "33.3", unknown: null },
  { eligible: 3n, observed: 3n, reported: 2n, percent: "66.6", unknown: null },
  // 未满不显示满：99.96% 四舍五入会成 100.0。
  { eligible: 10000n, observed: 10000n, reported: 9996n, percent: "99.9", unknown: null },
  // 恰好落在 0.1% 边界：浮点 (57/100)×100 = 56.99…，按浮点截断会错成 56.9。
  { eligible: 100n, observed: 100n, reported: 57n, percent: "57.0", unknown: null },
  { eligible: 10000n, observed: 10000n, reported: 1n, percent: "0.0", unknown: null },
])("覆盖率 $reported/$observed → $percent，未知 $unknown", ({ eligible, observed, reported, percent, unknown }) => {
  const view = coverageView(create(CoverageSummarySchema, {
    coverageStart: 1_700_000_000n,
    eligibleMinutes: eligible,
    observedMinutes: observed,
    observedReportedMinutes: reported,
  }));
  expect(view).toEqual({ kind: "rate", percent, unknown });
});

it.each([
  { unknown: 1n, text: "1 分钟" },
  { unknown: 59n, text: "59 分钟" },
  { unknown: 120n, text: "2 小时" },
  { unknown: 150n, text: "2.5 小时" },
  { unknown: 2879n, text: "48.0 小时" },
  { unknown: 2880n, text: "2 天" },
  { unknown: 3600n, text: "2.5 天" },
])("未知时长 $unknown 分钟 → $text", ({ unknown, text }) => {
  const view = coverageView(create(CoverageSummarySchema, {
    coverageStart: 1_700_000_000n,
    eligibleMinutes: unknown + 1n,
    observedMinutes: 1n,
    observedReportedMinutes: 1n,
  }));
  expect(view).toEqual({ kind: "rate", percent: "100.0", unknown: text });
});
