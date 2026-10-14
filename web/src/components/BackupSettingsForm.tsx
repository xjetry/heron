import { useMutation, useQuery } from "@connectrpc/connect-query";
import { type FormEvent, useState } from "react";
import { errorText } from "../api/auth";
import { errorBanner, queryGate } from "../api/queryGate";
import { SAVE_SETTINGS, useAdoptSavedSettings, useSettingsSaving } from "../api/saveSettings";
import { AdminService, type BackupSettings } from "../gen/heron/v1/admin_pb";
import { MAX_NOTIFY_CHANNELS } from "../lib/alerts";
import { liveIds } from "../lib/ids";
import { Picks } from "./Picks";

// hasSecret 只来自 hub 的读侧，表单不改它；secret 只写，不从读侧复制进草稿，成功保存后随草稿重建而清空。
type BackupDraft = {
  endpoint: string; bucket: string; region: string; accessKey: string; prefix: string;
  configIntervalS: number; metricsIntervalS: number; configKeep: number; metricsKeep: number;
  channelIds: Set<bigint>; secret?: string; hasSecret: boolean;
};

const backupDraft = (b?: BackupSettings): BackupDraft => ({
  endpoint: b?.endpoint ?? "", bucket: b?.bucket ?? "", region: b?.region || "auto", accessKey: b?.accessKey ?? "", prefix: b?.prefix ?? "",
  configIntervalS: b?.configIntervalS ?? 300, metricsIntervalS: b?.metricsIntervalS ?? 86400,
  configKeep: b?.configKeep ?? 48, metricsKeep: b?.metricsKeep ?? 14,
  channelIds: new Set(b?.notify?.channelIds ?? []), secret: undefined, hasSecret: b?.hasSecret ?? false,
});

// 备份设置单独一个表单，只提交 backup 这一组：UpdateSettings 按组判定，外观、总闸与国家查询两项缺席即不改。于是只改
// 备份既不会顺带保存外观表单的未保存改动，也不会把缓存里可能已过时的外观写回去；外观表单与查询表单不提交 backup，hub
// 对缺席的 backup 不改。保存与其余设置表单互斥（SAVE_SETTINGS）。
// 提交的 backup 恒带 notify（表单显示的就是完整的渠道选择，与当前渠道列表求交）；secret 留空即缺席，保留 hub 已存的值；
// hasSecret 不提交。渠道列表读到之前不渲染表单：拿空列表求交会把已选渠道当作显式空集合提交，等于关掉备份失败通知。
export function BackupSettingsForm({ current }: { current: BackupSettings | undefined }) {
  const adoptSaved = useAdoptSavedSettings();
  const channels = useQuery(AdminService.method.listNotifyChannels, {});
  const [draft, setDraft] = useState<BackupDraft | null>(null);
  const [saved, setSaved] = useState(false);
  const saving = useSettingsSaving();
  const update = useMutation(AdminService.method.updateSettings, {
    mutationKey: SAVE_SETTINGS,
    onSuccess: (r) => {
      setDraft(backupDraft(r.settings?.backup));
      setSaved(true);
      return adoptSaved(r.settings);
    },
  });
  const gate = queryGate(channels);
  if (!gate.ready) return <>{gate.loading ?? errorBanner(...gate.errors)}</>;
  const channelList = gate.data.channels;
  const form = draft ?? backupDraft(current);
  const edit = (patch: Partial<BackupDraft>) => {
    setDraft((d) => ({ ...(d ?? backupDraft(current)), ...patch }));
    setSaved(false);
    update.reset();
  };
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (!e.currentTarget.checkValidity() || saving) return;
    const { channelIds, hasSecret: _, ...rest } = form;
    update.mutate({ settings: { backup: { ...rest, notify: { channelIds: liveIds(channelIds, channelList) } } } });
  };
  return (
    <>
      {gate.banner}
      <form className="card" aria-label="备份到 S3" onSubmit={submit}>
        <fieldset className="bare" disabled={saving}>
          <label>Endpoint<input type="url" value={form.endpoint} maxLength={2048} placeholder="https://account.r2.cloudflarestorage.com" onChange={(e) => edit({ endpoint: e.target.value })} /></label>
          <div className="row">
            <label>Bucket<input value={form.bucket} maxLength={63} onChange={(e) => edit({ bucket: e.target.value })} /></label>
            <label>区域<input value={form.region} maxLength={64} placeholder="auto" onChange={(e) => edit({ region: e.target.value })} /></label>
          </div>
          <label>Access key<input autoComplete="off" value={form.accessKey} maxLength={128} onChange={(e) => edit({ accessKey: e.target.value })} /></label>
          <label>Secret<input type="password" autoComplete="new-password" value={form.secret ?? ""} maxLength={4096} placeholder="只写不读；留空保留已存值" aria-describedby="backup-secret-state" onChange={(e) => edit({ secret: e.target.value || undefined })} /></label>
          <p className="muted" id="backup-secret-state">{form.hasSecret ? "Secret 已保存" : "Secret 未设置"}</p>
          <label>对象键前缀<input value={form.prefix} maxLength={512} placeholder="例如 hub；不以 / 开头或结尾" onChange={(e) => edit({ prefix: e.target.value })} /></label>
          <div className="row">
            <label>配置周期（秒）<input type="number" required min={60} max={86400} step={1} value={form.configIntervalS} onChange={(e) => edit({ configIntervalS: Number(e.target.value) })} /></label>
            <label>指标周期（秒）<input type="number" required min={3600} max={604800} step={1} value={form.metricsIntervalS} onChange={(e) => edit({ metricsIntervalS: Number(e.target.value) })} /></label>
          </div>
          <div className="row">
            <label>配置保留份数<input type="number" required min={1} max={1000} step={1} value={form.configKeep} onChange={(e) => edit({ configKeep: Number(e.target.value) })} /></label>
            <label>指标保留份数<input type="number" required min={1} max={1000} step={1} value={form.metricsKeep} onChange={(e) => edit({ metricsKeep: Number(e.target.value) })} /></label>
          </div>
          <p className="muted">配置周期 60 至 86400 秒，指标周期 3600 至 604800 秒；保留份数均为 1 至 1000。回到默认值请填写 300、86400、48、14。</p>
          <Picks legend="备份通知渠道" max={MAX_NOTIFY_CHANNELS} items={channelList} selected={form.channelIds} onChange={(channelIds) => edit({ channelIds })} />
          {update.error != null && <p role="alert" className="error">{errorText(update.error)}</p>}
          {saved && <p role="status">已保存。</p>}
          <button type="submit">保存</button>
        </fieldset>
      </form>
    </>
  );
}
