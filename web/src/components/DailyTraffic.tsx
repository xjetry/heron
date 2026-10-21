import { useQuery } from "@connectrpc/connect-query";
import { skipToken } from "@tanstack/react-query";
import { type CSSProperties, useId, useState } from "react";
import { useRetained } from "../api/useRetained";
import { AdminService } from "../gen/heron/v1/admin_pb";
import type { QueryMetricsResponse } from "../gen/heron/v1/query_pb";
import { type Traffic, TrafficQuotaMode } from "../gen/heron/v1/types_pb";
import { DAILY_MAX_POINTS, type DayTraffic, type Direction, amount, dailyTraffic, dailyWindow, periodDays, quotaDirection, topDays, unattributedBytes } from "../lib/dailyTraffic";
import { bytes } from "../lib/format";
import { SERIES_PALETTE } from "../lib/seriesPalette";
import { QUOTA_MODE_LABELS } from "../lib/traffic";
import { InfoTip } from "./InfoTip";

// 下行、上行沿用网络图的前两种系列色（History 的网络面板 rx_bytes、tx_bytes 依次取色），同一节点上两张图的方向配色一致。
const RX_COLOR = SERIES_PALETTE[0];
const TX_COLOR = SERIES_PALETTE[1];

// 查询只在流量 tab 打开时发（enabled），hook 仍放在页面顶层，与告警事件同一种写法：错误进页面横幅。
// identity 传 nodeId：切到另一个节点时不沿用上一个节点的数据（见 useRetained）。
export function useDailyTraffic(nodeId: bigint, periodStart: bigint | undefined, now: number | undefined, enabled: boolean) {
  const w = periodStart !== undefined && now !== undefined ? dailyWindow(periodStart, now) : undefined;
  const request = enabled && w ? { nodeId, from: BigInt(w.from), to: BigInt(w.to), maxPoints: DAILY_MAX_POINTS } : skipToken;
  return useRetained(useQuery(AdminService.method.queryMetrics, request), nodeId);
}

type Period = "current" | "previous";

const total = (d: DayTraffic) => d.rx + d.tx;
const dayLabel = (d: DayTraffic, today: string) => (d.day === today ? `${d.day}（今天，统计中）` : d.day);
const describe = (d: DayTraffic, today: string) =>
  d.known ? `${dayLabel(d, today)} 下行 ${bytes(d.rx)} 上行 ${bytes(d.tx)} 合计 ${bytes(total(d))}` : `${dayLabel(d, today)} 无数据`;
// 上行叠在下行之上；零用量的方向不画，免得段间 2px 间隔顶在一段空柱上。第一段是柱顶，圆角只给它。
const segments = (d: DayTraffic) =>
  [{ key: "tx", value: d.tx, color: TX_COLOR }, { key: "rx", value: d.rx, color: RX_COLOR }].filter((seg) => seg.value > 0);
const readout = (d: DayTraffic) =>
  d.known ? `${d.day}：下行 ${bytes(d.rx)}，上行 ${bytes(d.tx)}，合计 ${bytes(total(d))}` : `${d.day}：无数据`;
// 计入方式沿用流量卡副标题的叫法；取大者另注明这段时间按哪个方向排。
const measureLabel = (mode: TrafficQuotaMode, dir: Direction) =>
  mode === TrafficQuotaMode.MAX ? `${QUOTA_MODE_LABELS[mode]}，按${dir === "rx" ? "下载" : "上传"}计` : QUOTA_MODE_LABELS[mode];

export function DailyTraffic({ response, traffic, timeZone, now }: { response: QueryMetricsResponse | undefined; traffic: Traffic; timeZone: string; now: number }) {
  const titleId = useId();
  const [period, setPeriod] = useState<Period>("current");
  const [hovered, setHovered] = useState<string | null>(null);
  const days = periodDays(traffic.periodStart, now, timeZone);
  const today = days.current.at(-1) ?? "";
  const list = response ? dailyTraffic(response, timeZone, period === "current" ? days.current : days.previous) : undefined;
  const peak = list ? Math.max(0, ...list.map(total)) : 0;
  const dir = list ? quotaDirection(traffic.quotaMode, list) : "sum";
  const top = list ? topDays(list, dir) : [];
  const rank = new Map(top.map((d, i) => [d.day, i + 1]));
  const residual = list && period === "current" ? unattributedBytes(traffic.periodRx + traffic.periodTx, list) : undefined;
  const shown = list?.find((d) => d.day === hovered) ?? top[0];
  // 高度相对本周期（或上周期）内用量最高的一天；没有任何用量时不画柱子。
  const height = (v: number): CSSProperties => ({ height: peak > 0 ? `${(v / peak) * 100}%` : 0 });
  return (
    <section className="card daily-traffic" aria-labelledby={titleId}>
      <header className="row">
        <h2 id={titleId}>每日流量</h2>
        <nav aria-label="周期" className="segmented">
          <button type="button" aria-pressed={period === "current"} onClick={() => setPeriod("current")}>本周期</button>
          <button type="button" aria-pressed={period === "previous"} onClick={() => setPeriod("previous")}>上周期</button>
        </nav>
        <InfoTip label="口径说明">
          由历史数据里每小时的下行、上行字节数按 hub 时区（{timeZone}）的日期相加，用来粗略定位哪天用得多；用量最高的日子按配额的计入方式排，
          与流量卡的"本周期"一致。离线期间、hub 重启后首次上报的流量与手动校正只计入周期用量，不归到具体日期；超出 1 小时级保留期的日子显示无数据。
        </InfoTip>
      </header>
      {!list ? <p className="muted">加载中…</p> : <>
        <ul className="daily-legend" aria-label="图例">
          <li><span className="daily-swatch" style={{ background: RX_COLOR }} />下行</li>
          <li><span className="daily-swatch" style={{ background: TX_COLOR }} />上行</li>
        </ul>
        <p className="daily-readout num" data-testid="daily-readout">{shown ? readout(shown) : "这段时间没有用量"}</p>
        <div className="daily-plot">
          <span className="daily-axis muted num" aria-hidden="true">{bytes(peak)}</span>
          <ol className="daily-bars" aria-label="每日用量" onPointerLeave={() => setHovered(null)}>
            {list.map((d) => (
              <li key={d.day} tabIndex={0} aria-label={describe(d, today)} data-known={d.known || undefined} data-today={d.day === today || undefined}
                data-hovered={d.day === hovered || undefined} onPointerEnter={() => setHovered(d.day)} onFocus={() => setHovered(d.day)} onBlur={() => setHovered(null)}>
                <span className="daily-column">
                  {rank.has(d.day) && <span className="daily-rank num" aria-hidden="true">{rank.get(d.day)}</span>}
                  {/* 无数据与用量为 0 都没有柱段，只靠这个占位在图上区分；读屏走 aria-label。 */}
                  {!d.known && <span className="daily-missing muted" aria-hidden="true">–</span>}
                  {segments(d).map((seg, i) => <span key={seg.key} className="daily-segment" data-top={i === 0 || undefined} style={{ ...height(seg.value), background: seg.color }} />)}
                </span>
                <span className="daily-day num muted" aria-hidden="true">{d.day === today ? "今" : Number(d.day.slice(8))}</span>
              </li>
            ))}
          </ol>
        </div>
        {top.length > 0 && <p>用量最高（{measureLabel(traffic.quotaMode, dir)}）：{top.map((d) => `${d.day.slice(5)} ${bytes(amount(d, dir))}`).join("、")}</p>}
        {residual !== undefined && <p className="muted">
          {residual > 0
            ? `按日合计比本周期的下行 + 上行少约 ${bytes(residual)}：离线期间与 hub 重启后首次上报的流量、往大的手动校正只计入周期用量，不归到具体日期；最近几分钟的流量也还没写入历史。`
            : `按日合计比本周期的下行 + 上行多约 ${bytes(-residual)}：手动校正只改周期用量、不改历史，本周期往小校正过就会这样。`}
        </p>}
        <details className="daily-table">
          <summary>按日明细</summary>
          <table>
            <thead><tr><th scope="col">日期</th><th scope="col">下行</th><th scope="col">上行</th><th scope="col">合计</th></tr></thead>
            <tbody>
              {list.map((d) => <tr key={d.day}>
                <th scope="row">{dayLabel(d, today)}</th>
                {d.known ? <><td className="num">{bytes(d.rx)}</td><td className="num">{bytes(d.tx)}</td><td className="num">{bytes(total(d))}</td></>
                  : <td colSpan={3} className="muted">无数据</td>}
              </tr>)}
            </tbody>
          </table>
        </details>
      </>}
    </section>
  );
}
