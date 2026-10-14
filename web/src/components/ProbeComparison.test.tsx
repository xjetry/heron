import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, type HandlerContext } from "@connectrpc/connect";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { createRef, useImperativeHandle, useState } from "react";
import { afterEach, expect, it, vi } from "vitest";
import type { AlignedData } from "uplot";
import { ProbeComparison, type ProbeComparisonMethods } from "./ProbeComparison";
import { AdminService, ListNodesResponseSchema, ListProbeTasksResponseSchema } from "../gen/heron/v1/admin_pb";
import { PublicService } from "../gen/heron/v1/public_pb";
import { ListProbeComparisonNodesResponseSchema, QueryProbeComparisonResponseSchema, QueryProbesResponseSchema, type QueryProbeComparisonRequest } from "../gen/heron/v1/query_pb";
import { ProbeKind } from "../gen/heron/v1/types_pb";
import { ProbeTasks } from "../pages/ProbeTasks";
import { NodePage } from "../public/NodePage";
import { rangeHeader, renderWithAdmin, renderWithService } from "../test/harness";

vi.mock("./Chart", () => ({
  Chart: ({ labels, unit, data, hidden, legend, onFocus }: { labels: string[]; unit: string; data: AlignedData; hidden?: ReadonlySet<number>; legend?: boolean; onFocus?: (i: number | null) => void }) => (
    <div data-testid={`chart-${unit}`} data-labels={labels.join("|")} data-values={JSON.stringify(data.slice(1).map((column) => column[0]))} data-hidden={JSON.stringify([...(hidden ?? [])])} data-legend={String(legend ?? true)}>
      <button type="button" data-testid="focus-1" onClick={() => onFocus?.(1)}>聚焦第二条线</button>
    </div>
  ),
}));

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

const methods: ProbeComparisonMethods = {
  list: AdminService.method.listProbeComparisonNodes,
  query: AdminService.method.queryProbeComparison,
};
const names = [
  { id: 1n, name: "edge" },
  { id: 2n, name: "edge" },
  { id: 3n, name: "edge" },
  { id: 4n, name: "gone" },
  { id: 5n, name: "ok" },
];

const HUB_NOW = 1_700_000_000;
// 当前挂载的 TimedComparison 经 useImperativeHandle 把推进 hub now 的函数挂到这个 ref 上；渲染期不写组件外的变量。
const hubClock = createRef<{ advance: () => void }>();

function advanceHub() {
  if (!hubClock.current) throw new Error("TimedComparison is not mounted");
  hubClock.current.advance();
}

function TimedComparison({ nodes = names }: { nodes?: typeof names }) {
  const [now, setNow] = useState(HUB_NOW);
  useImperativeHandle(hubClock, () => ({ advance: () => setNow((previous) => previous + 60) }), []);
  return <ProbeComparison taskId={9n} methods={methods} nodes={nodes} now={now} />;
}

function pinClock() {
  const to = (Math.floor(HUB_NOW / 60) + 1) * 60;
  const from = to - 86400;
  return { from, to, ts: from - (from % 60) };
}

function renderComparison(impl: Parameters<typeof renderWithAdmin>[0], nodes = names, path = "/c") {
  return renderWithAdmin(impl, [{ path, element: <TimedComparison nodes={nodes} /> }], path);
}

function listed(nodeIds: bigint[], maxNodesPerQuery: number) {
  return create(ListProbeComparisonNodesResponseSchema, {
    kind: ProbeKind.ICMP, target: "edge.example", nodeIds, maxNodesPerQuery,
  });
}

function chunkOf(req: QueryProbeComparisonRequest, ts: number, unavailable: readonly bigint[] = []) {
  return create(QueryProbeComparisonResponseSchema, {
    level: "1m", stepS: 60, unavailableNodeIds: [...unavailable],
    series: req.nodeIds.filter((id) => !unavailable.includes(id)).map((id) => ({ nodeId: id, samples: [{ ts: BigInt(ts), sent: 1, lost: 0, errors: 1 }] })),
  });
}

it.each([2, 4])("块大小取自 List 的 max_nodes_per_query=%i，不写死", async (max) => {
  pinClock();
  const seen: bigint[][] = [];
  renderComparison({
    listProbeComparisonNodes: async () => listed([1n, 2n, 3n, 4n, 5n], max),
    queryProbeComparison: async (req) => {
      seen.push([...req.nodeIds]);
      return create(QueryProbeComparisonResponseSchema, { level: "1m", stepS: 60, series: req.nodeIds.map((id) => ({ nodeId: id, samples: [] })) });
    },
  });
  expect(await screen.findByRole("heading", { name: "ICMP edge.example" })).toBeInTheDocument();
  const want = max === 2 ? [[1n, 2n], [3n, 4n], [5n]] : [[1n, 2n, 3n, 4n], [5n]];
  expect(seen).toEqual(want);
});

it("max_nodes_per_query 为 0 时显示协议错误，不发分块请求", async () => {
  pinClock();
  const queryProbeComparison = vi.fn();
  renderComparison({
    listProbeComparisonNodes: async () => listed([1n, 2n], 0),
    queryProbeComparison,
  });
  expect(await screen.findByRole("alert")).toHaveTextContent("分块上限无效");
  expect(queryProbeComparison).not.toHaveBeenCalled();
  expect(screen.queryByRole("table")).toBeNull();
});

it("List NotFound 只说明没有可对比的节点，不展示服务端原文", async () => {
  pinClock();
  renderComparison({
    listProbeComparisonNodes: async () => { throw new ConnectError("task missing secret", Code.NotFound); },
  });
  expect(await screen.findByText("没有可对比的节点。")).toBeInTheDocument();
  expect(screen.queryByText(/secret/)).toBeNull();
  expect(screen.queryByRole("alert")).toBeNull();
});

it("FailedPrecondition 显示服务端消息", async () => {
  pinClock();
  renderComparison({
    listProbeComparisonNodes: async () => listed([1n], 8),
    queryProbeComparison: async () => { throw new ConnectError("读量超出额度，请缩小窗口", Code.FailedPrecondition); },
  });
  expect(await screen.findByRole("alert")).toHaveTextContent("读量超出额度，请缩小窗口");
});

it("全超时 100%、全本地错误 0%、混合按 lost/sent，三者都不进 RTT 图；同名消歧，不可见单列", async () => {
  const { ts } = pinClock();
  renderComparison({
    listProbeComparisonNodes: async () => listed([1n, 2n, 3n, 5n, 4n], 8),
    queryProbeComparison: async (req) => create(QueryProbeComparisonResponseSchema, {
      level: "1m", stepS: 60, unavailableNodeIds: req.nodeIds.filter((id) => id === 4n),
      series: [
        { nodeId: 1n, samples: [{ ts: BigInt(ts), sent: 10, lost: 10, errors: 0 }] },
        { nodeId: 2n, samples: [{ ts: BigInt(ts), sent: 5, lost: 0, errors: 5 }] },
        { nodeId: 3n, samples: [{ ts: BigInt(ts), sent: 10, lost: 4, errors: 3 }] },
        { nodeId: 5n, samples: [{ ts: BigInt(ts), sent: 10, lost: 1, errors: 0, rttMeanUs: 0 }] },
      ].filter((series) => req.nodeIds.includes(series.nodeId)),
    }),
  });
  const rtt = await screen.findByTestId("chart-ms");
  expect(rtt.dataset.labels).toBe("ok");
  expect(rtt.dataset.values).toBe("[0]");
  for (const [name, loss, errors] of [["edge #1", "100%", "0"], ["edge #2", "0.0%", "5"], ["edge #3", "40%", "3"]]) {
    expect(within(screen.getByRole("row", { name })).getAllByRole("cell").map((td) => td.textContent)).toEqual(["", name, "–", "–", "–", loss, errors]);
  }
  expect(screen.getByRole("checkbox", { name: "显示 gone" })).toBeDisabled();
});

it("未完成的块不算无结果，全部到齐才画", async () => {
  const { ts } = pinClock();
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  renderComparison({
    listProbeComparisonNodes: async () => listed([1n, 2n, 3n, 4n], 2),
    queryProbeComparison: async (req) => {
      if (req.nodeIds.includes(3n)) await gate;
      return chunkOf(req, ts);
    },
  }, names.slice(0, 4));
  expect(await screen.findByText("加载中…")).toBeInTheDocument();
  await waitFor(() => expect(screen.queryByText("加载中…")).toBeInTheDocument());
  expect(screen.queryByText(/没有读数/)).toBeNull();
  expect(screen.queryByText("edge")).toBeNull();
  expect(screen.queryByRole("table")).toBeNull();
  await act(async () => { release(); });
  expect(within(await screen.findByRole("table")).getAllByRole("row")).toHaveLength(5);
});

it("同时至多两块在飞", async () => {
  const { ts } = pinClock();
  let inFlight = 0;
  let maxInFlight = 0;
  const resolvers: (() => void)[] = [];
  renderComparison({
    listProbeComparisonNodes: async () => listed([1n, 2n, 3n, 4n], 1),
    queryProbeComparison: async (req) => {
      inFlight += 1;
      maxInFlight = Math.max(maxInFlight, inFlight);
      await new Promise<void>((resolve) => { resolvers.push(resolve); });
      inFlight -= 1;
      return chunkOf(req, ts);
    },
  }, names.slice(0, 4));
  await waitFor(() => expect(resolvers).toHaveLength(2));
  await act(async () => { await new Promise((resolve) => setTimeout(resolve, 30)); });
  expect(resolvers).toHaveLength(2);
  expect(maxInFlight).toBe(2);
  await act(async () => { resolvers[0](); });
  await waitFor(() => expect(resolvers).toHaveLength(3));
  expect(maxInFlight).toBe(2);
  await act(async () => {
    for (let i = 0; i < 6; i++) {
      const batch = resolvers.splice(0);
      batch.forEach((resolve) => resolve());
      await new Promise((r) => setTimeout(r, 0));
    }
  });
  expect(within(await screen.findByRole("table")).getAllByRole("row")).toHaveLength(5);
  expect(maxInFlight).toBe(2);
});

it("部分块失败显示错误与重试，不自动再请求", async () => {
  pinClock();
  const queryProbeComparison = vi.fn(async () => { throw new ConnectError("chunk down", Code.Unavailable); });
  renderComparison({
    listProbeComparisonNodes: async () => listed([1n, 2n, 3n], 1),
    queryProbeComparison,
  }, names.slice(0, 3));
  expect(await screen.findByRole("alert")).toHaveTextContent(/^chunk down/);
  const calls = queryProbeComparison.mock.calls.length;
  expect(calls).toBeGreaterThan(0);
  await act(async () => { await new Promise((resolve) => setTimeout(resolve, 40)); });
  expect(queryProbeComparison).toHaveBeenCalledTimes(calls);
  fireEvent.click(screen.getByRole("button", { name: "重试" }));
  // 重试是一次新请求：上一次的错误立即撤下、回到加载中，不等新请求的结果。
  expect(screen.queryByRole("alert")).toBeNull();
  expect(screen.getByText("加载中…")).toBeInTheDocument();
  await waitFor(() => expect(queryProbeComparison.mock.calls.length).toBeGreaterThan(calls));
});

it("刷新与换窗口取消尚未完成的块，离开页面同样取消", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(1_700_000_000_000);
  const signals: AbortSignal[] = [];
  const hang = async (_req: QueryProbeComparisonRequest, ctx: HandlerContext) => {
    signals.push(ctx.signal);
    await new Promise<void>((_resolve, reject) => {
      if (ctx.signal.aborted) reject(new ConnectError("canceled", Code.Canceled));
      else ctx.signal.addEventListener("abort", () => reject(new ConnectError("canceled", Code.Canceled)), { once: true });
    });
    return create(QueryProbeComparisonResponseSchema, { level: "1m", stepS: 60 });
  };
  const { router } = renderWithAdmin({
    listProbeComparisonNodes: async () => listed([1n], 1),
    queryProbeComparison: hang,
  }, [
    { path: "/c", element: <TimedComparison nodes={names.slice(0, 1)} /> },
    { path: "/gone", element: <p>离开</p> },
  ], "/c");
  await waitFor(() => expect(signals).toHaveLength(1));
  const first = signals[0];
  fireEvent.click(screen.getByRole("button", { name: "1h" }));
  await waitFor(() => expect(first.aborted).toBe(true));
  await waitFor(() => expect(signals.length).toBeGreaterThanOrEqual(2));
  const second = signals.at(-1)!;
  await act(async () => advanceHub());
  await waitFor(() => expect(second.aborted).toBe(true));
  await act(async () => { await router.navigate("/gone"); });
  expect(signals.at(-1)!.aborted).toBe(true);
  expect(screen.getByText("离开")).toBeInTheDocument();
});

it("刷新期间保留上一张完整的图，并标成更新中", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(1_700_000_000_000);
  const to = (Math.floor(HUB_NOW / 60) + 1) * 60;
  const ts = (to - 86400) - ((to - 86400) % 60);
  let queries = 0;
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  renderComparison({
    listProbeComparisonNodes: async () => listed([1n], 1),
    queryProbeComparison: async (req) => {
      queries += 1;
      if (queries > 1) await gate;
      return chunkOf(req, ts);
    },
  }, names.slice(0, 1));
  expect(await screen.findByRole("table")).toBeInTheDocument();
  await act(async () => advanceHub());
  expect((await screen.findByText("更新中")).parentElement).toBe(rangeHeader());
  expect(screen.getByText(/级别 1m，每点 60 秒/).closest("header")).toBe(rangeHeader());
  expect(screen.getByRole("table")).toBeInTheDocument();
  expect(screen.queryByText("加载中…")).toBeNull();
  await act(async () => { release(); });
  await waitFor(() => expect(screen.queryByText("更新中")).toBeNull());
  expect(screen.getByRole("heading", { name: "ICMP edge.example" })).toBeInTheDocument();
});

it("换窗口后新窗口的结果未到时，提示图表还不是该窗口的结果", async () => {
  const { ts } = pinClock();
  let queries = 0;
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  renderComparison({
    listProbeComparisonNodes: async () => listed([1n], 1),
    queryProbeComparison: async (req) => {
      queries += 1;
      if (queries > 1) await gate;
      return chunkOf(req, ts);
    },
  }, names.slice(0, 1));
  expect(await screen.findByRole("table")).toBeInTheDocument();
  expect(screen.queryByText(/图表还不是/)).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "7d" }));
  expect((await screen.findByText(/图表还不是 7d 窗口的结果/)).parentElement).toBe(rangeHeader());
  expect(screen.getByRole("table")).toBeInTheDocument();
  await act(async () => { release(); });
  await waitFor(() => expect(screen.queryByText(/图表还不是/)).toBeNull());
});

it("没有 RTT 的节点仍逐行显示丢包和错误，不折叠汇总表", async () => {
  const { ts } = pinClock();
  const ids = [1n, 2n, 3n, 4n, 5n, 6n, 7n, 8n, 9n];
  renderComparison({
    listProbeComparisonNodes: async () => listed(ids, 9),
    queryProbeComparison: async (req) => chunkOf(req, ts),
  }, ids.map((id) => ({ id, name: `n${id}` })));
  const table = within(await screen.findByRole("table"));
  expect(table.getAllByRole("row")).toHaveLength(10);
  for (const id of ids) {
    expect(within(table.getByRole("row", { name: `n${id}` })).getAllByRole("cell").map((td) => td.textContent)).toEqual(["", `n${id}`, "–", "–", "–", "0.0%", "1"]);
    expect(table.getByRole("checkbox", { name: `显示 n${id}` })).toBeDisabled();
  }
  expect(screen.getByText("窗口内没有读数")).toBeInTheDocument();
  expect(screen.queryByTestId("chart-ms")).toBeNull();
});

it("页头显示类型、目标与节点数，汇总表排序、显隐和悬停联动，丢包标色并解释口径", async () => {
  const { ts } = pinClock();
  renderComparison({
    listProbeComparisonNodes: async () => listed([1n, 4n, 5n], 8),
    queryProbeComparison: async () => create(QueryProbeComparisonResponseSchema, { level: "1m", stepS: 60, unavailableNodeIds: [4n], series: [
      { nodeId: 1n, samples: [{ ts: BigInt(ts), sent: 4, lost: 0, errors: 0, rttMeanUs: 20_000, rttMinUs: 10_000, rttMaxUs: 30_000 }] },
      { nodeId: 5n, samples: [{ ts: BigInt(ts), sent: 100, lost: 2, errors: 1, rttMeanUs: 5_000, rttMinUs: 1_000, rttMaxUs: 9_000 }] },
    ] }),
  });
  const heading = await screen.findByRole("heading", { name: "ICMP edge.example" });
  expect(within(heading).getByText("ICMP")).toHaveClass("kind-badge");
  expect(screen.getByText("3 个节点执行此探测")).toBeInTheDocument();
  expect(screen.getByTestId("chart-ms")).toHaveAttribute("data-legend", "false");
  const table = within(screen.getByRole("table"));
  expect(table.getAllByRole("columnheader").map((th) => th.textContent)).toEqual(["显示", "节点", "均值", "最小", "最大", "丢包率", "错误数"]);
  const cells = (name: string) => within(table.getByRole("row", { name })).getAllByRole("cell").map((td) => td.textContent);
  expect(cells("edge")).toEqual(["", "edge", "20.0 ms", "10.0 ms", "30.0 ms", "0.0%", "0"]);
  expect(cells("ok")).toEqual(["", "ok", "5.00 ms", "1.00 ms", "9.00 ms", "2.0%", "1"]);
  expect(within(table.getByRole("row", { name: "ok" })).getByText("2.0%")).toHaveAttribute("data-level", "attention");
  expect(within(table.getByRole("row", { name: "edge" })).getByText("0.0%")).not.toHaveAttribute("data-level");
  expect(cells("gone")).toEqual(["", "gone", "–", "–", "–", "–", "0"]);
  expect(table.getByRole("checkbox", { name: "显示 gone" })).toBeDisabled();
  fireEvent.click(table.getByRole("checkbox", { name: "显示 edge" }));
  expect(screen.getByTestId("chart-ms")).toHaveAttribute("data-hidden", "[0]");
  fireEvent.click(table.getByRole("checkbox", { name: "显示 edge" }));
  expect(screen.getByTestId("chart-ms")).toHaveAttribute("data-hidden", "[]");
  fireEvent.click(table.getByRole("button", { name: "均值" }));
  expect(table.getAllByRole("row").slice(1).map((row) => row.getAttribute("aria-label"))).toEqual(["ok", "edge", "gone"]);
  expect(table.getByRole("columnheader", { name: "均值" })).toHaveAttribute("aria-sort", "ascending");
  fireEvent.click(table.getByRole("button", { name: "均值" }));
  expect(table.getAllByRole("row").slice(1).map((row) => row.getAttribute("aria-label"))).toEqual(["edge", "ok", "gone"]);
  expect(table.getByRole("columnheader", { name: "均值" })).toHaveAttribute("aria-sort", "descending");
  fireEvent.click(screen.getByTestId("focus-1"));
  expect(table.getByRole("row", { name: "ok" })).toHaveAttribute("data-focused", "true");
  expect(screen.getByText("丢包率 = 超时数 ÷ 发送数；错误不计入丢包。")).toBeInTheDocument();
});

it("管理端任务行进入对比页，公开端只给已标注的任务入口", async () => {
  const { router } = renderWithAdmin({
    listNodes: async () => create(ListNodesResponseSchema, { nodes: [{ id: 1n, name: "n" }] }),
    listProbeTasks: async () => create(ListProbeTasksResponseSchema, { tasks: [{ task: { id: 3n, kind: ProbeKind.ICMP, target: "1.1.1.1", intervalS: 60, timeoutMs: 1000 }, nodeIds: [1n] }] }),
  }, [
    { path: "/probes", Component: ProbeTasks },
    { path: "/probes/:id/compare", element: <p>对比页</p> },
  ], "/probes");
  fireEvent.click(await screen.findByRole("button", { name: "更多操作 1.1.1.1（#3）" }));
  const compare = screen.getByRole("menuitem", { name: "对比 1.1.1.1（#3）" });
  expect(compare).toHaveAttribute("href", "/probes/3/compare");
  fireEvent.click(compare);
  await waitFor(() => expect(router.state.location.pathname).toBe("/probes/3/compare"));
  expect(screen.getByText("对比页")).toBeInTheDocument();
});

it("公开节点页的已标注任务链到 /probes/:id，未标注的不给入口", async () => {
  const { router } = renderWithService(PublicService, {
    getSnapshot: async () => ({ nodes: [{ id: 7n, name: "edge" }] }),
    queryMetrics: async () => ({ level: "1m", stepS: 60, ts: [], series: [] }),
    queryProbes: async () => create(QueryProbesResponseSchema, { level: "1m", stepS: 60, series: [
      { taskId: 3n, kind: ProbeKind.TCP, target: "example.com:443" },
      { taskId: 4n, kind: ProbeKind.UNSPECIFIED },
    ] }),
  }, [
    { path: "/nodes/:id", Component: NodePage },
    { path: "/probes/:id", element: <p>公开对比</p> },
  ], "/nodes/7");
  const link = await screen.findByRole("link", { name: "TCP example.com:443" });
  expect(screen.queryByRole("link", { name: /任务 #4/ })).toBeNull();
  fireEvent.click(link);
  await waitFor(() => expect(router.state.location.pathname).toBe("/probes/3"));
  expect(screen.getByText("公开对比")).toBeInTheDocument();
});

// 显隐偏好属于节点，不属于线的位置：RTT 图只给窗口内有读数的节点分配线（assembleComparison），每分钟刷新
// 后线的集合会变。按索引保存会让"隐藏 a"在 a 暂时没有读数、b 顶到原索引时变成"隐藏 b"。
it("刷新后线集合变化，隐藏的仍是同一个节点；节点没有读数期间偏好保留，读数回来后继续隐藏", async () => {
  const to = (Math.floor(HUB_NOW / 60) + 1) * 60;
  const from = to - 86400;
  // 取窗口内一小时后的桶：窗口右端随 hub 时钟每分钟前进，起点的桶会掉出网格，这个桶在三次查询里都在。
  const ts = from - (from % 60) + 3600;
  let calls = 0;
  renderComparison({
    listProbeComparisonNodes: async () => listed([1n, 2n], 8),
    queryProbeComparison: async () => {
      calls += 1;
      const aHasRtt = calls !== 2;
      return create(QueryProbeComparisonResponseSchema, { level: "1m", stepS: 60, series: [
        { nodeId: 1n, samples: [aHasRtt ? { ts: BigInt(ts), sent: 4, lost: 0, errors: 0, rttMeanUs: 20_000 } : { ts: BigInt(ts), sent: 4, lost: 4, errors: 0 }] },
        { nodeId: 2n, samples: [{ ts: BigInt(ts), sent: 4, lost: 0, errors: 0, rttMeanUs: 30_000 }] },
      ] });
    },
  }, [{ id: 1n, name: "a" }, { id: 2n, name: "b" }]);
  const table = within(await screen.findByRole("table"));
  expect(screen.getByTestId("chart-ms")).toHaveAttribute("data-labels", "a|b");
  fireEvent.click(table.getByRole("checkbox", { name: "显示 a" }));
  expect(screen.getByTestId("chart-ms")).toHaveAttribute("data-hidden", "[0]");

  await act(async () => advanceHub());
  await waitFor(() => expect(screen.getByTestId("chart-ms")).toHaveAttribute("data-labels", "b"));
  expect(screen.getByTestId("chart-ms")).toHaveAttribute("data-hidden", "[]");
  expect(table.getByRole("checkbox", { name: "显示 a" })).toBeDisabled();
  expect(table.getByRole("checkbox", { name: "显示 b" })).toBeChecked();

  await act(async () => advanceHub());
  await waitFor(() => expect(screen.getByTestId("chart-ms")).toHaveAttribute("data-labels", "a|b"));
  expect(screen.getByTestId("chart-ms")).toHaveAttribute("data-hidden", "[0]");
  expect(table.getByRole("checkbox", { name: "显示 a" })).not.toBeChecked();
  expect(table.getByRole("checkbox", { name: "显示 b" })).toBeChecked();
});
