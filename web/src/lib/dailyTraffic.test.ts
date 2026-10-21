import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";
import { MetricSampleSchema, MetricSeriesSchema, QueryMetricsResponseSchema } from "../gen/heron/v1/query_pb";
import { TrafficQuotaMode } from "../gen/heron/v1/types_pb";
import { DAILY_MAX_POINTS, dailyTraffic, dailyWindow, periodDays, quotaDirection, topDays, unattributedBytes } from "./dailyTraffic";

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

describe("periodDays", () => {
  it("本周期从起点到 hub 时区的今天，上周期是按同一重置日前推一个月到本周期前一天", () => {
    const start = BigInt(at("2026-10-04T16:00:00Z")); // 上海 10-05 00:00
    const days = periodDays(start, at("2026-10-21T03:00:00Z"), "Asia/Shanghai");
    expect(days.current[0]).toBe("2026-10-05");
    expect(days.current.at(-1)).toBe("2026-10-21");
    expect(days.current).toHaveLength(17);
    expect(days.previous[0]).toBe("2026-09-05");
    expect(days.previous.at(-1)).toBe("2026-10-04");
    expect(days.previous).toHaveLength(30);
  });

  it("跨年时上周期落在上一年", () => {
    const days = periodDays(BigInt(at("2027-01-01T00:00:00Z")), at("2027-01-02T00:00:00Z"), "UTC");
    expect(days.previous[0]).toBe("2026-12-01");
    expect(days.previous.at(-1)).toBe("2026-12-31");
  });
});

describe("dailyTraffic", () => {
  it("按 hub 时区的日界归日，不按 UTC", () => {
    // 15:00Z 是上海 23:00（10-11），16:00Z 是上海次日 00:00（10-12）。按 UTC 归日两者会落在同一天。
    const resp = response([
      { ts: at("2026-10-11T15:00:00Z"), rx: 100, tx: 10 },
      { ts: at("2026-10-11T16:00:00Z"), rx: 200, tx: 20 },
      { ts: at("2026-10-12T15:00:00Z"), rx: 1, tx: 2 },
    ]);
    expect(dailyTraffic(resp, "Asia/Shanghai", ["2026-10-11", "2026-10-12"])).toEqual([
      { day: "2026-10-11", rx: 100, tx: 10, known: true },
      { day: "2026-10-12", rx: 201, tx: 22, known: true },
    ]);
  });

  it("没有点或点上两个方向都没有读数的日子是未知，不是 0", () => {
    const resp = response([{ ts: at("2026-10-12T01:00:00Z") }, { ts: at("2026-10-13T01:00:00Z"), rx: 0, tx: 0 }]);
    expect(dailyTraffic(resp, "UTC", ["2026-10-11", "2026-10-12", "2026-10-13"])).toEqual([
      { day: "2026-10-11", rx: 0, tx: 0, known: false },
      { day: "2026-10-12", rx: 0, tx: 0, known: false },
      { day: "2026-10-13", rx: 0, tx: 0, known: true },
    ]);
  });

  it("不在所给日期里的点不计入", () => {
    const resp = response([{ ts: at("2026-09-30T12:00:00Z"), rx: 9, tx: 9 }, { ts: at("2026-10-01T12:00:00Z"), rx: 1, tx: 1 }]);
    expect(dailyTraffic(resp, "UTC", ["2026-10-01"])).toEqual([{ day: "2026-10-01", rx: 1, tx: 1, known: true }]);
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
  it("左端前推 32 天覆盖整个上周期，右端按 hub 分钟归一", () => {
    const w = dailyWindow(1_000_000_000n, 1_000_000_123);
    expect(w.from).toBe(1_000_000_000 - 32 * 86_400);
    expect(w.to).toBe(1_000_000_140);
  });

  // hub 的 store.ChooseLevel：跨度超过 7 天选 1h 级，步长取 max(3600, ceil(跨度 / max_points) 向上取整到 3600)。
  // 按小时归日依赖步长恰好是 3600：最长的周期（31 天）走到最后一秒时，窗口仍须落在这两条边界之内。
  it("最长周期末尾的窗口仍让 hub 选 1h 级、步长 3600", () => {
    const start = 1_800_000_000n;
    const w = dailyWindow(start, Number(start) + 31 * 86_400 - 1);
    expect(w.to - w.from).toBeGreaterThan(7 * 86_400);
    expect(w.to - w.from).toBeLessThanOrEqual(DAILY_MAX_POINTS * 3600 - 3600);
  });
});
