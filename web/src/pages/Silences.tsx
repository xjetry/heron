import { createConnectQueryKey, useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { errorText } from "../api/auth";
import { errorBanner, queryGateAll } from "../api/queryGate";
import { useLatestError } from "../api/useLatestError";
import { PageHeader } from "../components/PageHeader";
import { RowMenu } from "../components/RowMenu";
import { AdminService, SilenceKind, type Silence } from "../gen/heron/v1/admin_pb";
import { useNodeList } from "../api/useNodeList";
import { withId } from "../lib/ids";
import { dateTime } from "../lib/format";
import { EmptyState } from "../components/EmptyState";
import { emptySilenceDraft, SilenceDrawer, silenceDraftOf, toSilence, useSilenceClock } from "../components/SilenceDrawer";

export function Silences() {
  const qc = useQueryClient();
  const [drawer, setDrawer] = useState<{ kind: "create"; opener: HTMLElement } | { kind: "edit"; silence: Silence; opener: HTMLElement } | null>(null);
  const { error, mutationOptions } = useLatestError();
  const nodes = useNodeList({});
  const rules = useQuery(AdminService.method.listAlertRules, {}, { refetchInterval: 10_000 });
  const { list: silences, hubNow } = useSilenceClock();
  const refresh = () => qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listSilences, cardinality: "finite" }) });
  const create = useMutation(AdminService.method.saveSilence, { ...mutationOptions, onSuccess: refresh });
  const update = useMutation(AdminService.method.saveSilence, { ...mutationOptions, onSuccess: refresh });
  const remove = useMutation(AdminService.method.deleteSilence, { ...mutationOptions, onSuccess: refresh });
  const busy = create.isPending || update.isPending;
  const gate = queryGateAll(nodes, silences, rules);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  const [nodesData, silencesData, rulesData] = gate.data;
  const nodeList = nodesData.nodes;
  const ruleList = rulesData.rules;
  const nodeName = (id: bigint) => nodeList.find((n) => n.id === id)?.name ?? `节点 #${id}`;
  const ruleName = (id: bigint) => ruleList.find((r) => r.id === id)?.name ?? `规则 #${id}`;
  return (
    <section>
      <PageHeader title="维护静默" description="覆盖内的规则与节点在窗口内产生的告警事件照常记录但不投递；恢复是否投递只看配对的触发是否投递过。到期提醒与系统事件不受静默影响。节点的维护开关在节点编辑里设置。"
        actions={<button type="button" className="primary-button" disabled={busy} onClick={(event) => { create.reset(); setDrawer({ kind: "create", opener: event.currentTarget }); }}>新建维护静默</button>} />
      {gate.banner}
      {drawer === null && error != null && <p role="alert" className="error">{errorText(error)}</p>}
      {silencesData.silences.length === 0 ? <EmptyState title="还没有维护静默。" /> : <div className="table-scroll" role="region" aria-label="维护静默管理" tabIndex={0}>
        <table className="nodes">
          <thead><tr><th>名称</th><th>窗口</th><th>规则</th><th>作用域</th><th>状态</th><th><span className="sr-only">操作</span></th></tr></thead>
          <tbody>
            {silencesData.silences.map((entry) => {
              const s = entry.silence;
              if (!s) return null;
              const label = withId(s.name, s.id);
              return <tr key={String(s.id)}>
                <td data-label="名称">{s.name}{s.reason && <p className="node-subtext node-note clip-text" title={s.reason}>{s.reason}</p>}</td>
                <td data-label="窗口">{windowText(s)}</td>
                <td data-label="规则" className="wrap-text">{s.allRules ? "全部规则" : s.ruleIds.length ? s.ruleIds.map(ruleName).join("、") : <span className="muted">规则已全部删除，不再生效</span>}</td>
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
      {drawer?.kind === "create" && <SilenceDrawer title="新建维护静默" submitLabel="创建" nodes={nodeList} rules={ruleList} hubNow={hubNow} initial={emptySilenceDraft()} pending={create.isPending} error={create.error} opener={drawer.opener} onClose={() => setDrawer(null)}
        onSubmit={(d) => create.mutate({ silence: toSilence(0n, d, nodeList, ruleList) }, { onSuccess: () => setDrawer(null) })} />}
      {drawer?.kind === "edit" && <SilenceDrawer key={String(drawer.silence.id)} title={`编辑 ${withId(drawer.silence.name, drawer.silence.id)}`} submitLabel="保存" nodes={nodeList} rules={ruleList} hubNow={hubNow} initial={silenceDraftOf(drawer.silence)} pending={update.isPending} error={update.error} opener={drawer.opener} onClose={() => setDrawer(null)}
        onSubmit={(d) => update.mutate({ silence: toSilence(drawer.silence.id, d, nodeList, ruleList) }, { onSuccess: () => setDrawer(null) })} />}
    </section>
  );
}

function windowText(s: Silence): string {
  if (s.kind === SilenceKind.DAILY) return `每日 ${s.startHhmm}–${s.endHhmm}（hub 时区）`;
  return `${dateTime(s.fromAt)} – ${dateTime(s.untilAt)}`;
}
