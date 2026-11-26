import { useQuery } from "@connectrpc/connect-query";
import { type FormEvent, useState } from "react";
import { Link } from "react-router";
import { errorBanner, queryGate } from "../api/queryGate";
import { AdminService, OperationPrincipal } from "../gen/heron/v1/admin_pb";
import { OperationList, PRINCIPAL_LABELS } from "../components/OperationList";
import { PageHeader } from "../components/PageHeader";
import { useSyncedSearchParams } from "../lib/useSyncedSearchParams";

// URL 里的主体参数。未知值不退化成"全部主体"——那是放宽筛选；页面直接报错，不发请求。
const PRINCIPAL_PARAMS: Record<string, OperationPrincipal> = {
  session: OperationPrincipal.SESSION,
  api_token: OperationPrincipal.API_TOKEN,
  system: OperationPrincipal.SYSTEM,
};

// 一页最多列这么多条，按提交时刻倒序；hub 单次最多返回 500 条。
const LIMIT = 200;

export function Operations() {
  const [params, setParams] = useSyncedSearchParams();
  const raw = params.get("principal");
  const valid = raw === null || raw in PRINCIPAL_PARAMS;
  const principal = raw !== null && valid ? PRINCIPAL_PARAMS[raw] : OperationPrincipal.UNSPECIFIED;
  const action = params.get("action") ?? "";
  const [draft, setDraft] = useState(action);
  const list = useQuery(AdminService.method.listOperations, { principal, action, limit: LIMIT }, { enabled: valid });
  if (!valid) return <p role="alert" className="error">主体参数 {raw} 无效。<Link to="/operations">查看全部操作记录</Link></p>;
  const set = (key: string, value: string) => setParams((current) => {
    const next = new URLSearchParams(current);
    if (value === "") next.delete(key); else next.set(key, value);
    return next;
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    set("action", draft.trim());
  };
  const gate = queryGate(list);
  return (
    <section>
      <PageHeader title="操作记录" description="面板会话、API token 与 hub 自身的每一次写入：谁、从哪里、调了哪个方法、改了哪个对象，以及改动前后的对象快照。快照 90 天后清空，其余字段一直保留；快照不含任何密钥。" />
      <form className="filter-row" role="group" aria-label="筛选" onSubmit={submit}>
        <label>主体<select value={raw ?? ""} onChange={(e) => set("principal", e.target.value)}>
          <option value="">全部主体</option>
          {Object.entries(PRINCIPAL_PARAMS).map(([value, p]) => <option key={value} value={value}>{PRINCIPAL_LABELS[p]}</option>)}
        </select></label>
        <label>动作<input type="search" value={draft} placeholder="例如 update_node" onChange={(e) => setDraft(e.target.value)} /></label>
        <button type="submit">筛选</button>
        {(raw !== null || action !== "") && <button type="button" className="link" onClick={() => { setDraft(""); setParams(new URLSearchParams()); }}>清除筛选</button>}
      </form>
      {!gate.ready ? gate.loading ?? errorBanner(...gate.errors) : <section className="card" aria-label="操作记录">
        {gate.banner}
        <OperationList operations={gate.data.operations} />
        {gate.data.operations.length === LIMIT && <p className="muted">只列出最近 {LIMIT} 条；按主体或动作筛选可查看更早的记录。</p>}
      </section>}
    </section>
  );
}
