import { dateTime } from "./format";

// API token 有效期的判断与计算以 hub 墙钟为准（ListApiTokensResponse.now 加上自取回列表以来经过的时间），不用浏览器时钟：
// 两者可能相差数分钟，一小时的 token 在偏快的浏览器上会被算短，判断"已过期"也会与 hub 不一致。
export type ExpiryChoice = "keep" | "never" | "1h" | "1d" | "7d" | "30d" | "custom";

export const EXPIRY_PRESETS: ReadonlyArray<readonly [ExpiryChoice, string, bigint]> = [
  ["1h", "1 小时", 3_600n],
  ["1d", "1 天", 86_400n],
  ["7d", "7 天", 604_800n],
  ["30d", "30 天", 2_592_000n],
];

// expires_at 是失效的第一个时刻：hub 墙钟到达它即过期，与 hub 的判定（store.APIToken.Expired）同一边界。
export function isExpired(expiresAt: bigint | undefined, now: bigint): boolean {
  return expiresAt !== undefined && now >= expiresAt;
}

export function expiryText(expiresAt: bigint | undefined, now: bigint): string {
  if (expiresAt === undefined) return "不过期";
  return isExpired(expiresAt, now) ? `已过期（${dateTime(expiresAt)}）` : `至 ${dateTime(expiresAt)}`;
}

// resolveExpiry 把下拉的选择换成请求里的 expires_at：undefined 表示字段缺失（不过期），null 表示自定义时刻填得不对、
// 由表单拦下。custom 是 datetime-local 的值，按浏览器时区解释成绝对时刻（不带偏移的日期时间按本地时间解析）。
export function resolveExpiry(choice: ExpiryChoice, now: bigint, custom: string, current: bigint | undefined): bigint | undefined | null {
  switch (choice) {
    case "keep":
      return current;
    case "never":
      return undefined;
    case "custom": {
      const ms = Date.parse(custom);
      return custom === "" || Number.isNaN(ms) ? null : BigInt(Math.floor(ms / 1000));
    }
    default: {
      const preset = EXPIRY_PRESETS.find(([value]) => value === choice);
      return preset ? now + preset[2] : null;
    }
  }
}
