import { BillingCycle } from "../gen/heron/v1/types_pb";
import { BILLING_CYCLES } from "../lib/billing";
import { DateInput } from "./DateInput";

// 计费五项的表单草稿：字符串与枚举原样进请求，校验在 hub（billingOf），表单不另设一套规则。
export type BillingDraft = {
  price: string;
  currency: string;
  billingCycle: BillingCycle;
  expiresOn: string;
  autoRenew: boolean;
};

export const emptyBillingDraft = (): BillingDraft => ({ price: "", currency: "", billingCycle: BillingCycle.UNSPECIFIED, expiresOn: "", autoRenew: false });

// 任一字段非缺省才算"填了"：创建请求只在填了时带 billing，与缺省即不填的语义一致。
export function billingDraftSet(draft: BillingDraft): boolean {
  return draft.price !== "" || draft.currency !== "" || draft.billingCycle !== BillingCycle.UNSPECIFIED || draft.expiresOn !== "" || draft.autoRenew;
}

const BILLING_CURRENCIES = ["CNY", "USD", "HKD", "CAD", "EUR", "GBP"];

// 节点编辑器与添加节点弹窗共用的计费表单。币种与付款周期是平铺单选：选项固定且少，全部摆出来比下拉框少一次点击。
export function BillingEditor({ label, draft, autoFocus, onChange }: {
  label: string; draft: BillingDraft; autoFocus?: boolean; onChange: (patch: Partial<BillingDraft>) => void;
}) {
  // 可选范围外的当前币种（协议比页面新时）只读回显：选中但禁用，可改选支持币种，改走之后回不来，与原来下拉框的禁用选项一致。
  const foreignCurrency = draft.currency !== "" && !BILLING_CURRENCIES.includes(draft.currency);
  return <div className="billing-edit">
    <label>价格<input {...(autoFocus ? { "data-autofocus": true } : {})} aria-label={`价格 ${label}`} inputMode="decimal" placeholder="12.50" value={draft.price} onChange={(e) => onChange({ price: e.target.value })} /></label>
    <fieldset className="picks"><legend>币种</legend>
      <label><input type="radio" name={`currency-${label}`} checked={draft.currency === ""} onChange={() => onChange({ currency: "" })} />未设置</label>
      {BILLING_CURRENCIES.map((currency) => <label key={currency}><input type="radio" name={`currency-${label}`} checked={draft.currency === currency} onChange={() => onChange({ currency })} />{currency}</label>)}
      {foreignCurrency && <label><input type="radio" name={`currency-${label}`} checked disabled />{draft.currency}（当前值）</label>}
    </fieldset>
    <fieldset className="picks"><legend>付款周期</legend>
      <label><input type="radio" name={`cycle-${label}`} checked={draft.billingCycle === BillingCycle.UNSPECIFIED} onChange={() => onChange({ billingCycle: BillingCycle.UNSPECIFIED })} />无周期</label>
      {BILLING_CYCLES.map(({ value, label: cycle }) => <label key={value}><input type="radio" name={`cycle-${label}`} checked={draft.billingCycle === value} onChange={() => onChange({ billingCycle: value })} />每{cycle}</label>)}
    </fieldset>
    <DateInput label={`到期日 ${label}`} caption="到期日" hint="年满四位、月满两位自动跳转；可留空" value={draft.expiresOn} onChange={(expiresOn) => onChange({ expiresOn })} />
    <label className="switch-field"><span>自动续期<small>到期后自动按付款周期推后日期</small></span><input type="checkbox" aria-label={`自动续期 ${label}`} checked={draft.autoRenew} onChange={(e) => onChange({ autoRenew: e.target.checked })} /></label>
    <p className="muted">只用于展示与到期提醒。开着自动续期时，到期日过了 hub 按周期推后；需要周期与到期日。</p>
  </div>;
}
