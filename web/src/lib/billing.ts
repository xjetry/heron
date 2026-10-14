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

export const BILLING_CYCLES: readonly { value: BillingCycle; label: string }[] = [
  { value: BillingCycle.MONTHLY, label: "月" },
  { value: BillingCycle.QUARTERLY, label: "季" },
  { value: BillingCycle.SEMIANNUAL, label: "半年" },
  { value: BillingCycle.YEARLY, label: "年" },
  { value: BillingCycle.BIENNIAL, label: "两年" },
  { value: BillingCycle.TRIENNIAL, label: "三年" },
  { value: BillingCycle.QUINQUENNIAL, label: "五年" },
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
// 非三字母大写或数额不是纯小数的遗留值过不了本地检查：两种都原样显示、不抛错。
function moneyText(currency: string, price: string): string {
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
