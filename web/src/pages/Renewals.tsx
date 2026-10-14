import { createConnectQueryKey, useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { type KeyboardEvent, useEffect, useRef, useState } from "react";
import { Link } from "react-router";
import { errorBanner, queryGate } from "../api/queryGate";
import { errorText } from "../api/auth";
import { useLatestError } from "../api/useLatestError";
import { EmptyState } from "../components/EmptyState";
import { Expiry } from "../components/Expiry";
import { Icon } from "../components/Icon";
import { PageHeader } from "../components/PageHeader";
import { AdminService, type Node } from "../gen/heron/v1/admin_pb";
import { byExpiryDay, canRenew, hubToday, priceText } from "../lib/billing";
import { isDate } from "../lib/format";
import { withId } from "../lib/ids";
import { TRAFFIC_MS } from "../lib/poll";
import { expiryLevel } from "../lib/status";
import { addMonths, calendarStep, monthDays } from "../lib/ymd";
import { NodeEditor } from "./NodeEditor";

const WEEKDAYS = ["一", "二", "三", "四", "五", "六", "日"];

// 续费日历：按月看节点的到期日。数据只来自 ListNodes 的 billing；日历日与"今天"都是 hub 时区（--timezone）的，
// 今天由 hub 下发的 days_left 反推（hubToday），日期运算走 lib/ymd 的 UTC 运算——浏览器的时钟与时区一处都不读，
// 管理员与 hub 不在同一时区时格子也不会错开一天。"已续费"调 RenewNodeBilling，推后的日期由 hub 算。
export function Renewals() {
  const qc = useQueryClient();
  const nodes = useQuery(AdminService.method.listNodes, {}, { refetchInterval: TRAFFIC_MS });
  const tags = useQuery(AdminService.method.listTags, {});
  const { error, isLatest, mutationOptions } = useLatestError();
  const [notice, setNotice] = useState("");
  // 用户翻到的月份与选中的日子；为 null 时跟随 hub 的今天（数据到达前不知道今天是哪天）。
  const [picked, setPicked] = useState<{ focus: string; selected: string } | null>(null);
  const [editor, setEditor] = useState<{ node: Node; opener: HTMLElement } | null>(null);
  const keyboard = useRef(false);
  const grid = useRef<HTMLTableElement>(null);
  const refresh = (options?: { throwOnError: boolean }) => qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listNodes, cardinality: "finite" }) }, options);
  const update = useMutation(AdminService.method.updateNode, {
    onSuccess: async () => {
      try { await refresh({ throwOnError: true }); }
      catch (err) { throw new Error(`已保存，但回读失败：${errorText(err)}`, { cause: err }); }
    },
  });
  const renew = useMutation(AdminService.method.renewNodeBilling, {
    ...mutationOptions,
    onSuccess: (result, _request, operation) => {
      const n = result.node;
      if (n && isLatest(operation)) setNotice(`${withId(n.name, n.id)} 已续费，到期日推后到 ${n.billing?.expiresOn ?? "—"}`);
      return refresh();
    },
  });
  const focusDay = picked?.focus;
  useEffect(() => {
    if (!keyboard.current || !focusDay) return;
    grid.current?.querySelector<HTMLButtonElement>(`[data-ymd="${focusDay}"]`)?.focus();
  }, [focusDay]);

  const gate = queryGate(nodes);
  const header = <PageHeader title="续费日历" description="按到期日汇总节点；日期与今天按 hub 时区。点日期列出当日到期的节点。" />;
  const banner = errorBanner(nodes.error, error, update.error);
  if (!gate.ready) return <section>{header}{banner}{gate.loading}</section>;
  const list = gate.data.nodes;
  const today = hubToday(list.map((n) => n.billing));
  if (today === undefined) {
    return <section>{header}{banner}<EmptyState title="还没有设置到期日的节点。">在节点的编辑抽屉里填写到期日与计费周期后，节点按到期日出现在这里。</EmptyState></section>;
  }
  const days = byExpiryDay(list);
  const focus = picked?.focus ?? today;
  const selected = picked?.selected ?? today;
  const [year, month] = focus.split("-");
  // 0001–9999 年之外的日子 lib/ymd 写不出四位年份，翻到范围外时停在原地（与日期选择器同一约束）。
  const move = (next: string, byKeyboard: boolean, select: boolean) => {
    if (!isDate(next)) return;
    keyboard.current = byKeyboard;
    setPicked({ focus: next, selected: select ? next : selected });
  };
  const onKeyDown = (event: KeyboardEvent) => {
    const next = calendarStep(event.key, focus);
    if (next === undefined || !(event.target instanceof HTMLButtonElement) || !event.target.dataset.ymd) return;
    event.preventDefault();
    move(next, true, false);
  };
  const due = days.get(selected) ?? [];
  return (
    <section className="renewals">
      {header}
      {banner}
      {notice && <p role="status" className="muted">{notice}</p>}
      <div className="card renewal-calendar">
        <div className="calendar-head">
          <button type="button" className="icon-button" aria-label="上个月" onClick={() => move(addMonths(focus, -1), false, false)}><Icon name="chevronLeft" /></button>
          <span className="calendar-title" aria-live="polite">{Number(year)} 年 {Number(month)} 月</span>
          <span className="row">
            <button type="button" className="link" onClick={() => move(today, false, true)}>今天</button>
            <button type="button" className="icon-button" aria-label="下个月" onClick={() => move(addMonths(focus, 1), false, false)}><Icon name="chevronRight" /></button>
          </span>
        </div>
        <table ref={grid} className="calendar-grid" aria-label={`${Number(year)} 年 ${Number(month)} 月到期日历`} onKeyDown={onKeyDown}>
          <thead><tr>{WEEKDAYS.map((w) => <th key={w} scope="col" abbr={`星期${w}`}>{w}</th>)}</tr></thead>
          <tbody>
            {Array.from({ length: 6 }, (_, row) => (
              <tr key={row}>
                {monthDays(focus).slice(row * 7, row * 7 + 7).map((ymd) => {
                  const nodesOfDay = days.get(ymd) ?? [];
                  const count = nodesOfDay.length;
                  // 已出租节点照常列在当天、计入台数，但不参与格子上色：取当天第一个未出租节点的级别。
                  const colorSource = nodesOfDay.find((node) => !node.rented);
                  const level = expiryLevel(colorSource?.billing?.daysLeft, colorSource === undefined);
                  return <td key={ymd}>
                    {isDate(ymd) && <button type="button" data-ymd={ymd} tabIndex={ymd === focus ? 0 : -1}
                      aria-label={count > 0 ? `${ymd}，${count} 台到期` : `${ymd}，无到期`} aria-pressed={ymd === selected} aria-current={ymd === today ? "date" : undefined}
                      data-outside={ymd.slice(0, 7) !== focus.slice(0, 7) || undefined}
                      onClick={() => move(ymd, false, true)}>
                      <span className="num">{Number(ymd.slice(8))}</span>
                      {count > 0 && <span className="renewal-count" data-level={level} aria-hidden="true">{count} 台</span>}
                    </button>}
                  </td>;
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <h2 className="renewal-day-title">{selected} 到期</h2>
      {due.length === 0 ? <EmptyState title={`${selected} 没有到期的节点。`} status /> : <div className="table-scroll" role="region" aria-label={`${selected} 到期的节点`} tabIndex={0}>
        <table className="nodes">
          <thead><tr><th>节点</th><th>价格</th><th>到期</th><th>自动续期</th><th><span className="sr-only">操作</span></th></tr></thead>
          <tbody>{due.map((node) => {
            const label = withId(node.name, node.id);
            return <tr key={String(node.id)}>
              <td data-label="节点"><Link to={`/nodes/${node.id}`} className="clip-text" title={label} aria-label={label}>{node.name}</Link></td>
              <td data-label="价格">{priceText(node.billing) || <span className="muted">—</span>}</td>
              <td data-label="到期"><Expiry billing={node.billing} rented={node.rented} /></td>
              <td data-label="自动续期">{node.billing?.autoRenew ? "开" : "关"}</td>
              <td data-label="操作" className="row">
                <button type="button" aria-label={`编辑计费 ${label}`} disabled={editor !== null} onClick={(event) => { update.reset(); setEditor({ node, opener: event.currentTarget }); }}>编辑计费</button>
                {canRenew(node.billing) && <RenewButton label={label} pending={renew.isPending} onRenew={() => { setNotice(""); renew.mutate({ nodeId: node.id }); }} />}
              </td>
            </tr>;
          })}</tbody>
        </table>
      </div>}
      {editor && <NodeEditor key={String(editor.node.id)} node={editor.node} opener={editor.opener} knownTags={tags.data?.tags ?? []}
        saving={update.isPending} error={update.error} listError={nodes.error} onClose={() => setEditor(null)}
        onSave={(patch) => update.mutate({ id: editor.node.id, ...patch }, { onSuccess: () => setEditor(null) })} />}
    </section>
  );
}

// "已续费"两段式：首击只进入确认态，确认才提交，误点不会把到期日推后一个周期。确认态随行卸载撤销（节点续费后
// 离开这一天的列表）。
function RenewButton({ label, pending, onRenew }: { label: string; pending: boolean; onRenew: () => void }) {
  const [confirming, setConfirming] = useState(false);
  if (!confirming) return <button type="button" aria-label={`已续费 ${label}`} onClick={() => setConfirming(true)}>已续费</button>;
  return <>
    <button type="button" className="primary" disabled={pending} onClick={onRenew}>确认已续费</button>{" "}
    <span className="muted">到期日按计费周期推后，规则同自动续期</span>{" "}
    <button type="button" className="link" aria-label={`取消已续费 ${label}`} onClick={() => setConfirming(false)}>取消</button>
  </>;
}
