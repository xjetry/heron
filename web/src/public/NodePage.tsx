import { useQuery } from "@connectrpc/connect-query";
import { Link, useParams } from "react-router";
import { errorBanner, queryGate } from "../api/queryGate";
import { NowGrid } from "../components/NowGrid";
import { AddressFamilies } from "../components/AddressFamilies";
import { CountryBadge } from "../components/CountryBadge";
import { MetricCharts, ProbeTaskCharts, RangePicker, useHistory, type HistoryMethods } from "../components/History";
import { StatusBadge } from "../components/StatusBadge";
import { PublicService, type PublicNode } from "../gen/heron/v1/public_pb";
import { ago } from "../lib/format";
import { POLL_MS } from "../lib/poll";
import { nodeStatus } from "../lib/status";
import { hasHostFacts } from "./facts";

const PUBLIC_HISTORY: HistoryMethods = { queryMetrics: PublicService.method.queryMetrics, queryProbes: PublicService.method.queryProbes };

// 节点页：首屏是实时状态头与六个现值格，其下时间窗口、指标图、每任务一张 RTT 图、系统信息卡。
// 公开页不出现 IP、主机名、内核、agent 版本：PublicFacts 已 reserved 这些字段，前端不另有来源。
export function NodePage() {
  const { id } = useParams();
  const validId = /^\d+$/.test(id ?? "");
  const nodeId = validId ? BigInt(id!) : 0n;
  const snap = useQuery(PublicService.method.getSnapshot, {}, { enabled: validId, refetchInterval: POLL_MS });
  const node = snap.data?.nodes.find((n) => n.id === nodeId);
  const missing = <p role="alert" className="error">节点 {id} 不存在或未公开。<Link to="/">返回总览</Link></p>;
  if (!validId) return missing;
  const gate = queryGate(snap);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  if (!node) return missing;
  // 节点准入与 hub now 都由这份快照提供；缺任一项都不能开始历史查询。
  return <NodeContent node={node} now={Number(gate.data.now)} error={snap.error} />;
}

function NodeContent({ node, now, error }: { node: PublicNode; now: number; error: unknown }) {
  const history = useHistory(PUBLIC_HISTORY, node.id, now);
  const m = node.metrics;
  const f = node.facts;
  const daysLeft = node.billing?.daysLeft;
  return (
    <section className="node-page">
      {errorBanner(error, history.metrics.error, history.probes.error)}
      <header className="node-head">
        <div className="node-head-title">
          <h1>{node.name}</h1>
          {node.country && <CountryBadge code={node.country} />}
          <AddressFamilies network={f?.network} />
          <StatusBadge status={nodeStatus(node)} detail={node.lastSeenAt !== undefined ? `最近上报 ${ago(node.lastSeenAt, now)}` : undefined} />
        </div>
        {node.publicRemark && <p className="node-remark">{node.publicRemark}</p>}
        {node.tags.length > 0 && <ul className="tag-chips">{node.tags.map((tag) => <li key={tag} className="chip">{tag}</li>)}</ul>}
        {/* 系统信息写成页头里的一行，不单独铺一张全宽卡片；主机信息从未上报时整行不画（只有手填出口地址、facts 只带 network 的节点同样不画），缺的单项不写。 */}
        {hasHostFacts(f) && (
          <dl className="facts-inline">
            {f.os && <div><dt>系统</dt><dd>{f.os}</dd></div>}
            {f.arch && <div><dt>架构</dt><dd>{f.arch}</dd></div>}
            {f.cpuModel && <div><dt>CPU</dt><dd>{f.cpuModel}{f.cpuCores ? ` × ${f.cpuCores}` : ""}</dd></div>}
            <div><dt>虚拟化</dt><dd>{f.virtualization || "无 / 未知"}</dd></div>
          </dl>
        )}
        <NowGrid metrics={m} daysLeft={daysLeft} rented={node.rented} />
      </header>
      <header className="row detail-header">
        <RangePicker history={history} />
      </header>
      <MetricCharts history={history} />
      <ProbeTaskCharts history={history} noProbes={<p className="muted">窗口内没有探测结果。</p>} titleLink={(taskId, title) => <Link to={`/probes/${taskId}`}>{title}</Link>} />
    </section>
  );
}
