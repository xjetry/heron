import { create as newMessage } from "@bufbuild/protobuf";
import { EmptySchema } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { createConnectQueryKey, createQueryOptions, useMutation, useQuery, useTransport } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useState } from "react";
import { errorText } from "../api/auth";
import { errorBanner, queryGateAll } from "../api/queryGate";
import { useLatestError } from "../api/useLatestError";
import { useOrder } from "../api/useOrder";
import { Drawer } from "../components/Modal";
import { RowMenu } from "../components/RowMenu";
import { PageHeader } from "../components/PageHeader";
import { assignmentValid, NodeAssignment, type NodeSelection } from "../components/NodeAssignment";
import { AdminService, CertPinChangeSchema, SaveProbeTaskRequestSchema, type Node, type ProbeTaskDetail } from "../gen/heron/v1/admin_pb";
import { useNodeList } from "../api/useNodeList";
import { AddressDetectionState, DnsRecordType, HttpStatusPolicy, ProbeIpFamily, ProbeKind, ProbeTaskSchema, type ProbeTask } from "../gen/heron/v1/types_pb";
import { formatPin, parsePin } from "../lib/certpin";
import { ascending, withId } from "../lib/ids";
import { DNS_RECORD_TYPES, HTTP_STATUS_POLICIES, HTTP_STATUS_RANGE, PROBE_FAMILIES, PROBE_KINDS, familyApplies, isHTTPSTarget, targetRule, targetWithAddress, taskTypeLabel } from "../lib/probes";
import { EmptyState } from "../components/EmptyState";

type Draft = NodeSelection & {
  kind: ProbeKind; target: string; dnsServer: string; ipFamily: ProbeIpFamily; dnsRecordType: DnsRecordType;
  httpStatusPolicy: HttpStatusPolicy; httpExpectedStatus: string;
  intervalS: string; timeoutMs: string; pin: string; clearPin: boolean; configId?: Uint8Array;
};
type TaskEntry = { task: ProbeTask; allNodes: boolean; nodeIds: bigint[]; selectorTags: string[] };

type DrawerState = { kind: "create"; opener: HTMLElement } | { kind: "edit"; entry: TaskEntry; opener: HTMLElement } | null;

const taskError = (error: unknown) => error instanceof ConnectError && error.code === Code.FailedPrecondition
  ? `配置已变化，请刷新后再试。${errorText(error)}` : errorText(error);

// 新建默认严格 2xx：协议缺省的 < 400 只为存量任务保持升级前的行为，新任务不该把反代跳去登录页的 3xx 当成正常。
const emptyDraft = (): Draft => ({ kind: ProbeKind.ICMP, target: "", dnsServer: "", ipFamily: ProbeIpFamily.UNSPECIFIED, dnsRecordType: DnsRecordType.UNSPECIFIED,
  httpStatusPolicy: HttpStatusPolicy.SUCCESS, httpExpectedStatus: "", intervalS: "60", timeoutMs: "1000", allNodes: false, nodeIds: new Set(), selectorTags: [], dynamic: false, pin: "", clearPin: false });
const taskEntries = (tasks: readonly ProbeTaskDetail[]): TaskEntry[] => tasks.flatMap((d) => d.task ? [{ task: d.task, allNodes: d.allNodes, nodeIds: d.nodeIds, selectorTags: d.selectorTags }] : []);
// all_nodes 任务的 nodeIds 是 hub 展开的当前全部节点；编辑切换为指定节点时以它们作为显式分配的起点。
// HTTP 任务原样回显判据；别的种类本没有判据，编辑中改成 HTTP 等于新配一个，取新建的默认。
const draftOf = ({ task, allNodes, nodeIds, selectorTags }: TaskEntry): Draft => ({
  kind: task.kind, target: task.target, dnsServer: task.dnsServer, ipFamily: task.ipFamily, dnsRecordType: task.dnsRecordType,
  httpStatusPolicy: task.kind === ProbeKind.HTTP ? task.httpStatusPolicy : HttpStatusPolicy.SUCCESS,
  httpExpectedStatus: task.kind === ProbeKind.HTTP && task.httpStatusPolicy === HttpStatusPolicy.EXACT ? String(task.httpExpectedStatus) : "", intervalS: String(task.intervalS),
  timeoutMs: String(task.timeoutMs), allNodes, nodeIds: new Set(nodeIds), selectorTags, dynamic: selectorTags.length > 0,
  pin: formatPin(task.certSpkiSha256), clearPin: false, configId: task.configId,
});

export function ProbeTasks() {
  const qc = useQueryClient();
  const transport = useTransport();
  const [drawer, setDrawer] = useState<DrawerState>(null);
  const { error, mutationOptions } = useLatestError();
  const nodes = useNodeList({});
  const list = useQuery(AdminService.method.listProbeTasks, {});
  // 任务与分配的每次修改都让列表重新拉取；节点列表没有变化，不失效它。
  const refresh = () => qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listProbeTasks, cardinality: "finite" }) });
  const create = useMutation(AdminService.method.saveProbeTask, { ...mutationOptions, onSuccess: refresh });
  // mutation 的 onSuccess 返回刷新 promise，提交回调在列表回读完成后才关闭抽屉。
  const update = useMutation(AdminService.method.saveProbeTask, { ...mutationOptions, onSuccess: refresh });
  const busy = create.isPending || update.isPending;
  const remove = useMutation(AdminService.method.deleteProbeTask, { ...mutationOptions, onSuccess: refresh });
  const reorder = useMutation(AdminService.method.reorderProbeTasks);
  const order = useOrder({
    items: list.data?.tasks ?? [], id: (entry) => entry.task?.id ?? 0n,
    enabled: list.data !== undefined && !list.isError,
    save: (ids) => reorder.mutateAsync({ ids }),
    reload: async () => {
      const options = createQueryOptions(AdminService.method.listProbeTasks, {}, { transport });
      await qc.cancelQueries({ queryKey: options.queryKey, exact: true });
      return (await qc.fetchQuery({ ...options, staleTime: 0 })).tasks;
    },
  });
  // 分配求交依赖节点列表已到达；未到达前不渲染任何可提交的表单。
  const gate = queryGateAll(nodes, list);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  const [nodesData] = gate.data;
  const nodeList = nodesData.nodes;
  const availableNodeIds = new Set(nodeList.map((n) => n.id));
  // submit 用 availableNodeIds 把分配求交到当前节点列表，避免提交已删除节点；allNodes 仍取草稿中的显式选择。
  // hub 按 all_nodes 展开覆盖，因此显式分配求交为空时照常提交空集、不覆盖节点，不能因空集改成全部。
  const submit = (m: typeof create, id: bigint, d: Draft, onSuccess?: () => void) => {
    const pin = !d.clearPin && d.pin.trim() ? parsePin(d.pin) : undefined;
    const req = newMessage(SaveProbeTaskRequestSchema, {
      // 地址族、记录类型与状态码判据各自只对适用的种类提交，精确码只随精确判据提交：切换种类或判据后草稿里残留的值
      // 若随请求发出，hub 会拒绝整个保存。
      task: newMessage(ProbeTaskSchema, {
        id, kind: d.kind, target: d.target.trim(), dnsServer: d.kind === ProbeKind.DNS ? d.dnsServer.trim() : "", intervalS: Number(d.intervalS), timeoutMs: Number(d.timeoutMs),
        ipFamily: familyApplies(d.kind) ? d.ipFamily : ProbeIpFamily.UNSPECIFIED,
        dnsRecordType: d.kind === ProbeKind.DNS ? d.dnsRecordType : DnsRecordType.UNSPECIFIED,
        httpStatusPolicy: d.kind === ProbeKind.HTTP ? d.httpStatusPolicy : HttpStatusPolicy.UNSPECIFIED,
        httpExpectedStatus: d.kind === ProbeKind.HTTP && d.httpStatusPolicy === HttpStatusPolicy.EXACT ? Number(d.httpExpectedStatus) : 0,
      }),
      allNodes: d.allNodes, nodeIds: d.allNodes || d.dynamic ? [] : ascending([...d.nodeIds].filter((nodeId) => availableNodeIds.has(nodeId))),
      selectorTags: !d.allNodes && d.dynamic ? d.selectorTags : [],
      // 编辑带上读到的配置身份，避免并发保存把别人的修改盖掉。新建没有身份可对。
      expectedConfigId: id !== 0n && d.configId && d.configId.length > 0 ? d.configId : undefined,
      certPin: d.clearPin
        ? newMessage(CertPinChangeSchema, { action: { case: "clear", value: newMessage(EmptySchema) } })
        : pin ? newMessage(CertPinChangeSchema, { action: { case: "setSpkiSha256", value: pin } }) : undefined,
    });
    m.mutate(req, { onSuccess });
  };
  const tasks = taskEntries(order.items);
  const assignmentText = (entry: TaskEntry) => {
    const names = entry.nodeIds.map((id) => nodeList.find((n) => n.id === id)?.name ?? `#${id}`);
    if (entry.allNodes) return { text: "全部节点", title: names.join("、") || "暂无节点" };
    if (entry.selectorTags.length) return { text: `标签：${entry.selectorTags.join(" ∩ ")}（当前 ${names.length}）`, title: names.join("、") || "无匹配" };
    // 显式分配为空不覆盖任何节点，必须与全部节点区分。
    return names.length ? { text: `${names.length} 个指定节点`, title: names.join("、") } : { text: "未分配", title: "" };
  };
  return (
    <section>
      <PageHeader title="探测任务" actions={<button type="button" className="primary-button" disabled={busy} onClick={(event) => { create.reset(); setDrawer({ kind: "create", opener: event.currentTarget }); }}>新建探测任务</button>} />
      {gate.banner}
      {drawer === null && error != null && <p role="alert" className="error">{taskError(error)}</p>}
      {order.error != null && <p role="alert" className="error">排序未完成：{errorText(order.error)}</p>}
      {order.pending && <p role="status" className="muted">正在保存并确认排序…</p>}
      {order.blocked && <button type="button" onClick={order.recover} disabled={order.pending}>重新读取排序</button>}
      {tasks.length === 0 ? <EmptyState title="还没有探测任务。" /> : <div className="table-scroll" role="region" aria-label="探测任务管理" tabIndex={0}>
        <table className="nodes probe-table">
          <thead><tr><th><span className="sr-only">排序</span></th><th>类型</th><th>目标</th><th>间隔</th><th>超时</th><th>分配</th><th><span className="sr-only">操作</span></th></tr></thead>
          <tbody>{tasks.map((entry) => {
            const t = entry.task;
            const label = withId(t.target, t.id);
            const assignment = assignmentText(entry);
            const movable = !(order.blocked || list.isError);
            return <tr key={String(t.id)} aria-label={t.target}>
              <td data-label="排序"><button type="button" className="link" aria-label={`上移 ${label}`} disabled={!movable} onClick={() => order.move(t.id, -1)}>↑</button><button type="button" className="link" aria-label={`下移 ${label}`} disabled={!movable} onClick={() => order.move(t.id, 1)}>↓</button></td>
              <td data-label="类型">{taskTypeLabel(t)}</td>
              <td data-label="目标" className="num"><span className="clip-text clip-wide" title={t.target}>{t.target}</span></td>
              <td data-label="间隔" className="num">{t.intervalS} s</td>
              <td data-label="超时" className="num">{t.timeoutMs} ms</td>
              <td data-label="分配" className="wrap-text"><span title={assignment.title || undefined} className={assignment.text === "未分配" ? "muted" : undefined}>{assignment.text}</span></td>
              <td data-column="actions"><RowMenu label={label} items={[
                { label: "编辑", disabled: busy, onSelect: (trigger) => { update.reset(); setDrawer({ kind: "edit", entry, opener: trigger }); } },
                // 导航项不随保存禁用：保存只发生在抽屉里，抽屉打开时整页已 inert，行菜单本就不可达。
                { label: "对比", to: `/probes/${t.id}/compare` },
                ...(isHTTPSTarget(t.kind, t.target) ? [{ label: "证书", to: `/probes/${t.id}/certs` }] : []),
                { label: "删除", danger: true, confirm: `确认删除 ${label}`, note: "历史随后由维护任务分块清除", disabled: busy || remove.isPending, onSelect: () => remove.mutate({ id: t.id }) },
              ]} /></td>
            </tr>;
          })}</tbody>
        </table>
      </div>}
      {drawer?.kind === "create" && <ProbeTaskDrawer title="新建探测任务" submitLabel="创建" nodes={nodeList} initial={emptyDraft()} pending={create.isPending} error={create.error} opener={drawer.opener} onClose={() => setDrawer(null)}
        onSubmit={(d) => submit(create, 0n, d, () => setDrawer(null))} />}
      {drawer?.kind === "edit" && <ProbeTaskDrawer key={String(drawer.entry.task.id)} title={`编辑 ${withId(drawer.entry.task.target, drawer.entry.task.id)}`} submitLabel="保存" nodes={nodeList} initial={draftOf(drawer.entry)} pending={update.isPending} error={update.error} opener={drawer.opener} onClose={() => setDrawer(null)}
        onSubmit={(d) => submit(update, drawer.entry.task.id, d, () => setDrawer(null))} />}
    </section>
  );
}

type NodeAddressOption = { key: string; label: string; address: string; v6: boolean };

// 快选只列节点已就绪（AVAILABLE）的出口地址，也就是节点页显示的那个值（Node.network：手填、agent 探测、hub 看到的来源
// 依次取先有的）；双栈节点的两族各成一项，因为同一个节点按 IPv4 与按 IPv6 是两条不同的线路。
function nodeAddresses(nodes: readonly Node[]): NodeAddressOption[] {
  return nodes.flatMap((node) => ([["IPv4", node.network?.ipv4, false], ["IPv6", node.network?.ipv6, true]] as const).flatMap(([family, entry, v6]) =>
    entry?.state === AddressDetectionState.AVAILABLE && entry.address
      ? [{ key: `${node.id}:${family}`, label: `${node.name} · ${family} ${entry.address}`, address: entry.address, v6 }]
      : []));
}

// 创建与编辑共用；字段约束用原生属性表达，hub 的 probelimit 是最终裁决，错误原文回到页面上。
function ProbeTaskDrawer({ title, submitLabel, nodes, initial, pending, error, opener, onClose, onSubmit }: {
  title: string; submitLabel: "创建" | "保存"; nodes: Node[]; initial: Draft; pending: boolean; error: unknown; opener: HTMLElement; onClose: () => void; onSubmit: (d: Draft) => void;
}) {
  // initial 只在挂载时读取；编辑期间的列表刷新不覆盖草稿，节点列表以 props 实时更新，提交时与当前列表求交。
  const [draft, setDraft] = useState(initial);
  const [pinError, setPinError] = useState("");
  const showPin = draft.kind === ProbeKind.HTTP || draft.pin.trim() !== "" || draft.clearPin;
  const addresses = nodeAddresses(nodes);
  const pinFits = isHTTPSTarget(draft.kind, draft.target);
  const handle = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (!e.currentTarget.checkValidity()) return;
    if (!assignmentValid(draft)) return;
    if (!draft.clearPin && draft.pin.trim()) {
      try {
        parsePin(draft.pin);
      } catch (err) {
        setPinError(err instanceof Error ? err.message : String(err));
        return;
      }
    }
    setPinError("");
    onSubmit(draft);
  };
  return (
    <Drawer title={title} busy={pending} opener={opener} onClose={onClose}>
      <form aria-label={title} onSubmit={handle}>
        <div className="modal-body">
          {error != null && <p role="alert" className="error">{taskError(error)}</p>}
          <fieldset className="bare" disabled={pending}>
            <div className="segmented" role="radiogroup" aria-label="类型">
              {PROBE_KINDS.map(({ kind, label }) => <label key={kind}><input type="radio" name="probe-kind" aria-label={label} checked={draft.kind === kind} onChange={() => setDraft({ ...draft, kind })} /><span>{label}</span></label>)}
            </div>
            <label>目标<input data-autofocus required maxLength={targetRule(draft.kind).maxLength} placeholder={targetRule(draft.kind).placeholder} value={draft.target}
              onChange={(e) => setDraft({ ...draft, target: e.target.value })} /></label>
            {familyApplies(draft.kind) && addresses.length > 0 && (
              <select aria-label="从节点地址填入" value="" onChange={(e) => {
                const picked = addresses.find((entry) => entry.key === e.target.value);
                if (!picked) return;
                // 显式地址族与地址不同族时换成地址的族：字面量目标与显式地址族冲突的任务 hub 必然拒绝。
                const family = picked.v6 ? ProbeIpFamily.IPV6 : ProbeIpFamily.IPV4;
                setDraft({ ...draft, target: targetWithAddress(draft.kind, draft.target, picked.address, picked.v6), ipFamily: draft.ipFamily === ProbeIpFamily.UNSPECIFIED ? draft.ipFamily : family });
              }}>
                <option value="">选择节点地址…</option>
                {addresses.map((entry) => <option key={entry.key} value={entry.key}>{entry.label}</option>)}
              </select>
            )}
            {familyApplies(draft.kind) ? (
              <div className="segmented" role="radiogroup" aria-label="地址族">
                {PROBE_FAMILIES.map(({ family, label }) => <label key={family}><input type="radio" name="probe-family" aria-label={label} checked={draft.ipFamily === family} onChange={() => setDraft({ ...draft, ipFamily: family })} /><span>{label}</span></label>)}
              </div>
            ) : (
              <div className="segmented" role="radiogroup" aria-label="记录类型">
                {DNS_RECORD_TYPES.map(({ type, label }) => <label key={type}><input type="radio" name="probe-record" aria-label={label} checked={draft.dnsRecordType === type} onChange={() => setDraft({ ...draft, dnsRecordType: type })} /><span>{label}</span></label>)}
              </div>
            )}
            {draft.kind === ProbeKind.HTTP && (
              <div className="segmented" role="radiogroup" aria-label="期望状态码">
                {HTTP_STATUS_POLICIES.map(({ policy, label }) => <label key={policy}><input type="radio" name="probe-status" aria-label={label} checked={draft.httpStatusPolicy === policy} onChange={() => setDraft({ ...draft, httpStatusPolicy: policy })} /><span>{label}</span></label>)}
              </div>
            )}
            {draft.kind === ProbeKind.HTTP && draft.httpStatusPolicy === HttpStatusPolicy.EXACT && (
              <label>状态码<input type="number" required min={HTTP_STATUS_RANGE.min} max={HTTP_STATUS_RANGE.max} step={1} placeholder="如 204" value={draft.httpExpectedStatus}
                onChange={(e) => setDraft({ ...draft, httpExpectedStatus: e.target.value })} /></label>
            )}
            {draft.kind === ProbeKind.DNS && (
              <label>解析器<input required maxLength={47} placeholder="ip:port，如 1.1.1.1:53" value={draft.dnsServer}
                onChange={(e) => setDraft({ ...draft, dnsServer: e.target.value })} /></label>
            )}
            <div className="form-grid two">
              <label>间隔 (s)<input type="number" required min={5} max={3600} value={draft.intervalS} onChange={(e) => setDraft({ ...draft, intervalS: e.target.value })} /></label>
              <label>超时 (ms)<input type="number" required min={100} max={5000} value={draft.timeoutMs} onChange={(e) => setDraft({ ...draft, timeoutMs: e.target.value })} /></label>
            </div>
            {showPin && (
              <label>证书指纹
                <input aria-label="证书指纹" placeholder="sha256// 加 base64，留空表示不改" value={draft.pin}
                  onChange={(e) => setDraft({ ...draft, pin: e.target.value, clearPin: false })} />
              </label>
            )}
            {showPin && (
              <p className="muted">{draft.clearPin ? "保存时将清除指纹。" : draft.pin ? `当前显示 ${draft.pin}` : "未钉指纹。"}
                {" "}<button type="button" className="link" onClick={() => setDraft({ ...draft, pin: "", clearPin: true })}>清除指纹</button>
              </p>
            )}
            {showPin && !pinFits && <p className="muted">这个种类或地址不能钉指纹，改种类不会自动清除。请先清除指纹再保存。</p>}
            {pinError && <p role="alert" className="error">{pinError}</p>}
            <NodeAssignment nodes={nodes} value={draft} onChange={(patch) => setDraft({ ...draft, ...patch })} legend="分配到节点" />
          </fieldset>
        </div>
        <footer className="modal-footer"><button type="button" disabled={pending} onClick={onClose}>取消</button><button type="submit" className="primary-button" disabled={pending}>{submitLabel}</button></footer>
      </form>
    </Drawer>
  );
}
