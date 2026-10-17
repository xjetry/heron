import { useQuery } from "@connectrpc/connect-query";
import { type FormEvent, useState } from "react";
import { errorBanner } from "../api/queryGate";
import { AdminService, AlertKind, SilenceKind, type AlertRule, type Node, type Silence } from "../gen/heron/v1/admin_pb";
import { isDate, isTime } from "../lib/format";
import { liveIds } from "../lib/ids";
import { DateTimeInput, TimeInput } from "./DateInput";
import { Drawer } from "./Modal";
import { assignmentValid, NodeAssignment, type NodeSelection } from "./NodeAssignment";
import { Picks } from "./Picks";

export type SilenceDraft = NodeSelection & {
  name: string; enabled: boolean; kind: SilenceKind; allRules: boolean; ruleIds: Set<bigint>;
  startHhmm: string; endHhmm: string; fromAt: bigint | null; untilAt: bigint | null; reason: string;
};

export const canSilence = (kind: AlertKind) => kind !== AlertKind.EXPIRY && kind !== AlertKind.CERT_EXPIRY;

export const emptySilenceDraft = (): SilenceDraft => ({
  name: "", enabled: true, kind: SilenceKind.DAILY, allNodes: true, nodeIds: new Set(), selectorTags: [], dynamic: false,
  allRules: true, ruleIds: new Set(), startHhmm: "22:00", endHhmm: "06:00", fromAt: null, untilAt: null, reason: "",
});

export const silenceDraftOf = (s: Silence): SilenceDraft => ({
  name: s.name, enabled: s.enabled, kind: s.kind, allNodes: s.allNodes, nodeIds: new Set(s.nodeIds),
  selectorTags: s.selectorTags, dynamic: s.selectorTags.length > 0, allRules: s.allRules, ruleIds: new Set(s.ruleIds),
  startHhmm: s.startHhmm || "22:00", endHhmm: s.endHhmm || "06:00",
  fromAt: s.kind === SilenceKind.ONCE ? s.fromAt : null, untilAt: s.kind === SilenceKind.ONCE ? s.untilAt : null, reason: s.reason,
});

export function firingSilenceDraft(rule: AlertRule, nodeId: bigint, nodeName: string, now: bigint): SilenceDraft {
  const name = (rule.name.trim() ? `${rule.name} · ${nodeName}` : nodeName).trim();
  return {
    ...emptySilenceDraft(), name: Array.from(name).slice(0, 64).join("").trimEnd(),
    allRules: false, ruleIds: new Set([rule.id]), allNodes: false, nodeIds: new Set([nodeId]),
    kind: SilenceKind.ONCE, fromAt: now, untilAt: now + 86400n,
  };
}

// ListSilences 提供 hub 时钟；浏览器只计取回响应后的经过时间。调用方只在打开抽屉与点击快捷时长时取读数。
export function useSilenceClock() {
  const list = useQuery(AdminService.method.listSilences, {}, { refetchInterval: 10_000 });
  const hubNow = () => list.data === undefined ? null : list.data.now + BigInt(Math.max(0, Math.floor((Date.now() - list.dataUpdatedAt) / 1000)));
  return { list, hubNow };
}

// 草稿与提交始终保留 Unix 秒；本地墙钟只用于控件显示，未填完整时由 DateTimeInput 的有效性阻止提交。
function toLocalInput(unix: bigint | null): string {
  if (unix === null) return "";
  const d = new Date(Number(unix) * 1000);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

function fromLocalInput(value: string): bigint | null | undefined {
  if (value === "") return null;
  const [date, time = ""] = value.split("T");
  if (!isDate(date) || !/^\d{2}:\d{2}:[0-5]\d$/.test(time) || !isTime(time.slice(0, 5))) return undefined;
  const seconds = new Date(value).getTime() / 1000;
  return Number.isFinite(seconds) ? BigInt(seconds) : undefined;
}

// 各范围显式二选一；求交后为空也不放宽为全部。窗口只发送当前种类的字段，hub 拒绝混合窗口。
export function toSilence(id: bigint, d: SilenceDraft, nodes: Node[], rules: AlertRule[]) {
  return {
    id, name: d.name.trim(), enabled: d.enabled, allNodes: d.allNodes,
    nodeIds: d.allNodes || d.dynamic ? [] : liveIds(d.nodeIds, nodes),
    selectorTags: !d.allNodes && d.dynamic ? d.selectorTags : [],
    allRules: d.allRules, ruleIds: d.allRules ? [] : liveIds(d.ruleIds, rules.filter((r) => canSilence(r.kind))),
    kind: d.kind, startHhmm: d.kind === SilenceKind.DAILY ? d.startHhmm : "", endHhmm: d.kind === SilenceKind.DAILY ? d.endHhmm : "",
    fromAt: d.kind === SilenceKind.ONCE ? d.fromAt ?? 0n : 0n,
    untilAt: d.kind === SilenceKind.ONCE ? d.untilAt ?? 0n : 0n,
    reason: d.reason,
  };
}

export function SilenceDrawer({ title, submitLabel, nodes, rules, initial, hubNow, pending, error, opener, onClose, onSubmit }: {
  title: string; submitLabel: "创建" | "保存"; nodes: Node[]; rules: AlertRule[]; initial: SilenceDraft;
  hubNow: () => bigint | null; pending: boolean; error: unknown; opener: HTMLElement; onClose: () => void; onSubmit: (d: SilenceDraft) => void;
}) {
  // initial 只在挂载时读取；列表的周期刷新不覆盖草稿。
  const [draft, setDraft] = useState(initial);
  const set = (patch: Partial<SilenceDraft>) => setDraft({ ...draft, ...patch });
  const candidates = rules.filter((r) => canSilence(r.kind));
  const rulesValid = draft.allRules || liveIds(draft.ruleIds, candidates).length > 0;
  const handle = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (pending || !e.currentTarget.checkValidity() || !assignmentValid(draft) || !rulesValid) return;
    onSubmit(draft);
  };
  const setTime = (field: "fromAt" | "untilAt", value: string) => {
    const seconds = fromLocalInput(value);
    if (seconds !== undefined) set({ [field]: seconds });
  };
  return <Drawer title={title} busy={pending} opener={opener} onClose={onClose}>
    <form aria-label={title} onSubmit={handle}>
      <div className="modal-body">
        {errorBanner(error)}
        <fieldset className="bare" disabled={pending}>
          <div className="row">
            <label>名称<input data-autofocus required value={draft.name} onChange={(e) => set({ name: e.target.value })} /></label>
            <label>类型<select value={draft.kind} onChange={(e) => set({ kind: Number(e.target.value) })}>
              <option value={SilenceKind.DAILY}>每日重复</option><option value={SilenceKind.ONCE}>一次性</option>
            </select></label>
            <label className="inline"><input type="checkbox" checked={draft.enabled} onChange={(e) => set({ enabled: e.target.checked })} />启用</label>
          </div>
          {draft.kind === SilenceKind.DAILY ? <div className="row">
            <TimeInput label="每日开始" caption="开始" required value={draft.startHhmm} onChange={(startHhmm) => set({ startHhmm })} />
            <TimeInput label="每日结束" caption="结束" required value={draft.endHhmm} onChange={(endHhmm) => set({ endHhmm })} />
            <p className="muted">按 hub 时区判定，允许跨午夜（如 22:00–06:00）；开始含、结束不含。</p>
          </div> : <>
            <div className="row" role="group" aria-label="快捷时长">
              {([[3600n, "1 小时"], [86400n, "1 天"], [604800n, "7 天"]] as const).map(([seconds, label]) => <button type="button" key={String(seconds)} onClick={() => {
                const now = hubNow();
                if (now !== null) set({ fromAt: now, untilAt: now + seconds });
              }}>{label}</button>)}
            </div>
            <div className="row">
              <DateTimeInput label="开始" caption="开始" required value={toLocalInput(draft.fromAt)} onChange={(value) => setTime("fromAt", value)} />
              <DateTimeInput label="结束" caption="结束" required value={toLocalInput(draft.untilAt)} onChange={(value) => setTime("untilAt", value)} />
              <p className="muted">含开始、不含结束。到期后保留供审计，随告警事件的保留期清理。</p>
            </div>
          </>}
          <label>规则<select value={draft.allRules ? "all" : "selected"} onChange={(e) => set({ allRules: e.target.value === "all" })}>
            <option value="all">全部规则</option><option value="selected">指定规则</option>
          </select></label>
          {!draft.allRules && <>
            <Picks legend="指定规则" items={candidates} selected={draft.ruleIds} onChange={(ruleIds) => set({ ruleIds })} />
            {!rulesValid && <p className="muted">至少选择一条规则。</p>}
          </>}
          <NodeAssignment nodes={nodes} value={draft} onChange={set} legend="作用域节点" noun="作用域" />
          <div className="row"><label>原因（选填）<input value={draft.reason} maxLength={256} onChange={(e) => set({ reason: e.target.value })} /></label></div>
        </fieldset>
      </div>
      <footer className="modal-footer"><button type="button" disabled={pending} onClick={onClose}>取消</button><button type="submit" className="primary-button" disabled={pending || !rulesValid}>{submitLabel}</button></footer>
    </form>
  </Drawer>;
}
