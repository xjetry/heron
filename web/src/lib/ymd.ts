// 日历日（YYYY-MM-DD）的运算，日期选择器（components/Calendar.tsx）与续费日历（pages/Renewals.tsx）共用。
// 全部在 UTC 上做：只关心日历日，不经过任何时区，夏令时切换日不会少一天或多一天，结果也与浏览器所在的时区无关。
// 本文件不取"今天"：今天是谁的今天由调用方决定（日期选择器用浏览器本地日期，续费日历用 hub 下发的 days_left 反推）。

// 用 setUTCFullYear 而不是 Date.UTC：后者把 0–99 年当成 1900–1999 年，而 isDate 收 0001 年起的日期。
function utc(year: number, monthIndex: number, day: number): number {
  const date = new Date(0);
  date.setUTCFullYear(year, monthIndex, day);
  return date.getTime();
}
const toUtc = (ymd: string) => { const [y, m, d] = ymd.split("-").map(Number); return utc(y, m - 1, d); };
const fromUtc = (ms: number) => new Date(ms).toISOString().slice(0, 10);

export const addDays = (ymd: string, n: number) => fromUtc(toUtc(ymd) + n * 86_400_000);

// 跨月时日子夹到目标月的最后一天：1 月 31 日的下个月是 2 月 28 / 29 日，不溢出到 3 月。
export function addMonths(ymd: string, n: number): string {
  const [y, m, d] = ymd.split("-").map(Number);
  const first = new Date(utc(y, m - 1 + n, 1));
  const last = new Date(utc(first.getUTCFullYear(), first.getUTCMonth() + 1, 0)).getUTCDate();
  return fromUtc(utc(first.getUTCFullYear(), first.getUTCMonth(), Math.min(d, last)));
}

// 星期几，周一为 0。
export const weekdayOf = (ymd: string) => (new Date(toUtc(ymd)).getUTCDay() + 6) % 7;

// ymd 所在月的月历：周一开头，6 行 42 格，前后月份的日子补齐首尾两行。
export function monthDays(ymd: string): string[] {
  const first = `${ymd.slice(0, 7)}-01`;
  const start = addDays(first, -weekdayOf(first));
  return Array.from({ length: 42 }, (_, i) => addDays(start, i));
}

// 月历的键盘移动：方向键按日 / 周，PageUp / PageDown 换月，Home / End 到本周首尾；其它键返回 undefined。
export function calendarStep(key: string, ymd: string): string | undefined {
  switch (key) {
    case "ArrowLeft": return addDays(ymd, -1);
    case "ArrowRight": return addDays(ymd, 1);
    case "ArrowUp": return addDays(ymd, -7);
    case "ArrowDown": return addDays(ymd, 7);
    case "PageUp": return addMonths(ymd, -1);
    case "PageDown": return addMonths(ymd, 1);
    case "Home": return addDays(ymd, -weekdayOf(ymd));
    case "End": return addDays(ymd, 6 - weekdayOf(ymd));
  }
  return undefined;
}
