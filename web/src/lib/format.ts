const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

// 二进制单位：内存与磁盘的读数都是字节，运维习惯 GiB 而非 GB。
export function bytes(n: number | bigint): string {
  let x = Number(n);
  let i = 0;
  while (x >= 1024 && i < units.length - 1) {
    x /= 1024;
    i++;
  }
  const digits = i > 0 && x < 10 ? 1 : 0;
  return `${x.toFixed(digits)} ${units[i]}`;
}

export function percent(v: number): string {
  return `${v.toFixed(v < 10 ? 1 : 0)}%`;
}

export function duration(seconds: number | bigint): string {
  let s = Number(seconds);
  const d = Math.floor(s / 86400);
  s -= d * 86400;
  const h = Math.floor(s / 3600);
  s -= h * 3600;
  const m = Math.floor(s / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

// 用 hub 给的 now 而不是浏览器时钟：客户机时钟偏了也不会显示"负几秒前"。
export function ago(unixSeconds: number | bigint, now: number | bigint): string {
  const diff = Math.max(0, Number(now) - Number(unixSeconds));
  if (diff < 5) return "刚刚";
  if (diff < 60) return `${diff} 秒前`;
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
  return `${Math.floor(diff / 86400)} 天前`;
}

// 单位随数据来（MetricSeries.unit），显示层不查表。
export function formatUnit(v: number, unit: string): string {
  switch (unit) {
    case "ms":
      return `${v.toFixed(v < 10 ? 2 : v < 100 ? 1 : 0)} ms`;
    case "percent":
      return percent(v);
    case "bytes":
      return bytes(v);
    case "bytes/s":
      return `${bytes(v)}/s`;
    case "count/s":
      return `${v.toFixed(v < 10 ? 2 : v < 100 ? 1 : 0)}/s`;
    case "count":
      return String(Math.round(v));
    default:
      return v.toFixed(2);
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

// 面板与公开页统一的时刻写法 YYYY-MM-DD HH:mm:ss 与日期写法 YYYY-MM-DD。timeZone 传 hub 的时区（如 GetTraffic.timezone）时按 hub 的日界显示。
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
