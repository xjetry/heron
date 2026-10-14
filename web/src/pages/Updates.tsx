import { useMutation, useQuery } from "@connectrpc/connect-query";
import { type CSSProperties, useEffect, useRef, useState } from "react";
import { errorText } from "../api/auth";
import { errorBanner, queryGateAll } from "../api/queryGate";
import { MixedCheckbox } from "../components/MixedCheckbox";
import { Modal } from "../components/Modal";
import { PageHeader } from "../components/PageHeader";
import { AdminService, type Node } from "../gen/heron/v1/admin_pb";
import type { UpdateStatus } from "../gen/heron/v1/update_pb";
import { liveById, liveStatus } from "../lib/adminStatus";
import { POLL_MS } from "../lib/poll";
import type { NodeStatus } from "../lib/status";
import { laggingNode } from "../lib/lagging";
import { isRelease, isStableRelease, olderThan } from "../lib/version";
import { updateReasonText } from "../lib/updateReason";
import { EmptyState } from "../components/EmptyState";

const labels: Record<string, string> = {
  queued: "等待节点上线", dispatched: "已下发", downloading: "下载并校验", stopping: "停止旧服务",
  installing: "替换程序", verifying: "验证新版本", rolling_back: "正在回滚", succeeded: "更新成功", failed: "更新失败",
  rolled_back: "已回滚", cancelled: "已取消", expired: "已过期", unconfirmed: "结果未确认",
};
const activeStates = new Set(["queued", "dispatched", "downloading", "stopping", "installing", "verifying", "rolling_back"]);
// 任务的终态（成功、失败、已回滚、已取消、已过期、结果未确认）是历史，不是页面状态：hub 保存它供协议判定（防重放、过期、
// Superseded），面板只对本页会话发起或取消过的目标显示它；首次进入或刷新后，带终态任务的机器与没有任务的机器显示相同，
// 按版本对比陈述。进行中的任务对谁都显示：它们是正在发生的事实，别处发起的也要能看到并取消。
function visibleTask(status: UpdateStatus | undefined, showResult: boolean) {
  const task = status?.task;
  if (!task || activeStates.has(task.state) || showResult) return task;
  return undefined;
}
// 取产物的来源由节点安装时的 --update-source 决定；更新失败时先看它走的哪条路径。
const sourceLabels: Record<string, string> = { github: "GitHub 直连", hub: "经 hub 中转" };
function eligible(status: UpdateStatus | undefined, version: string) {
  return !!status?.supported && !!version && olderThan(status.version, version) && !activeStates.has(status.task?.state ?? "");
}
// 已达到目标版本：目标与实际版本都是正式版且实际不低于目标。目标未知（空串）或版本不是正式版时无从比较，不算达到。
function atTarget(status: UpdateStatus | undefined, target: string) {
  return !!status && isRelease(target) && isRelease(status.version) && !olderThan(status.version, target);
}

// 节点表的分组顺序，组之间不画分隔，组内保持节点列表顺序：0 可更新或正在更新（进行中的任务不让行在更新途中挪走），
// 1 已是目标版本，2 其余（低于目标但不支持在线更新、尚未报告更新能力、目标未知），3 离线与从未上报（排队等节点上线的
// 任务也在这里）。live 来自快照，快照未到或失败时为 undefined，按在线排：离线节点这时不沉底，其余顺序照常。
export function updateRank(status: UpdateStatus | undefined, live: NodeStatus | undefined, target: string): number {
  if (live === "offline" || live === "never") return 3;
  if (eligible(status, target) || activeStates.has(status?.task?.state ?? "")) return 0;
  if (atTarget(status, target)) return 1;
  return 2;
}
// target 是这台机器的更新目标（节点为 hub 绑定的 agent 版本，hub 为检查到的官方最新版），未知时为空串。
// 没有任务时：已达到目标显示"已是目标版本"，否则只陈述在线更新能力；目标未知时无从比较，也只陈述能力。
// 已登记的原因写中文；未登记的（更新引擎的原始错误）照写原文，用等宽小字和中文说明区分开。
function UnsupportedReason({ reason }: { reason: string }) {
  if (reason === "") return <span className="muted">不支持在线更新：本机更新器不可用</span>;
  const text = updateReasonText(reason);
  return text !== undefined
    ? <span className="muted">不支持在线更新：{text}</span>
    : <span className="muted">不支持在线更新<code className="update-reason-raw">{reason}</code></span>;
}

function Progress({ status, target, showResult }: { status?: UpdateStatus; target: string; showResult: boolean }) {
  if (!status) return <span className="muted">尚未收到更新能力，请先升级安装器与 agent。</span>;
  const task = visibleTask(status, showResult);
  return <div className="update-progress">
    {!status.supported && <UnsupportedReason reason={status.reason} />}
    {task && <><strong>{labels[task.state] ?? task.state}</strong><span className="muted">目标 {task.version}</span>
      {task.state === "unconfirmed" && <span className="muted">下发后等待超时，无法确认执行结果；后续上报仍会校正。重试由节点本机更新器检查是否可执行。</span>}
      {task.error && <span className="error">{task.error}</span>}</>}
    {status.supported && !task && <span className="muted">{atTarget(status, target) ? "已是目标版本" : "可以在线更新"}</span>}
  </div>;
}

// 一次"检查官方新版本"的结果：三项取自同一个 GetUpdates({ checkLatest: true }) 应答。轮询的 GetUpdates({}) 不查询官方
// 版本，latestVersion 与 latestBoundAgentVersion 恒为空，所以 hub 更新的目标与绑定变化只能从这里取。
type LatestCheck = { version: string; latestBound: string; bound: string };
// hub 更新会把绑定的 agent 版本换成目标 release 的绑定值。unknown：hub 读不到目标 release 的
// AGENT_VERSION 资产，按会变化处理；changed：两边都已知且不同。相同时不陈述（undefined）。
type BindingChange =
  | { kind: "unknown" }
  | { kind: "changed"; from: string; to: string; lagging: number; total: number; noVersion: number; incomparable: number };
// 落后只经 laggingNode。已出租没有 agent，先从计数里拿掉，分母也不含它们（与 GetUpdates 不列它们同一口径）。
// 版本缺失或解析不了（开发构建等）的另计为版本未知，不计入落后数，也不当作已达到——谓词对它们为 false，
// 混进落后数会把未知说成"不落后"。
function bindingChange(check: LatestCheck, nodes: readonly Node[]): BindingChange | undefined {
  if (check.latestBound === "") return { kind: "unknown" };
  if (check.latestBound === check.bound) return undefined;
  const counted = nodes.filter((node) => !node.rented);
  const versions = counted.map((node) => node.facts?.agentVersion ?? "");
  return {
    kind: "changed", from: check.bound, to: check.latestBound, total: counted.length,
    lagging: counted.filter((node) => laggingNode(node, check.latestBound)).length,
    noVersion: versions.filter((v) => v === "").length,
    incomparable: versions.filter((v) => v !== "" && !isRelease(v)).length,
  };
}
const commitment = "hub 只承诺支持绑定版本的 agent，更新后请尽快更新节点。";
function BindingNotice({ change }: { change: BindingChange }) {
  if (change.kind === "unknown") return <>
    <p>无法读到目标版本绑定的 agent 版本，按会变化处理；将落后的节点数无法计算。</p>
    <p className="muted">{commitment}</p>
  </>;
  const unknown = change.noVersion + change.incomparable;
  return <>
    <p>更新后 hub 绑定的 agent 版本将从 <strong>{change.from || "（无）"}</strong> 变为 <strong>{change.to}</strong>。</p>
    <p>更新后将落后的节点：{change.lagging} / {change.total}</p>
    <p>版本未知的节点：{unknown} / {change.total}（未上报版本 {change.noVersion}，版本号无法比较 {change.incomparable}），不计入落后数。</p>
    <p className="muted">{commitment}</p>
  </>;
}

export function Updates() {
  const updates = useQuery(AdminService.method.getUpdates, {}, { refetchInterval: 3000 });
  const nodes = useQuery(AdminService.method.listNodes, {});
  // 快照只用于把离线与从未上报的节点排到最后，不进 gate：它失败时页面照常可用，只是失去这一条排序。
  const snapshot = useQuery(AdminService.method.getSnapshot, {}, { refetchInterval: POLL_MS });
  const [latestCheck, setLatestCheck] = useState<LatestCheck>({ version: "", latestBound: "", bound: "" });
  const latest = latestCheck.version;
  const [selected, setSelected] = useState<Set<bigint>>(new Set());
  // 确认框的内容在打开时整体定下：目标版本、绑定变化与节点计数都是那一刻的快照，之后的轮询、节点列表刷新与
  // 重新检查都不改变已打开的确认框，确认的就是看到的。binding 只有 hub 更新才有。
  const [confirmation, setConfirmation] = useState<{ ids: bigint[]; version: string; binding?: BindingChange; opener: HTMLElement } | null>(null);
  // 提交进度：null 表示没有在提交。busy 只由它推出，锁住所有发起更新、取消与勾选的控件。
  const [submitting, setSubmitting] = useState<{ done: number; total: number } | null>(null);
  const busy = submitting !== null;
  // results[i] 是 progress.ids[i] 的提交结果，尚未返回时为 undefined；长度在打开弹窗时就定为目标数，结果到达只填位不追加。
  const [results, setResults] = useState<Array<string | undefined>>([]);
  // 提交进度与逐目标结果放在弹窗里，而不是页面上逐行往下长的卡片：几十个目标的结果不该把节点表顶出视口。弹窗打开时
  // 就为 ids 的每个目标留一行，列表高度只由目标数决定（见 admin.css 的 .update-results），结果逐条到达不改变弹窗高度，
  // 关闭按钮也就不随之移动。弹窗随时可关：关闭只是收起弹窗，不取消逐个提交——提交循环照常跑完，每个目标的结果照常进
  // resultTargets，关掉弹窗后从表格的任务状态看结果；busy 仍锁住页面上发起更新、取消与勾选的控件直到本轮提交结束。
  // 焦点固定在始终可用的关闭按钮上（data-autofocus），结果行只做文字替换并经 aria-live 播报，不会带走焦点；opener 是
  // 打开确认框的那个按钮，关闭后焦点回到它。
  const [progress, setProgress] = useState<{ opener: HTMLElement; ids: bigint[] } | null>(null);
  // 本页会话发起或取消过更新的目标（hub 为 0n）：只有它们的任务终态会显示（见 visibleTask）。只在 hub 受理之后加入：
  // 被拒的发起没有建任务，加入只会把别处留下的旧终态翻出来。
  const [resultTargets, setResultTargets] = useState<Set<bigint>>(new Set());
  const showResultFor = (id: bigint) => setResultTargets((old) => new Set(old).add(id));
  const check = useMutation(AdminService.method.getUpdates, {
    onSuccess: (data) => setLatestCheck({ version: data.latestVersion, latestBound: data.latestBoundAgentVersion, bound: data.boundAgentVersion }),
  });
  // 打开页面即自动检查一次官方新版本，与按钮共用同一个 mutation：按钮的进行中与失败呈现对这次检查同样生效。
  // ref 守卫保证只发一次：StrictMode 双重挂载会重复跑 effect，没有守卫每次挂载都会各查一次。
  const autoChecked = useRef(false);
  useEffect(() => {
    if (autoChecked.current) return;
    autoChecked.current = true;
    check.mutate({ checkLatest: true });
  }, [check]);
  const start = useMutation(AdminService.method.startUpdate);
  const cancel = useMutation(AdminService.method.cancelUpdate, { onSuccess: (_data, req) => { if (req.nodeId !== undefined) showResultFor(req.nodeId); void updates.refetch(); } });
  const gate = queryGateAll(updates, nodes);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  const targets = new Map(updates.data!.targets.map((target) => [target.nodeId, target.status]));
  const hub = targets.get(0n);
  // 节点更新到 hub 绑定的 agent 版本，不跟随官方最新——只改 hub 的版本不要求节点升级；绑定不是
  // 正式版（开发构建或预发布）时没有可下发的产物目标，不提供节点更新。hub 自身仍以官方最新正式版为目标，要先检查。
  const nodeTarget = isStableRelease(updates.data!.boundAgentVersion) ? updates.data!.boundAgentVersion : "";
  const live = liveById(snapshot.data?.nodes);
  const ranks = new Map(nodes.data!.nodes.map((node) => [node.id, updateRank(targets.get(node.id), liveStatus(node, live.get(node.id)), nodeTarget)]));
  // 已出租节点没有 agent，不是更新目标。GetUpdates 也不列它们；表、全选与提交跟同一口径。
  const rows = [...nodes.data!.nodes].filter((node) => !node.rented).sort((a, b) => ranks.get(a.id)! - ranks.get(b.id)!);
  // 行勾选框、全选与提交只认 eligible 这一个判定：updatable 是此刻可更新的节点（按表格顺序），chosen 是它与已选的
  // 交集。轮询让节点失去资格时，selected 里的旧 id 不再计入 chosen，计数、全选状态与点击"更新选中节点"时取的目标一起收缩；
  // 确认框打开后目标固定，期间失去资格的节点由 hub 的 updates.Manager.Start 按同样的条件（支持、版本更旧、无进行中任务）拒绝。
  const updatable = rows.filter((node) => eligible(targets.get(node.id), nodeTarget)).map((node) => node.id);
  const chosen = updatable.filter((id) => selected.has(id));
  const nameOf = (id: bigint) => id === 0n ? "Hub" : nodes.data!.nodes.find((node) => node.id === id)?.name ?? `节点 #${id}`;
  // 确认框只负责确认：点了确认就关闭，逐个提交的进度与每个目标的结果显示在进度弹窗里。目标与版本在关闭前取出，
  // 之后的轮询与勾选变化不影响这次提交；提交期间 busy 禁止再打开确认框，同一批目标不会被重复提交。
  const execute = async () => {
    if (!confirmation || busy) return;
    const { ids, version, opener } = confirmation;
    setConfirmation(null); setResults(ids.map(() => undefined)); setSubmitting({ done: 0, total: ids.length }); setProgress({ opener, ids });
    const settle = (i: number, text: string) => setResults((old) => old.map((result, j) => j === i ? text : result));
    for (const [i, id] of ids.entries()) {
      try {
        await start.mutateAsync({ nodeId: id, version });
        showResultFor(id);
        settle(i, `${nameOf(id)}：更新任务已提交，最终结果以状态回读为准。`);
      } catch (error) {
        settle(i, `${nameOf(id)}：${errorText(error)}${id === 0n ? "。若连接已断开，请等待状态回读，不能据此判定更新成功或失败。" : ""}`);
      }
      setSubmitting((old) => old && { ...old, done: old.done + 1 });
    }
    setSubmitting(null); setSelected(new Set()); void updates.refetch();
  };
  return <section>
    <PageHeader title="在线更新" description="官方正式发行版 · Linux systemd" actions={
      <button type="button" disabled={check.isPending || busy} onClick={() => check.mutate({ checkLatest: true })}>{check.isPending ? "正在检查…" : "检查官方新版本"}</button>
    } />
    {gate.banner}
    {updates.error && <p role="status">暂时无法连接 Hub，正在重试。更新结果尚未确认，请勿重复提交。</p>}
    {check.error && <p role="alert" className="error">{errorText(check.error)}</p>}
    {check.data?.checkError && <p role="alert" className="error">检查官方版本失败：{check.data.checkError}</p>}
    {cancel.error && <p role="alert" className="error">{errorText(cancel.error)}</p>}
    {/* 与下面「节点 Agent」同一种写法：小标题一行，操作按钮在标题右侧，卡片里只放版本与状态。 */}
    <section aria-label="Hub">
      <div className="page-header"><div><h2>Hub</h2></div>
        <button type="button" disabled={!eligible(hub, latest) || busy} onClick={(event) => setConfirmation({ ids: [0n], version: latest, binding: bindingChange(latestCheck, nodes.data!.nodes), opener: event.currentTarget })}>更新 Hub</button>
      </div>
      <div className="hub-card">
      <dl>
        <div><dt>当前版本</dt><dd>{hub?.version || "未知"}</dd></div>
        <div><dt>官方最新正式版</dt><dd>{latest || "尚未检查"}</dd></div>
        <div><dt>绑定的 agent 版本</dt><dd>{updates.data!.boundAgentVersion || "—"}</dd></div>
      </dl>
      <Progress status={hub} target={latest} showResult={resultTargets.has(0n)} />
      <div className="hub-card-description">
        <p className="muted">只安装 xjetry/heron 正式 Release 中带官方签名的产物，不执行远程命令；节点按安装时的选择直接从 GitHub 或经 hub 中转取得。节点更新到 hub 绑定的 agent 版本；只改 hub 的版本不要求节点升级。</p>
        <p className="muted">首次启用需用新版安装器安装本机更新服务。Docker、OpenRC 与 macOS 请使用各自安装方式。</p>
      </div>
      </div>
    </section>
    <div className="page-header"><div><h2>节点 Agent</h2><p className="muted">{nodeTarget ? `目标版本 ${nodeTarget}（hub 绑定的 agent 版本）。` : "这个 hub 没有绑定正式的 agent 版本（开发构建或预发布），不能在线更新节点。"}离线任务最多等待 24 小时；新版本成功上报后才算完成。</p></div>
      <button type="button" disabled={chosen.length === 0 || busy} onClick={(event) => setConfirmation({ ids: chosen, version: nodeTarget, opener: event.currentTarget })}>更新选中节点（{chosen.length}）</button>
    </div>
    {/* 表是否出现跟 rows 走，不跟 ListNodes 的长度。全是已出租时列表非空，但这些节点没有 agent，只剩表头会像没加载完。 */}
    {rows.length === 0 ? <EmptyState title={nodes.data!.nodes.length === 0 ? "还没有节点。" : "没有可更新的节点：已出租的节点没有 agent，不参与在线更新。"} /> : <div className="table-scroll" role="region" aria-label="节点更新" tabIndex={0}><table className="nodes">
      <thead><tr><th><label className="inline"><MixedCheckbox label="选择全部可更新节点" checked={chosen.length === 0 ? false : chosen.length === updatable.length ? true : "mixed"}
        disabled={updatable.length === 0 || busy} onChange={() => setSelected(new Set(chosen.length === updatable.length ? [] : updatable))} />全选</label></th><th>节点</th><th>当前版本</th><th>来源</th><th>更新状态</th><th><span className="sr-only">操作</span></th></tr></thead>
      <tbody>{rows.map((node) => {
        const status = targets.get(node.id);
        return <tr key={String(node.id)}><td data-label="全选"><input type="checkbox" aria-label={`选择 ${node.name}（#${node.id}）`} checked={selected.has(node.id)} disabled={!eligible(status, nodeTarget) || busy} onChange={(event) => setSelected((old) => {
          const next = new Set(old); if (event.target.checked) next.add(node.id); else next.delete(node.id); return next;
        })} /></td>
        <td data-label="节点">{node.name}<small className="muted"> #{String(node.id)}</small></td>
        {/* 格子里的数字是更新状态的版本，不是 facts。徽章仍走 laggingNode，所以已出租不标；把状态版本放进 facts 是因为谓词只读那一个字段，徽章必须跟格子里的数字是同一个版本。 */}
        <td data-label="当前版本"><code>{status?.version || "未知"}</code>{laggingNode({ rented: node.rented, facts: { agentVersion: status?.version } }, nodeTarget || undefined) && <span className="badge-attention">低于 {nodeTarget}</span>}</td>
        <td data-label="来源">{sourceLabels[status?.source ?? ""] ?? status?.source ?? "—"}</td>
        <td data-label="更新状态"><Progress status={status} target={nodeTarget} showResult={resultTargets.has(node.id)} /></td><td data-label="操作">
          {status?.task?.state === "queued" && <button type="button" disabled={cancel.isPending || busy} onClick={() => cancel.mutate({ nodeId: node.id, id: status.task!.id })}>取消排队</button>}
        </td></tr>;
      })}</tbody>
    </table></div>}
    {progress && <Modal title="提交更新任务" opener={progress.opener} onClose={() => setProgress(null)}>
      <div className="modal-body">
        <p role="status" className="update-submit-status">{submitting ? `正在提交更新任务：${submitting.done}/${submitting.total}` : `已提交 ${progress.ids.length} 个目标，最终结果以状态回读为准。`}</p>
        <ul className="update-results" aria-label="提交结果" aria-live="polite" style={{ "--rows": progress.ids.length } as CSSProperties}>{progress.ids.map((id, i) => results[i] === undefined
          ? <li key={String(id)} className="muted">{nameOf(id)}：等待提交…</li>
          : <li key={String(id)}>{results[i]}</li>)}</ul>
      </div>
      <div className="modal-footer"><button type="button" data-autofocus onClick={() => setProgress(null)}>关闭</button></div>
    </Modal>}
    {confirmation && <Modal title={confirmation.ids[0] === 0n ? "确认更新 Hub" : "确认更新节点"} opener={confirmation.opener} onClose={() => setConfirmation(null)}>
      <div className="modal-body"><p>将 {confirmation.ids.map(nameOf).join("、")} 更新到 <strong>{confirmation.version}</strong>。</p>
        <p className="muted">{confirmation.ids[0] === 0n ? "Hub 将短暂断连。更新器会在停服后备份数据库，启动验证失败时恢复程序和数据库。请等待重新连接后的任务结果。" : "更新会短暂中断节点上报。已经下发的任务无法取消；失败时由本机更新器恢复旧程序。"}</p>
        {confirmation.binding && <BindingNotice change={confirmation.binding} />}
      </div><div className="modal-footer"><button type="button" onClick={() => setConfirmation(null)}>返回</button><button type="button" className="primary" onClick={() => { void execute(); }}>确认更新</button></div>
    </Modal>}
  </section>;
}
