import type { DescMethodUnary } from "@bufbuild/protobuf";
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { useEffect, useMemo, useState } from "react";
import { errorText } from "../api/auth";
import type { ListProbeComparisonNodesRequestSchema, ListProbeComparisonNodesResponseSchema, QueryProbeComparisonRequestSchema, QueryProbeComparisonResponseSchema } from "../gen/heron/v1/query_pb";
import { ProbeKind } from "../gen/heron/v1/types_pb";
import { formatUnit, percent } from "../lib/format";
import { assembleComparison, chunkNodeIds, summarizeComparison, sortRows, type SortKey, type ComparisonChunk } from "../lib/probeComparison";
import { kindLabel } from "../lib/probes";
import { Chart } from "./Chart";
import { HISTORY_MAX_POINTS, RangeButtons, RangeStatus, useTimeWindow } from "./History";

// 同时在飞的分块数。这不是块的大小：块的大小只来自 List 的 max_nodes_per_query。
const MAX_IN_FLIGHT_CHUNKS = 2;

const LIMIT_ERROR = "对比协议错误：服务端返回的分块上限无效。";
const ALIGN_ERROR = "对比协议错误：分块结果无法对齐成一张图。";
export const NO_COMPARISON_NODES = "没有可对比的节点。";

export type ProbeComparisonMethods = {
  list: DescMethodUnary<typeof ListProbeComparisonNodesRequestSchema, typeof ListProbeComparisonNodesResponseSchema>;
  query: DescMethodUnary<typeof QueryProbeComparisonRequestSchema, typeof QueryProbeComparisonResponseSchema>;
};

type Hold = {
  nodeIds: bigint[];
  kind: ProbeKind;
  target: string;
  level: string;
  stepS: number;
  chunks: ComparisonChunk[];
  from: number;
  to: number;
  rangeLabel: string;
};

function gaveUp(err: unknown, signal: AbortSignal): boolean {
  return signal.aborted || (err instanceof ConnectError && err.code === Code.Canceled);
}

async function queryChunks(
  transport: Transport,
  method: ProbeComparisonMethods["query"],
  taskId: bigint,
  chunks: readonly (readonly bigint[])[],
  from: number,
  to: number,
  signal: AbortSignal,
): Promise<{ chunks: ComparisonChunk[]; level: string }> {
  const results = new Array<ComparisonChunk & { level: string }>(chunks.length);
  let next = 0;
  let failure: Error | undefined;
  const worker = async () => {
    for (;;) {
      // next++ 与 failure 的读写都在 await 之前，单线程下不会交叉。失败后不再发后面的块。
      if (signal.aborted || failure !== undefined) return;
      const i = next++;
      if (i >= chunks.length) return;
      try {
        const resp = await transport.unary(method, signal, undefined, undefined, {
          taskId,
          nodeIds: [...chunks[i]],
          from: BigInt(from),
          to: BigInt(to),
          maxPoints: HISTORY_MAX_POINTS,
        });
        const message = resp.message;
        results[i] = { level: message.level, stepS: message.stepS, series: message.series, unavailableNodeIds: message.unavailableNodeIds };
      } catch (err) {
        if (failure === undefined) failure = err instanceof Error ? err : new Error(String(err), { cause: err });
        return;
      }
    }
  };
  await Promise.all(Array.from({ length: Math.min(MAX_IN_FLIGHT_CHUNKS, chunks.length) }, () => worker()));
  if (failure !== undefined) throw failure;
  if (signal.aborted) throw new ConnectError("canceled", Code.Canceled);
  return { chunks: results, level: results[0]?.level ?? "" };
}

function TaskHeading({ taskId, hold }: { taskId: bigint; hold: Hold | null }) {
  if (!hold || hold.kind === ProbeKind.UNSPECIFIED || hold.target === "") return <h1>任务 #{String(taskId)}</h1>;
  return <h1><span className="kind-badge">{kindLabel(hold.kind)}</span> {hold.target}</h1>;
}

// 管理端与公开页同一份。节点名由调用方给：管理端来自 ListNodes，公开端来自 GetSnapshot。
// 一次查询是 (taskId, from, to, maxPoints)。List 的 max_nodes_per_query 决定块大小；全部块到齐才替换画面。
// 刷新、换窗口、换任务或离开页面都会取消还没回来的块。刷新期间留着上一张完整的图，并标成更新中。
export function ProbeComparison({ taskId, methods, nodes, now }: { taskId: bigint; methods: ProbeComparisonMethods; nodes: readonly { id: bigint; name: string }[]; now: number }) {
  const transport = useTransport();
  const { range, setRange, from, to } = useTimeWindow(now);
  const rangeLabel = range.label;
  const [hold, setHold] = useState<Hold | null>(null);
  const [holdTask, setHoldTask] = useState(taskId);
  const [error, setError] = useState<string | null>(null);
  const [notFound, setNotFound] = useState(false);
  const [updating, setUpdating] = useState(true);
  const [attempt, setAttempt] = useState(0);
  // 显隐与悬停按节点 id 记：线的位置随刷新变化（见 ComparisonChart.ids 的说明），索引只在交给 Chart 时换算。
  const [hiddenIds, setHiddenIds] = useState<ReadonlySet<bigint>>(() => new Set());
  const [focusedId, setFocusedId] = useState<bigint | null>(null);
  const [sortKey, setSortKey] = useState<SortKey>("label");
  const [sortDir, setSortDir] = useState<"asc" | "desc">("asc");
  // 一次请求由任务、窗口、重试次数与两个方法、transport 决定；任何一个变了就是一次新请求。状态在渲染期切成
  // "更新中"，不先画出一帧上一次请求的错误或结果状态；effect 只负责发请求与收结果。
  const requestKey = `${taskId}|${from}|${to}|${rangeLabel}|${attempt}`;
  const [requested, setRequested] = useState({ key: requestKey, list: methods.list, query: methods.query, transport });
  if (requested.key !== requestKey || requested.list !== methods.list || requested.query !== methods.query || requested.transport !== transport) {
    setRequested({ key: requestKey, list: methods.list, query: methods.query, transport });
    setUpdating(true);
    setError(null);
    setNotFound(false);
  }
  // 换任务不能沿用上一张图：那是另一个任务的节点。换窗口则留着，直到新窗口自己的块到齐。
  if (holdTask !== taskId) {
    setHoldTask(taskId);
    setHold(null);
    setError(null);
    setNotFound(false);
    setUpdating(true);
    setHiddenIds(new Set());
    setFocusedId(null);
  }
  const shown = holdTask === taskId ? hold : null;
  const names = useMemo(() => new Map(nodes.map((node) => [node.id, node.name])), [nodes]);
  const view = useMemo(
    () => (shown ? assembleComparison(shown.nodeIds, names, shown.chunks, shown.from, shown.to) : null),
    [shown, names],
  );
  const rows = useMemo(() => (shown ? sortRows(summarizeComparison(shown.nodeIds, names, shown.chunks), sortKey, sortDir) : []), [shown, names, sortKey, sortDir]);

  useEffect(() => {
    const ac = new AbortController();
    let active = true;
    const fail = (message: string) => {
      if (!active) return;
      setError(message);
      setUpdating(false);
    };
    void (async () => {
      let listed;
      try {
        listed = await transport.unary(methods.list, ac.signal, undefined, undefined, { taskId });
      } catch (err) {
        if (!active || gaveUp(err, ac.signal)) return;
        // NotFound 只属于 List：任务不存在，或一个可见的分配节点都没有。两种原因同一句说明，不展示服务端原文。
        if (err instanceof ConnectError && err.code === Code.NotFound) {
          setHold(null);
          setNotFound(true);
          setUpdating(false);
          return;
        }
        fail(errorText(err));
        return;
      }
      if (!active || ac.signal.aborted) return;
      const message = listed.message;
      const plan = chunkNodeIds(message.nodeIds, message.maxNodesPerQuery);
      if (!plan.ok) {
        fail(LIMIT_ERROR);
        return;
      }
      // 空候选在协议里是 NotFound。若响应仍带着空列表，没有节点可请求；空的 node_ids 会被服务端拒绝。
      if (plan.chunks.length === 0) {
        if (!active) return;
        setHold(null);
        setNotFound(true);
        setUpdating(false);
        return;
      }
      let fetched;
      try {
        fetched = await queryChunks(transport, methods.query, taskId, plan.chunks, from, to, ac.signal);
      } catch (err) {
        if (!active || gaveUp(err, ac.signal)) return;
        fail(errorText(err));
        return;
      }
      if (!active || ac.signal.aborted) return;
      // 完整性不依赖节点名。名字只在绘制时再套上，节点改名不必重新取数。
      const assembled = assembleComparison(message.nodeIds, new Map(), fetched.chunks, from, to);
      if (!assembled.complete) {
        fail(ALIGN_ERROR);
        return;
      }
      setHold({
        nodeIds: [...message.nodeIds],
        kind: message.kind,
        target: message.target,
        level: fetched.level,
        stepS: assembled.stepS,
        chunks: fetched.chunks,
        from,
        to,
        rangeLabel,
      });
      setNotFound(false);
      setUpdating(false);
    })();
    return () => {
      active = false;
      ac.abort();
    };
  }, [attempt, from, methods.list, methods.query, rangeLabel, taskId, to, transport]);

  const stale = shown !== null && shown.rangeLabel !== rangeLabel;
  const lineIds = view?.rtt.ids ?? [];
  const lineOf = new Map(lineIds.map((id, i) => [id, i]));
  const hiddenLines = new Set(lineIds.flatMap((id, i) => (hiddenIds.has(id) ? [i] : [])));
  const toggleHidden = (id: bigint) => {
    const next = new Set(hiddenIds);
    if (next.has(id)) next.delete(id); else next.add(id);
    setHiddenIds(next);
  };
  // Chart 只会在图例上改显隐（这里图例关着），仍按同一口径换算回 id；当前没有线的节点的偏好原样保留。
  const onHiddenLines = (next: ReadonlySet<number>) =>
    setHiddenIds(new Set([...[...hiddenIds].filter((id) => !lineOf.has(id)), ...lineIds.filter((_, i) => next.has(i))]));
  const sortButton = (key: SortKey, text: string) => (
    <th aria-sort={sortKey === key ? (sortDir === "asc" ? "ascending" : "descending") : "none"}>
      <button type="button" className="link" onClick={() => {
        if (sortKey === key) setSortDir(sortDir === "asc" ? "desc" : "asc");
        else { setSortKey(key); setSortDir("asc"); }
      }}>{text}</button>
    </th>
  );
  return (
    <section className="compare" aria-busy={updating}>
      <header className="row detail-header">
        <TaskHeading taskId={taskId} hold={shown} />
        {shown && <span className="muted">{shown.nodeIds.length} 个节点执行此探测</span>}
        <RangeButtons range={range} setRange={setRange} />
        <RangeStatus shown={shown} stale={stale} rangeLabel={rangeLabel} updating={updating} note="均值是时间桶内成功探测的 RTT 均值；表格按整个窗口汇总。" />
      </header>
      {error && (
        <p role="alert" className="error">
          {error} <button type="button" onClick={() => setAttempt((n) => n + 1)}>重试</button>
        </p>
      )}
      {!shown && updating && !error && !notFound && <p className="muted">加载中…</p>}
      {notFound && !updating && <p className="muted">{NO_COMPARISON_NODES}</p>}
      {view?.complete && !notFound && (
        <>
          <div className="card compare-chart">
            {view.rtt.labels.length > 0
              ? <Chart data={view.rtt.data} labels={view.rtt.labels} unit="ms" height={280} legend={false} hidden={hiddenLines} onHiddenChange={onHiddenLines} onFocus={(i) => setFocusedId(i === null ? null : lineIds[i] ?? null)} />
              : <p className="muted">窗口内没有读数</p>}
          </div>
          <table className="compare-table">
            <thead><tr>
              <th>显示</th>
              {sortButton("label", "节点")}{sortButton("mean", "均值")}{sortButton("min", "最小")}{sortButton("max", "最大")}{sortButton("lossPercent", "丢包率")}{sortButton("errors", "错误数")}
            </tr></thead>
            <tbody>
              {rows.map((row) => {
                // assembleComparison 只给有 RTT 读数的节点分配线；其他行没有可切换的线。
                const line = lineOf.get(row.id);
                return (
                  <tr key={String(row.id)} aria-label={row.label} data-focused={line !== undefined && focusedId === row.id ? "true" : undefined} className={row.unavailable ? "muted" : undefined}>
                    <td><input type="checkbox" aria-label={`显示 ${row.label}`} disabled={line === undefined} checked={line !== undefined && !hiddenIds.has(row.id)} onChange={() => { if (line !== undefined) toggleHidden(row.id); }} /></td>
                    <td>{row.label}</td>
                    <td className="num">{row.mean === null ? "–" : formatUnit(row.mean, "ms")}</td>
                    <td className="num">{row.min === null ? "–" : formatUnit(row.min, "ms")}</td>
                    <td className="num">{row.max === null ? "–" : formatUnit(row.max, "ms")}</td>
                    <td className="num">{row.lossPercent === null ? "–" : <span data-level={row.lossPercent > 1 ? "attention" : undefined}>{percent(row.lossPercent)}</span>}</td>
                    <td className="num">{row.errors}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          <p className="muted">丢包率 = 超时数 ÷ 发送数；错误不计入丢包。</p>
        </>
      )}
    </section>
  );
}
