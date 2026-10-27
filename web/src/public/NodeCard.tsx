import { Link } from "react-router";
import { AddressFamilies, addressFamilies } from "../components/AddressFamilies";
import { Bar, Missing, usage } from "../components/Bar";
import { CountryBadge } from "../components/CountryBadge";
import { CountryFlag, useFlagEmoji } from "../components/CountryFlag";
import { Expiry } from "../components/Expiry";
import { OsIcon } from "../components/OsIcon";
import { StatusBadge } from "../components/StatusBadge";
import type { PublicNode } from "../gen/heron/v1/public_pb";
import { priceText } from "../lib/billing";
import { ago, bytes, duration, percent } from "../lib/format";
import { nodeStatus } from "../lib/status";
import { trafficDetail, trafficText } from "../lib/traffic";
import { hasHostFacts } from "./facts";

export function NodeCard({ node }: { node: PublicNode; now: number }) {
  const status = nodeStatus(node);
  const m = node.metrics;
  const f = node.facts;
  // 国家 / 地区：访客的浏览器能渲染该旗帜时名称前放 emoji（CountryFlag），否则保留名称后的文字徽章。
  const flag = useFlagEmoji(node.country);
  // 系统行：os 不再出文字，只剩一个图标（OsIcon），文字是 虚拟化 · 架构；没有主机信息时整段不画，
  // 只画双栈标记（与节点页、详情面板同一判定 hasHostFacts）；连双栈标记都没有时整行不画。
  const system = [f?.virtualization, f?.arch].filter(Boolean).join(" · ");
  const host = hasHostFacts(f);
  const families = addressFamilies(f?.network);
  const price = priceText(node.billing);
  const b = node.billing;
  return (
    <article className="node-card" aria-label={node.name} data-status={status}>
      <header className="node-card-head">
        {flag !== "" && <CountryFlag code={node.country} />}
        <h2><Link to={`/nodes/${node.id}`} title={node.name}>{node.name}</Link></h2>
        {flag === "" && node.country && <CountryBadge code={node.country} />}
        <StatusBadge status={status} />
      </header>
      {node.publicRemark && <p className="node-remark">{node.publicRemark}</p>}
      {/* 双栈标记放在系统信息行尾而不是标题行：卡片约 260px 宽，标题行再放两个标记会把短名称也截断。 */}
      {(host || families.length > 0) && (
        <p className="node-card-meta muted">
          {host && <><OsIcon os={f.os} />{system !== "" && <> {system}</>}</>}
          {host && families.length > 0 && " "}
          <AddressFamilies network={f?.network} />
        </p>
      )}
      <div className="node-meters">
        <Meter label="CPU" value={m?.cpuPct} detail={f?.cpuCores ? `${f.cpuCores} 核` : undefined} />
        <Capacity label="内存" used={m?.memUsed} total={m?.memTotal} />
        <Capacity label="磁盘" used={m?.diskUsed} total={m?.diskTotal} />
      </div>
      <p className="node-network num" role="group" aria-label="网络速率">
        ↓ {m?.netRxBps !== undefined ? `${bytes(m.netRxBps)}/s` : <Missing />} ↑ {m?.netTxBps !== undefined ? `${bytes(m.netTxBps)}/s` : <Missing />}
      </p>
      <dl className="node-card-foot">
        <dt>本周期</dt>
        <dd>{node.traffic ? <><span className="num">{trafficText(node.traffic)}</span><span className="muted">{trafficDetail(node.traffic)}</span></> : "–"}</dd>
        {/* total_rx / total_tx 是 Traffic 里不带 optional 的计数，0 就是没有流量而不是没取到；
            只有整个 traffic 缺失才是没有数据，此时不出这一行，而不是写成 ↓ 0 B ↑ 0 B。 */}
        {node.traffic && <>
          <dt>累计流量</dt>
          <dd><span className="num">↓ {bytes(node.traffic.totalRx)} ↑ {bytes(node.traffic.totalTx)}</span></dd>
        </>}
        <dt>费用</dt>
        <dd><span className="num">{price || "–"}</span></dd>
        <dt>到期</dt>
        <dd>{b?.expiresOn ? <Expiry billing={b} rented={node.rented} /> : "–"}</dd>
        <dt>运行时长</dt>
        <dd><span className="num">{m?.uptimeS !== undefined ? duration(m.uptimeS) : "–"}</span></dd>
      </dl>
    </article>
  );
}

// 无读数与 0 是两个事实：缺失不画条，画一条虚线轨。
// 百分比文字与条长都取自同一个 value，两者不会各说各的；detail 是绝对量，另起一行放在条下：
// 多列网格里卡片可窄到 240px（.cards 的列宽下限），"1023 GiB / 1024 GiB (100%)" 与条同排时会把条挤没。
function Meter({ label, value, detail }: { label: string; value?: number; detail?: string }) {
  return (
    <div className="node-meter">
      <span className="muted">{label}</span>
      {value !== undefined ? <Bar thin value={value} label={`${label} ${detail ? `${detail} (${percent(value)})` : percent(value)}`} /> : <span className="meter-missing" aria-hidden="true" />}
      <span className="num">{value !== undefined ? percent(value) : <Missing />}</span>
      {detail && <span className="node-meter-detail num muted">{detail}</span>}
    </div>
  );
}

// 使用率与无读数的判定取自 usage，与状态墙、节点页、列表及管理端同一个函数。
function Capacity({ label, used, total }: { label: string; used?: bigint; total?: bigint }) {
  const u = usage(used, total);
  if (!u) return <Meter label={label} />;
  return <Meter label={label} value={u.pct} detail={`${bytes(u.used)} / ${bytes(u.total)}`} />;
}

// 卡片网格 + 网格下方两张紧凑表：离线与从未上报一张（名称、地区、状态、最后上报），已出租一张（名称、地区、到期）——
// 租出去的机器不会有上报，状态与最后上报对它没有意义，到期是它仍关心的事；并进离线那张表会让它看起来像故障。
// 传入的 nodes 已按调用方的排序；紧凑表保持同一顺序。
// 紧凑表默认展开：open 与状态墙的地区分组一样只是初始值，React 只在这个 prop 变化时写 DOM，
// 轮询带来的重渲染不会把访客收起的分组再展开。
export function CardGrid({ nodes, now }: { nodes: readonly PublicNode[]; now: number }) {
  // 卡片只属于在线与维护中。已出租由 nodeStatus 排在二者之前，即使 online 仍为真也进已出租表，不在这里再判一次 rented。
  const live = nodes.filter((n) => { const s = nodeStatus(n); return s === "online" || s === "maintenance"; });
  const rented = nodes.filter((n) => nodeStatus(n) === "rented");
  const rest = nodes.filter((n) => !live.includes(n) && !rented.includes(n));
  return (
    <>
      <div className="cards">
        {live.map((n) => <NodeCard key={String(n.id)} node={n} now={now} />)}
      </div>
      {rest.length > 0 && (
        <details className="folded-nodes" open>
          <summary>离线与从未上报 · {rest.length}</summary>
          <table className="compact-table">
            <thead><tr><th>名称</th><th>地区</th><th>状态</th><th>最后上报</th></tr></thead>
            <tbody>
              {rest.map((n) => (
                <tr key={String(n.id)}>
                  <td><Link to={`/nodes/${n.id}`}>{n.name}</Link></td>
                  <td>{n.country && <CountryBadge code={n.country} />}</td>
                  <td><StatusBadge status={nodeStatus(n)} /></td>
                  <td className="num">{n.lastSeenAt !== undefined ? ago(n.lastSeenAt, now) : "–"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </details>
      )}
      {rented.length > 0 && (
        <details className="folded-nodes" open>
          <summary>已出租 · {rented.length}</summary>
          <table className="compact-table">
            <thead><tr><th>名称</th><th>地区</th><th>到期</th></tr></thead>
            <tbody>
              {rented.map((n) => (
                <tr key={String(n.id)}>
                  <td><Link to={`/nodes/${n.id}`}>{n.name}</Link></td>
                  <td>{n.country && <CountryBadge code={n.country} />}</td>
                  <td>{n.billing?.expiresOn ? <Expiry billing={n.billing} rented={n.rented} /> : "–"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </details>
      )}
    </>
  );
}
