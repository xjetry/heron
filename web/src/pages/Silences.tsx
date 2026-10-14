import { createConnectQueryKey, useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useState } from "react";
import { errorText } from "../api/auth";
import { errorBanner, queryGateAll } from "../api/queryGate";
import { useLatestError } from "../api/useLatestError";
import { Drawer } from "../components/Modal";
import { assignmentValid, NodeAssignment, type NodeSelection } from "../components/NodeAssignment";
import { PageHeader } from "../components/PageHeader";
import { RowMenu } from "../components/RowMenu";
import { AdminService, SilenceKind, type Node, type Silence } from "../gen/heron/v1/admin_pb";
import { liveIds, withId } from "../lib/ids";
import { dateTime } from "../lib/format";
import { DateTimeInput, TimeInput } from "../components/DateInput";
import { EmptyState } from "../components/EmptyState";

type Draft = NodeSelection & {
  name: string; enabled: boolean; kind: SilenceKind;
  startHhmm: string; endHhmm: string; fromAt: string; untilAt: string; reason: string;
};

const emptyDraft = (): Draft => ({
  name: "", enabled: true, kind: SilenceKind.DAILY, allNodes: true, nodeIds: new Set(), selectorTags: [], dynamic: false,
  startHhmm: "22:00", endHhmm: "06:00", fromAt: "", untilAt: "", reason: "",
});
// DateTimeInput 的值（与 datetime-local 同格式 YYYY-MM-DDTHH:MM）是本地墙钟；from_at/until_at 是 Unix 秒，含起点不含终点。
const toLocalInput = (unix: bigint) => {
  if (unix === 0n) return "";
  const d = new Date(Number(unix) * 1000);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
};
const fromLocalInput = (value: string) => BigInt(Math.floor(new Date(value).getTime() / 1000));
const draftOf = (s: Silence): Draft => ({
  name: s.name, enabled: s.enabled, kind: s.kind, allNodes: s.allNodes, nodeIds: new Set(s.nodeIds),
  selectorTags: s.selectorTags, dynamic: s.selectorTags.length > 0,
  startHhmm: s.startHhmm || "22:00", endHhmm: s.endHhmm || "06:00",
  fromAt: toLocalInput(s.fromAt), untilAt: toLocalInput(s.untilAt), reason: s.reason,
});

// 作用域与告警规则同一形状；只有当前种类的窗口字段随保存发出，hub 拒绝带着另一种类字段的静默。
function toSilence(id: bigint, d: Draft, nodes: Node[]) {
  return {
    id, name: d.name.trim(), enabled: d.enabled, allNodes: d.allNodes,
    nodeIds: d.allNodes || d.dynamic ? [] : liveIds(d.nodeIds, nodes),
    selectorTags: !d.allNodes && d.dynamic ? d.selectorTags : [],
    kind: d.kind,
    startHhmm: d.kind === SilenceKind.DAILY ? d.startHhmm : "",
    endHhmm: d.kind === SilenceKind.DAILY ? d.endHhmm : "",
    fromAt: d.kind === SilenceKind.ONCE ? fromLocalInput(d.fromAt) : 0n,
    untilAt: d.kind === SilenceKind.ONCE ? fromLocalInput(d.untilAt) : 0n,
    reason: d.reason,
  };
}

export function Silences() {
  const qc = useQueryClient();
  const [drawer, setDrawer] = useState<{ kind: "create"; opener: HTMLElement } | { kind: "edit"; silence: Silence; opener: HTMLElement } | null>(null);
  const { error, mutationOptions } = useLatestError();
  const nodes = useQuery(AdminService.method.listNodes, {});
  const silences = useQuery(AdminService.method.listSilences, {}, { refetchInterval: 10_000 });
  const refresh = () => qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listSilences, cardinality: "finite" }) });
  const create = useMutation(AdminService.method.saveSilence, { ...mutationOptions, onSuccess: refresh });
  const update = useMutation(AdminService.method.saveSilence, { ...mutationOptions, onSuccess: refresh });
  const remove = useMutation(AdminService.method.deleteSilence, { ...mutationOptions, onSuccess: refresh });
  const busy = create.isPending || update.isPending;
  const gate = queryGateAll(nodes, silences);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  const [nodesData, silencesData] = gate.data;
  const nodeList = nodesData.nodes;
  const nodeName = (id: bigint) => nodeList.find((n) => n.id === id)?.name ?? `节点 #${id}`;
  return (
    <section>
      <PageHeader title="维护静默" description="覆盖内的节点在窗口内产生的告警事件照常记录但不投递；恢复是否投递只看配对的触发是否投递过。到期提醒与系统事件不受静默影响。节点的维护开关在节点编辑里设置。"
        actions={<button type="button" className="primary-button" disabled={busy} onClick={(event) => { create.reset(); setDrawer({ kind: "create", opener: event.currentTarget }); }}>新建维护静默</button>} />
      {gate.banner}
      {drawer === null && error != null && <p role="alert" className="error">{errorText(error)}</p>}
      {silencesData.silences.length === 0 ? <EmptyState title="还没有维护静默。" /> : <div className="table-scroll" role="region" aria-label="维护静默管理" tabIndex={0}>
        <table className="nodes">
          <thead><tr><th>名称</th><th>窗口</th><th>作用域</th><th>状态</th><th><span className="sr-only">操作</span></th></tr></thead>
          <tbody>
            {silencesData.silences.map((entry) => {
              const s = entry.silence;
              if (!s) return null;
              const label = withId(s.name, s.id);
              return <tr key={String(s.id)}>
                <td data-label="名称">{s.name}{s.reason && <p className="node-subtext node-note clip-text" title={s.reason}>{s.reason}</p>}</td>
                <td data-label="窗口">{windowText(s)}</td>
                <td data-label="作用域" className="wrap-text">{s.allNodes ? "全部节点" : s.selectorTags.length ? `标签：${s.selectorTags.join(" ∩ ")}（当前 ${s.nodeIds.length}）` : s.nodeIds.length ? <span title={s.nodeIds.map(nodeName).join("、")}>{s.nodeIds.length} 个指定节点</span> : <span className="muted">无节点</span>}</td>
                <td data-label="状态">{!s.enabled ? <span className="muted">已停用</span> : entry.active ? <span>生效中</span> : <span className="muted">窗口外</span>}</td>
                <td data-column="actions"><RowMenu label={label} items={[
                  { label: "编辑", disabled: busy, onSelect: (trigger) => { update.reset(); setDrawer({ kind: "edit", silence: s, opener: trigger }); } },
                  { label: "删除", danger: true, confirm: `确认删除 ${label}`, note: "已产生的告警事件保留", disabled: busy || remove.isPending, onSelect: () => remove.mutate({ id: s.id }) },
                ]} /></td>
              </tr>;
            })}
          </tbody>
        </table>
      </div>}
      {drawer?.kind === "create" && <SilenceDrawer title="新建维护静默" submitLabel="创建" nodes={nodeList} initial={emptyDraft()} pending={create.isPending} error={create.error} opener={drawer.opener} onClose={() => setDrawer(null)}
        onSubmit={(d) => create.mutate({ silence: toSilence(0n, d, nodeList) }, { onSuccess: () => setDrawer(null) })} />}
      {drawer?.kind === "edit" && <SilenceDrawer key={String(drawer.silence.id)} title={`编辑 ${withId(drawer.silence.name, drawer.silence.id)}`} submitLabel="保存" nodes={nodeList} initial={draftOf(drawer.silence)} pending={update.isPending} error={update.error} opener={drawer.opener} onClose={() => setDrawer(null)}
        onSubmit={(d) => update.mutate({ silence: toSilence(drawer.silence.id, d, nodeList) }, { onSuccess: () => setDrawer(null) })} />}
    </section>
  );
}

function windowText(s: Silence): string {
  if (s.kind === SilenceKind.DAILY) return `每日 ${s.startHhmm}–${s.endHhmm}（hub 时区）`;
  const at = (unix: bigint) => dateTime(unix);
  return `${at(s.fromAt)} – ${at(s.untilAt)}`;
}

function SilenceDrawer({ title, submitLabel, nodes, initial, pending, error, opener, onClose, onSubmit }: {
  title: string; submitLabel: "创建" | "保存"; nodes: Node[]; initial: Draft; pending: boolean; error: unknown; opener: HTMLElement; onClose: () => void; onSubmit: (d: Draft) => void;
}) {
  // initial 只在挂载时读取；列表的周期刷新不覆盖草稿。
  const [draft, setDraft] = useState(initial);
  const set = (patch: Partial<Draft>) => setDraft({ ...draft, ...patch });
  const handle = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (pending || !e.currentTarget.checkValidity()) return;
    if (!assignmentValid(draft)) return;
    onSubmit(draft);
  };
  return (
    <Drawer title={title} busy={pending} opener={opener} onClose={onClose}>
      <form aria-label={title} onSubmit={handle}>
        <div className="modal-body">
          {errorBanner(error)}
          <fieldset className="bare" disabled={pending}>
            <div className="row">
              <label>名称<input data-autofocus required value={draft.name} onChange={(e) => set({ name: e.target.value })} /></label>
              <label>类型
                <select value={draft.kind} onChange={(e) => set({ kind: Number(e.target.value) })}>
                  <option value={SilenceKind.DAILY}>每日重复</option>
                  <option value={SilenceKind.ONCE}>一次性</option>
                </select>
              </label>
              <label className="inline"><input type="checkbox" checked={draft.enabled} onChange={(e) => set({ enabled: e.target.checked })} />启用</label>
            </div>
            {draft.kind === SilenceKind.DAILY ? (
              <div className="row">
                <TimeInput label="每日开始" caption="开始" required value={draft.startHhmm} onChange={(startHhmm) => set({ startHhmm })} />
                <TimeInput label="每日结束" caption="结束" required value={draft.endHhmm} onChange={(endHhmm) => set({ endHhmm })} />
                <p className="muted">按 hub 时区判定，允许跨午夜（如 22:00–06:00）；开始含、结束不含。</p>
              </div>
            ) : (
              <div className="row">
                <DateTimeInput label="开始" caption="开始" required value={draft.fromAt} onChange={(fromAt) => set({ fromAt })} />
                <DateTimeInput label="结束" caption="结束" required value={draft.untilAt} onChange={(untilAt) => set({ untilAt })} />
                <p className="muted">含开始、不含结束。到期后保留供审计，随告警事件的保留期清理。</p>
              </div>
            )}
            <NodeAssignment nodes={nodes} value={draft} onChange={set} legend="作用域节点" noun="作用域" />
            <div className="row">
              <label>原因（选填）<input value={draft.reason} maxLength={256} onChange={(e) => set({ reason: e.target.value })} /></label>
            </div>
          </fieldset>
        </div>
        <footer className="modal-footer"><button type="button" disabled={pending} onClick={onClose}>取消</button><button type="submit" className="primary-button" disabled={pending}>{submitLabel}</button></footer>
      </form>
    </Drawer>
  );
}
