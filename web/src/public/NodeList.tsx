import { Link } from "react-router";
import { AddressFamilies } from "../components/AddressFamilies";
import { CountryBadge } from "../components/CountryBadge";
import { Expiry } from "../components/Expiry";
import { ReadingsTable } from "../components/ReadingsTable";
import type { PublicNode } from "../gen/heron/v1/public_pb";
import { nodeStatus } from "../lib/status";

// 列表视图：与管理端总览同一张实时读数表（components/ReadingsTable），名称后带国家徽章与双栈标记，另加一列到期。全部节点一张表，
// 离线与从未上报不折叠；顺序按调用方的排序。只用 PublicNode 已有的字段，不引入公开快照之外的数据。
export function NodeList({ nodes, now }: { nodes: readonly PublicNode[]; now: number }) {
  return (
    <ReadingsTable label="节点列表" items={nodes} now={now}
      extra={[{ label: "到期", cell: (n) => n.billing?.expiresOn ? <Expiry billing={n.billing} rented={n.rented} /> : <span className="muted">–</span> }]}
      row={(n) => ({
        key: String(n.id), label: n.name, status: nodeStatus(n),
        name: <><Link to={`/nodes/${n.id}`} title={n.name}>{n.name}</Link>{n.country && <CountryBadge code={n.country} />}<AddressFamilies network={n.facts?.network} /></>,
        metrics: n.metrics, traffic: n.traffic, lastSeenAt: n.lastSeenAt,
      })} />
  );
}
