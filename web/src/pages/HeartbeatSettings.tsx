import { useMutation, useQuery } from "@connectrpc/connect-query";
import { type FormEvent, useState } from "react";
import { errorText } from "../api/auth";
import { errorBanner, queryGate } from "../api/queryGate";
import { SAVE_SETTINGS, useAdoptSavedSettings, useSettingsSaving } from "../api/saveSettings";
import { AdminService, HeartbeatMethod, type Heartbeat } from "../gen/heron/v1/admin_pb";
import { dateTime } from "../lib/format";

// 心跳地址是只写设置（ping 地址本身就是密钥），hub 只回 has_url 与 url_host。草稿里的 url 因此总从空开始：给出这一组
// 就是整体替换，留空保存即停用，要改间隔又保留地址必须重新填写。已配置时"保存"在地址为空时禁用，停用另有按钮，避免
// 改间隔时误清地址。
type Draft = { url: string; intervalS: number; method: HeartbeatMethod };
const toDraft = (h: Heartbeat | undefined): Draft => ({
  url: "",
  intervalS: h && h.intervalS >= 60 && h.intervalS <= 3600 ? h.intervalS : 60,
  method: h && h.method !== HeartbeatMethod.UNSPECIFIED ? h.method : HeartbeatMethod.POST,
});

export function HeartbeatSettingsForm({ current }: { current: Heartbeat | undefined }) {
  const adoptSaved = useAdoptSavedSettings();
  const [draft, setDraft] = useState<Draft | null>(null);
  const [saved, setSaved] = useState(false);
  const saving = useSettingsSaving();
  const update = useMutation(AdminService.method.updateSettings, {
    mutationKey: SAVE_SETTINGS,
    onSuccess: (r) => {
      setDraft(toDraft(r.settings?.heartbeat));
      setSaved(true);
      return adoptSaved(r.settings);
    },
  });
  const form = draft ?? toDraft(current);
  const configured = current?.hasUrl === true;
  const edit = (patch: Partial<Draft>) => {
    setDraft((d) => ({ ...(d ?? toDraft(current)), ...patch }));
    setSaved(false);
    update.reset();
  };
  // 给出这一组即整体替换，所以改动任何一项都要带上地址；配置过时地址为空会清掉它，改间隔时不许这样误清。
  const send = (url: string) => {
    if (saving) return;
    update.mutate({ settings: { heartbeat: { url, intervalS: form.intervalS, method: form.method } } });
  };
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (configured && form.url === "") return;
    send(form.url);
  };
  return (
    <>
      <form className="card" aria-label="心跳外推" onSubmit={submit}>
        <fieldset className="bare" disabled={saving}>
          <label>
            地址
            <input
              value={form.url}
              type="url"
              placeholder={configured ? `已配置（主机 ${current?.urlHost || "?"}），留空并保存即停用；要保留请重新填写` : "https://hc-ping.com/… 或监控服务的 push 地址"}
              onChange={(e) => edit({ url: e.target.value })}
              spellCheck={false}
            />
          </label>
          <p className="muted">绝对 http(s) 地址，不超过 2048 字节。只写不回显：保存后这里不再显示原文，只显示主机名。</p>
          <div className="row">
            <label>
              间隔（秒）
              <input
                type="number"
                min={60}
                max={3600}
                step={1}
                value={form.intervalS}
                onChange={(e) => edit({ intervalS: Math.trunc(Number(e.target.value)) || 0 })}
              />
            </label>
            <label>
              方法
              <select value={form.method} onChange={(e) => edit({ method: Number(e.target.value) })}>
                <option value={HeartbeatMethod.GET}>GET</option>
                <option value={HeartbeatMethod.POST}>POST（带计数）</option>
                <option value={HeartbeatMethod.HEAD}>HEAD</option>
              </select>
            </label>
          </div>
          <p className="muted">60–3600 秒，0 不是取默认。方法 POST 会带上节点总数、在线、离线、维护、已出租与告警中的计数；GET 与 HEAD 不带正文。</p>
          {update.error != null && <p role="alert" className="error">{errorText(update.error)}</p>}
          {saved && <p role="status">已保存。</p>}
          <button type="submit" disabled={saving || (configured && form.url === "")}>保存</button>
          {configured && (
            <button type="button" className="link" onClick={() => send("")} disabled={saving}>停用心跳</button>
          )}
        </fieldset>
      </form>
    </>
  );
}

const at = (unix: bigint) => dateTime(unix);

const failureAdvice: Record<string, string> = {
  transport: "连不上目标地址，检查地址、网络与对方服务是否可达",
  http_status: "对方返回了非 2xx，检查推送地址是否正确、是否需要换一个 token",
  request: "地址不是可用的绝对 http(s) 地址，请重新填写",
};

export function HeartbeatStatus() {
  const status = useQuery(AdminService.method.getHeartbeatStatus, {}, { refetchInterval: 5000 });
  const gate = queryGate(status);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  const s = gate.data;
  return (
    <section aria-label="心跳状态">
      {gate.banner}
      <h3>心跳状态</h3>
      <p>{s.enabled ? "心跳外推已启用" : "心跳外推未启用"}</p>
      <p>上次成功：{s.lastSuccessAt === 0n ? <span className="muted">从未成功</span> : at(s.lastSuccessAt)}</p>
      <p>
        上次失败：{s.lastFailureAt === 0n ? <span className="muted">从未失败</span> :
          <span className="error">{s.failureCategory}{s.failureHttpStatus ? `（HTTP ${s.failureHttpStatus}）` : ""}，{at(s.lastFailureAt)}</span>}
      </p>
      {s.failureCategory && s.lastFailureAt !== 0n && <p>{failureAdvice[s.failureCategory] ?? "查看 hub 日志以定位故障"}</p>}
      <p>下次外呼：{s.nextAt === 0n ? <span className="muted">—</span> : at(s.nextAt)}</p>
      <p className="muted">状态只在 hub 的内存里，重启后"从未跑过"；目标地址不回显。连续同一失败只记一行日志，成功不清除"上次失败"。</p>
    </section>
  );
}
