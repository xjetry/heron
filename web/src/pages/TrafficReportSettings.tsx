import { useMutation, useQuery } from "@connectrpc/connect-query";
import { type FormEvent, useState } from "react";
import { errorText } from "../api/auth";
import { errorBanner, queryGate } from "../api/queryGate";
import { SAVE_SETTINGS, useAdoptSavedSettings, useSettingsSaving } from "../api/saveSettings";
import { Picks } from "../components/Picks";
import { AdminService, type NotifyChannel, type TrafficReport } from "../gen/heron/v1/admin_pb";
import { MAX_NOTIFY_CHANNELS } from "../lib/alerts";

// hub 的 store.TrafficReportMaxHour：投递时刻是 hub 时区的整点 0–23。
const HOURS = Array.from({ length: 24 }, (_, h) => h);
const CADENCES = [
  { key: "daily", label: "日报（每天）" },
  { key: "weekly", label: "周报（每周一）" },
  { key: "monthly", label: "月报（每月 1 日）" },
] as const;

type Draft = { enabled: boolean; daily: boolean; weekly: boolean; monthly: boolean; hour: number; channelIds: bigint[] };
const toDraft = (r: TrafficReport | undefined): Draft => ({
  enabled: r?.enabled ?? false, daily: r?.daily ?? false, weekly: r?.weekly ?? false, monthly: r?.monthly ?? false,
  hour: r?.hour ?? 0, channelIds: [...(r?.channelIds ?? [])],
});

// 流量报告只提交 traffic_report 这一组，给出即整组替换（四个开关、时刻与渠道一起）：UpdateSettings 按组判定，外观、
// 总闸、国家查询、备份、登录通知与心跳缺席即不改。保存成功后与其余设置表单一样经 useAdoptSavedSettings 把回显写进
// getSettings 的缓存。启用而一种周期都没选时 hub 会拒绝，这里在提交前就拦下并说明。
export function TrafficReportSettings({ channels, deleting }: { channels: NotifyChannel[]; deleting: boolean }) {
  const adoptSaved = useAdoptSavedSettings();
  const settings = useQuery(AdminService.method.getSettings, {});
  const [draft, setDraft] = useState<Draft | null>(null);
  const [saved, setSaved] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  // 与其它设置表单互斥（SAVE_SETTINGS）：saving 覆盖任一设置表单在途，包括这里自己的保存。
  const saving = useSettingsSaving();
  const update = useMutation(AdminService.method.updateSettings, { mutationKey: SAVE_SETTINGS, onSuccess: (r) => {
    setDraft(toDraft(r.settings?.trafficReport));
    setSaved(true);
    return adoptSaved(r.settings);
  } });
  const gate = queryGate(settings);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  const form = draft ?? toDraft(gate.data.settings?.trafficReport);
  // 渠道删除后服务端会摘除引用；草稿也只能提交当前列表里仍存在的渠道。
  const selected = channels.filter((c) => form.channelIds.includes(c.id)).map((c) => c.id);
  const pending = saving || deleting;
  const edit = (patch: Partial<Draft>) => {
    setDraft({ ...form, ...patch });
    setSaved(false);
    setProblem(null);
    update.reset();
  };
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (pending) return;
    if (form.enabled && !form.daily && !form.weekly && !form.monthly) {
      setProblem("启用时至少选择一种周期");
      return;
    }
    update.mutate({ settings: { trafficReport: { enabled: form.enabled, daily: form.daily, weekly: form.weekly, monthly: form.monthly, hour: form.hour, channelIds: selected } } });
  };
  return (
    <form className="card" aria-label="流量报告" onSubmit={submit}>
      <h2>流量报告</h2>
      <p className="muted">按 Hub 的时区（--timezone）在所选整点之后一分钟内，把全部节点本计费周期的已提交用量、配额占比与距重置天数汇成一条消息发出；超过 20 台时只列占比最高的 20 台。首次启用时立即补发最近一期，Hub 停机错过的只补发最近一期。</p>
      {gate.banner}
      <fieldset className="bare" disabled={pending}>
        <label><input type="checkbox" checked={form.enabled} onChange={(e) => edit({ enabled: e.target.checked })} />启用流量报告</label>
        <fieldset className="picks">
          <legend>周期</legend>
          {CADENCES.map(({ key, label }) => (
            <label key={key}><input type="checkbox" checked={form[key]} onChange={(e) => edit({ [key]: e.target.checked })} />{label}</label>
          ))}
        </fieldset>
        <label>投递时刻
          <select value={form.hour} onChange={(e) => edit({ hour: Number(e.target.value) })}>
            {HOURS.map((h) => <option key={h} value={h}>{String(h).padStart(2, "0")}:00</option>)}
          </select>
        </label>
        {/* hub 按原始条数最多收 MAX_NOTIFY_CHANNELS 个；Picks 选满后禁用未选项，取消一个才能换选，不等提交被拒。 */}
        <Picks legend="接收渠道" max={MAX_NOTIFY_CHANNELS} items={channels} selected={new Set(selected)} onChange={(next) => edit({ channelIds: [...next] })} />
        <button type="submit">保存流量报告</button>
      </fieldset>
      {problem && <p role="alert" className="error">{problem}</p>}
      {update.error != null && <p role="alert" className="error">{errorText(update.error)}</p>}
      {saved && <p role="status">流量报告已保存。</p>}
    </form>
  );
}
