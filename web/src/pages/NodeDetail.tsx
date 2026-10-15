import { createConnectQueryKey, useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useState } from "react";
import { Link, useLocation, useParams } from "react-router";
import { errorBanner, queryGate } from "../api/queryGate";
import { errorText } from "../api/auth";
import { AgentDiagnostics } from "../components/AgentDiagnostics";
import { CountryBadge } from "../components/CountryBadge";
import { EventFeed, useAlertEvents } from "../components/EventFeed";
import { ExecutionScope } from "../components/ExecutionScope";
import { MetricCharts, ProbeTaskCharts, RangePicker, useHistory, type HistoryMethods } from "../components/History";
import { NodeAddresses, addressText } from "../components/NodeAddresses";
import { NowGrid } from "../components/NowGrid";
import { StatusBadge } from "../components/StatusBadge";
import { AdminService, type GetTrafficResponse } from "../gen/heron/v1/admin_pb";
import { useNodeList } from "../api/useNodeList";
import { liveStatus } from "../lib/adminStatus";
import { nodeListReturnPath } from "../lib/nodeFilters";
import { useSyncedSearchParams } from "../lib/useSyncedSearchParams";
import { ago, bytes, dateTime } from "../lib/format";
import { POLL_MS, TRAFFIC_MS } from "../lib/poll";
import { trafficDetail, trafficText } from "../lib/traffic";
import { laggingNode } from "../lib/lagging";
import { NodeEditor } from "./NodeEditor";

const ADMIN_HISTORY: HistoryMethods = { queryMetrics: AdminService.method.queryMetrics, queryProbes: AdminService.method.queryProbes };
export type DetailTab = "overview" | "traffic" | "diagnostics" | "events";
const TABS: readonly { id: DetailTab; label: string }[] = [{ id: "overview", label: "概览" }, { id: "traffic", label: "流量校正" }, { id: "diagnostics", label: "Agent 诊断" }, { id: "events", label: "告警事件" }];
// URL 是外部输入：不认识的 tab 当概览，不显示一个空面板。
const tabOf = (raw: string | null): DetailTab => (TABS.some((t) => t.id === raw) ? (raw as DetailTab) : "overview");

export function NodeDetail() {
  const { id } = useParams();
  const [params, setParams] = useSyncedSearchParams();
  // 从节点列表进入时 state 带着列表的查询串；切 tab 只换本页 URL，state 原样带上，返回仍落回同一组筛选。
  const location = useLocation();
  const tab = tabOf(params.get("tab"));
  const validId = /^\d+$/.test(id ?? "");
  const nodeId = validId ? BigInt(id!) : 0n;
  const qc = useQueryClient();
  const nodes = useNodeList({}, { enabled: validId, refetchInterval: TRAFFIC_MS });
  const snap = useQuery(AdminService.method.getSnapshot, {}, { enabled: validId, refetchInterval: POLL_MS });
  const tags = useQuery(AdminService.method.listTags, {}, { enabled: validId });
  const rules = useQuery(AdminService.method.listAlertRules, {}, { enabled: validId && tab === "events" });
  const channels = useQuery(AdminService.method.listNotifyChannels, {}, { enabled: validId && tab === "events" });
  // 流量响应同时携带历史窗口要用的 hub now；校正操作在卡片内保留自己的错误槽位。
  const traffic = useQuery(AdminService.method.getTraffic, {}, { enabled: validId, refetchInterval: TRAFFIC_MS });
  // 历史与 tab 无关：hook 留在页面顶层，切 tab 不卸载，查询键不变、不重发；hub now 到达前它不发请求。
  const history = useHistory(ADMIN_HISTORY, nodeId, validId && traffic.data ? Number(traffic.data.now) : undefined);
  // 事件 tab 才取事件：它是低频内容，不该随每次打开详情多发一条请求。
  const events = useAlertEvents(validId && tab === "events" ? nodeId : null);
  const [editor, setEditor] = useState<HTMLElement | null>(null);
  const update = useMutation(AdminService.method.updateNode, {
    onSuccess: async () => {
      try { await qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listNodes, cardinality: "finite" }) }, { throwOnError: true }); }
      catch (error) { throw new Error(`已保存，但回读失败：${errorText(error)}`, { cause: error }); }
    },
  });
  if (!validId) return <p role="alert" className="error">节点 {id} 不存在。<Link to="/">返回总览</Link></p>;
  const gate = queryGate(nodes);
  const time = queryGate(traffic);
  // 历史与流量不依赖 listNodes；listNodes 挂起或首次失败时它们完全可能已经就绪，不能被它的门控挡住。
  // 是否"不存在"只有 listNodes 真的到达后才能判断——它挂起或首次失败时无法区分"节点被删了"与"这次还没拿到列表"，按后者处理。
  const node = gate.ready ? gate.data.nodes.find((n) => n.id === nodeId) : undefined;
  if (gate.ready && !node) return <p role="alert" className="error">节点 {id} 不存在。<Link to="/">返回总览</Link></p>;
  const live = snap.data?.nodes.find((n) => n.id === nodeId);
  const bound = snap.data?.boundAgentVersion;
  const status = node ? liveStatus(node, live) : undefined;
  const select = (next: DetailTab) => { const p = new URLSearchParams(params); if (next === "overview") p.delete("tab"); else p.set("tab", next); const state: unknown = location.state; setParams(p, { state: typeof state === "object" ? state : null }); };
  const panel = (
    <section id={`panel-${tab}`} role="tabpanel" aria-labelledby={`tab-${tab}`}>
      {tab === "overview" && (time.ready ? <>
        <header className="row detail-header"><RangePicker history={history} showCoverage /></header>
        <MetricCharts history={history} />
        <ProbeTaskCharts history={history} noProbes={<p className="muted">窗口内没有探测结果。<Link to="/probes">管理探测任务</Link></p>} titleLink={(taskId, title) => <Link to={`/probes/${taskId}/compare`}>{title}</Link>} />
      </> : time.loading)}
      {tab === "traffic" && <TrafficCard nodeId={nodeId} data={traffic.data} />}
      {tab === "diagnostics" && (gate.ready && node ? <>
        {node.facts && <dl className="card facts">
          {/* 来源地址是 hub 在上报上看到的对端，不是 agent 自报；只在管理端显示，公开页没有这个字段。 */}
          <dt>主机名</dt><dd>{node.facts.hostname}{node.lastSource && <span className="muted">（来源 {node.lastSource}）</span>}</dd>
          <dt>双栈出口</dt><dd><NodeAddresses network={node.network} reported={node.facts.network} detailed /></dd>
          <dt>系统</dt><dd>{node.facts.os}</dd><dt>内核</dt><dd>{node.facts.kernel}</dd><dt>架构</dt><dd>{node.facts.arch}</dd>
          <dt>CPU</dt><dd>{node.facts.cpuModel} × {node.facts.cpuCores}</dd><dt>虚拟化</dt><dd>{node.facts.virtualization || "无 / 未知"}</dd>
          <dt>agent</dt><dd>{node.facts.agentVersion}</dd><dt>ICMP 探测</dt><dd>{node.facts.icmpAvailable ? "可用" : "不可用"}</dd>
        </dl>}
        <ExecutionScope execution={node.facts?.execution} updatedAt={node.factsUpdatedAt} />
        <AgentDiagnostics diagnostics={node.facts?.diagnostics} updatedAt={node.factsUpdatedAt} />
      </> : !gate.ready ? gate.loading : null)}
      {tab === "events" && node && <EventFeed events={events} rules={rules.data?.rules} nodeName={() => node.name} channelName={(cid) => channels.data?.channels.find((c) => c.id === cid)?.name ?? `渠道 #${cid}`} />}
    </section>
  );
  return (
    <section className="node-detail">
      <p><Link to={nodeListReturnPath(location.state)}>返回节点列表</Link></p>
      {errorBanner(nodes.error, snap.error, traffic.error, history.metrics.error, history.probes.error, tab === "events" ? events.error : undefined, tab === "events" ? channels.error : undefined, tab === "events" ? rules.error : undefined)}
      <header className="node-head" role="group" aria-label="节点状态">
        <div className="node-head-title">
          <h1>{node ? node.name : `节点 #${nodeId}`}</h1>
          {node?.country && <CountryBadge code={node.country} />}
          {status && <StatusBadge status={status} detail={live?.lastSeenAt !== undefined && snap.data ? `最近上报 ${ago(live.lastSeenAt, Number(snap.data.now))}` : undefined} />}
          {node && <button type="button" className="node-head-edit" disabled={editor !== null} onClick={(event) => { update.reset(); setEditor(event.currentTarget); }}>编辑</button>}
        </div>
        {node?.facts && <p className="node-head-meta num">
          {[addressText(node.network?.ipv4), addressText(node.network?.ipv6), node.facts.hostname || "—", node.facts.kernel || "—", node.facts.agentVersion || "—"].join(" · ")}
          {laggingNode(node, bound) && <> <span className="badge-attention" title={`低于 hub 绑定的 agent 版本 ${bound}`}>agent 低于 {bound}</span></>}
        </p>}
        <NowGrid metrics={live?.metrics} daysLeft={node?.billing?.daysLeft} rented={node?.rented ?? false} />
      </header>
      <div className="tabs" role="tablist" aria-label="详情">
        {TABS.map((t) => <button key={t.id} id={`tab-${t.id}`} type="button" role="tab" aria-selected={tab === t.id} aria-controls={`panel-${t.id}`} onClick={() => select(t.id)}>{t.label}</button>)}
      </div>
      {panel}
      {editor && node && <NodeEditor key={String(node.id)} node={node} opener={editor} knownTags={tags.data?.tags ?? []} saving={update.isPending} error={update.error} listError={nodes.error}
        onClose={() => setEditor(null)} onSave={(patch) => update.mutate({ id: node.id, ...patch }, { onSuccess: () => setEditor(null) })} />}
    </section>
  );
}

const GIB = 2 ** 30;
const toGiB = (v: bigint) => (Number(v) / GIB).toFixed(2);
// 校正值以 GiB 输入，四舍五入到整字节；只有有限且落在 uint64 范围内的非负值才能提交。
const toBytes = (s: string): bigint | null => {
  const v = Number(s);
  const rounded = Math.round(v * GIB);
  if (!(s.trim() !== "" && Number.isFinite(v) && v >= 0 && Number.isFinite(rounded))) return null;
  const result = BigInt(rounded);
  return result <= (1n << 64n) - 1n ? result : null;
};

function TrafficCard({ nodeId, data }: { nodeId: bigint; data: GetTrafficResponse | undefined }) {
  const qc = useQueryClient();
  const [draft, setDraft] = useState<{ rx: string; tx: string } | null>(null);
  const adjust = useMutation(AdminService.method.adjustTraffic, {
    onSuccess: () => {
      setDraft(null);
      void qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.getTraffic, cardinality: "finite" }) });
      void qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.getSnapshot, cardinality: "finite" }) });
    },
  });
  const t = data?.nodes.find((n) => n.nodeId === nodeId)?.traffic;
  const timeZone = data?.timezone;
  if (!t) return null;
  // 编辑框首次出现时预填当前值；之后由用户输入驱动。
  const form = draft ?? { rx: toGiB(t.periodRx), tx: toGiB(t.periodTx) };
  const rx = toBytes(form.rx);
  const tx = toBytes(form.tx);
  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    if (rx !== null && tx !== null) adjust.mutate({ nodeId, periodRx: rx, periodTx: tx });
  };
  return (
    <div className="card">
      <h2>流量</h2>
      <dl className="facts">
        <dt>本周期</dt><dd><span className="num">{trafficText(t)}</span> <span className="muted">{trafficDetail(t)}</span></dd>
        <dt>总量</dt><dd>↓ {bytes(t.totalRx)} ↑ {bytes(t.totalTx)}</dd>
        <dt>周期起点</dt><dd>{dateTime(t.periodStart, timeZone)}</dd>
        <dt>下次重置</dt><dd>{dateTime(t.nextResetAt, timeZone)}（每月 {t.resetDay} 日，{timeZone}）</dd>
      </dl>
      <form onSubmit={onSubmit} className="row">
        <label>本周期下行 (GiB)<input value={form.rx} onChange={(e) => setDraft({ ...form, rx: e.target.value })} inputMode="decimal" /></label>
        <label>本周期上行 (GiB)<input value={form.tx} onChange={(e) => setDraft({ ...form, tx: e.target.value })} inputMode="decimal" /></label>
        <button type="submit" disabled={adjust.isPending || rx === null || tx === null}>校正本周期</button>
      </form>
      {adjust.error && <p role="alert" className="error">{errorText(adjust.error)}</p>}
    </div>
  );
}
