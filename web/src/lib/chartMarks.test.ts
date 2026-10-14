import { expect, it } from "vitest";
import { formatChartTimes, isolatedPointIndices, readingGap } from "./chartMarks";

// 手机上的横轴只留时分：1h / 6h / 24h 窗口刻度不足一天，跨日界也不补日期；7d / 30d 的刻度落在日界，仍写月-日。
it("compact：小于一天的刻度只写时分，跨日界不补日期；不小于一天的刻度仍写月-日", () => {
  const t0 = Date.parse("2026-10-06T22:00:00+08:00") / 1000;
  const hourly = [t0, t0 + 3600, t0 + 7200, t0 + 10800];
  expect(formatChartTimes(hourly, { incrSec: 3600, timeZone: "Asia/Shanghai" })).toEqual(["10-06 22:00", "23:00", "10-07 00:00", "01:00"]);
  expect(formatChartTimes(hourly, { incrSec: 3600, timeZone: "Asia/Shanghai", compact: true })).toEqual(["22:00", "23:00", "00:00", "01:00"]);
  const daily = [t0 + 7200, t0 + 7200 + 86_400 * 2];
  expect(formatChartTimes(daily, { incrSec: 86_400 * 2, timeZone: "Asia/Shanghai", compact: true })).toEqual(["10-07", "10-09"]);
});

it("compact 不影响图例（没有 incrSec 时仍是完整时间）", () => {
  const t = Date.parse("2026-10-06T22:00:00+08:00") / 1000;
  expect(formatChartTimes([t], { timeZone: "Asia/Shanghai", compact: true })).toEqual(["2026-10-06 22:00"]);
});


const sh = (iso: string) => Date.parse(iso) / 1000;

it("单点窗口要画点，连续段不加点", () => {
  expect(isolatedPointIndices([4])).toEqual([0]);
  expect(isolatedPointIndices([1, 2])).toEqual([]);
  expect(isolatedPointIndices([1, 2, 3, 4])).toEqual([]);
  expect(isolatedPointIndices([null, 1, 2, null])).toEqual([]);
});

it("被缺口夹住的单点要画，两侧的连续段不画", () => {
  expect(isolatedPointIndices([1, 2, null, 5, null, 3, 4])).toEqual([3]);
  expect(isolatedPointIndices([null, 5, null])).toEqual([1]);
});

it("窗口两端没有可连邻值的点要画，0 也是这样的点", () => {
  expect(isolatedPointIndices([5, null, null, 9])).toEqual([0, 3]);
  expect(isolatedPointIndices([5, null])).toEqual([0]);
  expect(isolatedPointIndices([null, 9])).toEqual([1]);
  expect(isolatedPointIndices([null, 0, null])).toEqual([1]);
});

it("全是 null 没有点", () => {
  expect(isolatedPointIndices([null, null, null])).toEqual([]);
  expect(isolatedPointIndices([])).toEqual([]);
});

it("整列都没有有限值是图级无读数，部分列无读数只点名那些列", () => {
  expect(readingGap([[null, null], [null]], ["CPU 均值", "CPU 峰值"])).toEqual({ kind: "chart" });
  expect(readingGap([[null, undefined], [null, null]], ["内存均值", "内存峰值"])).toEqual({ kind: "chart" });
  expect(readingGap([[1, null], [null, null]], ["CPU 均值", "CPU 峰值"])).toEqual({ kind: "series", labels: ["CPU 峰值"] });
  expect(readingGap([[null, null], [2, null], [null]], ["下行均值", "上行均值", "下行峰值"])).toEqual({
    kind: "series",
    labels: ["下行均值", "下行峰值"],
  });
});

it("恒为 0 不是无读数，夹着 null 的 0 也不是", () => {
  expect(readingGap([[0, 0, 0]], ["负载均值"])).toEqual({ kind: "none" });
  expect(readingGap([[0, null, 0]], ["负载均值"])).toEqual({ kind: "none" });
  expect(readingGap([[null, null], [0, 0]], ["交换均值", "内存均值"])).toEqual({ kind: "series", labels: ["交换均值"] });
  expect(readingGap([[null, 0, null]], ["负载均值"])).toEqual({ kind: "none" });
});

it("没有序列时不报无读数", () => {
  expect(readingGap([], [])).toEqual({ kind: "none" });
});

it("1h 窗口的分钟刻度只显示时:分", () => {
  const start = sh("2026-10-06T14:00:00+08:00");
  const ticks = [0, 10, 20, 30, 40, 50].map((m) => start + m * 60);
  expect(formatChartTimes(ticks, { incrSec: 600, timeZone: "Asia/Shanghai" })).toEqual([
    "14:00", "14:10", "14:20", "14:30", "14:40", "14:50",
  ]);
});

it("1h 窗口跨过本地午夜时，日界和首格带月-日，其余仍是时:分", () => {
  const start = sh("2026-10-06T23:40:00+08:00");
  const ticks = [0, 10, 20, 30].map((m) => start + m * 60);
  expect(formatChartTimes(ticks, { incrSec: 600, timeZone: "Asia/Shanghai" })).toEqual([
    "10-06 23:40", "23:50", "10-07 00:00", "00:10",
  ]);
});

it("24h 窗口跨过本地日界的刻度带月-日，同一天里的其余刻度是时:分", () => {
  const start = sh("2026-10-05T15:00:00+08:00");
  const ticks = [0, 3, 6, 9, 12, 15, 18, 21, 24].map((h) => start + h * 3600);
  expect(formatChartTimes(ticks, { incrSec: 3 * 3600, timeZone: "Asia/Shanghai" })).toEqual([
    "10-05 15:00", "18:00", "21:00", "10-06 00:00", "03:00", "06:00", "09:00", "12:00", "15:00",
  ]);
});

it("7d 窗口的天刻度是月-日，不带时:分", () => {
  const start = sh("2026-10-01T00:00:00+08:00");
  const ticks = [0, 1, 2, 3, 4, 5, 6].map((d) => start + d * 86_400);
  expect(formatChartTimes(ticks, { incrSec: 86_400, timeZone: "Asia/Shanghai" })).toEqual([
    "10-01", "10-02", "10-03", "10-04", "10-05", "10-06", "10-07",
  ]);
});

it("30d 窗口跨月的天级刻度是月-日", () => {
  const start = sh("2026-09-10T00:00:00+08:00");
  const ticks = [0, 3, 6, 9, 12, 15, 18, 21, 24, 27].map((d) => start + d * 86_400);
  expect(formatChartTimes(ticks, { incrSec: 3 * 86_400, timeZone: "Asia/Shanghai" })).toEqual([
    "09-10", "09-13", "09-16", "09-19", "09-22", "09-25", "09-28", "10-01", "10-04", "10-07",
  ]);
});

it("跨年的天刻度只在首格和年份变化处带年份", () => {
  const ticks = ["2025-12-30", "2025-12-31", "2026-01-01", "2026-01-02"].map((d) => sh(`${d}T00:00:00+08:00`));
  expect(formatChartTimes(ticks, { incrSec: 86_400, timeZone: "Asia/Shanghai" })).toEqual([
    "2025-12-30", "12-31", "2026-01-01", "01-02",
  ]);
});

it("图例时刻是本地日期加 24 小时制，午夜是 00 不是 12am", () => {
  expect(formatChartTimes([sh("2026-10-06T14:05:00+08:00")], { timeZone: "Asia/Shanghai" })).toEqual(["2026-10-06 14:05"]);
  expect(formatChartTimes([sh("2026-10-06T00:00:00+08:00")], { timeZone: "Asia/Shanghai" })).toEqual(["2026-10-06 00:00"]);
  expect(formatChartTimes([sh("2026-10-06T12:00:00+08:00")], { timeZone: "Asia/Shanghai" })).toEqual(["2026-10-06 12:00"]);
});

it("同一时刻按给定时区解释，不按 UTC 日历日", () => {
  const ticks = [sh("2026-10-05T23:00:00+08:00"), sh("2026-10-06T01:00:00+08:00")];
  expect(formatChartTimes(ticks, { incrSec: 2 * 3600, timeZone: "Asia/Shanghai" })).toEqual(["10-05 23:00", "10-06 01:00"]);
  expect(formatChartTimes(ticks, { incrSec: 2 * 3600, timeZone: "UTC" })).toEqual(["15:00", "17:00"]);
});

it("不传 timeZone 时与显式传入本地时区一致，且不是英文 12 小时制", () => {
  const local = new Intl.DateTimeFormat().resolvedOptions().timeZone;
  const ts = sh("2026-10-06T14:05:00+08:00");
  const omitted = formatChartTimes([ts, ts + 600], { incrSec: 600 });
  expect(omitted).toEqual(formatChartTimes([ts, ts + 600], { incrSec: 600, timeZone: local }));
  expect(omitted.join(" ")).not.toMatch(/am|pm|上午|下午/);
  expect(formatChartTimes([ts])).toEqual(formatChartTimes([ts], { timeZone: local }));
});

it("非法时刻留空，不抛错", () => {
  expect(formatChartTimes([Number.NaN, null, undefined], { incrSec: 600 })).toEqual(["", "", ""]);
});
