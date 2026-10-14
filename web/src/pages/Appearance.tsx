import { useMutation, useQuery } from "@connectrpc/connect-query";
import { type ChangeEvent, type FormEvent, useState } from "react";
import { errorText } from "../api/auth";
import { errorBanner, queryGate } from "../api/queryGate";
import { SAVE_SETTINGS, useAdoptSavedSettings, useSettingsSaving } from "../api/saveSettings";
import { BackupSettingsForm } from "../components/BackupSettingsForm";
import { BackupStatus, RestoreRecords } from "../components/BackupStatus";
import { PageHeader } from "../components/PageHeader";
import { AdminService, GeoBackend, type Settings } from "../gen/heron/v1/admin_pb";
import { HeartbeatSettingsForm, HeartbeatStatus } from "./HeartbeatSettings";
import { LOGO_TYPES, MAX_TITLE_CHARS, THEMES, sizeProblems, type Theme } from "../lib/appearance";
import { ANSWERS_PER_NODE } from "../lib/country";
import { BUILT_IN_ACCENT } from "../lib/palette";
import { DEFAULT_TITLE } from "../public/site";
import { FileInput } from "../components/FileInput";
import { SettingsSection } from "../components/SettingsSection";

const THEME_LABELS: Record<Theme, string> = { auto: "跟随访客的系统设置", light: "浅色", dark: "深色" };

// publicEnabled 只在用户动过开关后才出现（经 edit），toDraft 不取它；缺席时 UpdateSettings 保持总闸不变。
type Draft = { title: string; theme: string; accentColor: string; logo: string; customCss: string; publicEnabled?: boolean };

const toDraft = (s: Settings | undefined): Draft => ({
  title: s?.title ?? "", theme: s?.theme || "auto", accentColor: s?.accentColor ?? "", logo: s?.logo ?? "", customCss: s?.customCss ?? "",
});

// 公开页的外观与总闸。UpdateSettings 按组判定：外观五项是一组，给出就整体替换，表单因此总是提交全部五项（明暗总有值，
// 这一组总算给出）。总闸是另一组，缺席表示不变，草稿只在用户动过开关后才带它：草稿是开始编辑（或上次保存）时的快照，
// 之后总闸可能被别处改过（另一个面板、脚本），把快照里的总闸随标题一起提交，会把别人刚关掉的公开页重新打开。开关没动
// 过时显示查询缓存里 hub 的当前值，重新拉取即跟上。国家查询的两项、备份设置与登录通知不在这个表单里、不提交：hub 对
// 它们缺席即不改（见 GeoLookup、BackupSettingsForm、HeartbeatSettingsForm、Channels 页的登录通知表单）。外观、国家查询、
// 备份、心跳外推与登录通知五个设置表单的保存互斥（SAVE_SETTINGS，见 api/saveSettings.ts），saving 覆盖任一个在途。
//
// 保存成功时 onSuccess 用 hub 的回显替换草稿；它不判断"是不是最新一次"，靠的是"有未结请求"与"草稿还能被改"互斥。
// 草稿的改动来自两处：用户改字段（同步），与读 logo 文件的回调（异步，读完才改）。互斥由两处承载：
//   - 保存进行中，fieldset 的 disabled={saving} 禁用整个表单，含文件输入：保存期间既改不了字段，也开始不了读取；
//   - 读取进行中，文件输入的 disabled={reading} 让至多一个读者在飞，reading 因此恰好等于"有读者在飞"；
//     submit 守卫（!reading）与保存按钮的禁用拒绝在这时保存。logo 字段另有第二个写者"移除 logo"，它在读取中
//     同样禁用：否则读取中点了移除，读完的回调又把 logo 写回来，用户最后一次操作被迟到的结果覆盖。
// 所以回显覆盖的总是这次提交自己送出的内容。reading 若不对应唯一的读者（文件输入在读取中仍可用），先读完的那个
// 把它置回 false，保存得以发出，后读完的在保存进行中改草稿，迟到的回显再把它改回去并误报"已保存"。
export function Appearance() {
  const adoptSaved = useAdoptSavedSettings();
  const settings = useQuery(AdminService.method.getSettings, {});
  // draft 为空时表单显示 hub 的当前值；保存成功后改为 hub 回显的实际保存值（标题已清洗、主色已转小写）。
  const [draft, setDraft] = useState<Draft | null>(null);
  const [saved, setSaved] = useState(false);
  const [fileError, setFileError] = useState<string | null>(null);
  const [reading, setReading] = useState(false);
  const saving = useSettingsSaving();
  const update = useMutation(AdminService.method.updateSettings, {
    mutationKey: SAVE_SETTINGS,
    onSuccess: (r) => {
      setDraft(toDraft(r.settings));
      setSaved(true);
      return adoptSaved(r.settings);
    },
  });
  const gate = queryGate(settings);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  const form = draft ?? toDraft(gate.data.settings);
  // 只有 hub 明说开着才显示为开；显示值不进请求，用户点了开关才由 edit 写进草稿。
  const publicEnabled = form.publicEnabled ?? gate.data.settings?.publicEnabled === true;
  // 用户侧对草稿的改动都经 edit：按最新的草稿合并（读 logo 的回调在读完时才调用它，选文件时的快照可能已过时），
  // 并清掉上一次保存的"已保存"与错误——改了草稿，它们就不再描述当前内容。保存成功的回显由 onSuccess
  // 直接替换草稿，那是 hub 已保存的值，"已保存"正是要留给它显示的。
  const edit = (patch: Partial<Draft>) => {
    setDraft((d) => ({ ...(d ?? toDraft(gate.data.settings)), ...patch }));
    setSaved(false);
    update.reset();
  };
  const problems = sizeProblems(form.title, form.logo, form.customCss);
  const pickLogo = (e: ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    e.target.value = "";
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => {
      setReading(false);
      // readAsDataURL 的结果是 data URL 字符串；不是字符串就没有可用的 logo，按读取失败报。
      if (typeof reader.result !== "string") { setFileError(`读取 ${file.name} 失败：结果不是 data URL`); return; }
      setFileError(null);
      edit({ logo: reader.result });
    };
    reader.onerror = () => { setReading(false); setFileError(`读取 ${file.name} 失败：${String(reader.error)}`); };
    setReading(true);
    reader.readAsDataURL(file);
  };
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (!e.currentTarget.checkValidity()) return;
    if (problems.length === 0 && !reading && !saving) update.mutate({ settings: form });
  };
  return (
    <section>
      {gate.banner}
      <PageHeader title="外观" />
      <SettingsSection title="公开页" description={<p className="muted">
        公开页（站点根路径 /）的标题、明暗、主色、logo 与自定义 CSS。保存后，访客刷新公开页才看到新外观；浏览器还可能再用最多 5 分钟的缓存。
        要改页面结构，可在「主题」页安装主题；--public-dir 仅用于部署者信任的静态目录。「启用公开页」统一控制内置页、可信目录、主题及其预览。
      </p>}>
      <form className="card" aria-label="公开页外观" onSubmit={submit}>
        <fieldset className="bare" disabled={saving}>
          <label className="row"><input type="checkbox" checked={publicEnabled} onChange={(e) => edit({ publicEnabled: e.target.checked })} />启用公开页</label>
          <p className="muted">关闭后公开页与公开接口整体不可访问，节点的公开标记保留，重新打开即可恢复。hub 内的快照缓存最多再命中 1 秒；经缓存代理时，GET 响应按各自的 max-age 过期（站点配置最长 5 分钟）。</p>
          <label>
            标题
            <input value={form.title} placeholder={DEFAULT_TITLE} onChange={(e) => edit({ title: e.target.value })} />
          </label>
          <p className="muted">最多 {MAX_TITLE_CHARS} 个字符；留空用「{DEFAULT_TITLE}」。</p>
          <label>
            明暗
            <select value={form.theme} onChange={(e) => edit({ theme: e.target.value })}>
              {THEMES.map((t) => <option key={t} value={t}>{THEME_LABELS[t]}</option>)}
            </select>
          </label>
          <div className="row">
            <label>
              主色
              <input value={form.accentColor} placeholder="#rrggbb，留空用内置配色" onChange={(e) => edit({ accentColor: e.target.value })} />
            </label>
            {/* 取色器必须有值，主色为空时显示内置浅色主题的主色。它只在用户选了与显示值不同的颜色时才进草稿：
                React 的 onChange 只在输入的值与渲染的值不同时触发，打开取色器又原样关上不会把兜底色存成设置。 */}
            <label>
              取色
              <input type="color" value={form.accentColor || BUILT_IN_ACCENT} onChange={(e) => edit({ accentColor: e.target.value })} />
            </label>
            <button type="button" className="link" onClick={() => edit({ accentColor: "" })} disabled={form.accentColor === ""}>用内置配色</button>
          </div>
          <div className="row">
            <FileInput label="logo" caption="logo" accept={LOGO_TYPES.join(",")} onChange={pickLogo} disabled={reading}
              fileName={reading ? "读取中…" : form.logo ? "已设置 logo" : undefined} />
            {form.logo && <img src={form.logo} alt="logo 预览" className="logo-preview" />}
            <button type="button" className="link" onClick={() => edit({ logo: "" })} disabled={form.logo === "" || reading}>移除 logo</button>
          </div>
          <label>
            自定义 CSS
            <textarea value={form.customCss} onChange={(e) => edit({ customCss: e.target.value })} spellCheck={false} />
          </label>
          <p className="muted">排在公开页内置样式之后。只接受 CSS，不能含 &lt;/。</p>
          {fileError && <p role="alert" className="error">{fileError}</p>}
          {problems.map((p) => <p key={p} role="alert" className="error">{p}</p>)}
          {update.error != null && <p role="alert" className="error">{errorText(update.error)}</p>}
          {saved && <p role="status">已保存。</p>}
          <button type="submit" disabled={reading || problems.length > 0}>保存</button>
        </fieldset>
      </form>
      </SettingsSection>
      <SettingsSection title="国家 / 地区查询" description={<p className="muted">按节点的公网来源地址查得国家 / 地区，用于节点表的国家码、公开页的地区筛选与状态墙分组。</p>}>
        <GeoLookup current={gate.data.settings} />
      </SettingsSection>
      <SettingsSection title="备份到 S3" description={<p className="muted">目标使用 path-style，兼容 R2。Bucket 必须为私有；建议使用 HTTPS。清空 Endpoint 并保存可整体关闭备份。缺少 Bucket、Access key 或 Secret 时也不会启动备份。</p>}>
        <BackupSettingsForm current={gate.data.settings?.backup} />
      </SettingsSection>
      <SettingsSection title="备份状态" description={<p className="muted">配置层连续故障只在首次失败与恢复时通知，未恢复标记跨 hub 重启保留；重启后故障类别重新观察。指标层故障不通知，重启后重新观察。停用备份即结束两层的故障跟踪，配置层已通知的故障以一条停用事件收尾。</p>}>
        <BackupStatus />
      </SettingsSection>
      <SettingsSection title="恢复记录" description={<p className="muted">离线恢复（heron-hub restore）落下的审计记录：每条是恢复那一刻的事实快照，不随之后的变化改写，没有删除路径。恢复只在停机时发生，这里只在打开时查询一次。</p>}>
        <RestoreRecords />
      </SettingsSection>
      <SettingsSection title="心跳外推" description={<p className="muted">
        hub 按周期向一个外部监控服务（如 Uptime Kuma 的 push 地址、healthchecks.io）发一次请求：hub 停机时对方收不到心跳而告警。
        请求只带聚合计数与版本号，不含节点名或地址。默认关闭；地址清空即停用，改动不需要重启。
      </p>}>
        <HeartbeatSettingsForm current={gate.data.settings?.heartbeat} />
        <HeartbeatStatus />
      </SettingsSection>
    </section>
  );
}

type GeoDraft = { geoEnabled: boolean; geoUrl: string };
const toGeoDraft = (s: Settings | undefined): GeoDraft => ({ geoEnabled: s?.geoEnabled ?? false, geoUrl: s?.geoUrl ?? "" });

// 国家 / 地区查询的开关与服务地址。只提交这两项：UpdateSettings 按组判定，外观、总闸与备份缺席即不改。于是只改查询设置
// 既不会顺带保存上面表单里外观与总闸的未保存改动，也不会把缓存里可能已过时的外观写回去（别处刚改过的外观不被覆盖）。
// current 是 hub 的回显：它初始化草稿，也给出"当前后端"一行（后端种类、本地文件路径、已保存的服务地址）。保存与其余
// 设置表单互斥（SAVE_SETTINGS）。出网告知写哪一版按回显的后端种类裁决，不由表单草稿决定；HTTP 后端下告知里的服务地址
// 取草稿，随输入更新。
function GeoLookup({ current }: { current: Settings | undefined }) {
  const adoptSaved = useAdoptSavedSettings();
  const [draft, setDraft] = useState<GeoDraft | null>(null);
  const [saved, setSaved] = useState(false);
  const saving = useSettingsSaving();
  const update = useMutation(AdminService.method.updateSettings, {
    mutationKey: SAVE_SETTINGS,
    onSuccess: (r) => {
      setDraft(toGeoDraft(r.settings));
      setSaved(true);
      return adoptSaved(r.settings);
    },
  });
  const form = draft ?? toGeoDraft(current);
  const local = current?.geoBackend === GeoBackend.MMDB;
  const edit = (patch: Partial<GeoDraft>) => {
    setDraft((d) => ({ ...(d ?? toGeoDraft(current)), ...patch }));
    setSaved(false);
    update.reset();
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!saving) update.mutate({ settings: form });
  };
  return (
    <>
      <form className="card" aria-label="国家 / 地区查询" onSubmit={submit}>
        <p className="muted" style={{ overflowWrap: "anywhere" }}>
          {local ? `当前后端：本地文件 ${current.geoMmdbPath}，不出网；服务地址不生效。` : `当前后端：HTTP 服务 ${current?.geoUrl ?? ""}`}
        </p>
        <fieldset className="bare" disabled={saving}>
          <label className="inline">
            <input type="checkbox" checked={form.geoEnabled} onChange={(e) => edit({ geoEnabled: e.target.checked })} />
            按来源地址查询节点的国家 / 地区
          </label>
          <p className="muted" id="geo-disclosure">
            {local ? <>
            开启后仅在本地文件中查询公网来源地址。节点停在同一地址时查得一次即止；hub 记住每个节点最近 {ANSWERS_PER_NODE} 个地址的答案，在这些地址之间切换不再重查，
            超过 {ANSWERS_PER_NODE} 个地址轮换或 hub 重启后会再查。查不到或国家码无效时一小时后重试。关闭时不查询。
            </> : <>
            开启即由 hub 把每个节点的来源地址发给 {form.geoUrl || "（未填写的服务地址）"}（{"{ip}"} 处换成地址），用它的应答作为节点的国家 / 地区。
            只发公网地址，不带任何凭据。节点停在同一地址时查得一次即止；hub 记住每个节点最近 {ANSWERS_PER_NODE} 个地址的答案，在这些地址之间切换不再外呼，
            超过 {ANSWERS_PER_NODE} 个地址轮换或 hub 重启后会再查。失败一小时后重试。关闭时 hub 不为此出网。
            </>}
          </p>
          <label>
            服务地址
            <input value={form.geoUrl} aria-describedby="geo-disclosure" onChange={(e) => edit({ geoUrl: e.target.value })} spellCheck={false} />
          </label>
          <p className="muted">http 或 https，含 {"{ip}"}，{"{ip}"} 要在路径或查询串里、不能放在主机或端口里（# 后的片段不随请求发出）；应答须恰为两个大写字母的国家码（ISO 3166-1），否则按失败处理。节点也可在节点页手动指定国家，手动值优先。</p>
          {update.error != null && <p role="alert" className="error">{errorText(update.error)}</p>}
          {saved && <p role="status">已保存。</p>}
          <button type="submit">保存</button>
        </fieldset>
      </form>
    </>
  );
}
