import { type FormEvent, useState } from "react";
import type { Node, Tag } from "../gen/heron/v1/admin_pb";
import { BillingCycle, TrafficQuotaMode } from "../gen/heron/v1/types_pb";
import { parseQuota, quotaInput, QUOTA_UNITS, type QuotaUnit } from "../lib/traffic";
import { errorBanner } from "../api/queryGate";
import { withId } from "../lib/ids";
import { literalPattern } from "../lib/fold";
import { sameTag, withTag, withoutTag } from "../lib/tags";
import { Drawer } from "../components/Modal";
import { Select } from "../components/Select";
import { SuggestInput } from "../components/SuggestInput";
import { BillingEditor } from "../components/BillingEditor";
import { NodeAddresses } from "../components/NodeAddresses";
import { lookupText, NodeCountry } from "../components/NodeCountry";
import { day } from "../lib/format";

const QUOTA_UNIT_OPTIONS = (Object.keys(QUOTA_UNITS) as QuotaUnit[]).map((unit) => ({ value: unit, label: unit }));
// 流量口径是数值枚举，自绘下拉只认字符串值：选项值写成枚举值的十进制，取回时再转成数。
const QUOTA_MODE_OPTIONS = [
  { value: String(TrafficQuotaMode.SUM), label: "收+发" }, { value: String(TrafficQuotaMode.RX), label: "只收" },
  { value: String(TrafficQuotaMode.TX), label: "只发" }, { value: String(TrafficQuotaMode.MAX), label: "收发取大者" },
];

// UpdateNode 整体替换全部可编辑字段；每次保存必须保留未编辑字段，不能用缺席表达“不变”。
// rented 缺席即 false：草稿漏掉当前值时，一次只改名称的保存会把已出租清掉。
const draftOf = (node: Node) => ({
  name: node.name, public: node.public, note: node.note, publicRemark: node.publicRemark, trafficResetDay: node.trafficResetDay, countryPin: node.countryPin,
  ipv4Pin: node.ipv4Pin, ipv6Pin: node.ipv6Pin,
  tags: [...node.tags], offlineGraceS: String(node.offlineGraceS ?? 0), maintenance: node.maintenance, rented: node.rented,
  trafficQuotaBytes: node.trafficQuotaBytes, trafficQuotaMode: node.trafficQuotaMode || TrafficQuotaMode.SUM,
  billing: {
    price: node.billing?.price ?? "", currency: node.billing?.currency ?? "", billingCycle: node.billing?.billingCycle ?? BillingCycle.UNSPECIFIED,
    expiresOn: node.billing?.expiresOn ?? "", autoRenew: node.billing?.autoRenew ?? false,
  },
});
type Draft = ReturnType<typeof draftOf>;
export type NodePatch = Omit<Draft, "offlineGraceS"> & { offlineGraceS: number };

export function NodeEditor({ node, knownTags, saving, error, listError, onClose, onSave, opener }: {
  node: Node; knownTags: readonly Tag[]; saving: boolean; error: unknown; listError: unknown;
  onClose: () => void; onSave: (patch: NodePatch) => void; opener: HTMLElement;
}) {
  const [draft, setDraft] = useState(() => draftOf(node));
  const [pendingTag, setPendingTag] = useState("");
  const [quota, setQuota] = useState(() => quotaInput(node.trafficQuotaBytes, "GiB"));
  const [unit, setUnit] = useState<QuotaUnit>("GiB");
  const [quotaTouched, setQuotaTouched] = useState(false);
  const quotaBytes = quotaTouched ? parseQuota(quota, unit) : node.trafficQuotaBytes;
  const label = withId(node.name, node.id);
  const valid = quotaBytes !== null && draft.name.trim() !== "" && Number.isInteger(draft.trafficResetDay) && draft.trafficResetDay >= 1 && draft.trafficResetDay <= 28 && /^\d+$/.test(draft.offlineGraceS);
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!saving && valid && quotaBytes !== null) onSave({ ...draft, ipv4Pin: draft.ipv4Pin.trim(), ipv6Pin: draft.ipv6Pin.trim(), trafficQuotaBytes: quotaBytes, tags: withTag(draft.tags, pendingTag), offlineGraceS: Number(draft.offlineGraceS) });
  };
  return <Drawer title={`编辑节点 · ${label}`} busy={saving} onClose={onClose} opener={opener}>
    <form onSubmit={submit}>
      <div className="modal-body">
        {errorBanner(error, listError)}
        <fieldset className="bare" disabled={saving}>
          <section className="form-section" aria-label="基本">
            <h3>基本</h3>
            <label>节点名称<input data-autofocus aria-label={`名称 ${label}`} value={draft.name} onChange={(e) => setDraft({ ...draft, name: e.target.value })} /></label>
            <label className="switch-field"><span>公开显示<small>关闭后仅在管理后台可见</small></span><input type="checkbox" aria-label={`公开 ${label}`} checked={draft.public} onChange={(e) => setDraft({ ...draft, public: e.target.checked })} /></label>
            <label>公开备注<input aria-label={`公开备注 ${label}`} placeholder="对访客可见的一行说明，如线路类型" value={draft.publicRemark} onChange={(e) => setDraft({ ...draft, publicRemark: e.target.value })} /><span className="muted">随公开页展示；至多 100 字、单行，留空不显示。</span></label>
            <label>备注<textarea aria-label={`备注 ${label}`} placeholder="商家、用途或其他内部备注" value={draft.note} onChange={(e) => setDraft({ ...draft, note: e.target.value })} /></label>
            <div><label htmlFor={`tag-input-${node.id}`}>节点标签</label><TagsEditor id={node.id} label={label} isPublic={draft.public} tags={draft.tags} known={knownTags} pending={pendingTag} onPending={setPendingTag} onChange={(tags) => setDraft({ ...draft, tags })} /></div>
          </section>
          <section className="form-section" aria-label="地区">
            <h3>地区</h3>
            {/* 查得值依赖 agent 探测到的出口地址，地址诊断与来源 IP 放在同一组：读者要判断"查得值为什么是这个"时，依据就在旁边。 */}
            <div className="address-diagnostics"><NodeAddresses network={node.network} reported={node.facts?.network} detailed /><p className="muted">agent 每 5 分钟探测一次出口，时间是当前结果首次出现的时刻。无可用地址或路由标记为不支持；超时与服务异常标记为探测失败。手填的地址优先显示，并标为手填。探测失败或没探测过、而 hub 在上报上看到该族公网来源时，显示该来源并标"上报来源"。</p></div>
            <label>手填 IPv4 地址<input aria-label={`手填 IPv4 地址 ${label}`} aria-describedby={`pin-hint-${node.id}`} placeholder="例如 8.8.8.8" spellCheck={false} value={draft.ipv4Pin} onChange={(e) => setDraft({ ...draft, ipv4Pin: e.target.value })} /></label>
            <label>手填 IPv6 地址<input aria-label={`手填 IPv6 地址 ${label}`} aria-describedby={`pin-hint-${node.id}`} placeholder="例如 2606:4700::1111" spellCheck={false} value={draft.ipv6Pin} onChange={(e) => setDraft({ ...draft, ipv6Pin: e.target.value })} /></label>
            <p className="muted" id={`pin-hint-${node.id}`}>留空用 agent 探测；填入后该族停止探测，清空即恢复。须是该族的公网地址，用于探测不到（出站 HTTPS 受限）或入口与出口不同的主机；不影响国家 / 地区的查询。</p>
            <div><span className="field-label">上报来源 IP</span><p>{node.lastSource ? <code>{node.lastSource}</code> : <span className="muted">尚未记录来源</span>}</p><p className="muted">hub 实际观察到的来源，经过反代时依赖可信代理配置；与 agent 探测结果独立。</p></div>
            <label>手动指定国家 / 地区<input aria-label={`手动指定国家 / 地区 ${label}`} aria-describedby={`country-hint-${node.id}`} placeholder="例如 JP，留空自动查询" value={draft.countryPin} onChange={(e) => setDraft({ ...draft, countryPin: e.target.value.toUpperCase() })} /><span className="muted" id={`country-hint-${node.id}`}>两个字母（ISO 3166-1），优先于查得值；留空用查得值：{node.countryLookup ? lookupText(node) : "尚无查得值"}。</span></label>
            <p className="muted"><NodeCountry node={node} /> · 创建于 {day(node.createdAt)}</p>
          </section>
          <section className="form-section" aria-label="费用">
            <h3>费用</h3>
            <BillingEditor label={label} draft={draft.billing} onChange={(patch) => setDraft({ ...draft, billing: { ...draft.billing, ...patch } })} />
          </section>
          <section className="form-section" aria-label="运行">
            <h3>运行</h3>
            <label>周期流量配额<input aria-label={`流量配额 ${label}`} inputMode="decimal" value={quota} onChange={(e) => { setQuota(e.target.value); setQuotaTouched(true); }} /><span className="muted">留空为未设配额；小数换算后四舍五入到整数字节，须在 1 至 2^62 字节（不含）之间。</span>{quotaBytes === null && <span role="alert">配额无效，换算后必须是有效的正整数字节数。</span>}</label>
            <Select label={`配额单位 ${label}`} caption="配额单位" value={unit} options={QUOTA_UNIT_OPTIONS} onChange={(next) => { setUnit(next); setQuotaTouched(true); }} />
            <Select label={`流量口径 ${label}`} caption="流量口径" value={String(draft.trafficQuotaMode)} options={QUOTA_MODE_OPTIONS} onChange={(next) => setDraft({ ...draft, trafficQuotaMode: Number(next) })} />
            <label>每月流量重置日<input type="number" min={1} max={28} aria-label={`重置日 ${label}`} value={draft.trafficResetDay} onChange={(e) => setDraft({ ...draft, trafficResetDay: Number(e.target.value) })} /><span className="muted">若从本周期起点算起新的重置日已经过去，本周期用量会立即清零。</span></label>
            <label>离线宽限期（秒）<input type="number" min={0} aria-label={`离线宽限期（秒） ${label}`} aria-describedby={`grace-hint-${node.id}`} value={draft.offlineGraceS} onChange={(e) => setDraft({ ...draft, offlineGraceS: e.target.value })} /><span className="muted" id={`grace-hint-${node.id}`}>0 表示取 hub 的 HERON_OFFLINE_AFTER；非 0 不能小于它。</span></label>
            <label className="switch-field"><span>维护中<small>告警事件照常记录但不投递，在线状态照实显示；到期的提醒不受影响；与已出租互斥</small></span><input type="checkbox" aria-label={`维护 ${label}`} checked={draft.maintenance} disabled={draft.rented} onChange={(e) => setDraft({ ...draft, maintenance: e.target.checked })} /></label>
            <label className="switch-field"><span>已出租<small>机器租给了别人、不会有上报：离线与到期都不评估，不参与 agent 更新；与维护中互斥</small></span><input type="checkbox" aria-label={`已出租 ${label}`} checked={draft.rented} disabled={draft.maintenance} onChange={(e) => setDraft({ ...draft, rented: e.target.checked })} /></label>
          </section>
        </fieldset>
      </div>
      <footer className="modal-footer"><button type="button" onClick={onClose} disabled={saving}>取消</button><button type="submit" className="primary-button" disabled={saving || !valid} aria-busy={saving}>保存</button></footer>
    </form>
  </Drawer>;
}

function TagsEditor({ id, label, isPublic, tags, known, pending, onPending, onChange }: {
  id: bigint; label: string; isPublic: boolean; tags: readonly string[]; known: readonly Tag[]; pending: string; onPending: (text: string) => void; onChange: (tags: string[]) => void;
}) {
  const add = () => { onChange(withTag(tags, pending)); onPending(""); };
  // 候选是已有标签里节点还没带的那些（按折叠比较），按输入的文字做折叠字面匹配；空输入时列出全部。
  const query = pending.trim();
  const pattern = query === "" ? null : literalPattern(query, false);
  const suggestions = known.map((tag) => tag.name).filter((name) => !tags.some((t) => sameTag(t, name)) && (!pattern || pattern.test(name)));
  return <div className="tags-edit">
    <div>{tags.map((tag) => <span key={tag} className="tag">{tag}<button type="button" className="link" aria-label={`移除标签 ${tag} ${label}`} onClick={() => onChange(withoutTag(tags, tag))}>×</button></span>)}</div>
    <div className="tags-input-row"><SuggestInput id={`tag-input-${id}`} label={`新标签 ${label}`} placeholder="选择已有标签，或输入新标签" value={pending} onChange={onPending}
      suggestions={suggestions} onPick={(name) => { onChange(withTag(tags, name)); onPending(""); }} onEnter={add} />
    <button type="button" aria-label={`添加标签 ${label}`} onClick={add}>添加</button></div>
    <p className="muted">大小写不敏感，已有的标签沿用先建的写法；每个节点至多 16 个。</p>
    {isPublic && <p className="muted">公开节点的标签在公开页对访客可见。</p>}
  </div>;
}
