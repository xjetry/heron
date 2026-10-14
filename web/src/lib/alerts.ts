import { AlertKind, BaselineMode, ChannelKind, DeliveryFailure, ProbeMetric, ResourceMetric, RttMode, type AlertDelivery, type AlertEvent, type AlertRule, type AlertStateEntry, type NotifyChannel, type ProbeTaskDetail, type Settings } from "../gen/heron/v1/admin_pb";
import { remainingText } from "./billing";
import { duration, formatUnit, percent } from "./format";
import { withId } from "./ids";
import { disambiguate, kindLabel } from "./probes";

type Entry<K> = { value: K; label: string };

export const CHANNEL_KINDS: readonly Entry<ChannelKind>[] = [
  { value: ChannelKind.TELEGRAM, label: "Telegram" },
  { value: ChannelKind.WEBHOOK, label: "Webhook" },
];

// 更新的 hub 可能返回面板不认识的枚举值；显示原值而不是抛错，整页不因一个标签失效。
export function labelOf<K extends number>(table: readonly Entry<K>[], value: K): string {
  return table.find((e) => e.value === value)?.label ?? `未知（${value}）`;
}

// 凭据只写不读：目标描述只用到 hub 回显的 chat_id、scheme://host、方法与头名，秘密值不出现在页面上。
export function channelTarget(c: NotifyChannel): string {
  if (c.kind === ChannelKind.TELEGRAM) return `会话 ${c.telegram?.chatId ?? ""}`;
  if (c.kind === ChannelKind.WEBHOOK) {
    const w = c.webhook;
    const head = `${methodOf(w?.method)} ${w?.urlHost ?? ""}`;
    return w && w.headerNames.length > 0 ? `${head}，头 ${w.headerNames.join("、")}` : head;
  }
  return labelOf(CHANNEL_KINDS, c.kind);
}

// 出站节奏上限（NotifyChannel.rate_per_minute）：0 表示不限。
// ListNotifyChannels 与 SaveNotifyChannel 经 channelProto 总是写入这个指针，含 0。缺席不是“不限”。
export function rateLabel(c: NotifyChannel): string {
  if (c.ratePerMinute === undefined) throw new Error("NotifyChannel.rate_per_minute is always set");
  return c.ratePerMinute === 0 ? "不限" : `每分钟 ${c.ratePerMinute} 条`;
}

// 已保存 Webhook 的空 method 由 hub 的 Engine.SaveChannel 规范成 POST；
// 面板为没有 Webhook 配置的渠道（例如 Telegram 渠道被编辑时）建立草稿也需要 POST 作初值。
export function methodOf(method: string | undefined): string {
  return method || "POST";
}

export const ALERT_KINDS: readonly Entry<AlertKind>[] = [
  { value: AlertKind.OFFLINE, label: "离线" },
  { value: AlertKind.PROBE, label: "探测" },
  { value: AlertKind.EXPIRY, label: "到期" },
  { value: AlertKind.RESOURCE, label: "资源" },
  { value: AlertKind.CERT_EXPIRY, label: "证书到期" },
  { value: AlertKind.TRAFFIC, label: "流量" },
];
// 资源指标的单位决定面板阈值输入的展示与换算：百分比与每核负载原值输入；速率以 Mbps 输入，
// 协议值仍是 bytes/s（proto ResourceMetric 注释），1 Mbps = 125000 bytes/s。
export type ResourceUnit = "percent" | "per-core" | "mbps";
export const MBPS_TO_BYTES_PER_S = 125000;
export const RESOURCE_METRICS: readonly (Entry<ResourceMetric> & { unit: ResourceUnit })[] = [
  { value: ResourceMetric.MEMORY_USED_PCT, label: "内存使用率", unit: "percent" },
  { value: ResourceMetric.DISK_USED_PCT, label: "磁盘使用率", unit: "percent" },
  { value: ResourceMetric.CPU_PCT, label: "CPU 使用率", unit: "percent" },
  { value: ResourceMetric.LOAD1_PER_CORE, label: "每核负载", unit: "per-core" },
  { value: ResourceMetric.NET_RX_BPS, label: "下行速率", unit: "mbps" },
  { value: ResourceMetric.NET_TX_BPS, label: "上行速率", unit: "mbps" },
];

// 面板不认识的枚举值按百分比处理：旧面板遇到新指标时至少不把数字换算错方向。
export function resourceUnit(metric: ResourceMetric): ResourceUnit {
  return RESOURCE_METRICS.find((m) => m.value === metric)?.unit ?? "percent";
}

// 阈值输入范围与 hub 的 CheckRule 一致：百分比 (0,100]、每核负载 (0,64]、速率 (0,2^40] bytes/s（按 MBPS_TO_BYTES_PER_S 换算成 Mbps）。
export function resourceThresholdMax(metric: ResourceMetric): number {
  switch (resourceUnit(metric)) {
    case "per-core": return 64;
    case "mbps": return (2 ** 40) / MBPS_TO_BYTES_PER_S;
    default: return 100;
  }
}

function resourceThresholdText(metric: ResourceMetric, v: number): string {
  switch (resourceUnit(metric)) {
    case "per-core": return String(v);
    case "mbps": return `${v / MBPS_TO_BYTES_PER_S} Mbps`;
    default: return `${v}%`;
  }
}
// unit 与 formatUnit 的单位名一致：丢包阈值是百分数，RTT 阈值是毫秒（proto AlertRule.threshold）。
export const PROBE_METRICS: readonly (Entry<ProbeMetric> & { unit: string })[] = [
  { value: ProbeMetric.LOSS_PCT, label: "丢包率", unit: "percent" },
  { value: ProbeMetric.RTT_MS, label: "RTT 均值", unit: "ms" },
];

// rtt 规则的判定方式与相对判定的基线来源（proto RttMode、BaselineMode）。rtt 规则的 rtt_mode 由 hub 显式回显，
// 表单只在这两个值之间选；UNSPECIFIED 只出现在非 rtt 规则上。
export const RTT_MODES: readonly Entry<RttMode>[] = [
  { value: RttMode.THRESHOLD, label: "固定阈值" },
  { value: RttMode.RELATIVE, label: "相对基线" },
];
export const BASELINE_MODES: readonly Entry<BaselineMode>[] = [
  { value: BaselineMode.ADAPTIVE, label: "自适应基线" },
  { value: BaselineMode.FIXED, label: "固定基线" },
];

// 相对判定字段的取值界，与 hub 的 store.checkRttFields 同值（proto AlertRule 注释）：基线窗口与冷却在表单里以分钟输入，
// 上偏差 (0, 1000]、下偏差 (0, 100]（100 即下界为 0 ms，只按上偏差判定）、固定基线 (0, 60000] ms、冷却 [1, 10080] 分钟、
// 窗口不超过 30 天。
export const RELATIVE_LIMITS = { upperMax: 1000, lowerMax: 100, fixedMax: 60000, cooldownMinMinutes: 1, cooldownMaxMinutes: 7 * 24 * 60, windowMaxMinutes: 30 * 24 * 60 } as const;

// 秒数按分钟写：整小时写小时，其余写分钟，摘要里不出现 86400 秒这种读不出量级的数。
function minutesText(seconds: number): string {
  return seconds % 3600 === 0 ? `${seconds / 3600} 小时` : `${Number((seconds / 60).toFixed(2))} 分钟`;
}

// 相对判定的条件摘要：基线来源、上下偏差、持续与冷却，与 hub 的判定逐项对应。
function relativeCondition(rule: AlertRule, task: string): string {
  const base = rule.baselineMode === BaselineMode.FIXED
    ? `固定基线 ${rule.fixedBaselineMs} ms`
    : rule.baselineMode === BaselineMode.ADAPTIVE
      ? `近 ${minutesText(rule.baselineWindowS)}的 5 分钟桶均值中位数（至少 ${rule.baselineMinSamples} 个桶）`
      : `基线来源 ${rule.baselineMode}`;
  return `${task} RTT 均值越出基线 +${rule.upperDeviationPct}% / −${rule.lowerDeviationPct}%（${base}），连续 ${rule.forMinutes} 分钟，冷却 ${minutesText(rule.cooldownS)}`;
}

// 调用方是告警规则页与 ruleCondition，任务取自管理端的任务列表；列表未到或查询失败时用编号，标签仍可辨认。
// 被告警规则引用的任务不能删除（store 的 checkAlertReferences），所以这里的编号回退不代表任务已删除。
// 历史图例不走这里：序列自带标注，用 probes.ts 的 seriesLabels。
export function taskLabel(id: bigint, tasks: readonly ProbeTaskDetail[] | undefined): string {
  const t = tasks?.find((d) => d.task?.id === id)?.task;
  if (!t) return `任务 #${id}`;
  return `${kindLabel(t.kind)} ${t.target}`;
}

export function taskLabels(ids: bigint[], tasks: readonly ProbeTaskDetail[] | undefined): string[] {
  return disambiguate(ids.map((id) => taskLabel(id, tasks)), ids);
}

export function ruleCondition(rule: AlertRule, tasks: ProbeTaskDetail[] | undefined): string {
  if (rule.kind === AlertKind.TRAFFIC) return `周期流量用量 ≥ 配额的 ${rule.threshold}%`;
  if (rule.kind === AlertKind.OFFLINE) return "超过宽限期未上报";
  if (rule.kind === AlertKind.EXPIRY) return `到期日距今不超过 ${rule.daysBefore} 天（含已过期）`;
  if (rule.kind === AlertKind.CERT_EXPIRY) return `${taskLabel(rule.taskId, tasks)} 的证书到期日距今不超过 ${rule.daysBefore} 天（含已过期）`;
  if (rule.kind === AlertKind.RESOURCE) return `${labelOf(RESOURCE_METRICS, rule.resourceMetric)} ≥ ${resourceThresholdText(rule.resourceMetric, rule.threshold)}，恢复 ≤ ${resourceThresholdText(rule.resourceMetric, rule.recoveryThreshold)}，各连续 ${rule.forMinutes} 分钟`;
  if (rule.metric === ProbeMetric.RTT_MS && rule.rttMode === RttMode.RELATIVE) return relativeCondition(rule, taskLabel(rule.taskId, tasks));
  const metric = PROBE_METRICS.find((m) => m.value === rule.metric);
  const threshold = metric ? formatUnit(rule.threshold, metric.unit) : String(rule.threshold);
  return `${taskLabel(rule.taskId, tasks)} ${labelOf(PROBE_METRICS, rule.metric)} ≥ ${threshold}，连续 ${rule.forMinutes} 分钟`;
}

export type RuleStates = { firing: AlertStateEntry[]; pending: AlertStateEntry[] };

// hub 只返回已有记录的组合，缺失即 ok（proto ListAlertRulesResponse.states）；ok 记录同样不展示。
export function statesOf(states: AlertStateEntry[]): Map<bigint, RuleStates> {
  const out = new Map<bigint, RuleStates>();
  for (const s of states) {
    if (s.state !== "firing" && s.state !== "pending") continue;
    const entry = out.get(s.ruleId) ?? { firing: [], pending: [] };
    entry[s.state].push(s);
    out.set(s.ruleId, entry);
  }
  return out;
}

// 事件的 transition 取值与 hub 的 store.Transition 常量（internal/hub/store/alert.go）逐值对齐，由 transitions.test.ts
// 读 Go 源码对照：hub 新增一种事件而这里漏写时，事件页（transitionLabel）只能原样显示 hub 的英文取值。
export const TRANSITIONS: Readonly<Record<string, string>> = {
  firing: "触发", recovered: "恢复", login_success: "登录成功", login_locked: "登录锁定",
  backup_failed: "备份失败", backup_recovered: "备份恢复", backup_disabled: "备份停用",
  login_failed: "登录失败", auth_changed: "认证方式变更", backup_success: "备份成功", backup_restored: "手动恢复",
  traffic_report: "流量报告",
};
export const transitionLabel = (t: string): string => TRANSITIONS[t] ?? t;
// 要人立即注意的变化，事件页标红：规则触发；登录锁定——有人在猜管理员密码；配置层备份失败——RPO 正在无声变长。
export const alarming = (t: string): boolean => t === "firing" || t === "login_locked" || t === "backup_failed";

// 通知渠道选择列表（备份失败通知、登录通知、流量报告）的条数上限，与 hub 的 internal/hub/api/settings.go 里 maxNotifyChannels
// 同值，由 appearanceLimits.test.ts 对照；面板据此在提交前就不让多选（Picks 的 max）。
export const MAX_NOTIFY_CHANNELS = 16;

// 设置里的通知渠道选择列表，逐个与 hub 的 store.NotifyLists 对应：key 是 hub 在 setting 表里的列表键，
// notifyLists.test.ts 读 internal/hub/store/notify_list.go 双向核对。hub 删渠道时在同一事务里把它从每个列表摘除，
// 删除确认据此逐个列表写影响（pages/Channels.tsx 的 deleteNote）；这里漏一个列表，删掉它唯一的接收渠道时确认不提示。
// 登录通知是密码泄漏当下唯一的信号，备份失败通知是 RPO 无声变长时唯一的告警，删掉唯一的接收渠道就关掉了它们；
// 流量报告删掉唯一的接收渠道后只记事件、不再发出。
export const NOTIFY_LISTS: readonly { key: string; name: string; ids: (s: Settings) => readonly bigint[] }[] = [
  { key: "notify.login_channels", name: "登录通知", ids: (s) => s.loginNotify?.channelIds ?? [] },
  { key: "notify.backup_channels", name: "备份失败通知", ids: (s) => s.backup?.notify?.channelIds ?? [] },
  { key: "notify.traffic_report_channels", name: "流量报告", ids: (s) => s.trafficReport?.channelIds ?? [] },
];

// 与 proto DeliveryFailure 逐值对齐（测试按枚举全集核对）；UNSPECIFIED 表示没有失败，不在表里。
// 面板依赖"HTTP_STATUS 必带 http_status"，由 hub 写库前的校验（DeliveryResult.check）保证；
// 反方向"http_status 只随 HTTP_STATUS 出现"由 api 的 ListAlertEvents 映射保证。
// hasText：hub 为该类别记下错误原文；渠道删除与结果未落盘没有原文（proto 枚举注释），不给查看入口。
export const DELIVERY_FAILURES: readonly { value: DeliveryFailure; text: (d: AlertDelivery) => string; hasText: boolean }[] = [
  { value: DeliveryFailure.HTTP_STATUS, text: (d) => `HTTP ${d.httpStatus}`, hasText: true },
  { value: DeliveryFailure.TRANSPORT, text: () => "连接失败", hasText: true },
  { value: DeliveryFailure.REQUEST, text: () => "请求无法构造", hasText: true },
  { value: DeliveryFailure.CHANNEL_INVALID, text: () => "渠道配置无效", hasText: true },
  { value: DeliveryFailure.CHANNEL_DELETED, text: () => "渠道已删除", hasText: false },
  { value: DeliveryFailure.RESULT_UNRECORDED, text: () => "结果未记录", hasText: false },
  { value: DeliveryFailure.UNCLASSIFIED, text: () => "未分类", hasText: true },
];

// 只对终态失败给原文入口：未终态时类别属于上一次尝试，原文随后续尝试变化。
export function hasErrorText(d: AlertDelivery): boolean {
  return !d.ok && d.done && DELIVERY_FAILURES.some((e) => e.value === d.failure && e.hasText);
}

// 表外的值（hub 比面板新）显示编号而不是空：失败必须带着可追查的类别出现。
export function failureText(d: AlertDelivery): string {
  return DELIVERY_FAILURES.find((e) => e.value === d.failure)?.text(d) ?? `类别 ${d.failure}`;
}

// 已送达只由 ok 为真决定；done 为假表示尚未终态，可能等待入窗、正在尝试或等待重试，不能显示为终止失败。
export function deliveryText(d: AlertDelivery, channel: string): string {
  if (d.ok) return `${channel}：已送达`;
  if (!d.done) return `${channel}：投递中（已尝试 ${d.attempts} 次）`;
  return `${channel}：失败（${d.attempts} 次）${failureText(d)}`;
}

// 缺失表示取 hub 的 HERON_OFFLINE_AFTER（proto Node.offline_grace_s）；清除后 hub 存 NULL，不会回显 0。
export const graceText = (s: number | undefined): string => (s === undefined ? "默认" : `${s} 秒`);

// 规则列表未到、查询失败或规则已删除时按编号回退，事件行仍可辨认。
export function ruleLabel(id: bigint, rules: readonly AlertRule[] | undefined): string {
  const rule = rules?.find((r) => r.id === id);
  return rule ? withId(rule.name, rule.id) : `规则 #${id}`;
}

// AlertEvent.value 的单位随规则种类（proto AlertEvent.value 与 internal/hub/alert 各 apply 调用处）：离线是未上报秒数，
// 探测同规则阈值的单位，到期与证书到期是剩余天数（负数已过期），流量是配额百分比，资源随指标（字节速率按面板的 Mbps 口径）。
// 没有观测值的事件（系统事件、触发 / 恢复之外的变化且值为 0）返回 null。规则未知（列表未到、已删除或面板不认识的种类）时
// 不猜单位，只写数字。到期规则因清除到期日而恢复时值为 0：剩 0 天必然仍在提醒窗口内，不会是恢复值，所以恢复的 0 不写成"剩 0 天"。
export function eventValueText(ev: AlertEvent, rule: AlertRule | undefined): string | null {
  const v = ev.value;
  if (v === 0 && ev.transition !== "firing" && ev.transition !== "recovered") return null;
  switch (rule?.kind) {
    case AlertKind.OFFLINE: return v < 60 ? `${Math.round(v)} s` : duration(Math.round(v));
    case AlertKind.PROBE: {
      const metric = PROBE_METRICS.find((m) => m.value === rule.metric);
      return metric ? formatUnit(v, metric.unit) : plain(v);
    }
    case AlertKind.EXPIRY:
    case AlertKind.CERT_EXPIRY:
      return v === 0 && ev.transition === "recovered" ? null : remainingText(Math.round(v));
    case AlertKind.TRAFFIC: return percent(v);
    case AlertKind.RESOURCE:
      switch (resourceUnit(rule.resourceMetric)) {
        case "per-core": return v.toFixed(2);
        case "mbps": return `${(v / MBPS_TO_BYTES_PER_S).toFixed(1)} Mbps`;
        default: return percent(v);
      }
    default: return plain(v);
  }
}

const plain = (v: number) => String(Number(v.toFixed(2)));
