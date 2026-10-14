import { useQuery } from "@connectrpc/connect-query";
import { Link } from "react-router";
import { errorBanner, queryGate } from "../api/queryGate";
import { AdminService } from "../gen/heron/v1/admin_pb";
import { EventFeed, useAlertEvents, filtersFromParams, paramsWithFilters, matchesFilters, type EventFilters } from "../components/EventFeed";
import { PageHeader } from "../components/PageHeader";
import { ruleLabel, TRANSITIONS } from "../lib/alerts";
import { withId } from "../lib/ids";
import { DateInput } from "../components/DateInput";
import { isDate } from "../lib/format";
import { useSyncedSearchParams } from "../lib/useSyncedSearchParams";

export function AlertEvents() {
  const [params, setParams] = useSyncedSearchParams();
  const raw = params.get("node");
  // 无效参数不能退化成"全部节点"——那是放宽；直接报错，不发请求。
  const valid = raw === null || /^[1-9]\d*$/.test(raw);
  const nodeId = raw !== null && valid ? BigInt(raw) : 0n;
  const nodes = useQuery(AdminService.method.listNodes, {});
  const channels = useQuery(AdminService.method.listNotifyChannels, {});
  const rules = useQuery(AdminService.method.listAlertRules, {});
  const events = useAlertEvents(valid ? nodeId : null);
  const filters = filtersFromParams(params);
  const setFilters = (next: EventFilters) => setParams((current) => paramsWithFilters(current, next));
  if (!valid) return <p role="alert" className="error">节点参数 {raw} 无效。<Link to="/events">查看全部事件</Link></p>;
  // 外壳（标题、筛选、节点名）只依赖节点列表；事件列表是页内区域，区域未就绪不卸载外壳与筛选焦点。
  const shell = queryGate(nodes);
  if (!shell.ready) return shell.loading ?? errorBanner(...shell.errors);
  const nodeList = shell.data.nodes;
  const nodeName = (id: bigint) => id === 0n ? "系统" : nodeList.find((n) => n.id === id)?.name ?? `节点 #${id}`;
  // 渠道只提供名称：它的失败只进横幅，不阻断事件；名称缺失时按编号回退。
  const channelName = (id: bigint) => channels.data?.channels.find((c) => c.id === id)?.name ?? `渠道 #${id}`;
  const ruleName = (id: bigint) => ruleLabel(id, rules.data?.rules);
  const filtered = nodeId !== 0n || filters.ruleId !== null || filters.transition !== null || filters.from !== "" || filters.to !== "";
  return (
    <section>
      <PageHeader title="告警事件" />
      <div className="filter-row" role="group" aria-label="筛选">
      <label>节点
        <select value={String(nodeId)} onChange={(e) => {
          const next = paramsWithFilters(params, filters);
          if (e.target.value === "0") next.delete("node"); else next.set("node", e.target.value);
          setParams(next);
        }}>
          <option value="0">全部节点</option>
          {nodeList.map((n) => <option key={String(n.id)} value={String(n.id)}>{withId(n.name, n.id)}</option>)}
          {nodeId !== 0n && !nodeList.some((n) => n.id === nodeId) && <option value={String(nodeId)}>{nodeName(nodeId)}</option>}
        </select>
      </label>
      <label>规则<select value={filters.ruleId === null ? "" : String(filters.ruleId)} onChange={(e) => setFilters({ ...filters, ruleId: e.target.value ? BigInt(e.target.value) : null })}>
        <option value="">全部规则</option>
        {rules.data?.rules.map((r) => <option key={String(r.id)} value={String(r.id)}>{withId(r.name, r.id)}</option>)}
        {filters.ruleId !== null && !rules.data?.rules.some((r) => r.id === filters.ruleId) && <option value={String(filters.ruleId)}>{ruleName(filters.ruleId)}</option>}
      </select></label>
      <label>变化<select value={filters.transition ?? ""} onChange={(e) => setFilters({ ...filters, transition: e.target.value || null })}>
        <option value="">全部</option>
        {Object.entries(TRANSITIONS).map(([value, label]) => <option key={value} value={value}>{label}</option>)}
      </select></label>
      {/* URL 里只放完整日期：没填完的值留在控件各段里显示，不写进地址，也不参与筛选。 */}
      <DateInput label="起始日期" caption="起始日期" value={filters.from} onChange={(from) => { if (from === "" || isDate(from)) setFilters({ ...filters, from }); }} />
      <DateInput label="结束日期" caption="结束日期" value={filters.to} onChange={(to) => { if (to === "" || isDate(to)) setFilters({ ...filters, to }); }} />
      {filtered && <button type="button" className="link" onClick={() => {
        const next = paramsWithFilters(params, { ruleId: null, transition: null, from: "", to: "" });
        next.delete("node");
        setParams(next);
      }}>清除筛选</button>}
      </div>
      {errorBanner(nodes.error, events.error, channels.error, rules.error)}
      <EventFeed events={events} nodeName={nodeName} channelName={channelName} rules={rules.data?.rules} visible={(ev) => matchesFilters(ev, filters)} />
    </section>
  );
}
