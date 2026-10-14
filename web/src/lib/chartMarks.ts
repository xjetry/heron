// 历史图的三条显示规则：孤立读数画点、整列没有有限值才算无读数、时刻用浏览器本地时区。
// 管理端与公开页共用 Chart，规则放在这里，不在两个页面各写一份。

import { clockParts, type ClockParts } from "./format";

const DAY_S = 86_400;

// 线只连接相邻的有限数。null 与 undefined 用 != null 比较时相等，uPlot 两者都不画线段；
// NaN 会让 canvas 丢掉这条线段。0 是有限数，是读数，能和邻点连线。
function isReading(v: number | null | undefined): v is number {
  return typeof v === "number" && Number.isFinite(v);
}

// 只标两侧都连不出线段的点。连续段的每个点至少有一个有限邻值，线段已经把它画出来了，
// 再逐点加圆点会在分钟级窗口里糊成一条粗线。窗口两端只有一侧，那一侧也没有有限邻值时同样没有线段。
// 邻值按整列下标看，不按当前缩放下的可见区间：可见区外的有限邻值仍会连出一条被裁切的线段。
export function isolatedPointIndices(values: readonly (number | null | undefined)[]): number[] {
  const out: number[] = [];
  for (let i = 0; i < values.length; i++) {
    if (!isReading(values[i])) continue;
    const left = i > 0 && isReading(values[i - 1]);
    const right = i + 1 < values.length && isReading(values[i + 1]);
    if (!left && !right) out.push(i);
  }
  return out;
}

export type ReadingGap =
  | { kind: "none" }
  | { kind: "chart" }
  | { kind: "series"; labels: string[] };

// 无读数指整列没有任何有限数。中间夹 null 仍算有读数；恒为 0 也是读数（贴轴的一条线），
// 不能和“整列都是 null”画成同一种空白。列与 labels 按下标对齐，由 Chart 用同一组标签传入。
export function readingGap(
  columns: readonly (readonly (number | null | undefined)[])[],
  labels: readonly string[],
): ReadingGap {
  if (columns.length === 0) return { kind: "none" };
  const lacking = columns.map((col) => !col.some(isReading));
  if (lacking.every(Boolean)) return { kind: "chart" };
  const names = labels.filter((label, i) => lacking[i] && label !== "");
  if (names.length === 0) return { kind: "none" };
  return { kind: "series", labels: names };
}

type Stamp = ClockParts;

// 时区缺省时用运行环境的本地时区，与 lib/format 的 dateTime 不带 timeZone 时相同；uPlot 的刻度也按这个时区对齐，调用方不要另传固定时区。
function stampsOf(timestampsSec: readonly (number | null | undefined)[], timeZone: string | undefined): (Stamp | null)[] {
  const parts = clockParts(timeZone);
  return timestampsSec.map((ts) => (typeof ts === "number" && Number.isFinite(ts) ? parts(ts * 1000) : null));
}

function dayKey(s: Stamp): string {
  return `${s.year}-${s.month}-${s.day}`;
}

// incrSec 缺省是图例：每一格都是完整的「年-月-日 时:分」，不看相邻刻度。
// incrSec 给出是横轴。小于一天的刻度显示时:分；整组跨过本地日界时，首格和每个日界带月-日
// （只标日界的话，日界之前的起始日在轴上无处可看）。间隔不小于一天时刻度落在本地午夜，只写月-日。
// 跨年时首格和年份变化的那一格带年份，其余不重复。
// compact 是手机横轴：间隔小于一天时只写时:分，避免窄屏日期文字挤在一起；日级刻度保持日期。
export function formatChartTimes(
  timestampsSec: readonly (number | null | undefined)[],
  opts: { incrSec?: number; timeZone?: string; compact?: boolean } = {},
): string[] {
  const stamps = stampsOf(timestampsSec, opts.timeZone);
  if (opts.incrSec == null) {
    return stamps.map((s) => (s ? `${s.year}-${s.month}-${s.day} ${s.hour}:${s.minute}` : ""));
  }
  const present = stamps.filter((s): s is Stamp => s != null);
  if (present.length === 0) return stamps.map(() => "");
  const spansDays = present.some((s) => dayKey(s) !== dayKey(present[0]));
  const spansYears = present.some((s) => s.year !== present[0].year);
  const dayLevel = opts.incrSec >= DAY_S;
  return stamps.map((s, i) => {
    if (!s) return "";
    let prev: Stamp | null = null;
    for (let j = i - 1; j >= 0; j--) {
      const earlier = stamps[j];
      if (earlier) {
        prev = earlier;
        break;
      }
    }
    const dayChanged = prev != null && dayKey(prev) !== dayKey(s);
    const yearChanged = prev != null && prev.year !== s.year;
    const showDate = dayLevel || (!opts.compact && (prev == null ? spansDays : dayChanged));
    const showYear = spansYears && (prev == null || yearChanged);
    const date = showYear ? `${s.year}-${s.month}-${s.day}` : `${s.month}-${s.day}`;
    if (!showDate) return `${s.hour}:${s.minute}`;
    if (dayLevel) return date;
    return `${date} ${s.hour}:${s.minute}`;
  });
}
