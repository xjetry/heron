import { useCallback, useRef } from "react";
import { FacetFilters } from "../components/Facet";
import { Icon } from "../components/Icon";
import { SearchHint } from "../components/SearchHint";
import { Select } from "../components/Select";
import type { Facet, FacetMode, FacetOption } from "../lib/facets";
import type { TagMatch } from "../lib/tags";
import { SEARCH_KEYSHORTCUTS, useSearchShortcut } from "../lib/useSearchShortcut";
import { NODE_SORTS, COLOR_BYS, GROUP_BYS, STATUS_FILTERS, type NodeSort, type ColorBy, type GroupBy, type PublicFilters, type StatusFilter } from "./filters";
import type { View } from "./prefs";

// 筛选行：搜索、地区与标签两个筛选入口、节点状态（全部、在线、离线）、视图切换；状态墙多一个分组依据（有标签时）与着色依据，
// 卡片与列表多一个排序（两者共用同一个排序选择）。地区与标签的选项、三档状态的计数都由调用方从当前快照算出。
export function FilterRow({ filters, onFilters, statusCounts, regions, tags, modes, onMode, onTagMatch, view, onView, groupBy, onGroupBy, colorBy, onColorBy, sort, onSort }: {
  filters: PublicFilters; onFilters: (next: PublicFilters) => void; statusCounts: Readonly<Record<StatusFilter, number>>;
  regions: readonly FacetOption[]; tags: readonly FacetOption[];
  modes: Readonly<Record<Facet, FacetMode>>; onMode: (facet: Facet, mode: FacetMode) => void; onTagMatch: (match: TagMatch) => void;
  view: View; onView: (view: View) => void;
  groupBy: GroupBy; onGroupBy: (by: GroupBy) => void;
  colorBy: ColorBy; onColorBy: (by: ColorBy) => void;
  sort: NodeSort; onSort: (sort: NodeSort) => void;
}) {
  const search = useRef<HTMLInputElement>(null);
  useSearchShortcut(useCallback(() => search.current?.focus(), []));
  return (
    <div className="filter-row" role="group" aria-label="筛选">
      <div className="search-field">
        <Icon name="search" />
        <input ref={search} type="search" aria-label="搜索节点" aria-keyshortcuts={SEARCH_KEYSHORTCUTS} value={filters.search} onChange={(event) => onFilters({ ...filters, search: event.target.value })} />
        {filters.search === "" && <SearchHint text="搜索名称、标签、备注" />}
      </div>
      {/* 没有任何标签时不出标签入口。 */}
      <FacetFilters modes={modes} onMode={onMode}
        regions={{ options: regions, selected: filters.regions, onChange: (next) => onFilters({ ...filters, regions: next }) }}
        tags={tags.length > 0 ? { options: tags, selected: filters.tags, onChange: (next) => onFilters({ ...filters, tags: next }), match: filters.tagMatch, onMatch: onTagMatch } : undefined} />
      {/* 计数写进可访问名称，与地区、标签胶囊同一写法：读屏读到的是「在线 12」而不是拆开的两段。 */}
      <div className="button-group" role="group" aria-label="节点状态">
        {STATUS_FILTERS.map(({ value, label }) => (
          <button key={value} type="button" aria-pressed={filters.status === value} aria-label={`${label} ${statusCounts[value]}`} onClick={() => onFilters({ ...filters, status: value })}>
            {label}{" "}<span className="num">{statusCounts[value]}</span>
          </button>
        ))}
      </div>
      <div className="button-group" role="group" aria-label="视图">
        <button type="button" aria-pressed={view === "wall"} onClick={() => onView("wall")}>状态墙</button>
        <button type="button" aria-pressed={view === "cards"} onClick={() => onView("cards")}>卡片</button>
        <button type="button" aria-pressed={view === "list"} onClick={() => onView("list")}>列表</button>
      </div>
      {view === "wall" && tags.length > 0 && <Select label="分组" value={groupBy} options={GROUP_BYS} onChange={onGroupBy} />}
      {view === "wall" && <Select label="着色依据" value={colorBy} options={COLOR_BYS} onChange={onColorBy} />}
      {view !== "wall" && <Select label="排序" value={sort} options={NODE_SORTS} onChange={onSort} />}
    </div>
  );
}
