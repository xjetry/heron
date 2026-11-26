import { OperationPrincipal, type Operation } from "../gen/heron/v1/admin_pb";
import { dateTime } from "../lib/format";
import { snapshotDiff } from "../lib/operationDiff";

// 主体种类的显示名。键是生成的枚举：proto 加了主体种类而这里没有加，类型检查就不过。
export const PRINCIPAL_LABELS: Record<OperationPrincipal, string> = {
  [OperationPrincipal.UNSPECIFIED]: "未知主体",
  [OperationPrincipal.SESSION]: "面板会话",
  [OperationPrincipal.API_TOKEN]: "API token",
  [OperationPrincipal.SYSTEM]: "hub 自身",
};

// 会话只显示 id 的前 8 位：id 是会话令牌的 SHA-256，与会话管理页的会话一一对应，完整值放在 title 里。
function principalText(o: Operation) {
  switch (o.principal) {
    case OperationPrincipal.SESSION:
      return o.sessionId ? <>面板会话 <code title={o.sessionId}>{o.sessionId.slice(0, 8)}</code></> : PRINCIPAL_LABELS[o.principal];
    case OperationPrincipal.API_TOKEN:
      return `API token #${o.ownerId}`;
    default:
      return PRINCIPAL_LABELS[o.principal] ?? PRINCIPAL_LABELS[OperationPrincipal.UNSPECIFIED];
  }
}

function objectText(o: Operation) {
  if (o.resourceKey) return o.resourceKey;
  return o.resourceId === 0n ? "" : `#${o.resourceId}`;
}

const pre = { whiteSpace: "pre-wrap", overflowWrap: "anywhere" } as const;

// 每条记录一个 details：摘要是时间、主体、动作与对象，展开后是来源、方法、请求与操作 ID、按字段的差异和原始快照。
export function OperationList({ operations }: { operations: Operation[] }) {
  if (operations.length === 0) return <p className="muted">暂无操作记录。</p>;
  return <>{operations.map((o) => {
    const changes = o.beforeJson === "" && o.afterJson === "" ? null : snapshotDiff(o.beforeJson, o.afterJson);
    const object = objectText(o);
    return <details key={o.id}>
      <summary>{dateTime(o.committedAt)} · {principalText(o)} · {o.action}{object && ` ${object}`}</summary>
      {o.source && <p>来源：{o.source}</p>}
      {o.method && <p>方法：<code>{o.method}</code></p>}
      <p>请求 ID：<code>{o.requestId}</code></p>
      <p>操作 ID：<code>{o.id}</code></p>
      {changes !== null && (changes.length === 0 ? <p className="muted">快照没有字段变化。</p> : <div className="table-scroll" role="region" aria-label={`字段变化 ${o.id}`} tabIndex={0}>
        <table className="nodes">
          <thead><tr><th>字段</th><th>修改前</th><th>修改后</th></tr></thead>
          <tbody>{changes.map((c) => <tr key={c.path}>
            <td data-label="字段"><code>{c.path}</code></td>
            <td data-label="修改前" className="wrap-text">{c.before ?? "—"}</td>
            <td data-label="修改后" className="wrap-text">{c.after ?? "—"}</td>
          </tr>)}</tbody>
        </table>
      </div>)}
      <p>修改前</p><pre style={pre}>{o.beforeJson || "详细记录已过保留期"}</pre>
      <p>修改后</p><pre style={pre}>{o.afterJson || "详细记录已过保留期"}</pre>
    </details>;
  })}</>;
}
