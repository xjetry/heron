import { Fragment, useId, useRef, useState, type Ref } from "react";
import type { Facet, FacetMode, FacetOption } from "../lib/facets";
import { literalPattern } from "../lib/fold";
import type { TagMatch } from "../lib/tags";
import { Icon } from "./Icon";

// 地区与标签由快照动态产生，数量不定；FacetFilters 用单个 openFacet 保证至多展开一项，筛选行只放入口，选项在下方折行。
// 公开页总览与管理端节点页共用这一份。选项多于 FACET_SEARCH_AT 个时面板里出现搜索框，几十个标签也能先收窄再点。
// 选项、选择集与选择方式都由调用方给出（lib/facets.ts 算选项与计数），本文件不存选择。
export const FACET_SEARCH_AT = 12;

// 单选模式下点一个就只看它，再点当前唯一选中的那一个回到全部；多选模式逐个翻转。单选分支只产出 0 或 1 项，所以只经面板
// 写入的选择集在单选下至多一项；选择集来自外部时（管理端从 URL 还原、别的浏览器选了多选）可能多于一项，单选下点一个
// 仍得到只含它的选择，切回单选时由 FacetPanel 用 narrowToSingle 收窄。
export function pickOption(mode: FacetMode, selected: readonly string[], value: string): string[] {
  if (mode === "single") return selected.length === 1 && selected[0] === value ? [] : [value];
  return selected.includes(value) ? selected.filter((v) => v !== value) : [...selected, value];
}

// 从多选切回单选时只留按选项顺序排在最前的已选项，与面板上看到的先后一致，不取决于勾选的先后。
export function narrowToSingle(options: readonly FacetOption[], selected: readonly string[]): string[] {
  const first = options.find((option) => selected.includes(option.value));
  return first ? [first.value] : [];
}

export function facetSummary(options: readonly FacetOption[], selected: readonly string[]): string {
  if (selected.length === 0) return "全部";
  if (selected.length > 1) return `已选 ${selected.length} 个`;
  return options.find((option) => option.value === selected[0])?.label ?? selected[0];
}

// 一个入口的选项与选择。onChange 写普通选项的选择集；有互斥项时它同时意味着取消互斥项（调用方的状态必须让两者不能
// 同时成立，管理端靠 lib/nodeFilters 的 TagFilter 判别式联合）。
export type FacetSelection = { options: readonly FacetOption[]; selected: readonly string[]; onChange: (next: string[]) => void };

// 与普通选项互斥的一项，排在「全部」之后、普通选项之前（管理端标签面板的「无标签」）：选中它即清空普通选项，选任一普通选项
// 或「全部」即取消它，所以它不分单选多选、不画勾选框。name 是胶囊的可访问名称（后接计数），要与同名的普通选项区分开——
// 运维可能真的建一个叫「无标签」的标签。
export type FacetExclusive = { label: string; name: string; count: number; on: boolean; onSelect: () => void };

type TagFacet = FacetSelection & { match: TagMatch; onMatch: (match: TagMatch) => void; exclusive?: FacetExclusive };

// 筛选行里的地区与标签两个入口。每个入口的面板在 DOM 里紧跟自己的入口按钮，Tab 从入口直接进面板；视觉上由 CSS order
// 排到筛选行最后、独占一行（styles.css 的 .facet-panel）。tags 缺省时不出标签入口，何时出现由调用方判定。
// 只有标签有匹配方式：一个节点只有一个地区，地区多选只能取并集（lib/facets.ts 的 matchesRegion）。
export function FacetFilters({ regions, tags, modes, onMode }: {
  regions: FacetSelection; tags?: TagFacet;
  modes: Readonly<Record<Facet, FacetMode>>; onMode: (facet: Facet, mode: FacetMode) => void;
}) {
  const [openFacet, setOpenFacet] = useState<Facet | null>(null);
  const triggers = useRef<Partial<Record<Facet, HTMLButtonElement | null>>>({});
  const panelId = useId();
  const facets = [
    { key: "region" as const, label: "地区", ...regions, hint: "显示属于任一所选地区的节点。" },
    ...(tags ? [{
      key: "tag" as const, label: "标签", ...tags, match: { value: tags.match, onChange: tags.onMatch },
      hint: tags.match === "all" ? "只显示同时带有全部所选标签的节点。" : "显示带有任一所选标签的节点。",
    }] : []),
  ];
  // 标签入口消失时把展开状态一并清掉，否则它在后续轮询里重新出现时面板会自己弹开。
  if (openFacet !== null && !facets.some((facet) => facet.key === openFacet)) setOpenFacet(null);
  const close = (facet: Facet) => { setOpenFacet(null); triggers.current[facet]?.focus(); };
  return <>
    {facets.map((facet) => (
      <Fragment key={facet.key}>
        <FacetTrigger ref={(el) => { triggers.current[facet.key] = el; }} label={facet.label} options={facet.options} selected={facet.selected} exclusive={"exclusive" in facet ? facet.exclusive : undefined}
          open={openFacet === facet.key} panelId={panelId} onToggle={() => setOpenFacet(openFacet === facet.key ? null : facet.key)} onClose={() => close(facet.key)} />
        {openFacet === facet.key && (
          <FacetPanel id={panelId} label={facet.label} options={facet.options} selected={facet.selected} onChange={facet.onChange} exclusive={"exclusive" in facet ? facet.exclusive : undefined}
            mode={modes[facet.key]} onMode={(mode) => onMode(facet.key, mode)} match={"match" in facet ? facet.match : undefined} hint={facet.hint} onClose={() => close(facet.key)} />
        )}
      </Fragment>
    ))}
  </>;
}

function FacetTrigger({ ref, label, options, selected, exclusive, open, panelId, onToggle, onClose }: {
  ref: Ref<HTMLButtonElement>;
  label: string; options: readonly FacetOption[]; selected: readonly string[]; exclusive?: FacetExclusive;
  open: boolean; panelId: string; onToggle: () => void; onClose: () => void;
}) {
  return (
    <button ref={ref} type="button" className="select-trigger facet-trigger" data-active={selected.length > 0 || exclusive?.on || undefined} aria-expanded={open} aria-controls={open ? panelId : undefined}
      onClick={onToggle} onKeyDown={(event) => { if (event.key === "Escape" && open) onClose(); }}>
      <span className="select-label">{label}</span>{" "}<span className="select-value">{exclusive?.on ? exclusive.label : facetSummary(options, selected)}</span>
      <Icon name="chevronDown" className="select-caret" width={14} height={14} />
    </button>
  );
}

// match 给出时（只有标签），多选模式下多一个匹配方式切换；文案写成「满足任一 / 同时满足」，不写逻辑运算符。
function FacetPanel({ id, label, options, selected, onChange, exclusive, mode, onMode, match, hint, onClose }: {
  id: string; label: string; options: readonly FacetOption[]; selected: readonly string[]; onChange: (next: string[]) => void;
  exclusive?: FacetExclusive;
  mode: FacetMode; onMode: (mode: FacetMode) => void;
  match?: { value: TagMatch; onChange: (match: TagMatch) => void };
  hint: string; onClose: () => void;
}) {
  const [query, setQuery] = useState("");
  const pattern = query === "" ? null : literalPattern(query, false);
  const shown = pattern ? options.filter((option) => pattern.test(option.label)) : options;
  const chooseMode = (next: FacetMode) => {
    onMode(next);
    if (next === "single" && selected.length > 1) onChange(narrowToSingle(options, selected));
  };
  return (
    <div id={id} className="facet-panel" role="group" aria-label={label} onKeyDown={(event) => { if (event.key === "Escape") onClose(); }}>
      <div className="facet-panel-head">
        <div className="segmented" role="group" aria-label="选择方式">
          <button type="button" aria-pressed={mode === "single"} onClick={() => chooseMode("single")}>单选</button>
          <button type="button" aria-pressed={mode === "multi"} onClick={() => chooseMode("multi")}>多选</button>
        </div>
        {mode === "multi" && match && (
          <div className="segmented" role="group" aria-label="匹配方式">
            <button type="button" aria-pressed={match.value === "any"} onClick={() => match.onChange("any")}>满足任一</button>
            <button type="button" aria-pressed={match.value === "all"} onClick={() => match.onChange("all")}>同时满足</button>
          </div>
        )}
        {options.length > FACET_SEARCH_AT && <input type="search" aria-label={`搜索${label}`} value={query} onChange={(event) => setQuery(event.target.value)} />}
        {mode === "multi" && <p className="facet-hint">{hint}</p>}
      </div>
      <ul className="facet-options">
        <li><button type="button" className="facet-chip" aria-pressed={selected.length === 0 && !exclusive?.on} onClick={() => onChange([])}>全部</button></li>
        {/* 互斥项与「全部」一样不随搜索词收窄：它不是被搜索的那类名字。再点选中的它回到全部，与单选点当前项同一手感。 */}
        {exclusive && (
          <li>
            <button type="button" className="facet-chip" aria-pressed={exclusive.on} aria-label={`${exclusive.name} ${exclusive.count}`} onClick={() => (exclusive.on ? onChange([]) : exclusive.onSelect())}>
              <span className="facet-chip-label">{exclusive.label}</span>{" "}<span className="num">{exclusive.count}</span>
            </button>
          </li>
        )}
        {shown.map((option) => {
          const on = selected.includes(option.value);
          return (
            <li key={option.value}>
              <button type="button" className="facet-chip" aria-pressed={on} onClick={() => onChange(pickOption(mode, selected, option.value))}>
                {mode === "multi" && <span className="facet-box" aria-hidden="true">{on && <Icon name="check" width={12} height={12} strokeWidth={2.5} />}</span>}
                <span className="facet-chip-label">{option.label}</span>{" "}<span className="num">{option.count}</span>
              </button>
            </li>
          );
        })}
        {shown.length === 0 && <li className="muted">没有匹配的选项</li>}
      </ul>
    </div>
  );
}
