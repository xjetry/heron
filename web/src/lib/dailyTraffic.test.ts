import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { MetricSampleSchema, MetricSeriesSchema, QueryMetricsResponseSchema } from "../gen/heron/v1/query_pb";
import { TrafficQuotaMode } from "../gen/heron/v1/types_pb";
import { DAILY_MAX_POINTS, dailyTraffic, dailyWindow, type PeriodSlice, periodSlices, quotaDirection, topDays, unattributedBytes } from "./dailyTraffic";

const at = (iso: string) => Date.parse(iso) / 1000;
const sample = (sum: number | undefined) => create(MetricSampleSchema, sum === undefined ? { n: 0 } : { n: 1, sum });
const response = (points: { ts: number; rx?: number; tx?: number }[]) => create(QueryMetricsResponseSchema, {
  level: "1h", stepS: 3600, ts: points.map((p) => BigInt(p.ts)),
  series: [
    create(MetricSeriesSchema, { name: "cpu", unit: "percent", samples: points.map(() => create(MetricSampleSchema, { n: 1, mean: 5 })) }),
    create(MetricSeriesSchema, { name: "rx_bytes", unit: "bytes", samples: points.map((p) => sample(p.rx)) }),
    create(MetricSeriesSchema, { name: "tx_bytes", unit: "bytes", samples: points.map((p) => sample(p.tx)) }),
  ],
});

const traffic = (previous: string, start: string, next: string, timezone: string) =>
  ({ previousPeriodStart: BigInt(at(previous)), periodStart: BigInt(at(start)), nextResetAt: BigInt(at(next)), timezone });

describe("periodSlices", () => {
  it("本周期从起点到计费时区的今天，上周期从 hub 下发的上一周期起点到本周期起点前一刻", () => {
    // 上海重置日 5、00:00：上周期 [09-05, 10-05)，本周期从 10-05 起。
    const t = traffic("2026-09-04T16:00:00Z", "2026-10-04T16:00:00Z", "2026-11-04T16:00:00Z", "Asia/Shanghai");
    const { current, previous } = periodSlices(t, at("2026-10-21T03:00:00Z"));
    expect(current).toMatchObject({ from: at("2026-10-04T16:00:00Z"), to: at("2026-11-04T16:00:00Z") });
    expect(current.days[0]).toBe("2026-10-05");
    expect(current.days.at(-1)).toBe("2026-10-21");
    expect(current.days).toHaveLength(17);
    expect(previous).toMatchObject({ from: at("2026-09-04T16:00:00Z"), to: at("2026-10-04T16:00:00Z") });
    expect(previous.days[0]).toBe("2026-09-05");
    expect(previous.days.at(-1)).toBe("2026-10-04");
    expect(previous.days).toHaveLength(30);
  });

  it("日期按节点的计费时区读，不按浏览器或 UTC", () => {
    // 同一组边界（UTC 10-01 00:00）在纽约是 9 月 30 日 20:00：两个周期的首尾日都在纽约的日历上。
    const t = traffic("2026-09-01T00:00:00Z", "2026-10-01T00:00:00Z", "2026-11-01T00:00:00Z", "America/New_York");
    const { current, previous } = periodSlices(t, at("2026-10-02T12:00:00Z"));
    expect(previous.days[0]).toBe("2026-08-31");
    expect(previous.days.at(-1)).toBe("2026-09-30");
    expect(current.days).toEqual(["2026-09-30", "2026-10-01", "2026-10-02"]);
  });

  it("重置时刻不是零点时，重置那天同时是上周期的最后一天与本周期的第一天", () => {
    // UTC 重置日 1、08:30。
    const t = traffic("2026-09-01T08:30:00Z", "2026-10-01T08:30:00Z", "2026-11-01T08:30:00Z", "UTC");
    const { current, previous } = periodSlices(t, at("2026-10-03T00:00:00Z"));
    expect(previous.days[0]).toBe("2026-09-01");
    expect(previous.days.at(-1)).toBe("2026-10-01");
    expect(current.days[0]).toBe("2026-10-01");
  });
});

describe("dailyTraffic", () => {
  const slice = (days: string[], from = -Infinity, to = Infinity): PeriodSlice => ({ from, to, days });

  it("按计费时区的日界归日，不按 UTC", () => {
    // 15:00Z 是上海 23:00（10-11），16:00Z 是上海次日 00:00（10-12）。按 UTC 归日两者会落在同一天。
    const resp = response([
      { ts: at("2026-10-11T15:00:00Z"), rx: 100, tx: 10 },
      { ts: at("2026-10-11T16:00:00Z"), rx: 200, tx: 20 },
      { ts: at("2026-10-12T15:00:00Z"), rx: 1, tx: 2 },
    ]);
    expect(dailyTraffic(resp, "Asia/Shanghai", slice(["2026-10-11", "2026-10-12"]))).toEqual([
      { day: "2026-10-11", rx: 100, tx: 10, known: true },
      { day: "2026-10-12", rx: 201, tx: 22, known: true },
    ]);
  });

  it("没有点或点上两个方向都没有读数的日子是未知，不是 0", () => {
    const resp = response([{ ts: at("2026-10-12T01:00:00Z") }, { ts: at("2026-10-13T01:00:00Z"), rx: 0, tx: 0 }]);
    expect(dailyTraffic(resp, "UTC", slice(["2026-10-11", "2026-10-12", "2026-10-13"]))).toEqual([
      { day: "2026-10-11", rx: 0, tx: 0, known: false },
      { day: "2026-10-12", rx: 0, tx: 0, known: false },
      { day: "2026-10-13", rx: 0, tx: 0, known: true },
    ]);
  });

  it("不在所给日期里的点不计入", () => {
    const resp = response([{ ts: at("2026-09-30T12:00:00Z"), rx: 9, tx: 9 }, { ts: at("2026-10-01T12:00:00Z"), rx: 1, tx: 1 }]);
    expect(dailyTraffic(resp, "UTC", slice(["2026-10-01"]))).toEqual([{ day: "2026-10-01", rx: 1, tx: 1, known: true }]);
  });

  // 重置时刻 08:30：重置那天 08:00 起的那一小时跨过边界，整块归到它开始的上周期；09:00 起的点属于本周期。
  it("周期边界切开同一天：各周期只计 [from, to) 内开始的点，跨边界的小时归到它开始的一侧", () => {
    const boundary = at("2026-10-01T08:30:00Z");
    const resp = response([
      { ts: at("2026-10-01T07:00:00Z"), rx: 1, tx: 0 },
      { ts: at("2026-10-01T08:00:00Z"), rx: 10, tx: 0 },
      { ts: at("2026-10-01T09:00:00Z"), rx: 100, tx: 0 },
    ]);
    expect(dailyTraffic(resp, "UTC", slice(["2026-10-01"], -Infinity, boundary))).toEqual([{ day: "2026-10-01", rx: 11, tx: 0, known: true }]);
    expect(dailyTraffic(resp, "UTC", slice(["2026-10-01"], boundary))).toEqual([{ day: "2026-10-01", rx: 100, tx: 0, known: true }]);
  });
});

describe("topDays", () => {
  it("按合计从高到低取前三", () => {
    const days = [
      { day: "d1", rx: 5, tx: 0, known: true },
      { day: "d3", rx: 50, tx: 50, known: true },
      { day: "d5", rx: 10, tx: 1, known: true },
      { day: "d6", rx: 1, tx: 1, known: true },
    ];
    expect(topDays(days, "sum").map((d) => d.day)).toEqual(["d3", "d5", "d1"]);
  });

  it("按给定方向排名，那个方向上没有量的日子不入榜", () => {
    const days = [
      { day: "d1", rx: 10, tx: 1, known: true },
      { day: "d2", rx: 1, tx: 5, known: true },
      { day: "d3", rx: 4, tx: 0, known: true },
    ];
    expect(topDays(days, "tx").map((d) => d.day)).toEqual(["d2", "d1"]);
    expect(topDays(days, "rx").map((d) => d.day)).toEqual(["d1", "d3", "d2"]);
  });

  // 有用量的日子不到三天时，剩下的名额不拿未知或零用量的日子凑。
  it("跳过未知与零用量", () => {
    const days = [
      { day: "d1", rx: 5, tx: 0, known: true },
      { day: "d2", rx: 0, tx: 0, known: true },
      { day: "d4", rx: 0, tx: 0, known: false },
    ];
    expect(topDays(days, "sum").map((d) => d.day)).toEqual(["d1"]);
  });
});

describe("quotaDirection", () => {
  const days = [{ day: "d1", rx: 10, tx: 1, known: true }, { day: "d2", rx: 1, tx: 5, known: true }];
  it("与 hub 的 traffic.Quota 同一口径：仅下载、仅上传各取一个方向，下载 + 上传与未指定取合计", () => {
    expect(quotaDirection(TrafficQuotaMode.RX, days)).toBe("rx");
    expect(quotaDirection(TrafficQuotaMode.TX, days)).toBe("tx");
    expect(quotaDirection(TrafficQuotaMode.SUM, days)).toBe("sum");
    expect(quotaDirection(TrafficQuotaMode.UNSPECIFIED, days)).toBe("sum");
  });
  it("取大者按所列日子里合计占优的方向", () => {
    expect(quotaDirection(TrafficQuotaMode.MAX, days)).toBe("rx");
    expect(quotaDirection(TrafficQuotaMode.MAX, [{ day: "d1", rx: 1, tx: 9, known: true }])).toBe("tx");
  });
});

describe("unattributedBytes", () => {
  const days = [{ day: "d1", rx: 600, tx: 300, known: true }];
  it("周期用量与按日合计之差，正负都给出", () => {
    expect(unattributedBytes(1000n, days)).toBe(100);
    expect(unattributedBytes(800n, days)).toBe(-100);
  });
  it("差额不到周期用量的 1% 时不提示", () => {
    expect(unattributedBytes(905n, days)).toBeUndefined();
    expect(unattributedBytes(0n, [])).toBeUndefined();
  });
});

describe("dailyWindow", () => {
  it("左端是 hub 下发的上一周期起点，右端按 hub 分钟归一", () => {
    const w = dailyWindow(1_000_000_000n, 1_000_000_123);
    expect(w.from).toBe(1_000_000_000);
    expect(w.to).toBe(1_000_000_140);
  });

  // hub 的 Store.ChooseLevel：跨度超过 7 天选 1h 级，步长取 max(3600, ceil(跨度 / max_points) 向上取整到 3600)。
  // 按小时归日依赖步长恰好是 3600：最短的上周期（2 月拨快少一小时）刚开始新周期时，与最长的两个周期（各 31 天又多一小时）
  // 走到最后一秒时，窗口仍须落在这两条边界之内。
  it("窗口的最短与最长跨度都让 hub 选 1h 级、步长 3600", () => {
    const previous = 1_800_000_000n;
    const shortest = dailyWindow(previous, Number(previous) + 28 * 86_400 - 3600);
    expect(shortest.to - shortest.from).toBeGreaterThan(7 * 86_400);
    const longest = dailyWindow(previous, Number(previous) + 2 * (31 * 86_400 + 3600) - 1);
    expect(longest.to - longest.from).toBeLessThanOrEqual(DAILY_MAX_POINTS * 3600 - 3600);
  });
});
