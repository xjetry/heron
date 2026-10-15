import { useQuery } from "@connectrpc/connect-query";
import { useState } from "react";
import { Link } from "react-router";
import { errorBanner, queryGateAll } from "../api/queryGate";
import { PageHeader } from "../components/PageHeader";
import { ReadingsTable } from "../components/ReadingsTable";
import { AdminService } from "../gen/heron/v1/admin_pb";
import { useNodeList } from "../api/useNodeList";
import { liveById, liveStatus } from "../lib/adminStatus";
import { attentionCards } from "../lib/attention";
import { withId } from "../lib/ids";
import { filterNodes } from "../lib/nodeSearch";
import { POLL_MS } from "../lib/poll";
import { EmptyState } from "../components/EmptyState";

// ListNodes 提供维护状态、到期与 agent 版本，GetSnapshot 提供在线裁决与读数；
// queryGateAll 等待两者首次到达，避免缺少维护状态时误判节点状态。
const DETAILS_MS = 10_000;

// Overview 的待办卡使用 ListNodes 的维护、到期与 agent 版本以及 GetSnapshot 的在线裁决；
// 卡片链接携带同一条件进入节点列表，数字不能另造一套状态口径。
export function Overview() {
  const [search, setSearch] = useState("");
  const snap = useQuery(AdminService.method.getSnapshot, {}, { refetchInterval: POLL_MS });
  const details = useNodeList({}, { refetchInterval: DETAILS_MS });
  const rules = useQuery(AdminService.method.listAlertRules, {}, { refetchInterval: DETAILS_MS });
  const gate = queryGateAll(snap, details);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  const [snapshot, listed] = gate.data;
  const now = Number(snapshot.now);
  const live = liveById(snapshot.nodes);
  const cards = attentionCards({ nodes: listed.nodes, live, boundAgentVersion: snapshot.boundAgentVersion, states: rules.data?.states });
  const rows = filterNodes(listed.nodes, search);
  return (
    <section>
      <PageHeader title="总览" />
      {gate.banner}
      {errorBanner(rules.error)}
      <ul className="attention" aria-label="需要处理">
        {cards.map((card) => (
          <li key={card.key} data-key={card.key} data-zero={card.count === 0 || undefined}>
            <Link to={card.to}><strong className="num">{card.count === null ? "—" : card.count}</strong><span>{card.label}</span>{card.note && <small className="muted">{card.note}</small>}</Link>
          </li>
        ))}
      </ul>
      <div className="filter-row" role="group" aria-label="筛选">
        <input type="search" aria-label="搜索节点" placeholder="名称、IP、地区、备注或主机名" value={search} onChange={(event) => setSearch(event.target.value)} />
        <span className="muted">实时 · 每 {POLL_MS / 1000} 秒</span>
      </div>
      {listed.nodes.length === 0 && <EmptyState title="还没有节点。">去 <Link to="/nodes">节点</Link> 页创建，或用 <Link to="/register">批量添加节点</Link> 一次接入多台。</EmptyState>}
      {listed.nodes.length > 0 && rows.length === 0 && <EmptyState status title="没有匹配的节点。" />}
      {rows.length > 0 && (
        <ReadingsTable label="节点实时读数" className="nodes" items={rows} now={now} row={(node) => {
          const l = live.get(node.id);
          return {
            key: String(node.id), label: node.name, status: liveStatus(node, l),
            name: <Link to={`/nodes/${node.id}`} aria-label={withId(node.name, node.id)}>{node.name}</Link>,
            metrics: l?.metrics, traffic: l?.traffic, lastSeenAt: l?.lastSeenAt,
          };
        }} />
      )}
    </section>
  );
}
