import { useInfiniteQuery, useQuery } from "@connectrpc/connect-query";
import { skipToken, type InfiniteData } from "@tanstack/react-query";
import { useState } from "react";
import { errorBanner, queryGate } from "../api/queryGate";
import { AdminService, type AlertDelivery, type AlertEvent, type AlertRule, type ListAlertEventsResponse } from "../gen/heron/v1/admin_pb";
import { alarming, deliveryText, eventValueText, hasErrorText, ruleLabel, transitionLabel, TRANSITIONS } from "../lib/alerts";
import { withId } from "../lib/ids";
import { dateTime, isDate } from "../lib/format";
import { EmptyState } from "./EmptyState";

// 与 hub 的默认页长一致；不足一页即已到最早的事件。
export const EVENT_PAGE = 100;

export function useAlertEvents(nodeId: bigint | null) {
  return useInfiniteQuery(AdminService.method.listAlertEvents, nodeId === null ? skipToken : { nodeId, beforeId: 0n, limit: EVENT_PAGE }, {
    pageParamKey: "beforeId",
    // 事件按 id 倒序；下一页从本页最小 id 之前开始。
    getNextPageParam: (last) => (last.events.length < EVENT_PAGE ? undefined : last.events[last.events.length - 1].id),
  });
}

export function EventFeed({ events, nodeName, channelName, rules, visible }: {
  events: ReturnType<typeof useAlertEvents>; nodeName: (id: bigint) => string; channelName: (id: bigint) => string;
  rules: readonly AlertRule[] | undefined; visible?: (ev: AlertEvent) => boolean;
}) {
  const region = queryGate(events);
  if (!region.ready) return <>{region.loading}</>;
  return <EventList data={region.data} nodeName={nodeName} channelName={channelName} rules={rules} visible={visible}
    hasNextPage={events.hasNextPage} fetchingNext={events.isFetchingNextPage} onMore={() => void events.fetchNextPage()} />;
}

function EventList({ data, nodeName, channelName, rules, visible, hasNextPage, fetchingNext, onMore }: {
  data: InfiniteData<ListAlertEventsResponse>; nodeName: (id: bigint) => string; channelName: (id: bigint) => string;
  rules: readonly AlertRule[] | undefined; visible?: (ev: AlertEvent) => boolean;
  hasNextPage: boolean; fetchingNext: boolean; onMore: () => void;
}) {
  const rows = data.pages.flatMap((p) => p.events);
  const shown = visible ? rows.filter(visible) : rows;
  return (
    <>
      {shown.length === 0
        ? rows.length === 0 ? <EmptyState title="没有告警事件。" /> : <EmptyState status title={`已加载的 ${rows.length} 条里没有匹配的事件。`} />
        : <div className="table-scroll" role="region" aria-label="告警事件" tabIndex={0}>
        <table className="nodes events-table">
          <thead><tr><th>时间</th><th>节点</th><th>规则</th><th>变化</th><th>观测值</th><th>投递</th></tr></thead>
          <tbody>
            {shown.map((ev) => {
              const value = eventValueText(ev, rules?.find((r) => r.id === ev.ruleId));
              return <tr key={String(ev.id)}>
                <td data-label="时间" className="num">{dateTime(ev.at)}</td>
                <td data-label="节点">{ev.nodeId === 0n ? <span className="muted">—</span> : nodeName(ev.nodeId)}</td>
                <td data-label="规则">{ev.ruleId === 0n ? <span className="muted">—</span> : ruleLabel(ev.ruleId, rules)}</td>
                <td data-label="变化" className={alarming(ev.transition) ? "error" : undefined}>{transitionLabel(ev.transition)}<small className="muted event-summary">{ev.summary}</small></td>
                <td data-label="观测值">{value === null ? <span className="muted">—</span> : value}</td>
                <td data-label="投递">
                  {ev.silenced
                    ? <span className="muted">已静默（维护窗口内，未投递）</span>
                    : ev.deliveries.length === 0
                      ? <span className="muted">未配置渠道</span>
                      : ev.deliveries.map((d) => <DeliveryItem key={String(d.id)} d={d} channel={channelName(d.channelId)} />)}
                </td>
              </tr>;
            })}
          </tbody>
        </table>
      </div>}
      {hasNextPage && (
        <button type="button" disabled={fetchingNext} onClick={onMore}>加载更早的事件</button>
      )}
    </>
  );
}

export type EventFilters = { ruleId: bigint | null; transition: string | null; from: string; to: string };

export function filtersFromParams(params: URLSearchParams): EventFilters {
  const rule = params.get("rule");
  const transition = params.get("transition");
  const from = params.get("from") ?? "";
  const to = params.get("to") ?? "";
  return {
    ruleId: rule !== null && /^[1-9]\d*$/.test(rule) ? BigInt(rule) : null,
    transition: transition !== null && Object.hasOwn(TRANSITIONS, transition) ? transition : null,
    from: isDate(from) ? from : "",
    to: isDate(to) ? to : "",
  };
}

export function paramsWithFilters(params: URLSearchParams, filters: EventFilters): URLSearchParams {
  const next = new URLSearchParams(params);
  for (const [key, value] of [["rule", filters.ruleId === null ? "" : String(filters.ruleId)], ["transition", filters.transition ?? ""], ["from", filters.from], ["to", filters.to]] as const) {
    if (value) next.set(key, value); else next.delete(key);
  }
  return next;
}

// 日期筛选按本地日包含首尾两天；用日历上的次日零点作上界，夏令时切换日不一定长 86400 秒。
function dayStart(ymd: string, nextDay = false): number {
  const date = new Date(`${ymd}T00:00:00`);
  if (nextDay) date.setDate(date.getDate() + 1);
  return date.getTime() / 1000;
}

export function matchesFilters(ev: AlertEvent, filters: EventFilters): boolean {
  if (filters.ruleId !== null && ev.ruleId !== filters.ruleId) return false;
  if (filters.transition !== null && ev.transition !== filters.transition) return false;
  const at = Number(ev.at);
  if (filters.from && at < dayStart(filters.from)) return false;
  if (filters.to && at >= dayStart(filters.to, true)) return false;
  return true;
}

// 原文可能含接收方回显的密钥，只读口径不带它；按需经仅会话的 GetAlertDeliveryError 取，展开前不发请求。
// 同一事件可能有同名渠道，按钮的可访问名带投递 id 才可区分；id 属于投递而不是渠道，名字写成"渠道名 的投递（#id）"。
// 收起再展开会重新取（数据已过期），这次失败时缓存的原文仍在，按 queryGate 的约定只加横幅。
function DeliveryItem({ d, channel }: { d: AlertDelivery; channel: string }) {
  const [open, setOpen] = useState(false);
  const text = useQuery(AdminService.method.getAlertDeliveryError, open ? { deliveryId: d.id } : skipToken);
  const gate = queryGate(text);
  return (
    <div>
      {deliveryText(d, channel)}
      {hasErrorText(d) && (
        <>
          {" "}
          <button type="button" className="link" aria-label={`查看错误原文 ${withId(`${channel} 的投递`, d.id)}`} aria-expanded={open}
            onClick={() => setOpen((o) => !o)}>查看错误原文</button>
        </>
      )}
      {open && (gate.ready
        ? <>
            {gate.banner}
            {gate.data.error ? <pre className="secret">{gate.data.error}</pre> : <p className="muted">（没有错误原文）</p>}
          </>
        : gate.loading ?? errorBanner(...gate.errors))}
    </div>
  );
}
