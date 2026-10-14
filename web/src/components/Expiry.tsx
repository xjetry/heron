import { remainingText, type BillingView } from "../lib/billing";
import { expiryLevel } from "../lib/status";

// 到期日与剩余天数分两段：日期进等宽的 .num，中文的剩余天数不进；两段共用一个 data-level，临近与已过期整体着色。
// 剩余天数取 hub 下发的 daysLeft（BillingView 注释说明了为什么不在浏览器里算），没有时只写日期。没有到期日时不渲染。
// rented 必填：已出租的日期照常显示，但不上色（expiryLevel 对它恒为中性）。漏传会按未出租上色。
export function Expiry({ billing, rented }: { billing: BillingView | undefined; rented: boolean }) {
  if (!billing || billing.expiresOn === "") return null;
  const { expiresOn, daysLeft } = billing;
  return (
    <span className="expiry" data-level={expiryLevel(daysLeft, rented)}>
      <span className="num">{expiresOn}</span>
      {daysLeft !== undefined && <span className="expiry-remaining">{remainingText(daysLeft)}</span>}
    </span>
  );
}
