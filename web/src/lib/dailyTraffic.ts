import type { QueryMetricsResponse } from "../gen/heron/v1/query_pb";
import { TrafficQuotaMode } from "../gen/heron/v1/types_pb";
import { clockParts, day } from "./format";
import { addDays, addMonths } from "./ymd";

// 每日流量由 QueryMetrics 的 rx_bytes / tx_bytes 按 hub 时区的日界相加得出，是粗略定位用的估算，不是账本口径：
// ingest 只把相邻上报间隔小于 TTL 的增量记进分钟桶（hub 重启后的首次上报同样不记），跨过离线的那笔只进周期用量；
// 手动校正只改周期用量、不改历史；分钟桶每分钟才刷出已闭合的那些（ingest.Flush），历史比账本晚几分钟。所以按日合计
// 与本周期的下行 + 上行可以不等，差额由 unattributedBytes 给出。

// 按小时归日要求 hub 返回的步长恰好是 3600。store.ChooseLevel 对超过 7 天的窗口选 1h 级，步长从 3600 起加大，
// 直到按步长对齐后的桶数 ceil(to / step) − floor(from / step) 不超过 max_points；这个桶数小于 跨度 / step + 2，
// 所以跨度不超过 max_points − 1 小时，步长就停在 3600。窗口最长是 31 天的周期加 DAILY_LOOKBACK_S，约 1513 小时。
export const DAILY_MAX_POINTS = 2000;
// 上周期最长 31 天；多留一天，夏令时切换让一个月多出的一小时也落在窗口内。
export const DAILY_LOOKBACK_S = 32 * 86_400;

// 右端与历史图同样按 hub 分钟归一（components/History.tsx 的 useTimeWindow）：同一分钟内不换查询键。
export function dailyWindow(periodStart: bigint, now: number): { from: number; to: number } {
  return { from: Number(periodStart) - DAILY_LOOKBACK_S, to: (Math.floor(now / 60) + 1) * 60 };
}

function span(first: string, last: string): string[] {
  const out: string[] = [];
  for (let d = first; d <= last; d = addDays(d, 1)) out.push(d);
  return out;
}

// 周期起点是重置日在 hub 时区的零点（traffic.PeriodStart），重置日不超过 28，前推一个月不会被夹到月末。
// 本周期只列到 hub 时区的今天；上周期从前推一个月的那天到本周期起点前一天。
export function periodDays(periodStart: bigint, now: number, timeZone: string): { current: string[]; previous: string[] } {
  const start = day(periodStart, timeZone);
  return { current: span(start, day(now, timeZone)), previous: span(addMonths(start, -1), addDays(start, -1)) };
}

export type DayTraffic = { day: string; rx: number; tx: number; known: boolean };

// 每个点按起始时刻归到 hub 时区的日期；known 表示这一天至少有一个点在任一方向上有读数（MetricSample.n > 0），
// 没有读数的日子不是 0。时区偏移不是整小时的 hub 上，跨日界的那个小时整块归到它起始的那一天。
export function dailyTraffic(resp: Pick<QueryMetricsResponse, "ts" | "series">, timeZone: string, days: readonly string[]): DayTraffic[] {
  const out = new Map(days.map((d) => [d, { day: d, rx: 0, tx: 0, known: false }]));
  const rx = resp.series.find((s) => s.name === "rx_bytes")?.samples ?? [];
  const tx = resp.series.find((s) => s.name === "tx_bytes")?.samples ?? [];
  const parts = clockParts(timeZone);
  resp.ts.forEach((ts, i) => {
    const p = parts(Number(ts) * 1000);
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
