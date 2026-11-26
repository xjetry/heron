import { literalPattern } from "../lib/fold";
import { knownTimeZone, timeZoneNames } from "../lib/timeZones";
import { SuggestInput } from "./SuggestInput";

// 计费时区输入，节点编辑器与添加节点抽屉共用。留空表示沿用 hub 的 --timezone（缺省，不是放宽）；候选是浏览器认得的
// IANA 名，按输入做折叠字面匹配。valid 由调用方据 billingTimezoneValid 决定能否保存。
export function BillingTimezoneInput({ id, label, value, onChange }: { id: string; label: string; value: string; onChange: (zone: string) => void }) {
  const query = value.trim();
  const pattern = query === "" ? null : literalPattern(query, false);
  const suggestions = timeZoneNames().filter((name) => name !== query && (!pattern || pattern.test(name)));
  return <div>
    <label htmlFor={id}>计费时区</label>
    <SuggestInput id={id} label={`计费时区 ${label}`} placeholder="留空沿用 hub 的时区，如 UTC、Asia/Shanghai" value={value} onChange={onChange} suggestions={suggestions} onPick={onChange} onEnter={() => undefined} />
    <p className="muted">IANA 时区名。流量周期的重置日与重置时刻、到期剩余天数与自动续期的日界都按它；留空沿用 hub 的 --timezone。</p>
    {!billingTimezoneValid(value) && <p role="alert">浏览器不认得这个时区名；请从候选里选，或留空。</p>}
  </div>;
}

// 能否保存：留空，或去首尾空白后浏览器认得。提交时调用方传去首尾空白后的值。
export const billingTimezoneValid = (value: string) => value.trim() === "" || knownTimeZone(value.trim());
