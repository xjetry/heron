import type { QueryMetricsResponse } from "../gen/heron/v1/query_pb";
import { type Traffic, TrafficQuotaMode } from "../gen/heron/v1/types_pb";
import { clockParts, day } from "./format";
import { addDays } from "./ymd";

// 每日流量由 QueryMetrics 的 rx_bytes / tx_bytes 按节点计费时区（Traffic.timezone）的日界相加得出，是粗略定位用的估算，
// 不是账本口径：ingest 只把相邻上报间隔小于 TTL 的增量记进分钟桶（hub 重启后的首次上报同样不记），跨过离线的那笔只进
// 周期用量；手动校正只改周期用量、不改历史；分钟桶每分钟才刷出已闭合的那些（ingest.Flush），历史比账本晚几分钟。所以
// 按日合计与本周期的下行 + 上行可以不等，差额由 unattributedBytes 给出。

// 按小时归日要求 hub 返回的步长恰好是 3600。store.ChooseLevel 对超过 7 天的窗口选 1h 级，步长从 3600 起加大，
// 直到按步长对齐后的桶数 ceil(to / step) − floor(from / step) 不超过 max_points；这个桶数小于 跨度 / step + 2，
// 所以跨度不超过 max_points − 1 小时，步长就停在 3600。窗口从上周期起点到现在，两个周期各至多 31 天加夏令时多出的
// 一小时，约 1490 小时。
export const DAILY_MAX_POINTS = 2000;

// 右端与历史图同样按 hub 分钟归一（components/History.tsx 的 useTimeWindow）：同一分钟内不换查询键。左端是 hub 算好的
// 上一周期起点（Traffic.previous_period_start），浏览器不按重置日自己前推：短月钳位、重置时刻与夏令时只在 hub 算一处。
export function dailyWindow(previousPeriodStart: bigint, now: number): { from: number; to: number } {
  return { from: Number(previousPeriodStart), to: (Math.floor(now / 60) + 1) * 60 };
}

function span(first: string, last: string): string[] {
  const out: string[] = [];
  for (let d = first; d <= last; d = addDays(d, 1)) out.push(d);
  return out;
}

// 一个周期在图上的切片：[from, to) 是 Unix 秒，days 是这段时间在计费时区里跨过的日期。重置时刻不是 00:00 时，边界那天
// 被周期边界切成两段，前一段属于上周期、后一段属于本周期，同一个日期因此在两个周期里各出现一次，各自只计自己那一段。
export type PeriodSlice = { from: number; to: number; days: string[] };

// 周期的三个边界都取 hub 下发的值（上一周期起点、本周期起点、下次重置），日期按节点计费时区读。本周期只列到计费时区的今天。
export function periodSlices(t: Pick<Traffic, "previousPeriodStart" | "periodStart" | "nextResetAt" | "timezone">, now: number): { current: PeriodSlice; previous: PeriodSlice } {
  const previous = Number(t.previousPeriodStart);
  const start = Number(t.periodStart);
  return {
    current: { from: start, to: Number(t.nextResetAt), days: span(day(start, t.timezone), day(now, t.timezone)) },
    previous: { from: previous, to: start, days: span(day(previous, t.timezone), day(start - 1, t.timezone)) },
  };
}

export type DayTraffic = { day: string; rx: number; tx: number; known: boolean };

// 每个点（一小时）按起始时刻归到周期切片与计费时区的日期；known 表示这一天至少有一个点在任一方向上有读数
// （MetricSample.n > 0），没有读数的日子不是 0。点只有小时粒度：周期边界或日界不在整点上时（重置时刻 HH:30、
// +05:30 这类时区），跨边界的那个小时整块归到它起始的那一侧。账本的周期用量不受这条影响，它在 hub 按分钟拆分
// （store.NetBytesBetween）。
export function dailyTraffic(resp: Pick<QueryMetricsResponse, "ts" | "series">, timeZone: string, slice: PeriodSlice): DayTraffic[] {
  const out = new Map(slice.days.map((d) => [d, { day: d, rx: 0, tx: 0, known: false }]));
  const rx = resp.series.find((s) => s.name === "rx_bytes")?.samples ?? [];
  const tx = resp.series.find((s) => s.name === "tx_bytes")?.samples ?? [];
  const parts = clockParts(timeZone);
  resp.ts.forEach((ts, i) => {
    const t = Number(ts);
    if (t < slice.from || t >= slice.to) return;
    const p = parts(t * 1000);
    const entry = p && out.get(`${p.year}-${p.month}-${p.day}`);
    if (!entry) return;
    for (const [samples, key] of [[rx, "rx"], [tx, "tx"]] as const) {
      const s = samples[i];
      if (s && s.n > 0) {
        entry[key] += s.sum ?? 0;
        entry.known = true;
      }
    }
  });
  return [...out.values()];
}

// 排名的计量方向：单向，或下行加上行。
export type Direction = "rx" | "tx" | "sum";
export const amount = (d: DayTraffic, dir: Direction): number => (dir === "sum" ? d.rx + d.tx : d[dir]);

// 与 hub 的 traffic.Quota 同一口径，排名因此与流量卡"本周期"的分子按同一种方式计入；配额为 0 时节点唯一的写入口
// store.UpdateNodeTasks 把方式存成 sum，这里不必另判配额是否为 0。取大者的分子是整段两向合计的较大者，按日拆不开：
// 这里按所列日子里合计占优的方向取每天的量，两向合计接近时排名只是近似。
export function quotaDirection(mode: TrafficQuotaMode, days: readonly DayTraffic[]): Direction {
  switch (mode) {
    case TrafficQuotaMode.RX:
      return "rx";
    case TrafficQuotaMode.TX:
      return "tx";
    case TrafficQuotaMode.MAX:
      return amountOf(days, "rx") >= amountOf(days, "tx") ? "rx" : "tx";
    default:
      return "sum";
  }
}

const amountOf = (days: readonly DayTraffic[], dir: Direction) => days.reduce((sum, d) => sum + amount(d, dir), 0);

export function topDays(days: readonly DayTraffic[], dir: Direction, n = 3): DayTraffic[] {
  return days.filter((d) => d.known && amount(d, dir) > 0).sort((a, b) => amount(b, dir) - amount(a, dir)).slice(0, n);
}

// 本周期的下行加上行减去按日合计，与配额方式无关：它量的是有多少字节没归到具体日期。差额不到周期用量的 1% 时返回
// undefined：历史比账本晚几分钟（见文件开头），这部分常态落差不必提示。周期刚开始的几个小时里它也可能超过 1%，
// 所以提示文字把"最近几分钟尚未写入历史"列为原因之一。
export function unattributedBytes(periodTotal: bigint, days: readonly DayTraffic[]): number | undefined {
  const total = Number(periodTotal);
  const diff = total - amountOf(days, "sum");
  return total > 0 && Math.abs(diff) >= total / 100 ? diff : undefined;
}
