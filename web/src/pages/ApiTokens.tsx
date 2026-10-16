import { createConnectQueryKey, useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useState } from "react";
import { errorText } from "../api/auth";
import { errorBanner, queryGate } from "../api/queryGate";
import { useLatestError } from "../api/useLatestError";
import { Drawer } from "../components/Modal";
import { PageHeader } from "../components/PageHeader";
import { RowMenu } from "../components/RowMenu";
import { Secret } from "../components/Secret";
import { UserscriptButton } from "../components/UserscriptButton";
import { Picks } from "../components/Picks";
import { AdminService, TokenPermission, type ApiToken, type CreateApiTokenResponse } from "../gen/heron/v1/admin_pb";
import { useNodeList } from "../api/useNodeList";
import { withId } from "../lib/ids";
import { dateTime, day } from "../lib/format";
import { EmptyState } from "../components/EmptyState";
import { downloadText } from "../lib/download";
import { EXPIRY_PRESETS, expiryText, resolveExpiry, type ExpiryChoice } from "../lib/tokenExpiry";

const permissionChoices = [
  [TokenPermission.CONFIGURE, "监控配置"],
  [TokenPermission.CREATE, "创建节点"],
  [TokenPermission.REGISTER, "注册入口"],
  [TokenPermission.ROTATE, "轮换节点凭据"],
  [TokenPermission.DELETE, "删除节点及历史"],
  [TokenPermission.UPDATE, "官方节点更新"],
] as const;

function Operations({ ownerId }: { ownerId: bigint }) {
  const list = useQuery(AdminService.method.listOperations, { ownerId, limit: 100 });
  const gate = queryGate(list);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  return <section className="card" aria-label="操作记录">
    {gate.banner}
    <h2>最近操作</h2>
    {gate.data.operations.length === 0 && <p className="muted">暂无已提交操作。</p>}
    {gate.data.operations.map((o) => <details key={o.id}>
      <summary>{o.action} #{String(o.resourceId)} · {dateTime(o.committedAt)}</summary>
      <p>请求 ID：<code>{o.requestId}</code></p>
      <p>操作 ID：<code>{o.id}</code></p>
      <p>修改前</p><pre style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>{o.beforeJson || "详细记录已过保留期"}</pre>
      <p>修改后</p><pre style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>{o.afterJson || "详细记录已过保留期"}</pre>
    </details>)}
  </section>;
}

// 技能文件的内容以 hub 下发的为准：与 hub 同版本，面板不另存一份。
export function ApiTokens() {
  const qc = useQueryClient();
  const [drawer, setDrawer] = useState<{ kind: "create"; opener: HTMLElement } | { kind: "edit"; token: ApiToken; opener: HTMLElement } | null>(null);
  const [auditOwner, setAuditOwner] = useState<bigint | null>(null);
  const { error, mutationOptions } = useLatestError();
  const list = useQuery(AdminService.method.listApiTokens, {});
  const refresh = () => qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listApiTokens, cardinality: "finite" }) });
  const create = useMutation(AdminService.method.createApiToken, {
    ...mutationOptions,
    onSuccess: refresh,
  });
  const update = useMutation(AdminService.method.updateApiToken, {
    ...mutationOptions,
    onSuccess: refresh,
  });
  const remove = useMutation(AdminService.method.deleteApiToken, {
    ...mutationOptions,
    onSuccess: refresh,
  });
  const reference = useMutation(AdminService.method.getApiReference, { ...mutationOptions, onSuccess: (r) => downloadText("SKILL.md", r.guide) });
  const gate = queryGate(list);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  // 自取回列表以来经过的时间按浏览器时钟的两次读数之差计，与浏览器和 hub 之间的偏差无关。
  const hubNow = () => gate.data.now + BigInt(Math.max(0, Math.floor((Date.now() - list.dataUpdatedAt) / 1000)));
  return (
    <section>
      {gate.banner}
      <PageHeader title="API token" description={<>供 agent 与脚本使用的预授权凭据：以 <code>Authorization: Bearer &lt;token&gt;</code> 调用 API。默认只读，勾选的操作可在授权节点范围内自主执行，无需逐次审批。</>} actions={
        <button type="button" className="primary-button" onClick={(e) => { create.reset(); setDrawer({ kind: "create", opener: e.currentTarget }); }}>新建 API token</button>
      } />
      {drawer && <ApiTokenDrawer key={drawer.kind === "edit" ? `edit-${drawer.token.id}` : "create"} opener={drawer.opener}
        initial={drawer.kind === "edit" ? drawer.token : undefined} hubNow={hubNow}
        pending={drawer.kind === "edit" ? update.isPending : create.isPending} error={drawer.kind === "edit" ? update.error : create.error}
        onClose={() => { setDrawer(null); create.reset(); update.reset(); }}
        onCreate={(fields) => create.mutateAsync(fields)}
        onUpdate={async (fields) => {
          if (drawer.kind !== "edit") return;
          await update.mutateAsync({ id: drawer.token.id, ...fields });
          setDrawer(null);
        }} />}
      {!drawer && error != null && <p role="alert" className="error">{errorText(error)}</p>}
      {gate.data.tokens.length === 0 ? <EmptyState title="还没有 API token。">新建一个 token 交给 agent 或脚本；权限与可操作的节点在新建时勾选，之后可随时编辑或吊销。</EmptyState> : <div className="table-scroll" role="region" aria-label="API token 管理" tabIndex={0}>
        <table className="nodes">
          <thead><tr><th>名称</th><th>权限 / 范围</th><th>创建于</th><th>最后使用</th><th>有效期</th><th><span className="sr-only">操作</span></th></tr></thead>
          <tbody>
            {gate.data.tokens.map((t) => (
              <tr key={String(t.id)}>
                <td data-label="名称">{t.name}</td>
                <td data-label="权限 / 范围" className="wrap-text">{t.grant?.permissions.length ? t.grant.permissions.map((p) => permissionChoices.find(([v]) => v === p)?.[1] ?? "未知权限").join("、") : "只读"}<br />
                  {t.grant?.allNodes !== false ? "全站" : t.grant.nodeIds.length ? t.grant.nodeIds.map((id) => `#${id}`).join("、") : "无现有节点"}</td>
                <td data-label="创建于" className="muted">{day(t.createdAt)}</td>
                <td data-label="最后使用" className="muted">{t.lastUsedAt == null ? "从未使用" : dateTime(t.lastUsedAt)}</td>
                <td data-label="有效期" className="muted">{expiryText(t.expiresAt, gate.data.now)}</td>
                <td data-label="操作">
                  <RowMenu label={withId(t.name, t.id)} items={[
                    { label: "编辑", disabled: update.isPending, onSelect: (trigger) => { update.reset(); setDrawer({ kind: "edit", token: t, opener: trigger }); } },
                    { label: "查看操作记录", onSelect: () => setAuditOwner(t.id) },
                    { label: "吊销", danger: true, confirm: `确认吊销 ${withId(t.name, t.id)}`, note: "用它的请求立即失效",
                      disabled: remove.isPending, onSelect: () => remove.mutate({ id: t.id }) },
                  ]} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>}
      {auditOwner !== null && <Operations ownerId={auditOwner} />}
      {/* 两种接入方式各占一行：说明在左，动作在右。它们是辅助材料，排在 token 列表之后。 */}
      <section className="card integrations" aria-label="接入方式">
        <h2>接入方式</h2>
        <div className="integration">
          <p>
            <strong>Agent 技能文件（SKILL.md）</strong>
            <span className="muted">写给 Claude Code 等 AI agent 的接口说明：接口地址与鉴权方式、schema 的取得、时间与分页约定，以及可直接运行的例子，与这个 hub 同版本。保存为 agent 的 skills 目录下的 heron-hub/SKILL.md（Claude Code 为 ~/.claude/skills/heron-hub/SKILL.md）。</span>
            <span className="muted">token 不写进 SKILL.md，由环境变量提供：在上面新建一个 API token（默认只读，要让 agent 改配置时再勾选写入权限），复制创建后只显示一次的 token，给 agent 设置 <code>HERON_HUB={window.location.origin}</code> 与 <code>HERON_TOKEN=</code>（这个 token）。Claude Code 写进 ~/.claude/settings.json 的 env 段：<code>{`{"env": {"HERON_HUB": "${window.location.origin}", "HERON_TOKEN": "heron_at_…"}}`}</code>；其他 agent 在启动它的 shell 里 export 这两个变量。</span>
          </p>
          <button type="button" onClick={() => reference.mutate({})} disabled={reference.isPending}>下载 SKILL.md</button>
        </div>
        <div className="integration">
          <p><strong>油猴脚本</strong><span className="muted">给运维自己用：粘贴进 Tampermonkey 等脚本管理器后，任意站点右下角出现悬浮按钮，在 IDC 页面看着价格与到期一键建节点，建好后直接给出安装命令。脚本经 <code>ExecuteChange</code> 写，token 需勾选「创建节点」。</span></p>
          <UserscriptButton />
        </div>
      </section>
    </section>
  );
}

type TokenFields = { name: string; grant: { permissions: TokenPermission[]; allNodes: boolean; nodeIds: bigint[] }; expiresAt?: bigint };

function ApiTokenDrawer({ opener, initial, hubNow, pending, error, onClose, onCreate, onUpdate }: {
  opener: HTMLElement; initial?: ApiToken; hubNow: () => bigint; pending: boolean; error: unknown; onClose: () => void;
  onCreate: (fields: TokenFields) => Promise<CreateApiTokenResponse>; onUpdate: (fields: TokenFields) => Promise<void>;
}) {
  const editing = initial !== undefined;
  const title = editing ? "编辑 API token" : "新建 API token";
  const [name, setName] = useState(initial?.name ?? "");
  const [permissions, setPermissions] = useState<TokenPermission[]>(initial?.grant?.permissions ?? []);
  // 与列表同一口径：没有 grant 即全站。
  const [allNodes, setAllNodes] = useState(initial?.grant?.allNodes !== false);
  const [selected, setSelected] = useState<Set<bigint>>(new Set(initial?.grant?.nodeIds ?? []));
  const [choice, setChoice] = useState<ExpiryChoice>(editing ? "keep" : "never");
  const [custom, setCustom] = useState("");
  const nodes = useNodeList({}, { enabled: !allNodes });
  // 明文和草稿只属于本次抽屉；卸载后重新打开不能恢复。
  const [secret, setSecret] = useState<{ label: string; value: string; canCreate: boolean } | null>(null);
  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (!e.currentTarget.checkValidity() || pending || (!allNodes && !nodes.data)) return;
    const expiresAt = resolveExpiry(choice, hubNow(), custom, initial?.expiresAt);
    if (expiresAt === null) return;
    const fields: TokenFields = { name: name.trim(), grant: { permissions, allNodes, nodeIds: allNodes ? [] : (nodes.data?.nodes ?? []).filter((n) => selected.has(n.id)).map((n) => n.id) }, expiresAt };
    try {
      if (editing) {
        await onUpdate(fields);
        return;
      }
      const r = await onCreate(fields);
      if (r.apiToken) setSecret({ label: `API token ${withId(r.apiToken.name, r.apiToken.id)}`, value: r.token,
        canCreate: permissions.includes(TokenPermission.CREATE) });
    } catch {
      // mutation 的错误由抽屉呈现，失败时保留草稿以便重试。
    }
  };
  return <Drawer title={title} opener={opener} busy={pending} onClose={onClose}>
    {secret ? <>
      <div className="modal-body">
        <Secret label={secret.label} value={secret.value} />
        {secret.canCreate && <p><UserscriptButton token={secret.value} label="复制油猴脚本（已填入此 token）" /></p>}
      </div>
      <footer className="modal-footer"><button type="button" disabled={pending} onClick={onClose}>完成</button></footer>
    </> : <form aria-label={title} onSubmit={(e) => void submit(e)}>
      <div className="modal-body">
        {error != null && <p role="alert" className="error">{errorText(error)}</p>}
        <label>名称<input required maxLength={64} value={name} onChange={(e) => setName(e.target.value)} /></label>
        <fieldset className="picks"><legend>允许的写操作</legend>
          {permissionChoices.map(([value, label]) => <label key={value}><input type="checkbox" checked={permissions.includes(value)} onChange={(e) => setPermissions((current) => e.target.checked ? [...current, value] : current.filter((p) => p !== value))} />{label}</label>)}
        </fieldset>
        <label>节点范围<select value={allNodes ? "all" : "selected"} onChange={(e) => setAllNodes(e.target.value === "all")}>
          <option value="all">全站（包含未来节点）</option><option value="selected">指定节点</option>
        </select></label>
        {!allNodes && <>
          {nodes.error && errorBanner(nodes.error)}
          <Picks legend="授权节点" items={nodes.data?.nodes ?? []} selected={selected} onChange={setSelected} />
          <p className="muted">未选择节点表示不能访问现有节点。此凭据创建或注册的节点自动纳入范围；标签不改变授权。</p>
        </>}
        <label>有效期<select value={choice} onChange={(e) => setChoice(e.target.value as ExpiryChoice)}>
          {editing && <option value="keep">保持当前（{expiryText(initial.expiresAt, hubNow())}）</option>}
          <option value="never">不过期</option>
          {EXPIRY_PRESETS.map(([value, label]) => <option key={value} value={value}>{label}</option>)}
          <option value="custom">自定义</option>
        </select></label>
        {choice === "custom" && <label>失效时间<input type="datetime-local" required value={custom} onChange={(e) => setCustom(e.target.value)} /></label>}
        <p className="muted">到期后用它的请求立即失效；之后在这里改有效期即可恢复，明文不变。</p>
        <p className="muted">写入支持预览、版本检查和安全重试。不能更新 Hub、执行远程命令、管理 API 凭据或修改通知渠道密钥。</p>
      </div>
      <footer className="modal-footer">
        <button type="button" disabled={pending} onClick={onClose}>取消</button>
        <button type="submit" className="primary-button" disabled={pending || (!allNodes && !nodes.data)}>{editing ? "保存" : "创建"}</button>
      </footer>
    </form>}
  </Drawer>;
}
