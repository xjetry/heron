import { type Traffic, TrafficQuotaMode } from "../gen/heron/v1/types_pb";
import { bytes } from "./format";

// 重置日的准入范围，与 hub 的 store.MinTrafficResetDay / MaxTrafficResetDay 一致；越界值 hub 会拒绝，表单先禁止保存。
// 不足该日的月份由 hub 钳到当月最后一日（traffic.PeriodStart），配置的原值不变。
export const RESET_DAY_MIN = 1;
export const RESET_DAY_MAX = 31;
// 2 月最少 28 天，大于 28 的重置日才会在某些月份被钳到月末。
export const resetDayClamps = (day: number) => day > 28;

export const QUOTA_UNITS = { GiB: 1n << 30n, TiB: 1n << 40n, GB: 1000000000n, TB: 1000000000000n };
export type QuotaUnit = keyof typeof QUOTA_UNITS;

// 十进制输入先用整数分子与分母表示，舍入只发生在最终字节数，避免浮点误差扩大到大配额。
export function parseQuota(text: string, unit: QuotaUnit): bigint | null {
  if (text.trim() === "") return 0n;
  if (!/^\d+(?:\.\d+)?$/.test(text) || text.length > 100) return null;
  const [whole, fraction = ""] = text.split(".");
  const denominator = 10n ** BigInt(fraction.length);
  const numerator = BigInt(whole + fraction) * QUOTA_UNITS[unit];
  const value = (numerator + denominator / 2n) / denominator;
  return value > 0n && value < (1n << 62n) ? value : null;
}

export function quotaInput(bytes: bigint, unit: QuotaUnit): string {
  if (bytes === 0n) return "";
  const scale = QUOTA_UNITS[unit];
  return `${bytes / scale}.${((bytes % scale) * 1000000n / scale).toString().padStart(6, "0")}`.replace(/\.?0+$/, "");
}

// 只有数字与单位：调用方把它放进等宽的 .num 里，中文说明（口径、是否设了配额）由 trafficDetail 另起一段给出。
// trafficParts 是同一份文字的两段：用量（有配额时带分母）与配额百分比，供窄列分两行排；其余位置用 trafficText 写成一行。
export function trafficParts(t: Traffic): { amount: string; percent?: string } {
  const used = bytes(t.quotaUsedBytes);
  return t.quotaBytes > 0n ? { amount: `${used} / ${bytes(t.quotaBytes)}`, percent: `${(t.quotaUsedPct ?? 0).toFixed(1)}%` } : { amount: used };
}

export function trafficText(t: Traffic): string {
  const { amount, percent } = trafficParts(t);
  return percent ? `${amount} (${percent})` : amount;
}

// 配额的计入口径决定 quota_used_bytes 是哪个分子；展示分子的地方必须用同一口径作副标题，否则"只收"的数值配着"下载 + 上传"的说明。
// 未设配额时 hub 按 sum 计分子（traffic.Quota 的默认分支），副标题与之一致，并注明未设配额：trafficText 此时没有分母，
// 读者要从副标题知道这是"没设"而不是"没取到"。
export const QUOTA_MODE_LABELS: Record<TrafficQuotaMode, string> = {
  [TrafficQuotaMode.UNSPECIFIED]: "下载 + 上传",
  [TrafficQuotaMode.SUM]: "下载 + 上传",
  [TrafficQuotaMode.RX]: "仅下载",
  [TrafficQuotaMode.TX]: "仅上传",
  [TrafficQuotaMode.MAX]: "下载与上传取大者",
};

export function trafficDetail(t: Traffic): string {
  return t.quotaBytes > 0n ? QUOTA_MODE_LABELS[t.quotaMode] : `${QUOTA_MODE_LABELS[TrafficQuotaMode.SUM]} · 未设配额`;
}

export type PeriodBreakdown = {
  rx: string;
  tx: string;
  rxNote?: string;
  txNote?: string;
  note?: string;
};

// 本周期下行、上行分量及各方向相对当前配额口径的归属标注。
// 未设配额时与 hub 的 traffic.Quota 一致落回下载 + 上传（SUM），不沿用配置表里残留的 quotaMode。
export function periodBreakdown(t: Traffic): PeriodBreakdown {
  const rx = bytes(t.periodRx);
  const tx = bytes(t.periodTx);
  const mode = t.quotaBytes > 0n ? t.quotaMode : TrafficQuotaMode.SUM;
  switch (mode) {
    case TrafficQuotaMode.RX:
      return { rx, tx, rxNote: "计入配额", txNote: "不计入配额" };
    case TrafficQuotaMode.TX:
      return { rx, tx, rxNote: "不计入配额", txNote: "计入配额" };
    case TrafficQuotaMode.MAX:
      if (t.periodRx > t.periodTx) return { rx, tx, rxNote: "当前取大" };
      if (t.periodTx > t.periodRx) return { rx, tx, txNote: "当前取大" };
      return { rx, tx, note: "两向相等" };
    default:
      return { rx, tx, note: "两向合计" };
  }
}

// 紧凑表格与摘要卡片的悬停提示文本，在不撑宽列的前提下给出上下行分量与口径归属。
export function trafficBreakdownText(t: Traffic): string {
  const b = periodBreakdown(t);
  const rx = `↓ ${b.rx}${b.rxNote ? `（${b.rxNote}）` : ""}`;
  const tx = `↑ ${b.tx}${b.txNote ? `（${b.txNote}）` : ""}`;
  return b.note ? `${rx} ${tx}（${b.note}）` : `${rx} ${tx}`;
}
