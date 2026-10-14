import { useCallback, useRef } from "react";
import { FacetFilters } from "../components/Facet";
import { Icon } from "../components/Icon";
import { SearchHint } from "../components/SearchHint";
import { Select } from "../components/Select";
import type { Facet, FacetMode, FacetOption } from "../lib/facets";
import type { TagMatch } from "../lib/tags";
import { SEARCH_KEYSHORTCUTS, useSearchShortcut } from "../lib/useSearchShortcut";
import { NODE_SORTS, COLOR_BYS, GROUP_BYS, type NodeSort, type ColorBy, type GroupBy, type PublicFilters } from "./filters";
import type { View } from "./prefs";

// 筛选行：搜索、地区与标签两个筛选入口、只看在线、视图切换；状态墙多一个分组依据（有标签时）与着色依据，
// 卡片与列表多一个排序（两者共用同一个排序选择）。地区与标签的选项由调用方从当前快照算出并带计数。
export function FilterRow({ filters, onFilters, regions, tags, modes, onMode, onTagMatch, view, onView, groupBy, onGroupBy, colorBy, onColorBy, sort, onSort }: {
  filters: PublicFilters; onFilters: (next: PublicFilters) => void;
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
      <button type="button" className="toggle" aria-pressed={filters.onlineOnly} onClick={() => onFilters({ ...filters, onlineOnly: !filters.onlineOnly })}>只看在线</button>
      <div className="view-switch" role="group" aria-label="视图">
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
