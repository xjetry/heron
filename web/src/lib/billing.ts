import { BillingCycle } from "../gen/heron/v1/types_pb";
import { addDays } from "./ymd";

// 面板的 Billing（types_pb）与公开页的 PublicBilling（public_pb）共有的字段；自动续期只在管理端有，这里不用。
// 节点五项都没填时 hub 不下发 billing，所以下面的函数都接受 undefined。
// 本文件只 import types_pb：公开入口也引用它，不能经它带进管理服务的生成代码（importScan.test.ts 钉住）。
export type BillingView = {
  price: string;
  currency: string;
  billingCycle: BillingCycle;
  expiresOn: string;
  // hub 按 --timezone 算好的剩余天数；负数是已过期天数，没有到期日时缺失。浏览器不自己再算：访客的时区与 hub 不同时，按本地日期算可能差一天。
  daysLeft?: number;
};

// months 是周期的月数（与 types.proto 里各枚举值的注释一致），持有成本按它摊到每月。
export const BILLING_CYCLES: readonly { value: BillingCycle; label: string; months: number }[] = [
  { value: BillingCycle.MONTHLY, label: "月", months: 1 },
  { value: BillingCycle.QUARTERLY, label: "季", months: 3 },
  { value: BillingCycle.SEMIANNUAL, label: "半年", months: 6 },
  { value: BillingCycle.YEARLY, label: "年", months: 12 },
  { value: BillingCycle.BIENNIAL, label: "两年", months: 24 },
  { value: BillingCycle.TRIENNIAL, label: "三年", months: 36 },
  { value: BillingCycle.QUINQUENNIAL, label: "五年", months: 60 },
];

// 未指定是"没有周期"，给空串；表外的值（hub 比页面新）显示编号而不是空，免得读成没有周期。
export function cycleLabel(c: BillingCycle): string {
  if (c === BillingCycle.UNSPECIFIED) return "";
  return BILLING_CYCLES.find((e) => e.value === c)?.label ?? `未知（${c}）`;
}

// 货币金额文本，管理与公开共用：符号按 zh-CN 的货币习惯（USD→US$、JPY→JP¥、CNY→¥，取自
// Intl.NumberFormat(...).formatToParts），数额保留存储的小数位数（minimumFractionDigits =
// maximumFractionDigits = 存储值的小数位）——不按币种默认精度四舍五入（JPY 12.5 显示 12.5 而不是 13），
// 整数也不补 ".00"。未知三字母币种 CLDR 把代码本身当符号回退，个别引擎直接拒绝时落入 catch；
// 非三字母大写或数额不是纯小数的值（hub 读出的不会是这样，见 internal/hub/store/billing.go）过不了本地检查：两种都
// 原样显示、不抛错——这里只防 Intl 对非法输入抛错，不承担格式准入。
export function moneyText(currency: string, price: string): string {
  if (!/^\d+(\.\d+)?$/.test(price) || !/^[A-Z]{3}$/.test(currency)) return `${currency} ${price}`;
  const decimals = price.includes(".") ? price.length - price.indexOf(".") - 1 : 0;
  try {
    return new Intl.NumberFormat("zh-CN", { style: "currency", currency, minimumFractionDigits: decimals, maximumFractionDigits: decimals })
      .formatToParts(Number(price)).map((part) => part.value).join("");
  } catch {
    return `${currency} ${price}`;
  }
}

// "US$12.50 / 月"；没有价格但有周期时是“每月”；都没有时是空串。
export function priceText(b: BillingView | undefined): string {
  if (!b) return "";
  const cycle = cycleLabel(b.billingCycle);
  if (b.price === "") return cycle && `每${cycle}`;
  const amount = moneyText(b.currency, b.price);
  return cycle ? `${amount} / ${cycle}` : amount;
}

// daysLeft 是 BillingView.daysLeft：负数为已过期天数。
export function remainingText(daysLeft: number): string {
  return daysLeft < 0 ? `已过期 ${-daysLeft} 天` : `剩 ${daysLeft} 天`;
}

// 按到期从早到晚排：已过期的到期最早，排最前；没有到期日、或到期日无法解析（hub 不下发 daysLeft）的排最后。
// 排序键取 hub 下发的 daysLeft 而不是 expiresOn 字符串：同一次 GetSnapshot 里所有节点的 daysLeft 由同一个 today 算出，
// 按它排就是按到期日排；无法解析的到期日没有顺序可言，字符串比较会把它按字典序塞进有效日期之间。到期相同的保持传入的
// 相对顺序（Array.prototype.sort 自 ES2019 起稳定）；不改动传入的数组。
export function sortByExpiry<T extends { billing?: BillingView }>(nodes: readonly T[]): T[] {
  const key = (n: T) => n.billing?.daysLeft ?? Infinity;
  return [...nodes].sort((a, b) => {
    const ka = key(a);
    const kb = key(b);
    return ka === kb ? 0 : ka < kb ? -1 : 1;
  });
}

// hub 时区（--timezone）的今天，由 hub 下发的 days_left 反推：expiresOn − daysLeft 天。同一次 ListNodes 里所有节点的
// daysLeft 由同一个 today 算出（hub 的 ListNodes 只读一次钟），取第一个有 daysLeft 的节点即可。没有 daysLeft 的节点
// （没有到期日，或到期日 hub 读不懂）不参与；全都没有时返回 undefined——浏览器的时钟与时区不能代替 hub 的今天。
export function hubToday(billings: readonly (BillingView | undefined)[]): string | undefined {
  for (const b of billings) {
    if (b?.daysLeft !== undefined) return addDays(b.expiresOn, -b.daysLeft);
  }
  return undefined;
}

// 按到期日分组，键是 hub 下发的 expiresOn 原文；没有 daysLeft 的节点不进任何一天（与 hubToday 同一口径：hub 读不懂的
// 日期不该被浏览器解释成某一天）。组内保持传入顺序。
export function byExpiryDay<T extends { billing?: BillingView }>(nodes: readonly T[]): Map<string, T[]> {
  const days = new Map<string, T[]>();
  for (const n of nodes) {
    if (n.billing?.daysLeft === undefined) continue;
    const list = days.get(n.billing.expiresOn);
    if (list) list.push(n);
    else days.set(n.billing.expiresOn, [n]);
  }
  return days;
}

// 能否"已续费"：hub 的 RenewNodeBilling 要求周期与到期日都有，缺一项就拒绝；这里只决定按钮显不显示，推后的日期由
// hub 计算，浏览器不重算。
export function canRenew(b: BillingView | undefined): boolean {
  return b !== undefined && b.billingCycle !== BillingCycle.UNSPECIFIED && b.expiresOn !== "";
}

// 待续费现金流的窗口：hub 下发的 daysLeft 落在 [0, RENEWAL_WINDOW_DAYS] 内的节点计入。与 status.ts 的 EXPIRING_DAYS
// （到期上色与「N 天内到期」筛选）是两个口径：这里回答"近一个月要掏多少钱"，那边回答"哪些节点该看一眼"。
export const RENEWAL_WINDOW_DAYS = 30;

// 一个币种的汇总行。金额是十进制文本，格式同 Billing.price（整数不补 ".00"，否则两位小数），直接交给 moneyText 显示。
export type CurrencyTotal = { currency: string; count: number; amount: string };
// monthly 是月均，yearly 是它乘 12 的年化参考；两者各自由精确合计舍入到分，所以 yearly 不一定等于 monthly × 12。
export type AmortizedTotal = { currency: string; count: number; monthly: string; yearly: string };

export type CostSummary = {
  // 待续费现金流：0 ≤ daysLeft ≤ RENEWAL_WINDOW_DAYS 的自用节点，含开着自动续期的。
  upcoming: CurrencyTotal[];
  // 逾期未处理：daysLeft < 0 的自用节点。单列而不并进 upcoming：它们是已经错过的付款，并进去会把"近 30 天要付多少"
  // 说多，丢掉又会让站长看不到还没处理的到期。开着自动续期的节点 hub 在日界就推后到期日，正常不会停在这里。
  overdue: CurrencyTotal[];
  // 月均持有成本：全部自用节点按周期月数摊销，与到期日远近、有没有到期日无关。
  amortized: AmortizedTotal[];
  // 已出租节点的月均金额，供页面作旁注；不并进上面任何一项。
  rentedAmortized: AmortizedTotal[];
};

// hub 读出的价格与币种都满足保存时的格式（internal/hub/store/billing.go 的 CheckPriceCurrency：写入口、快照恢复预检与
// 迁移 54 的存量规范化共用这一个判定），价格非空时币种必然是三个大写字母。这里只消费：PRICE 与 hub 的价格格式同形，
// 用来拆出整数与小数两段，不匹配只会是没填价格。价格至多两位小数，所以按"分"（1/100）存成 BigInt 定点数，
// 相加没有浮点误差。
const PRICE = /^(\d{1,9})(?:\.(\d{1,2}))?$/;

function priceCents(b: BillingView): bigint | undefined {
  const m = PRICE.exec(b.price);
  if (!m) return undefined;
  return BigInt(m[1]) * 100n + BigInt((m[2] ?? "").padEnd(2, "0"));
}

// 分 → 十进制文本：整数不带小数，否则补足两位（与价格的存储格式一致）。只处理非负数：价格不会是负的。
function centsText(cents: bigint): string {
  const whole = cents / 100n;
  const frac = cents % 100n;
  return frac === 0n ? String(whole) : `${whole}.${String(frac).padStart(2, "0")}`;
}

// 摊销的定点单位是"分 / SHARES"，SHARES 取周期表全部月数的最小公倍数（1、3、6、12、24、36、60 → 360）：任一周期的
// 价格摊到每月都是整数个单位，币种内先精确合计、最后只舍入一次，不会把逐台舍入的误差乘进年化。周期表加了新月数时
// SHARES 跟着变，不用改这里。
const gcd = (a: bigint, b: bigint): bigint => (b === 0n ? a : gcd(b, a % b));
const SHARES = BILLING_CYCLES.reduce((l, { months }) => (l * BigInt(months)) / gcd(l, BigInt(months)), 1n);
const MONTHS = new Map(BILLING_CYCLES.map(({ value, months }) => [value, BigInt(months)]));

// 单位换回分，四舍五入（半分进位）；只处理非负数。
const sharesToCents = (shares: bigint) => (2n * shares + SHARES) / (2n * SHARES);

// 按币种累加，输出按币种代码排序，页面每次刷新顺序不跳。
class Totals {
  private readonly byCurrency = new Map<string, { count: number; sum: bigint }>();
  add(currency: string, value: bigint) {
    const t = this.byCurrency.get(currency) ?? { count: 0, sum: 0n };
    t.count += 1;
    t.sum += value;
    this.byCurrency.set(currency, t);
  }
  lines<T>(line: (currency: string, count: number, sum: bigint) => T): T[] {
    return [...this.byCurrency.keys()].sort().map((c) => {
      const t = this.byCurrency.get(c)!;
      return line(c, t.count, t.sum);
    });
  }
}

// 续费日历顶部的费用概览，完全在浏览器里由 ListNodes 的计费字段算出：hub 不汇总价格（见 types.proto 的 Billing）。
// 各币种分列、不做汇率换算。只计"填了价格与周期"的节点：没有周期的是一次性买断，没有续费也没有摊销。
// 已出租节点的续费是租约的事，不进自用的现金流与持有成本（与 expiryLevel 对已出租恒为中性同一口径），只单列月均。
// 窗口按 hub 下发的 daysLeft 判定：日历日是 hub 时区的，浏览器的时钟与时区不参与（同 hubToday）；没有 daysLeft 的
// 节点（没填到期日或 hub 读不懂）不进现金流与逾期，但照常摊销。周期是表外值（hub 比页面新）时不知道月数，
// 只进现金流与逾期、不摊销。
export function costSummary(nodes: readonly { rented: boolean; billing?: BillingView }[]): CostSummary {
  const upcoming = new Totals();
  const overdue = new Totals();
  const amortized = new Totals();
  const rentedAmortized = new Totals();
  for (const n of nodes) {
    const b = n.billing;
    if (!b || b.billingCycle === BillingCycle.UNSPECIFIED) continue;
    const cents = priceCents(b);
    if (cents === undefined) continue;
    const months = MONTHS.get(b.billingCycle);
    if (months !== undefined) (n.rented ? rentedAmortized : amortized).add(b.currency, (cents * SHARES) / months);
    if (n.rented || b.daysLeft === undefined) continue;
    if (b.daysLeft < 0) overdue.add(b.currency, cents);
    else if (b.daysLeft <= RENEWAL_WINDOW_DAYS) upcoming.add(b.currency, cents);
  }
  const total = (currency: string, count: number, sum: bigint): CurrencyTotal => ({ currency, count, amount: centsText(sum) });
  const amortizedTotal = (currency: string, count: number, shares: bigint): AmortizedTotal =>
    ({ currency, count, monthly: centsText(sharesToCents(shares)), yearly: centsText(sharesToCents(shares * 12n)) });
  return { upcoming: upcoming.lines(total), overdue: overdue.lines(total), amortized: amortized.lines(amortizedTotal), rentedAmortized: rentedAmortized.lines(amortizedTotal) };
}
