export const BYTE_UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"] as const;

export type PrecisionTier = { readonly below: number; readonly digits: number };
export type ScaledSpec = {
  readonly base: number;
  readonly units: readonly string[];
  readonly tiers: (unitIndex: number) => readonly PrecisionTier[];
  readonly fallbackDigits?: number;
};

const ONE_DECIMAL_BELOW_10: readonly PrecisionTier[] = [{ below: 10, digits: 1 }];
const FINE_TIERS: readonly PrecisionTier[] = [{ below: 10, digits: 2 }, { below: 100, digits: 1 }];

// 按小数位数缓存十进制半进位格式化器：Number.prototype.toFixed 按二进制浮点精确值舍入，
// 9.95 与 9.995 在 IEEE 754 下略低于半值会被舍成 9.9 / 9.99；Intl.NumberFormat 按最短十进制表示做 halfExpand，
// 使十进制边界与视觉预期一致。仅用于有限非负数（非有限数由调用方先拦截）。
const fixedFormatters = new Map<number, Intl.NumberFormat>();
function fixed(abs: number, digits: number): string {
  let fmt = fixedFormatters.get(digits);
  if (!fmt) {
    fmt = new Intl.NumberFormat("en-US", { minimumFractionDigits: digits, maximumFractionDigits: digits, useGrouping: false });
    fixedFormatters.set(digits, fmt);
  }
  return fmt.format(abs);
}

// 按舍入后的值决定小数位数：若当前档位四舍五入后越过本档上界（例如 9.95 舍为 10.0、9.995 舍为 10.00），
// 则升入下一档（更少小数位），避免在门槛附近出现 "10.0%" 或 "10.00 ms" 这类位数跳变。
// 前提：tiers 按 below 严格递增、digits 单调递减；调用方保证 abs 为有限非负数。
function roundTiers(abs: number, tiers: readonly PrecisionTier[], fallbackDigits: number): string {
  for (const { below, digits } of tiers) {
    if (abs < below) {
      const text = fixed(abs, digits);
      if (Number(text) < below) return text;
    }
  }
  return fixed(abs, fallbackDigits);
}

// 统一的无单位数值格式化入口：对负数按绝对值应用同一位数规则，舍入结果为 0 时不带负号（不出现 "-0.0%"），
// 非有限数（NaN、±Infinity）直接转字符串以保持原样语义。
export function formatNumber(value: number, tiers: readonly PrecisionTier[], fallbackDigits = 0): string {
  if (!Number.isFinite(value)) return String(value);
  const text = roundTiers(Math.abs(value), tiers, fallbackDigits);
  return value < 0 && Number(text) !== 0 ? `-${text}` : text;
}

// 统一的进位换单位格式化入口：先按量级初选单位，再按该单位的位数规则四舍五入；
// 若舍入后的数值达到进位基数（如 1024 或 1000）且尚未到达最大单位，则继续换到下一单位重算，
// 保证已进位的值不会滞留在旧单位（如 1023.6 B 变为 1.0 KiB，1048575 B 变为 1.0 MiB）。
export function formatScaled(value: number, spec: ScaledSpec): string {
  if (!Number.isFinite(value)) return `${value} ${spec.units[0]}`;
  const fallback = spec.fallbackDigits ?? 0;
  let abs = Math.abs(value);
  let i = 0;
  while (abs >= spec.base && i < spec.units.length - 1) {
    abs /= spec.base;
    i++;
  }
  let text = roundTiers(abs, spec.tiers(i), fallback);
  while (Number(text) >= spec.base && i < spec.units.length - 1) {
    abs /= spec.base;
    i++;
    text = roundTiers(abs, spec.tiers(i), fallback);
  }
  const sign = value < 0 && Number(text) !== 0 ? "-" : "";
  return `${sign}${text} ${spec.units[i]}`;
}

// 二进制单位：内存与磁盘的读数都是字节，运维习惯 GiB 而非 GB。
export function bytes(n: number | bigint): string {
  return formatScaled(Number(n), {
    base: 1024,
    units: BYTE_UNITS,
    tiers: (i) => (i > 0 ? ONE_DECIMAL_BELOW_10 : []),
  });
}

export function percent(v: number): string {
  return `${formatNumber(v, ONE_DECIMAL_BELOW_10, 0)}%`;
}

export function duration(seconds: number | bigint): string {
  const raw = Number(seconds);
  if (!Number.isFinite(raw)) return String(raw);
  const neg = raw < 0;
  let s = Math.floor(Math.abs(raw));
  const d = Math.floor(s / 86400);
  s -= d * 86400;
  const h = Math.floor(s / 3600);
  s -= h * 3600;
  const m = Math.floor(s / 60);
  const sign = neg && (d > 0 || h > 0 || m > 0) ? "-" : "";
  if (d > 0) return `${sign}${d}d ${h}h`;
  if (h > 0) return `${sign}${h}h ${m}m`;
  return `${sign}${m}m`;
}

// 用 hub 给的 now 而不是浏览器时钟：客户机时钟偏了也不会显示"负几秒前"。
export function ago(unixSeconds: number | bigint, now: number | bigint): string {
  const diff = Math.max(0, Number(now) - Number(unixSeconds));
  if (diff < 5) return "刚刚";
  if (diff < 60) return `${Math.floor(diff)} 秒前`;
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
  return `${Math.floor(diff / 86400)} 天前`;
}

// 单位随数据来（MetricSeries.unit），显示层不查表。
export function formatUnit(v: number, unit: string): string {
  switch (unit) {
    case "ms":
      return `${formatNumber(v, FINE_TIERS, 0)} ms`;
    case "percent":
      return percent(v);
    case "bytes":
      return bytes(v);
    case "bytes/s":
      return `${bytes(v)}/s`;
    case "count/s":
      return `${formatNumber(v, FINE_TIERS, 0)}/s`;
    case "count":
      return formatNumber(v, [], 0);
    default:
      return formatNumber(v, [], 2);
  }
}

export type ClockParts = { year: string; month: string; day: string; hour: string; minute: string; second: string };

// 数字取自 Intl 的部件，不自备月份名表，也不随浏览器语言换顺序或换成 12 小时制；hourCycle h23 让午夜是 00 而不是 24。
// timeZone 缺省时用运行环境的本地时区；无效的时区名由 Intl 抛 RangeError。复用同一个 Intl 实例，逐个时间戳调用。
export function clockParts(timeZone?: string): (unixMs: number) => ClockParts | null {
  const options: Intl.DateTimeFormatOptions = { year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23" };
  if (timeZone !== undefined) options.timeZone = timeZone;
  const fmt = new Intl.DateTimeFormat("zh-CN", options);
  return (unixMs) => {
    if (!Number.isFinite(unixMs)) return null;
    const parts: Partial<ClockParts> = {};
    for (const p of fmt.formatToParts(unixMs)) {
      if (p.type === "year" || p.type === "month" || p.type === "day" || p.type === "hour" || p.type === "minute" || p.type === "second") parts[p.type] = p.value;
    }
    return parts.year && parts.month && parts.day && parts.hour && parts.minute && parts.second ? parts as ClockParts : null;
  };
}

// 面板与公开页统一的时刻写法 YYYY-MM-DD HH:mm:ss 与日期写法 YYYY-MM-DD。timeZone 传 hub 下发的时区名（如节点的 Traffic.timezone）时按那个时区的日界显示。
export function dateTime(unixSeconds: number | bigint, timeZone?: string): string {
  const p = clockParts(timeZone)(Number(unixSeconds) * 1000);
  return p ? `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}:${p.second}` : "";
}

export function day(unixSeconds: number | bigint, timeZone?: string): string {
  const p = clockParts(timeZone)(Number(unixSeconds) * 1000);
  return p ? `${p.year}-${p.month}-${p.day}` : "";
}

// YYYY-MM-DD 且是日历上存在的日子。0000 年不收，比 hub 的 alert.ParseDate（收 0000–9999）更严：日期控件的年份段要求非零的四位年份。
export function isDate(value: string): boolean {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(value) || value.startsWith("0000")) return false;
  const date = new Date(`${value}T00:00:00Z`);
  return Number.isFinite(date.getTime()) && date.toISOString().slice(0, 10) === value;
}

// 24 小时制 HH:MM。
export function isTime(value: string): boolean {
  return /^(?:[01]\d|2[0-3]):[0-5]\d$/.test(value);
}
