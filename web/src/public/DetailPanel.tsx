import { useQuery } from "@connectrpc/connect-query";
import type { ReactNode } from "react";
import { Link } from "react-router";
import { Bar, Missing, usage } from "../components/Bar";
import { AddressFamilies } from "../components/AddressFamilies";
import { CountryBadge } from "../components/CountryBadge";
import { Expiry } from "../components/Expiry";
import { Sparkline } from "../components/Sparkline";
import { StatusBadge } from "../components/StatusBadge";
import { PublicService, type PublicNode } from "../gen/heron/v1/public_pb";
import { priceText } from "../lib/billing";
import { ago, bytes, duration, formatUnit, percent } from "../lib/format";
import { toAligned } from "../lib/series";
import { nodeStatus } from "../lib/status";
import { trafficDetail, trafficText } from "../lib/traffic";
import { hasHostFacts } from "./facts";

const HOUR_S = 3600;
// 请求最多 60 点；边界对齐 hub 时钟的整分钟，使同一分钟内的快照共用查询键。
const SPARK_POINTS = 60;

function Row({ label, children }: { label: string; children: ReactNode }) {
  return <div className="detail-row"><dt>{label}</dt><dd>{children}</dd></div>;
}

// 与墙上方块、卡片同一种细条：条只表示比例，读数写在条右侧，不压在条上。读屏名仍是「标签 读数」。
function Meter({ value, label, text }: { value: number; label: string; text: string }) {
  return <span className="detail-meter"><Bar thin value={value} label={`${label} ${text}`} /><span className="num">{text}</span></span>;
}

// 使用率与无读数的判定取自 usage，与节点卡片同口径；读数写法也与卡片进度条的读屏名相同："用量 / 总量 (百分比)"。
// 无读数时整行写破折号、不画条，对应卡片的虚线轨加破折号。
function Capacity({ label, used, total }: { label: string; used?: bigint; total?: bigint }) {
  const u = usage(used, total);
  if (!u) return <Row label={label}><Missing /></Row>;
  return <Row label={label}><Meter value={u.pct} label={label} text={`${bytes(u.used)} / ${bytes(u.total)} (${percent(u.pct)})`} /></Row>;
}

// 详情面板消费选中节点的公开字段；hub 按请求来源限流，因此网络速率的最近 1 小时迷你线只为选中的节点查询，
// 不为总览中每张卡片各发历史请求，避免请求数随卡片数增长。
export function DetailPanel({ node, now }: { node: PublicNode; now: number }) {
  const status = nodeStatus(node);
  const m = node.metrics;
  const f = node.facts;
  const minute = Math.floor(now / 60) * 60;
  const from = minute - HOUR_S;
  const to = minute + 60;
  const history = useQuery(PublicService.method.queryMetrics, { nodeId: node.id, from: BigInt(from), to: BigInt(to), maxPoints: SPARK_POINTS }, { staleTime: 60_000 });
  const spark = history.data ? toAligned(history.data, [{ name: "rx_bytes", value: "sum-rate", label: "下行" }, { name: "tx_bytes", value: "sum-rate", label: "上行" }], from, to) : null;
  const rate = spark ? (spark[1] as (number | null)[]).map((rx, i) => {
    const tx = (spark[2] as (number | null)[])[i];
    return rx === null && tx === null ? null : (rx ?? 0) + (tx ?? 0);
  }) : null;
  const price = priceText(node.billing);
  return (
    <aside className="detail-panel" aria-label="节点详情">
      <header>
        <h2>{node.name}</h2>
        {node.country && <CountryBadge code={node.country} />}
        <AddressFamilies network={f?.network} />
        <StatusBadge status={status} detail={node.lastSeenAt !== undefined ? `最近上报 ${ago(node.lastSeenAt, now)}` : undefined} />
      </header>
      {node.publicRemark && <p className="node-remark">{node.publicRemark}</p>}
      {node.tags.length > 0 && <ul className="tag-chips">{node.tags.map((tag) => <li key={tag} className="chip">{tag}</li>)}</ul>}
      <dl className="detail-list">
        {hasHostFacts(f) && <Row label="系统">{[f.os, f.virtualization, f.arch].filter(Boolean).join(" · ") || <Missing />}</Row>}
        {f && f.cpuModel && <Row label="CPU">{f.cpuModel} × {f.cpuCores}</Row>}
        <Row label="运行时长">{m?.uptimeS !== undefined ? `运行 ${duration(m.uptimeS)}` : <Missing />}</Row>
        <Row label="CPU 使用">{m?.cpuPct !== undefined ? <Meter value={m.cpuPct} label="CPU" text={percent(m.cpuPct)} /> : <Missing />}</Row>
        <Capacity label="内存" used={m?.memUsed} total={m?.memTotal} />
        <Capacity label="交换" used={m?.swapUsed} total={m?.swapTotal} />
        <Capacity label="磁盘" used={m?.diskUsed} total={m?.diskTotal} />
        <Row label="负载">{m?.load1 !== undefined && m.load5 !== undefined && m.load15 !== undefined ? <span className="num">{m.load1.toFixed(2)} / {m.load5.toFixed(2)} / {m.load15.toFixed(2)}</span> : <Missing />}</Row>
        <Row label="网络">
          {m?.netRxBps !== undefined && m.netTxBps !== undefined ? <span className="num">↓ {bytes(m.netRxBps)}/s ↑ {bytes(m.netTxBps)}/s</span> : <Missing />}
          {rate && <Sparkline values={rate} label="最近 1 小时网络速率" />}
        </Row>
        <Row label="本周期">{node.traffic ? <><span className="num">{trafficText(node.traffic)}</span> <span className="muted">{trafficDetail(node.traffic)}</span></> : <Missing />}</Row>
        <Row label="磁盘读写">{m?.diskReadBps !== undefined && m.diskWriteBps !== undefined ? <span className="num">读 {bytes(m.diskReadBps)}/s 写 {bytes(m.diskWriteBps)}/s</span> : <Missing />}</Row>
        <Row label="连接">{m?.tcpConns !== undefined && m.udpConns !== undefined && m.procs !== undefined ? <span className="num">TCP {m.tcpConns} · UDP {m.udpConns} · 进程 {m.procs}</span> : <Missing />}</Row>
        {/* 四项来自同一次采样、同时出现或同时缺失；发出段为 0 时重传率没有定义，写破折号而不是 0%。 */}
        <Row label="TCP">{m?.tcpActiveOpensPs !== undefined && m.tcpPassiveOpensPs !== undefined && m.tcpOutSegsPs !== undefined && m.tcpRetransSegsPs !== undefined
          ? <span className="num">建连 {formatUnit(m.tcpActiveOpensPs, "count/s")} · 接入 {formatUnit(m.tcpPassiveOpensPs, "count/s")} · 重传 {m.tcpOutSegsPs > 0 ? percent((m.tcpRetransSegsPs / m.tcpOutSegsPs) * 100) : "–"}</span>
          : <Missing />}</Row>
        {price && <Row label="费用"><span className="num">{price}</span></Row>}
        {node.billing?.expiresOn && <Row label="到期"><Expiry billing={node.billing} rented={node.rented} /></Row>}
      </dl>
      <Link to={`/nodes/${node.id}`} className="detail-more">查看完整历史 →</Link>
    </aside>
  );
}
