import { gridOf } from "./series";

// 晚高峰：hub 时区（--timezone，经 GetSettings.timezone / GetSite.timezone 下发）每个日历日的 20:00 到次日 00:00。
// 一个时刻在晚高峰内 ⇔ 它在 hub 时区的墙钟小时数 ≥ 20。墙钟取自 Intl，不按"20:00 加 4 小时"或固定偏移推算：
// 夏令时切换落在晚间时，这一段可以是 3、5 小时，同一个 23 点也可以出现两次——两次都算晚高峰。
export const EVENING_PEAK_START_HOUR = 20;
export const EVENING_PEAK_LABEL = "20:00–24:00";

// inside：桶 [ts, ts + stepS) 的每个时刻都在晚高峰内；partial：一部分在；outside：都不在。
export type PeakFit = "inside" | "partial" | "outside";

type Wall = { date: string; hour: number; offsetS: number };

function wallClock(timeZone: string): (unixS: number) => Wall {
  // 无效的时区名在这里由 Intl 抛 RangeError；调用方据此认定拿不到可用的 hub 时区。
  const fmt = new Intl.DateTimeFormat("en-US", { timeZone, year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23" });
  return (unixS) => {
    const p: Record<string, number> = {};
    for (const part of fmt.formatToParts(unixS * 1000)) if (part.type !== "literal") p[part.type] = Number(part.value);
    const asUTC = Date.UTC(p.year, p.month - 1, p.day, p.hour, p.minute, p.second) / 1000;
    return { date: `${p.year}-${p.month}-${p.day}`, hour: p.hour, offsetS: asUTC - unixS };
  };
}

// eveningPeakFit 返回按 timeZone 判定桶与晚高峰关系的函数；结果按 (ts, stepS) 记住，同一张网格上的各条序列共用一次判定。
// 面板与公开页的单节点探测历史、跨节点对比都只经这一个函数切晚高峰。
//
// 判定把桶按 UTC 偏移切成若干段，每段偏移不变、墙钟随时间单调前进：段内全在晚高峰 ⇔ 段首小时 ≥ 20 且段首段尾同一日历日
// （没越过午夜）；段与晚高峰相交 ⇔ 段首或段尾小时 ≥ 20，或段首段尾跨了日（越过午夜前必经 23 点）。找段的方式是二分：
// 区间两端偏移相同即当作一段。这依赖一个时区不在一个桶里改两次偏移又改回原值；能判成 inside 的桶都在同一日历日、
// 短于 26 小时，tzdata 2026f 在 2000–2040 年里没有哪个区在 26 小时内改两次偏移。不成立时，被夹在中间的那一段
// 墙钟会被当成连续的，可能把它判成 inside。
export function eveningPeakFit(timeZone: string): (ts: number, stepS: number) => PeakFit {
  const wall = wallClock(timeZone);
  const memo = new Map<string, PeakFit>();
  const segments = (a: number, b: number, wa: Wall, wb: Wall, out: [Wall, Wall][]) => {
    if (wa.offsetS === wb.offsetS) {
      out.push([wa, wb]);
      return;
    }
    if (b - a <= 1) {
      out.push([wa, wa], [wb, wb]);
      return;
    }
    const m = Math.floor((a + b) / 2);
    segments(a, m, wa, wall(m), out);
    segments(m + 1, b, wall(m + 1), wb, out);
  };
  return (ts, stepS) => {
    const key = `${ts}/${stepS}`;
    const known = memo.get(key);
    if (known !== undefined) return known;
    // 时间戳是整秒，桶右端开区间：最后一个时刻取 ts + stepS − 1。
    const last = ts + Math.max(stepS, 1) - 1;
    const parts: [Wall, Wall][] = [];
    segments(ts, last, wall(ts), wall(last), parts);
    let inside = true;
    let touches = false;
    for (const [s, e] of parts) {
      const sameDay = s.date === e.date;
      if (!(s.hour >= EVENING_PEAK_START_HOUR && sameDay)) inside = false;
      if (s.hour >= EVENING_PEAK_START_HOUR || e.hour >= EVENING_PEAK_START_HOUR || !sameDay) touches = true;
    }
    const fit: PeakFit = inside ? "inside" : touches ? "partial" : "outside";
    memo.set(key, fit);
    return fit;
  };
}

// 能否拿 hub 时区构造判定函数：Intl 不认识这个名字时返回 undefined，调用方不提供晚高峰切片，也不退回访客本地时区。
export function tryEveningPeakFit(timeZone: string | undefined): ((ts: number, stepS: number) => PeakFit) | undefined {
  if (!timeZone) return undefined;
  try {
    return eveningPeakFit(timeZone);
  } catch {
    return undefined;
  }
}

// covered：窗口网格上至少一个桶整个落在晚高峰内。step-too-large：窗口经过晚高峰，但每个经过的桶都跨了界——步长太粗，
// 缩短窗口才会换到更细的级别。outside-window：窗口根本没经过晚高峰，缩短窗口无济于事。只看网格，不看有没有数据：
// "这一段没有读数"与"这个步长切不出晚高峰"是两件事。
export type PeakCoverage = "covered" | "step-too-large" | "outside-window";

export function eveningPeakCoverage(from: number, to: number, stepS: number, fit: (ts: number, stepS: number) => PeakFit): PeakCoverage {
  let touches = false;
  for (const ts of gridOf(stepS, from, to)) {
    const f = fit(ts, stepS);
    if (f === "inside") return "covered";
    if (f === "partial") touches = true;
  }
  return touches ? "step-too-large" : "outside-window";
}

// 只留整桶落在晚高峰内的样本。跨界的桶整个丢掉：它的均值、最值与丢包里混着非晚高峰时段，拆不开。
export function sliceEveningPeak<T extends { ts: bigint }>(samples: readonly T[], stepS: number, fit: (ts: number, stepS: number) => PeakFit): T[] {
  return samples.filter((s) => fit(Number(s.ts), stepS) === "inside");
}
