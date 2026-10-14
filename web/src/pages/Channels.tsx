import { createConnectQueryKey, useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useRef, useState } from "react";
import { errorText } from "../api/auth";
import { errorBanner, queryGate } from "../api/queryGate";
import { SAVE_SETTINGS, useAdoptSavedSettings, useSettingsSaving } from "../api/saveSettings";
import { useLatestError } from "../api/useLatestError";
import { Drawer } from "../components/Modal";
import { PageHeader } from "../components/PageHeader";
import { RowMenu } from "../components/RowMenu";
import { Picks } from "../components/Picks";
import { AdminService, ChannelKind, type NotifyChannel, type Settings } from "../gen/heron/v1/admin_pb";
import { CHANNEL_KINDS, MAX_NOTIFY_CHANNELS, NOTIFY_LISTS, channelTarget, labelOf, methodOf, rateLabel } from "../lib/alerts";
import { withId } from "../lib/ids";
import { day } from "../lib/format";
import { EmptyState } from "../components/EmptyState";
import { TrafficReportSettings } from "./TrafficReportSettings";

const METHODS = ["POST", "PUT", "PATCH"] as const;
type HeaderRow = { id: number; name: string; value: string };
// rate 为空表示不给出节奏上限，由 hub 按种类取默认值（Telegram 20，Webhook 0 即不限）。
type Draft = {
  name: string; kind: ChannelKind; rate: string; botToken: string; chatId: string;
  url: string; method: string; headers: HeaderRow[]; removeHeaders: Set<string>; bodyTemplate: string;
};

const emptyDraft = (): Draft => ({
  name: "", kind: ChannelKind.TELEGRAM, rate: "", botToken: "", chatId: "",
  url: "", method: "POST", headers: [], removeHeaders: new Set(), bodyTemplate: "",
});
// hub 不回显 token、URL 与头值，编辑草稿里它们恒为空；同种类时留空提交由 hub 保留已存值，换种类必须重填。
const draftOf = (c: NotifyChannel): Draft => ({
  ...emptyDraft(), name: c.name, kind: c.kind, rate: c.ratePerMinute === undefined ? "" : String(c.ratePerMinute), chatId: c.telegram?.chatId ?? "",
  method: methodOf(c.webhook?.method), bodyTemplate: c.webhook?.bodyTemplate ?? "",
});

function toChannel(id: bigint, d: Draft) {
  const rate = d.rate.trim();
  const base = { id, name: d.name.trim(), kind: d.kind, ...(rate === "" ? {} : { ratePerMinute: Number(rate) }) };
  if (d.kind === ChannelKind.TELEGRAM) return { ...base, telegram: { botToken: d.botToken.trim(), chatId: d.chatId.trim() } };
  return { ...base, webhook: {
    url: d.url.trim(), method: d.method, bodyTemplate: d.bodyTemplate,
    headers: Object.fromEntries(d.headers.map((h) => [h.name.trim(), h.value])),
    removeHeaders: [...d.removeHeaders].sort(),
  } };
}

// 完全同名的两行在 map 里合并、后一行静默胜出，只有表单看得到这个丢失；
// 大小写不同的两行 hub 也会拒绝，这里一并拦下，两种重名在提交前得到同一条提示。
function duplicateHeader(rows: HeaderRow[]): string | undefined {
  const seen = new Set<string>();
  for (const r of rows) {
    const key = r.name.trim().toLowerCase();
    if (seen.has(key)) return r.name.trim();
    seen.add(key);
  }
  return undefined;
}

// 删除确认里逐个列表写明影响。设置没读到时不知道它在不在哪个列表里，照最坏的情况提醒，不省略。
function deleteNote(id: bigint, settings: Settings | undefined): string | undefined {
  if (settings === undefined) {
    const names = NOTIFY_LISTS.map((l) => l.name).join("或");
    return `通知设置未读到：它若是${names}的接收渠道，删除后不再发到它。`;
  }
  const notes = NOTIFY_LISTS.flatMap(({ name, ids }) => {
    const list = ids(settings);
    if (!list.includes(id)) return [];
    return [list.length === 1 ? `它是${name}唯一的接收渠道，删除后${name}关闭。` : `删除后${name}不再发到这个渠道。`];
  });
  return notes.length > 0 ? notes.join("") : undefined;
}

export function Channels() {
  const qc = useQueryClient();
  const [drawer, setDrawer] = useState<{ kind: "create"; opener: HTMLElement } | { kind: "edit"; channel: NotifyChannel; opener: HTMLElement } | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const { error, isLatest, mutationOptions } = useLatestError();
  // 任何新操作开始时，旧的成功提示与失败一并清除；在途请求的迟到结果由 isLatest 挡回，成功与失败同口径。
  const tracked = { ...mutationOptions, onMutate: () => { setNotice(null); return mutationOptions.onMutate(); } };
  const list = useQuery(AdminService.method.listNotifyChannels, {});
  const settings = useQuery(AdminService.method.getSettings, {});
  const current = settings.data?.settings;
  const refresh = () => qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listNotifyChannels, cardinality: "finite" }) });
  const create = useMutation(AdminService.method.saveNotifyChannel, { ...tracked, onSuccess: refresh });
  // update 共用一个 mutation observer，重叠的 mutate 只回调最后一次；保存在途时抽屉不可关闭。
  // 返回刷新 promise，编辑态在列表显示已保存值之后才关闭。
  const update = useMutation(AdminService.method.saveNotifyChannel, { ...tracked, onSuccess: refresh });
  const remove = useMutation(AdminService.method.deleteNotifyChannel, { ...tracked, onSuccess: async () => {
    await refresh();
    await qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.getSettings, cardinality: "finite" }) });
  } });
  const test = useMutation(AdminService.method.testNotifyChannel, tracked);
  const busy = create.isPending || update.isPending;
  const gate = queryGate(list);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  const channels = gate.data.channels;
  return (
    <section>
      <PageHeader title="通知渠道" actions={<button type="button" className="primary-button" disabled={busy} onClick={(event) => { create.reset(); setDrawer({ kind: "create", opener: event.currentTarget }); }}>新建通知渠道</button>} />
      {gate.banner}
      <LoginNotifications channels={channels} deleting={remove.isPending} />
      <TrafficReportSettings channels={channels} deleting={remove.isPending} />
      {drawer === null && error != null && <p role="alert" className="error">{errorText(error)}</p>}
      <p role="status">{notice ?? ""}</p>
      {channels.length === 0 ? <EmptyState title="还没有通知渠道。" /> : <div className="table-scroll" role="region" aria-label="通知渠道管理" tabIndex={0}>
        <table className="nodes">
          <thead><tr><th>名称</th><th>类型</th><th>目标</th><th>节奏上限</th><th>创建于</th><th><span className="sr-only">操作</span></th></tr></thead>
          <tbody>
            {channels.map((c) => {
              const label = withId(c.name, c.id);
              return <tr key={String(c.id)}>
                <td data-label="名称">{c.name}</td>
                <td data-label="类型">{labelOf(CHANNEL_KINDS, c.kind)}</td>
                <td data-label="目标"><span className="clip-text clip-wide" title={channelTarget(c)}>{channelTarget(c)}</span></td>
                <td data-label="节奏上限">{rateLabel(c)}</td>
                <td data-label="创建于" className="muted">{day(c.createdAt)}</td>
                <td data-column="actions"><RowMenu label={label} items={[
                  { label: "编辑", disabled: busy, onSelect: (trigger) => { update.reset(); setDrawer({ kind: "edit", channel: c, opener: trigger }); } },
                  { label: "发送测试", disabled: test.isPending, onSelect: () => test.mutate({ id: c.id }, { onSuccess: (_r, _v, op) => { if (isLatest(op)) setNotice(`已向 ${c.name} 发送测试消息`); } }) },
                  { label: "删除", danger: true, confirm: `确认删除 ${label}`, note: deleteNote(c.id, current), disabled: busy || remove.isPending, onSelect: () => remove.mutate({ id: c.id }) },
                ]} /></td>
              </tr>;
            })}
          </tbody>
        </table>
      </div>}
      {drawer?.kind === "create" && <ChannelDrawer title="新建通知渠道" submitLabel="创建" initial={emptyDraft()} pending={create.isPending} error={create.error} opener={drawer.opener} onClose={() => setDrawer(null)}
        onSubmit={(d) => create.mutate({ channel: toChannel(0n, d) }, { onSuccess: () => setDrawer(null) })} />}
      {drawer?.kind === "edit" && <ChannelDrawer key={String(drawer.channel.id)} title={`编辑 ${withId(drawer.channel.name, drawer.channel.id)}`} submitLabel="保存" initial={draftOf(drawer.channel)} original={drawer.channel} pending={update.isPending} error={update.error} opener={drawer.opener} onClose={() => setDrawer(null)}
        onSubmit={(d) => update.mutate({ channel: toChannel(drawer.channel.id, d) }, { onSuccess: () => setDrawer(null) })} />}
    </section>
  );
}

// 登录通知只提交 login_notify 这一组：UpdateSettings 按组判定，外观、总闸、国家查询与备份缺席即不改。保存成功后与
// 其余设置表单一样经 useAdoptSavedSettings 把回显写进 getSettings 的缓存。
function LoginNotifications({ channels, deleting }: { channels: NotifyChannel[]; deleting: boolean }) {
  const adoptSaved = useAdoptSavedSettings();
  const settings = useQuery(AdminService.method.getSettings, {});
  const [draft, setDraft] = useState<bigint[] | null>(null);
  const [saved, setSaved] = useState(false);
  // 与其它设置表单互斥（SAVE_SETTINGS）：saving 覆盖任一设置表单在途，包括这里自己的保存。
  const saving = useSettingsSaving();
  const update = useMutation(AdminService.method.updateSettings, { mutationKey: SAVE_SETTINGS, onSuccess: (r) => {
    setDraft(r.settings?.loginNotify?.channelIds ?? []);
    setSaved(true);
    return adoptSaved(r.settings);
  } });
  const gate = queryGate(settings);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  // 渠道删除后服务端会摘除引用；草稿也只能提交当前列表里仍存在的渠道。
  const selected = channels.filter((c) => (draft ?? gate.data.settings?.loginNotify?.channelIds ?? []).includes(c.id)).map((c) => c.id);
  const pending = saving || deleting;
  const pick = (next: Set<bigint>) => {
    setDraft([...next]);
    setSaved(false);
    update.reset();
  };
  return (
    <form className="card" aria-label="登录通知" onSubmit={(e) => {
      e.preventDefault();
      if (!pending) update.mutate({ settings: { loginNotify: { channelIds: selected } } });
    }}>
      <h2>登录通知</h2>
      <p className="muted">密码登录成功或登录失败达到锁定阈值时通知。未选择渠道即关闭；API token 使用不通知。来源地址以 Hub 观察为准，未配置可信代理时显示代理地址。</p>
      {gate.banner}
      <fieldset className="bare" disabled={pending}>
        {/* hub 按原始条数最多收 MAX_NOTIFY_CHANNELS 个；Picks 选满后禁用未选项，取消一个才能换选，不等提交被拒。 */}
        <Picks legend="接收渠道" max={MAX_NOTIFY_CHANNELS} items={channels} selected={new Set(selected)} onChange={pick} />
        <button type="submit">保存登录通知</button>
      </fieldset>
      {update.error != null && <p role="alert" className="error">{errorText(update.error)}</p>}
      {saved && <p role="status">登录通知已保存。</p>}
    </form>
  );
}

function ChannelDrawer({ title, submitLabel, initial, original, pending, error, opener, onClose, onSubmit }: {
  title: string; submitLabel: "创建" | "保存"; initial: Draft; original?: NotifyChannel; pending: boolean; error: unknown; opener: HTMLElement; onClose: () => void; onSubmit: (d: Draft) => void;
}) {
  // initial 只在挂载时读取；编辑期间的列表刷新不覆盖草稿。
  const [draft, setDraft] = useState(initial);
  const [problem, setProblem] = useState<string | null>(null);
  // 行号随删除前移，key 必须跟随行本身而不是位置，焦点才不会落到错位的节点上。
  const nextHeaderId = useRef(0);
  // hub 只在种类未变时用已存凭据补全空值；新建或换种类时凭据必须重新填写。
  const keeps = original !== undefined && original.kind === draft.kind;
  const saved = keeps ? original?.webhook?.headerNames ?? [] : [];
  const set = (patch: Partial<Draft>) => setDraft({ ...draft, ...patch });
  const setHeader = (i: number, patch: Partial<HeaderRow>) => set({ headers: draft.headers.map((h, j) => (j === i ? { ...h, ...patch } : h)) });
  const addHeader = () => {
    nextHeaderId.current += 1;
    set({ headers: [...draft.headers, { id: nextHeaderId.current, name: "", value: "" }] });
  };
  const toggleRemove = (name: string) => {
    const next = new Set(draft.removeHeaders);
    if (next.has(name)) next.delete(name); else next.add(name);
    set({ removeHeaders: next });
  };
  const handle = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (pending || !e.currentTarget.checkValidity()) return;
    const dup = draft.kind === ChannelKind.WEBHOOK ? duplicateHeader(draft.headers) : undefined;
    setProblem(dup === undefined ? null : `请求头 ${dup} 填写了两次`);
    if (dup === undefined) onSubmit(draft);
  };
  return (
    <Drawer title={title} busy={pending} opener={opener} onClose={onClose}>
      <form aria-label={title} onSubmit={handle}>
        <div className="modal-body">
          {errorBanner(error)}
          <fieldset className="bare" disabled={pending}>
            <div className="row">
              <label>名称<input data-autofocus required value={draft.name} onChange={(e) => set({ name: e.target.value })} /></label>
              <div className="segmented" role="radiogroup" aria-label="类型">
                {CHANNEL_KINDS.map(({ value, label }) => <label key={value}><input type="radio" name="channel-kind" aria-label={label} checked={draft.kind === value} onChange={() => set({ kind: value })} /><span>{label}</span></label>)}
              </div>
            </div>
            {draft.kind === ChannelKind.TELEGRAM ? (
              <div className="row">
                <label>Bot token<input type="password" autoComplete="new-password" required={!keeps} placeholder={keeps ? "已保存，留空保持不变" : ""}
                  value={draft.botToken} onChange={(e) => set({ botToken: e.target.value })} /></label>
                <label>Chat ID<input required value={draft.chatId} onChange={(e) => set({ chatId: e.target.value })} /></label>
              </div>
            ) : (
              <>
                <div className="row">
                  <label>URL<input type="url" autoComplete="off" required={!keeps}
                    placeholder={keeps ? `已保存 ${original?.webhook?.urlHost ?? ""}，留空保持不变` : "https://"}
                    value={draft.url} onChange={(e) => set({ url: e.target.value })} /></label>
                  <label>方法
                    <select value={draft.method} onChange={(e) => set({ method: e.target.value })}>
                      {METHODS.map((m) => <option key={m} value={m}>{m}</option>)}
                    </select>
                  </label>
                </div>
                {saved.length > 0 && (
                  <fieldset className="picks">
                    <legend>已保存的请求头（值不回显）</legend>
                    {saved.map((name) => (
                      <label key={name}><input type="checkbox" checked={draft.removeHeaders.has(name)} onChange={() => toggleRemove(name)} />删除 {name}</label>
                    ))}
                  </fieldset>
              )}
              {draft.headers.map((h, i) => (
                <div className="row" role="group" aria-label={`请求头 ${i + 1}`} key={h.id}>
                  <label>请求头名<input required value={h.name} onChange={(e) => setHeader(i, { name: e.target.value })} /></label>
                  <label>请求头值<input type="password" autoComplete="new-password" value={h.value} onChange={(e) => setHeader(i, { value: e.target.value })} /></label>
                  <button type="button" className="link" aria-label={`移除请求头 ${i + 1}`} onClick={() => set({ headers: draft.headers.filter((_, j) => j !== i) })}>移除</button>
                </div>
              ))}
              <button type="button" className="link" onClick={addHeader}>添加请求头</button>
              <label>请求体模板<textarea value={draft.bodyTemplate} placeholder="留空使用默认 JSON 模板" onChange={(e) => set({ bodyTemplate: e.target.value })} /></label>
            </>
          )}
          <div className="row">
            <label>每分钟上限<input type="number" min={0} max={4294967295} step={1} inputMode="numeric" value={draft.rate}
              placeholder={`留空取默认 ${draft.kind === ChannelKind.TELEGRAM ? "20" : "0（不限）"}`}
              onChange={(e) => set({ rate: e.target.value })} /></label>
            <span className="muted">每分钟至多发出的消息数，含重试；0 表示不限，超出的排队到下一分钟发出</span>
          </div>
          {problem && <p role="alert" className="error">{problem}</p>}
          </fieldset>
        </div>
        <footer className="modal-footer"><button type="button" disabled={pending} onClick={onClose}>取消</button><button type="submit" className="primary-button" disabled={pending}>{submitLabel}</button></footer>
      </form>
    </Drawer>
  );
}
