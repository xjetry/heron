import type { MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import type { BackupSettings, BackupSettingsSchema, Settings, SettingsSchema, UpdateSettingsRequest } from "../gen/heron/v1/admin_pb";
import { ascending } from "../lib/ids";
import type { AdminImpl } from "./harness";

// 状态只存普通的初始化对象：消息对象（带 $typeName、嵌套字段也是消息）与初始化对象混着展开，得到的值两种形态都不是。
export type SettingsInit = Exclude<MessageInitShape<typeof SettingsSchema>, Settings>;
type BackupInit = Exclude<MessageInitShape<typeof BackupSettingsSchema>, BackupSettings>;

// 请求里的 backup 是消息对象；状态只取领域字段拼成普通的初始化对象，不混入消息元数据。语义同 store 的 saveBackup：
// endpoint、bucket、区域（空串存为 auto）、access key、前缀整体替换；secret、四个数值与 notify 缺席即不变；secret 只写
// 不读，回显 hasSecret；渠道排序去重。
function savedBackup(b: BackupSettings, old: MessageInitShape<typeof BackupSettingsSchema> | undefined): BackupInit {
  return {
    endpoint: b.endpoint, bucket: b.bucket, region: b.region || "auto", accessKey: b.accessKey, prefix: b.prefix,
    configIntervalS: b.configIntervalS ?? old?.configIntervalS ?? 300, metricsIntervalS: b.metricsIntervalS ?? old?.metricsIntervalS ?? 86400,
    configKeep: b.configKeep ?? old?.configKeep ?? 48, metricsKeep: b.metricsKeep ?? old?.metricsKeep ?? 14,
    notify: { channelIds: b.notify ? ascending(new Set(b.notify.channelIds)) : [...(old?.notify?.channelIds ?? [])] },
    hasSecret: b.secret !== undefined ? b.secret !== "" : (old?.hasSecret ?? false),
  };
}

// 有状态的 hub 替身，分组与保存语义同 hub 的 UpdateSettings，不做取值校验：按组判定、各组彼此独立。外观五项任一非空即算
// 给出，整体替换（标题去首尾空白，代表 hub 的清洗）；总闸、国家查询两项、backup、loginNotify 与 trafficReport 缺席即不变
// （backup 各项见 savedBackup；loginNotify 给出即替换渠道列表，trafficReport 给出即整组替换，渠道都排序去重，同 store 的
// saveChannelIDs）；一组都没给出时按 InvalidArgument
// 拒绝；回显保存后的全部设置。国家查询后端两项（geoBackend、geoMmdbPath）与 api 的 settingsProto 同语义：取自 hub 启动时
// 选定的后端，请求里的值被忽略，保存不改它们。set 模拟别处（另一个面板、脚本）改了设置；holdReads 让之后的
// GetSettings 挂起到 releaseReads；failReads 让之后的 GetSettings 一直失败（hub 重启、网络中断），保存后的刷新因此
// 拿不到新值；holdSaves 让之后的 UpdateSettings 在记下请求后挂起到 releaseSaves，保存因此一直在途，
// 状态在放行后才改；holdResponses 让之后的 UpdateSettings 到达即改状态、响应挂起，deliverNewestFirst 按到达的逆序放行
// 全部挂起的响应，模拟两次保存按提交先后生效、响应却逆序到达。
export function statefulHub(initial: SettingsInit) {
  let state: SettingsInit = initial;
  const sent: UpdateSettingsRequest[] = [];
  let heldReads: Promise<void> | null = null;
  let releaseReads = () => {};
  let heldSaves: Promise<void> | null = null;
  let releaseSaves = () => {};
  let failing = false;
  let holdingResponses = false;
  const heldResponses: (() => void)[] = [];
  const impl: AdminImpl = {
    getSettings: async () => {
      if (failing) throw new ConnectError("hub restarting", Code.Unavailable);
      if (heldReads) await heldReads;
      return { settings: state };
    },
    updateSettings: async (req) => {
      sent.push(req);
      if (heldSaves) await heldSaves;
      const s = req.settings!;
      const appearance = [s.title, s.theme, s.accentColor, s.logo, s.customCss].some((v) => v !== "");
      if (!appearance && s.publicEnabled === undefined && s.geoEnabled === undefined && s.geoUrl === undefined && s.backup === undefined && s.loginNotify === undefined && s.trafficReport === undefined) {
        throw new ConnectError("settings must give at least one group", Code.InvalidArgument);
      }
      state = {
        ...state,
        ...(appearance && { title: s.title.trim(), theme: s.theme, accentColor: s.accentColor, logo: s.logo, customCss: s.customCss }),
        publicEnabled: s.publicEnabled ?? state.publicEnabled, geoEnabled: s.geoEnabled ?? state.geoEnabled, geoUrl: s.geoUrl ?? state.geoUrl,
        geoBackend: state.geoBackend, geoMmdbPath: state.geoMmdbPath,
        backup: s.backup ? savedBackup(s.backup, state.backup) : state.backup,
        loginNotify: s.loginNotify ? { channelIds: ascending(new Set(s.loginNotify.channelIds)) } : state.loginNotify,
        trafficReport: s.trafficReport
          ? { enabled: s.trafficReport.enabled, daily: s.trafficReport.daily, weekly: s.trafficReport.weekly, monthly: s.trafficReport.monthly, hour: s.trafficReport.hour, channelIds: ascending(new Set(s.trafficReport.channelIds)) }
          : state.trafficReport,
      };
      const saved = state;
      if (holdingResponses) await new Promise<void>((r) => heldResponses.push(r));
      return { settings: saved };
    },
  };
  return {
    impl, sent, state: () => state,
    set: (patch: SettingsInit) => { state = { ...state, ...patch }; },
    holdReads: () => { heldReads = new Promise((r) => { releaseReads = () => { heldReads = null; r(); }; }); },
    releaseReads: () => releaseReads(),
    failReads: () => { failing = true; },
    holdSaves: () => { heldSaves = new Promise((r) => { releaseSaves = () => { heldSaves = null; r(); }; }); },
    releaseSaves: () => releaseSaves(),
    holdResponses: () => { holdingResponses = true; },
    deliverNewestFirst: () => { for (const deliver of heldResponses.splice(0).reverse()) deliver(); },
  };
}
