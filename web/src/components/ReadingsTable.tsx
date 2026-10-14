import type { ReactNode } from "react";
import type { Traffic } from "../gen/heron/v1/types_pb";
import { ago, bytes, duration, percent } from "../lib/format";
import { STATUS_LABEL, type NodeStatus } from "../lib/status";
import { trafficText } from "../lib/traffic";
import { Bar, Missing, usage } from "./Bar";

// 管理端 Metrics 与公开页 PublicMetrics 共有的读数字段；两边的消息按结构满足它，表格不区分来源。
export type Readings = {
  cpuPct?: number; load1?: number; load5?: number; load15?: number;
  memUsed?: bigint; memTotal?: bigint; diskUsed?: bigint; diskTotal?: bigint;
  netRxBps?: bigint; netTxBps?: bigint; uptimeS?: bigint;
};

// 一行要画的东西。status 缺省表示状态未知（管理端节点不在快照里时）；name 是名称格的内容，链接与徽章由调用方给出。
export type ReadingsRow = {
  key: string; label: string; name: ReactNode;
  status: NodeStatus | undefined;
  metrics: Readings | undefined; traffic: Traffic | undefined; lastSeenAt: bigint | undefined;
};

// 调用方追加的列，插在「最近上报」之前。表头与每行的单元格出自同一份定义，列数对不上的情形写不出来。
export type ReadingsColumn<T> = { label: string; cell: (item: T) => ReactNode };

// 实时读数表：管理端总览与公开页的列表视图共用同一组列、同一种细条与缺读数画法；手机上每行折成一张卡
// （styles.css 的 .readings-table，按 data-label 放置单元格）。表格外框与表头的外观由各自的样式表给出，className 追加。
export function ReadingsTable<T>({ label, items, row, extra = [], now, className }: {
  label: string;
  items: readonly T[];
  row: (item: T) => ReadingsRow;
  extra?: readonly ReadingsColumn<T>[];
  now: number;
  className?: string;
}) {
  return (
    <div className="table-scroll" role="region" aria-label={label} tabIndex={0}>
      <table className={className ? `readings-table ${className}` : "readings-table"}>
        <thead>
          <tr>
            <th>状态</th><th>节点</th><th>CPU</th><th>内存</th><th>磁盘</th><th>负载</th><th>网络</th><th>本周期</th><th>运行时长</th>
            {extra.map((column) => <th key={column.label}>{column.label}</th>)}
            <th>最近上报</th>
          </tr>
        </thead>
        <tbody>
          {items.map((item) => {
            const r = row(item);
            const m = r.metrics;
            return (
              <tr key={r.key} aria-label={r.label} data-status={r.status ?? "unknown"}>
                <td data-label="状态"><span className="status-dot" data-status={r.status} role="img" aria-label={r.status ? STATUS_LABEL[r.status] : "状态未知"} /></td>
                <td data-label="节点">{r.name}</td>
                <td data-label="CPU"><Meter label="CPU" value={m?.cpuPct} /></td>
                <td data-label="内存"><Meter label="内存" value={usage(m?.memUsed, m?.memTotal)?.pct} /></td>
                <td data-label="磁盘"><Meter label="磁盘" value={usage(m?.diskUsed, m?.diskTotal)?.pct} /></td>
                <td data-label="负载" className="num">{m?.load1 !== undefined && m.load5 !== undefined && m.load15 !== undefined ? `${m.load1.toFixed(2)} / ${m.load5.toFixed(2)} / ${m.load15.toFixed(2)}` : <Missing />}</td>
                <td data-label="网络" className="num">{m?.netRxBps !== undefined && m.netTxBps !== undefined ? `↓ ${bytes(m.netRxBps)}/s ↑ ${bytes(m.netTxBps)}/s` : <Missing />}</td>
                <td data-label="本周期" className="num">{r.traffic ? trafficText(r.traffic) : <Missing />}</td>
                <td data-label="运行时长" className="num">{m?.uptimeS !== undefined ? duration(m.uptimeS) : <Missing />}</td>
                {extra.map((column) => <td key={column.label} data-label={column.label}>{column.cell(item)}</td>)}
                <td data-label="最近上报" className="muted">{r.lastSeenAt !== undefined ? ago(r.lastSeenAt, now) : STATUS_LABEL.never}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function Meter({ label, value }: { label: string; value: number | undefined }) {
  if (value === undefined) return <Missing />;
  return <><Bar thin value={value} label={`${label} ${percent(value)}`} /><span className="num">{percent(value)}</span></>;
}
