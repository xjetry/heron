import { useQuery } from "@connectrpc/connect-query";
import { useState } from "react";
import { errorBanner, queryGate } from "../api/queryGate";
import { PublicService } from "../gen/heron/v1/public_pb";
import { regionOptions, tagOptions, useFacetModes } from "../lib/facets";
import { POLL_MS } from "../lib/poll";
import { sameTag, type TagMatch } from "../lib/tags";
import { FilterRow } from "./FilterRow";
import { filterPublicNodes, NO_FILTERS, sortNodes, type NodeSort, type ColorBy, type GroupBy, type PublicFilters } from "./filters";
import { StatusSummary } from "./StatusSummary";
import { CardGrid } from "./NodeCard";
import { NodeList } from "./NodeList";
import { StatusWall } from "./StatusWall";
import { PUBLIC_FACET_MODE_KEYS, readPublicView, readTagMatch, readWallGroupBy, writePublicView, writeTagMatch, writeWallGroupBy, type View } from "./prefs";

export function PublicOverview() {
  const snap = useQuery(PublicService.method.getSnapshot, {}, { refetchInterval: POLL_MS });
  const [filters, setFilters] = useState<PublicFilters>(() => ({ ...NO_FILTERS, tagMatch: readTagMatch() }));
  const chooseTagMatch = (match: TagMatch) => { setFilters((current) => ({ ...current, tagMatch: match })); writeTagMatch(match); };
  const [view, setView] = useState<View>(readPublicView);
  const chooseView = (next: View) => { setView(next); writePublicView(next); };
  const [groupBy, setGroupBy] = useState<GroupBy>(readWallGroupBy);
  const chooseGroupBy = (next: GroupBy) => { setGroupBy(next); writeWallGroupBy(next); };
  const [modes, chooseMode] = useFacetModes(PUBLIC_FACET_MODE_KEYS);
  const [colorBy, setColorBy] = useState<ColorBy>("status");
  const [sort, setSort] = useState<NodeSort>("default");
  const [selectedId, setSelectedId] = useState<bigint | null>(null);
  const gate = queryGate(snap);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  const all = gate.data.nodes;
  const now = Number(gate.data.now);
  const regions = regionOptions(all);
  const tags = tagOptions(all, gate.data.tags);
  // 生效的选择集只取当前快照里仍存在的地区与标签（标签按折叠比较，换成标签栏上的写法）：被选的项在轮询后消失时
  // 从选择集里移除，而不是留下一个看不见的过滤条件。
  const liveRegions = filters.regions.filter((code) => regions.some((r) => r.value === code));
  const liveTags = gate.data.tags.filter((tag) => filters.tags.some((s) => sameTag(s, tag)));
  if (liveRegions.length !== filters.regions.length || liveTags.length !== filters.tags.length) setFilters({ ...filters, regions: liveRegions, tags: liveTags });
  const effective: PublicFilters = { ...filters, regions: liveRegions, tags: liveTags };
  const nodes = filterPublicNodes(all, effective);
  // 没有任何标签时分组下拉不出现，按标签分组也只剩「无标签」一组：此时按地区分组，不留下看不见的设置。
  const wallGroupBy: GroupBy = gate.data.tags.length > 0 ? groupBy : "region";
  return (
    <section className="public-overview">
      {gate.banner}
      {all.length === 0 && <p className="muted">没有公开的节点。</p>}
      {all.length > 0 && (
        <>
          <StatusSummary nodes={nodes} />
          <FilterRow filters={effective} onFilters={setFilters} regions={regions} tags={tags} modes={modes} onMode={chooseMode} onTagMatch={chooseTagMatch} view={view} onView={chooseView} groupBy={wallGroupBy} onGroupBy={chooseGroupBy} colorBy={colorBy} onColorBy={setColorBy} sort={sort} onSort={setSort} />
          {nodes.length === 0 && <p className="muted">没有符合筛选条件的节点。</p>}
          {nodes.length > 0 && view === "wall" && <StatusWall nodes={nodes} tags={gate.data.tags} now={now} groupBy={wallGroupBy} colorBy={colorBy} selectedId={selectedId} onSelect={setSelectedId} />}
          {nodes.length > 0 && view === "cards" && <CardGrid nodes={sortNodes(nodes, sort)} now={now} />}
          {nodes.length > 0 && view === "list" && <NodeList nodes={sortNodes(nodes, sort)} now={now} />}
        </>
      )}
    </section>
  );
}
