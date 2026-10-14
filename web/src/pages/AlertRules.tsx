import { createConnectQueryKey, useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useState } from "react";
import { errorText } from "../api/auth";
import { errorBanner, queryGateAll } from "../api/queryGate";
import { useLatestError } from "../api/useLatestError";
import { Drawer } from "../components/Modal";
import { PageHeader } from "../components/PageHeader";
import { RowMenu } from "../components/RowMenu";
import { Picks } from "../components/Picks";
import { NodeAssignment, assignmentValid, type NodeSelection } from "../components/NodeAssignment";
import { AdminService, AlertKind, BaselineMode, ProbeMetric, ResourceMetric, RttMode, type AlertRule, type Node, type NotifyChannel, type ProbeTaskDetail } from "../gen/heron/v1/admin_pb";
import { ALERT_KINDS, BASELINE_MODES, MBPS_TO_BYTES_PER_S, PROBE_METRICS, RELATIVE_LIMITS, RESOURCE_METRICS, RTT_MODES, labelOf, resourceThresholdMax, resourceUnit, ruleCondition, statesOf, taskLabels, type RuleStates } from "../lib/alerts";
import { liveIds, withId } from "../lib/ids";
import { isHTTPSTarget } from "../lib/probes";
import { EmptyState } from "../components/EmptyState";
import { useSyncedSearchParams } from "../lib/useSyncedSearchParams";

type Draft = NodeSelection & {
  name: string; kind: AlertKind; enabled: boolean; allNodes: boolean; nodeIds: Set<bigint>; channelIds: Set<bigint>;
  taskId: string; metric: ProbeMetric; threshold: string; forMinutes: string; daysBefore: string;
  resourceMetric: ResourceMetric; recoveryThreshold: string;
  // rtt 规则的判定方式与相对判定字段；基线窗口与冷却以分钟输入，提交时换算成秒（relativeFields）。
  rttMode: RttMode; baselineMode: BaselineMode; baselineWindowMinutes: string; baselineMinSamples: string;
  upperDeviationPct: string; lowerDeviationPct: string; cooldownMinutes: string; fixedBaselineMs: string;
};

// 相对判定的默认值：一天的自适应基线、至少 12 个 5 分钟桶（一小时），比基线慢一倍或快一半算越带，冷却 30 分钟。
const relativeDefaults = {
  rttMode: RttMode.THRESHOLD, baselineMode: BaselineMode.ADAPTIVE, baselineWindowMinutes: "1440", baselineMinSamples: "12",
  upperDeviationPct: "100", lowerDeviationPct: "50", cooldownMinutes: "30", fixedBaselineMs: "",
};

// 种类专用字段里有默认值的是指标（丢包率）、连续分钟 3 与提前天数 7；探测任务与阈值留空，切到探测时由表单的 required
// 挡住提交，要用户自己选填。种类、启用与"全部节点"另有各自的默认值。只有当前类型的那几项随保存发出（toRule）。
const emptyDraft = (): Draft => ({
  name: "", kind: AlertKind.OFFLINE, enabled: true, allNodes: true, nodeIds: new Set(), channelIds: new Set(),
  taskId: "", metric: ProbeMetric.LOSS_PCT, threshold: "", forMinutes: "3", daysBefore: "7",
  selectorTags: [], dynamic: false, resourceMetric: ResourceMetric.MEMORY_USED_PCT, recoveryThreshold: "80", ...relativeDefaults,
});
const draftOf = (r: AlertRule): Draft => {
  const probe = r.kind === AlertKind.PROBE;
  // 证书到期与探测一样引用任务，与到期一样带提前天数。
  const withTask = probe || r.kind === AlertKind.CERT_EXPIRY;
  // 速率指标的协议值是 bytes/s，草稿按 Mbps 展示（RESOURCE_METRICS 的 unit 决定换算方向）。
  const scale = r.kind === AlertKind.RESOURCE && resourceUnit(r.resourceMetric) === "mbps" ? MBPS_TO_BYTES_PER_S : 1;
  return {
    name: r.name, kind: r.kind, enabled: r.enabled, allNodes: r.allNodes, nodeIds: new Set(r.nodeIds), channelIds: new Set(r.channelIds),
    taskId: withTask ? String(r.taskId) : "", metric: probe ? r.metric : ProbeMetric.LOSS_PCT,
    threshold: probe || r.kind === AlertKind.TRAFFIC ? String(r.threshold) : r.kind === AlertKind.RESOURCE ? String(r.threshold / scale) : "", forMinutes: probe || r.kind === AlertKind.RESOURCE ? String(r.forMinutes) : "3",
    daysBefore: r.kind === AlertKind.EXPIRY || r.kind === AlertKind.CERT_EXPIRY ? String(r.daysBefore) : "7",
    selectorTags: r.selectorTags, dynamic: r.selectorTags.length > 0,
    resourceMetric: r.kind === AlertKind.RESOURCE ? r.resourceMetric : ResourceMetric.MEMORY_USED_PCT,
    recoveryThreshold: r.kind === AlertKind.RESOURCE ? String(r.recoveryThreshold / scale) : "80",
    ...relativeDraft(r),
  };
};

// 相对判定的规则回填自己的值，其余规则取默认值；切换成相对判定时表单从默认值起填。
function relativeDraft(r: AlertRule): Pick<Draft, keyof typeof relativeDefaults> {
  if (r.kind !== AlertKind.PROBE || r.metric !== ProbeMetric.RTT_MS || r.rttMode !== RttMode.RELATIVE) return { ...relativeDefaults };
  const adaptive = r.baselineMode !== BaselineMode.FIXED;
  return {
    rttMode: RttMode.RELATIVE, baselineMode: r.baselineMode,
    baselineWindowMinutes: adaptive ? String(r.baselineWindowS / 60) : relativeDefaults.baselineWindowMinutes,
    baselineMinSamples: adaptive ? String(r.baselineMinSamples) : relativeDefaults.baselineMinSamples,
    upperDeviationPct: String(r.upperDeviationPct), lowerDeviationPct: String(r.lowerDeviationPct), cooldownMinutes: String(r.cooldownS / 60),
    fixedBaselineMs: adaptive ? "" : String(r.fixedBaselineMs),
  };
}

// rtt 规则的判定字段。rtt_mode 总是显式发出（hub 对未指定的也按固定阈值收，显式发出让载荷与回显一致）；相对判定下
// threshold 为 0，只发所选基线来源用到的字段，hub 拒绝带着另一种来源字段的规则（store.checkRttFields）。分钟换算成秒时
// 四舍五入：回填写的是秒 / 60，整数秒经这一来一回不变，只改名称的保存不会把窗口改掉。
function relativeFields(d: Draft) {
  if (d.rttMode !== RttMode.RELATIVE) return { rttMode: RttMode.THRESHOLD, threshold: Number(d.threshold) };
  const adaptive = d.baselineMode === BaselineMode.ADAPTIVE;
  return {
    rttMode: RttMode.RELATIVE, threshold: 0, baselineMode: d.baselineMode,
    baselineWindowS: adaptive ? Math.round(Number(d.baselineWindowMinutes) * 60) : 0, baselineMinSamples: adaptive ? Number(d.baselineMinSamples) : 0,
    fixedBaselineMs: adaptive ? 0 : Number(d.fixedBaselineMs),
    upperDeviationPct: Number(d.upperDeviationPct), lowerDeviationPct: Number(d.lowerDeviationPct), cooldownS: Math.round(Number(d.cooldownMinutes) * 60),
  };
}

// 资源阈值在表单里按指标单位输入，协议值统一为 bytes/s 或原值：Mbps 在这里换算回 bytes/s。
function resourceRule(d: Draft) {
  const scale = resourceUnit(d.resourceMetric) === "mbps" ? MBPS_TO_BYTES_PER_S : 1;
  return { resourceMetric: d.resourceMetric, threshold: Number(d.threshold) * scale, recoveryThreshold: Number(d.recoveryThreshold) * scale, forMinutes: Number(d.forMinutes) };
}

// toRule 用 liveIds 把节点与渠道求交到当前列表，避免提交已删除对象；allNodes 仍取草稿中的显式选择，不从空集推导。
// 显式作用域求交为空时，hub 的 Engine.SaveRule 拒绝该保存，不会把已删除的目标放宽成全部节点。
// 种类专用字段只发当前类型的：hub 拒绝带着别的种类字段的规则（alert.CheckRule）。
function toRule(id: bigint, d: Draft, nodes: Node[], channels: NotifyChannel[]) {
  const own = d.kind === AlertKind.PROBE
    ? { taskId: BigInt(d.taskId), metric: d.metric, threshold: Number(d.threshold), forMinutes: Number(d.forMinutes), ...(d.metric === ProbeMetric.RTT_MS ? relativeFields(d) : {}) }
    : d.kind === AlertKind.EXPIRY ? { daysBefore: Number(d.daysBefore) }
      : d.kind === AlertKind.CERT_EXPIRY ? { taskId: BigInt(d.taskId), daysBefore: Number(d.daysBefore) }
        : d.kind === AlertKind.RESOURCE ? resourceRule(d) : d.kind === AlertKind.TRAFFIC ? { threshold: Number(d.threshold) } : {};
  return {
    id, name: d.name.trim(), kind: d.kind, enabled: d.enabled, allNodes: d.allNodes,
    nodeIds: d.allNodes || d.dynamic ? [] : liveIds(d.nodeIds, nodes), selectorTags: !d.allNodes && d.dynamic ? d.selectorTags : [], channelIds: liveIds(d.channelIds, channels), ...own,
  };
}

export function AlertRules() {
  const qc = useQueryClient();
  const [params, setParams] = useSyncedSearchParams();
  const firingOnly = params.get("state") === "firing";
  const setFiringOnly = (on: boolean) => {
    const next = new URLSearchParams(params);
    if (on) next.set("state", "firing"); else next.delete("state");
    setParams(next);
  };
  const [drawer, setDrawer] = useState<{ kind: "create"; opener: HTMLElement } | { kind: "edit"; rule: AlertRule; opener: HTMLElement } | null>(null);
  const [toggling, setToggling] = useState<bigint | null>(null);
  const { error, mutationOptions } = useLatestError();
  const nodes = useQuery(AdminService.method.listNodes, {});
  const channels = useQuery(AdminService.method.listNotifyChannels, {});
  const tasks = useQuery(AdminService.method.listProbeTasks, {});
  // 离线巡检每 10 秒一轮；按同一周期刷新，触发中的节点不用手动刷新就能看到。
  const rules = useQuery(AdminService.method.listAlertRules, {}, { refetchInterval: 10_000 });
  const refresh = () => qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listAlertRules, cardinality: "finite" }) });
  const create = useMutation(AdminService.method.saveAlertRule, { ...mutationOptions, onSuccess: refresh });
  // 行开关与抽屉共用 mutation observer，重叠的 mutate 只回调最后一次；任一保存挂起时禁用其它保存入口。
  // 返回刷新 promise，编辑态在列表显示已保存值之后才关闭。
  const update = useMutation(AdminService.method.saveAlertRule, { ...mutationOptions, onSuccess: refresh });
  const remove = useMutation(AdminService.method.deleteAlertRule, { ...mutationOptions, onSuccess: refresh });
  const busy = create.isPending || update.isPending;
  // 节点与渠道求交需要相应列表，任务列表用于任务选择与标签；依赖未到达时不渲染可提交表单。
  const gate = queryGateAll(nodes, channels, tasks, rules);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  const [nodesData, channelsData, tasksData, rulesData] = gate.data;
  const nodeList = nodesData.nodes;
  const channelList = channelsData.channels;
  const taskList = tasksData.tasks;
  const byRule = statesOf(rulesData.states);
  const nodeName = (id: bigint) => nodeList.find((n) => n.id === id)?.name ?? `节点 #${id}`;
  const channelName = (id: bigint) => channelList.find((c) => c.id === id)?.name ?? `渠道 #${id}`;
  const lists = { nodes: nodeList, channels: channelList, tasks: taskList };
  const shown = firingOnly ? rulesData.rules.filter((r) => (byRule.get(r.id)?.firing.length ?? 0) > 0) : rulesData.rules;
  const toggleEnabled = (r: AlertRule) => {
    setToggling(r.id);
    update.mutate({ rule: { ...toRule(r.id, draftOf(r), nodeList, channelList), enabled: !r.enabled } }, { onSettled: () => setToggling(null) });
  };
  return (
    <section>
      <PageHeader title="告警规则" actions={<button type="button" className="primary-button" disabled={busy} onClick={(event) => { create.reset(); setDrawer({ kind: "create", opener: event.currentTarget }); }}>新建告警规则</button>} />
      {gate.banner}
      <div className="filter-row" role="group" aria-label="筛选">
        <label className="check"><input type="checkbox" aria-label="只看触发中" checked={firingOnly} onChange={(event) => setFiringOnly(event.target.checked)} />只看触发中</label>
        {firingOnly && <button type="button" className="link" onClick={() => setFiringOnly(false)}>清除筛选</button>}
        <span className="muted num">{shown.length} / {rulesData.rules.length}</span>
      </div>
      {drawer === null && error != null && <p role="alert" className="error">{errorText(error)}</p>}
      {shown.length === 0
        ? rulesData.rules.length === 0 ? <EmptyState title="还没有告警规则。" /> : <EmptyState status title="没有触发中的规则。" />
        : <div className="table-scroll" role="region" aria-label="告警规则管理" tabIndex={0}>
        <table className="nodes">
          <thead><tr><th>名称</th><th>类型</th><th>条件</th><th>作用域</th><th>通知渠道</th><th>启用</th><th>状态</th><th><span className="sr-only">操作</span></th></tr></thead>
          <tbody>{shown.map((r) => {
            const label = withId(r.name, r.id);
            return <tr key={String(r.id)} aria-label={r.name}>
              <td data-label="名称">{r.name}</td>
              <td data-label="类型">{labelOf(ALERT_KINDS, r.kind)}</td>
              <td data-label="条件" className="wrap-text">{ruleCondition(r, taskList)}</td>
              <td data-label="作用域" className="wrap-text">{r.allNodes ? "全部节点" : r.selectorTags.length ? `标签：${r.selectorTags.join(" ∩ ")}（当前 ${r.nodeIds.length}）` : r.nodeIds.length ? <span title={r.nodeIds.map(nodeName).join("、")}>{r.nodeIds.length} 个指定节点</span> : <span className="muted">无节点</span>}</td>
              <td data-label="通知渠道" className="wrap-text">{r.channelIds.map(channelName).join("、") || <span className="muted">只记事件</span>}</td>
              <td data-label="启用"><input type="checkbox" role="switch" className="switch" aria-label={`启用 ${label}`} checked={r.enabled} disabled={busy} aria-busy={toggling === r.id || undefined} onChange={() => toggleEnabled(r)} /></td>
              <td data-label="状态">{r.enabled ? <RuleState states={byRule.get(r.id)} nodeName={nodeName} /> : <span className="muted">—</span>}</td>
              <td data-column="actions"><RowMenu label={label} items={[
                { label: "编辑", disabled: busy, onSelect: (trigger) => { update.reset(); setDrawer({ kind: "edit", rule: r, opener: trigger }); } },
                { label: "删除", danger: true, confirm: `确认删除 ${label}`, note: "事件记录保留", disabled: busy || remove.isPending, onSelect: () => remove.mutate({ id: r.id }) },
              ]} /></td>
            </tr>;
          })}</tbody>
        </table>
      </div>}
      {drawer?.kind === "create" && <AlertRuleDrawer title="新建告警规则" submitLabel="创建" {...lists} initial={emptyDraft()} pending={create.isPending} error={create.error} opener={drawer.opener} onClose={() => setDrawer(null)}
        onSubmit={(d) => create.mutate({ rule: toRule(0n, d, nodeList, channelList) }, { onSuccess: () => setDrawer(null) })} />}
      {drawer?.kind === "edit" && <AlertRuleDrawer key={String(drawer.rule.id)} title={`编辑 ${withId(drawer.rule.name, drawer.rule.id)}`} submitLabel="保存" {...lists} initial={draftOf(drawer.rule)} pending={update.isPending} error={update.error} opener={drawer.opener} onClose={() => setDrawer(null)}
        onSubmit={(d) => update.mutate({ rule: toRule(drawer.rule.id, d, nodeList, channelList) }, { onSuccess: () => setDrawer(null) })} />}
    </section>
  );
}

type Lists = { nodes: Node[]; channels: NotifyChannel[]; tasks: ProbeTaskDetail[] };

// 同类型同目标没有唯一约束。只在这份下拉里碰撞的标签追加编号，否则两个任务无法区分，选错会盯住另一个任务。
// httpsOnly 只列 https:// 的 HTTP 任务：证书到期规则只能挂在它们上面（hub 的 requireHTTPSProbeTask 同样裁决）。
function taskOptions(tasks: ProbeTaskDetail[], httpsOnly = false) {
  const listed = tasks.flatMap((d) => (d.task ? [d.task] : []))
    .filter((t) => !httpsOnly || isHTTPSTarget(t.kind, t.target));
  const labels = taskLabels(listed.map((t) => t.id), tasks);
  return listed.map((t, i) => <option key={String(t.id)} value={String(t.id)}>{labels[i]}</option>);
}

function AlertRuleDrawer({ title, submitLabel, nodes, channels, tasks, initial, pending, error, opener, onSubmit, onClose }: Lists & {
  title: string; submitLabel: string; initial: Draft; pending: boolean; error: unknown; opener: HTMLElement; onSubmit: (d: Draft) => void; onClose: () => void;
}) {
  // initial 只在挂载时读取；列表的周期刷新不覆盖草稿。
  const [draft, setDraft] = useState(initial);
  const set = (patch: Partial<Draft>) => setDraft({ ...draft, ...patch });
  const probe = draft.kind === AlertKind.PROBE;
  const loss = draft.metric === ProbeMetric.LOSS_PCT;
  const relative = !loss && draft.rttMode === RttMode.RELATIVE;
  const handle = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (!e.currentTarget.checkValidity()) return;
    if (!assignmentValid(draft)) return;
    onSubmit(draft);
  };
  return (
    <Drawer title={title} opener={opener} busy={pending} onClose={onClose}>
    <form aria-label={title} onSubmit={handle}>
      <div className="modal-body">
      {errorBanner(error)}
      <fieldset className="bare" disabled={pending}>
      <div className="row">
        <label>名称<input required value={draft.name} onChange={(e) => set({ name: e.target.value })} /></label>
        <label>类型
          <select value={draft.kind} onChange={(e) => set({ kind: Number(e.target.value) })}>
            {ALERT_KINDS.map(({ value, label }) => <option key={value} value={value}>{label}</option>)}
          </select>
        </label>
        <label className="inline"><input type="checkbox" checked={draft.enabled} onChange={(e) => set({ enabled: e.target.checked })} />启用</label>
      </div>
      {draft.kind === AlertKind.EXPIRY ? (
        <>
          <div className="row">
            <label>提前天数<input type="number" required min={1} max={365} step={1} value={draft.daysBefore} onChange={(e) => set({ daysBefore: e.target.value })} /></label>
          </div>
          <p className="muted">节点到期日距今不超过提前天数即触发（已过期的也算）；到期日改到这个范围之外、清除到期日，或调小提前天数使它落到范围之外，即恢复。续期后的到期日仍在范围内时不恢复。到期日在节点页设置。保存后立即评估，此后在 hub 启动时、hub 时区的每个日界（零点不存在的日子取新一天的第一个时刻）与修改节点计费时评估。</p>
        </>
      ) : draft.kind === AlertKind.CERT_EXPIRY ? (
        <>
          <div className="row">
            <label>探测任务
              <select required value={draft.taskId} onChange={(e) => set({ taskId: e.target.value })}>
                <option value="">选择任务</option>
                {taskOptions(tasks, true)}
              </select>
            </label>
            <label>提前天数<input type="number" required min={1} max={365} step={1} value={draft.daysBefore} onChange={(e) => set({ daysBefore: e.target.value })} /></label>
          </div>
          <p className="muted">任务的证书到期日距今不超过提前天数即触发（已过期的也算）；证书更换使到期日落到范围之外，或调小提前天数使它落到范围之外，即恢复。证书到期日由 HTTPS 探测握手成功时顺带带回（每任务每小时至多一次）。保存后立即评估，此后在 hub 启动时、hub 时区的每个日界与证书观测变化时评估。</p>
        </>
      ) : draft.kind === AlertKind.TRAFFIC ? (
        <label>流量阈值（%）<input type="number" required step="any" min={0} max={100} value={draft.threshold} onChange={(e) => set({ threshold: e.target.value })} /><span className="muted">大于 0、不超过 100；节点未设配额时不触发。</span></label>
      ) : draft.kind === AlertKind.RESOURCE ? (
        <ResourceFields draft={draft} set={set} />
      ) : probe ? (
        <div className="row">
          <label>探测任务
            <select required value={draft.taskId} onChange={(e) => set({ taskId: e.target.value })}>
              <option value="">选择任务</option>
              {taskOptions(tasks)}
            </select>
          </label>
          <label>指标
            <select value={draft.metric} onChange={(e) => set({ metric: Number(e.target.value) })}>
              {PROBE_METRICS.map(({ value, label }) => <option key={value} value={value}>{label}</option>)}
            </select>
          </label>
          {!loss && <label>判定方式
            <select value={draft.rttMode} onChange={(e) => set({ rttMode: Number(e.target.value) })}>
              {RTT_MODES.map(({ value, label }) => <option key={value} value={value}>{label}</option>)}
            </select>
          </label>}
          {!relative && <label>阈值（{loss ? "%" : "ms"}）<input type="number" required step="any" min={0} max={loss ? 100 : undefined}
            value={draft.threshold} onChange={(e) => set({ threshold: e.target.value })} /></label>}
          <label>连续分钟<input type="number" required min={1} max={60} value={draft.forMinutes} onChange={(e) => set({ forMinutes: e.target.value })} /></label>
        </div>
      ) : (
        <p className="muted">节点超过离线宽限期未上报即触发，收到上报即恢复；宽限期在节点页按节点设置，未设置时取 hub 的 HERON_OFFLINE_AFTER。</p>
      )}
      {probe && relative && <RelativeFields draft={draft} set={set} />}
      <NodeAssignment nodes={nodes} value={draft} onChange={set} legend="作用域节点" noun="作用域" />
      {probe && <p className="muted">探测规则只在既属于作用域、又分配了该任务的节点上评估。</p>}
      {draft.kind === AlertKind.CERT_EXPIRY && <p className="muted">证书到期规则只在既属于作用域、又分配了该任务的节点上评估；没有证书观测的节点不评估。</p>}
      {channels.length > 0
        ? <Picks legend="通知渠道" items={channels} selected={draft.channelIds} onChange={(channelIds) => set({ channelIds })} />
        : <p className="muted">还没有通知渠道；规则只记录事件，不发送通知。</p>}
      </fieldset>
      </div>
      <footer className="modal-footer">
        <button type="button" disabled={pending} onClick={onClose}>取消</button>
        <button type="submit" className="primary-button" disabled={pending}>{submitLabel}</button>
      </footer>
    </form>
    </Drawer>
  );
}

// 相对判定的字段与取值提示，范围与 hub 的 store.checkRttFields 一致（RELATIVE_LIMITS）。基线窗口不得短于判定窗口
// （连续分钟），输入的下限随它变。
function RelativeFields({ draft, set }: { draft: Draft; set: (patch: Partial<Draft>) => void }) {
  const adaptive = draft.baselineMode === BaselineMode.ADAPTIVE;
  return (
    <>
      <div className="row">
        <label>基线来源<select value={draft.baselineMode} onChange={(e) => set({ baselineMode: Number(e.target.value) })}>
          {BASELINE_MODES.map(({ value, label }) => <option key={value} value={value}>{label}</option>)}
        </select></label>
        {adaptive ? <>
          <label>基线窗口（分钟）<input type="number" required step="any" min={Number(draft.forMinutes) || 1} max={RELATIVE_LIMITS.windowMaxMinutes} value={draft.baselineWindowMinutes} onChange={(e) => set({ baselineWindowMinutes: e.target.value })} /></label>
          <label>最少桶数<input type="number" required step={1} min={1} value={draft.baselineMinSamples} onChange={(e) => set({ baselineMinSamples: e.target.value })} /></label>
        </> : <label>固定基线（ms）<input type="number" required step="any" min={0} max={RELATIVE_LIMITS.fixedMax} value={draft.fixedBaselineMs} onChange={(e) => set({ fixedBaselineMs: e.target.value })} /></label>}
      </div>
      <div className="row">
        <label>上偏差（%）<input type="number" required step="any" min={0} max={RELATIVE_LIMITS.upperMax} value={draft.upperDeviationPct} onChange={(e) => set({ upperDeviationPct: e.target.value })} /></label>
        <label>下偏差（%）<input type="number" required step="any" min={0} max={RELATIVE_LIMITS.lowerMax} value={draft.lowerDeviationPct} onChange={(e) => set({ lowerDeviationPct: e.target.value })} /></label>
        <label>冷却（分钟）<input type="number" required step="any" min={RELATIVE_LIMITS.cooldownMinMinutes} max={RELATIVE_LIMITS.cooldownMaxMinutes} value={draft.cooldownMinutes} onChange={(e) => set({ cooldownMinutes: e.target.value })} /></label>
      </div>
      <p className="muted">每分钟的 RTT 均值不高于基线 ×（1 + 上偏差）且不低于基线 ×（1 − 下偏差）为正常，越过任一边界（含边界）连续“连续分钟”即触发，回到带内即恢复。
        {adaptive
          ? "自适应基线取基线窗口内（不含判定窗口）各个 5 分钟桶的 RTT 均值的中位数，hub 每小时重算一次；桶数少于最少桶数时既不触发也不恢复。任务的类型或目标改了，基线从改动时起重新累积。"
          : "固定基线直接作为基线，与阈值无关。"}
        上偏差大于 0、不超过 {RELATIVE_LIMITS.upperMax}%，下偏差大于 0、不超过 {RELATIVE_LIMITS.lowerMax}%（取 {RELATIVE_LIMITS.lowerMax}% 即只看上偏差），冷却 1 分钟到 7 天：触发之后冷却期内不再触发，恢复照常通知。</p>
    </>
  );
}

// 阈值输入的单位与范围随指标切换（RESOURCE_METRICS 的 unit），提示文案与 hub CheckRule 的取值范围一致；
// 速率按 Mbps 输入，提交时换算成 bytes/s（resourceRule）。
function ResourceFields({ draft, set }: { draft: Draft; set: (patch: Partial<Draft>) => void }) {
  const unit = resourceUnit(draft.resourceMetric);
  const unitLabel = unit === "mbps" ? "Mbps" : unit === "per-core" ? "每核" : "%";
  const max = resourceThresholdMax(draft.resourceMetric);
  const source = unit === "mbps" ? "网卡速率的每分钟均值" : unit === "per-core" ? "按核负载的每分钟均值（采样时算好）；agent 不上报 load_cores 时，该分钟按缺失读数处理" : "同次采样的使用量与总量之比（CPU 为采样占比）的每分钟均值";
  return (
    <>
      <div className="row">
        <label>资源指标<select value={draft.resourceMetric} onChange={(e) => set({ resourceMetric: Number(e.target.value) })}>
          {RESOURCE_METRICS.map(({ value, label }) => <option key={value} value={value}>{label}</option>)}
        </select></label>
        <label>触发阈值（{unitLabel}）<input type="number" required step="any" min={0} max={max} value={draft.threshold} onChange={(e) => set({ threshold: e.target.value })} /></label>
        <label>恢复阈值（{unitLabel}）<input type="number" required step="any" min={0} max={max} value={draft.recoveryThreshold} onChange={(e) => set({ recoveryThreshold: e.target.value })} /></label>
        <label>连续分钟<input type="number" required min={1} max={60} value={draft.forMinutes} onChange={(e) => set({ forMinutes: e.target.value })} /></label>
      </div>
      <p className="muted">触发阈值大于 0 且不超过 {max} {unitLabel}，恢复阈值必须低于触发阈值。数据源：{source}；触发与恢复均需完整连续窗口，缺失读数不会恢复。</p>
    </>
  );
}

function RuleState({ states, nodeName }: { states: RuleStates | undefined; nodeName: (id: bigint) => string }) {
  if (!states) return <span className="muted">正常</span>;
  // flapping 由 hub 的离线巡检判定（pending 且只因抖动抑制而未触发），silenced 由事件生成处标记
  // （firing 进入时处于维护静默覆盖内，本次触发与它的恢复都不投递），面板只标出，不重算。
  const names = (list: RuleStates["firing"]) => list.map((s) => nodeName(s.nodeId) + (s.flapping ? "（抖动中）" : "") + (s.state === "firing" && s.silenced ? "（已静默）" : "")).join("、");
  return (
    <>
      {states.firing.length > 0 && <span className="error">触发：{names(states.firing)}</span>}
      {states.firing.length > 0 && states.pending.length > 0 && " "}
      {states.pending.length > 0 && <span>待定：{names(states.pending)}</span>}
    </>
  );
}
