import { create } from "@bufbuild/protobuf";
import { QueryProbesResponseSchema } from "../gen/heron/v1/query_pb";
import { CollectionComponent, ProbeKind } from "../gen/heron/v1/types_pb";
import { Code, ConnectError } from "@connectrpc/connect";
import { act, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { rangeHeader, renderWithAdmin, type AdminImpl } from "../test/harness";
import { NodeDetail } from "./NodeDetail";
import { dateTime } from "../lib/format";

afterEach(() => vi.useRealTimers());

vi.mock("../components/Chart", () => ({
  Chart: ({ labels, unit, data }: { labels: string[]; unit: string; data: unknown[] }) => (
    <div data-testid="chart" data-labels={labels.join(",")} data-unit={unit} data-points={String((data[0] as unknown[]).length)}>
      {labels.map((label) => <span key={label}>{label}</span>)}
    </div>
  ),
}));

const listNodes = async () => ({
  nodes: [{ id: 7n, name: "db-01", public: false, note: "", sortOrder: 0, createdAt: 0n,
    facts: { hostname: "db-01.internal", os: "Debian 12", kernel: "6.1", arch: "amd64", virtualization: "kvm", cpuModel: "EPYC", cpuCores: 4, agentVersion: "dev", icmpAvailable: false } }],
});

const trafficOf = (nodeId: bigint) => ({
  nodeId, name: "db-01",
  traffic: { totalRx: 10n * 1024n ** 3n, totalTx: 5n * 1024n ** 3n, periodRx: 1024n ** 3n, periodTx: 512n * 1024n ** 2n,
    periodStart: 1_756_684_800n, nextResetAt: 1_759_276_800n, resetDay: 1, quotaUsedBytes: 512n * 1024n ** 2n, quotaBytes: 1024n ** 3n, quotaUsedPct: 50 },
});
const getTraffic = async () => ({ timezone: "UTC", now: 1_757_000_000n, nodes: [trafficOf(7n)] });

const defaultImpl = {
  listNodes, getTraffic,
  getSnapshot: async () => ({ nodes: [] }),
  listTags: async () => ({ tags: [] }),
  queryMetrics: async () => ({ level: "1m", stepS: 60, ts: [], series: [], coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } }),
  queryProbes: async () => create(QueryProbesResponseSchema, { level: "1m", stepS: 60 }),
} satisfies AdminImpl;

it.each(["listNodes", "getTraffic"] as const)("详情 %s 刷新失败保留内容与校正草稿", async (method) => {
  let fail = false;
  const { queryClient } = renderWithAdmin({ ...defaultImpl, [method]: async () => {
    if (fail) throw new ConnectError(`${method} refresh failed`, Code.Unavailable);
    return defaultImpl[method]();
  } }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=traffic");
  const input = await screen.findByLabelText("本周期下行 (GiB)");
  await screen.findByRole("heading", { name: "db-01" });
  fireEvent.change(input, { target: { value: "2.5" } });
  fail = true;
  await act(async () => { await queryClient.refetchQueries(); });
  expect(await screen.findByRole("alert")).toHaveTextContent(`${method} refresh failed`);
  expect(screen.getByLabelText("本周期下行 (GiB)")).toBe(input);
  expect(input).toHaveValue("2.5");
  expect(screen.getByRole("heading", { name: "db-01" })).toBeInTheDocument();
  expect(screen.getByText("512 MiB / 1.0 GiB (50.0%)")).toBeInTheDocument();
});

it("四个查询同文刷新失败只显示一条", async () => {
  let fail = false;
  const failing = <A extends unknown[], R>(impl: (...args: A) => Promise<R>) => async (...args: A): Promise<R> => {
    if (fail) throw new ConnectError("hub unreachable", Code.Unavailable);
    return impl(...args);
  };
  const { queryClient } = renderWithAdmin({
    ...defaultImpl,
    listNodes: failing(defaultImpl.listNodes),
    getTraffic: failing(defaultImpl.getTraffic),
    queryMetrics: failing(defaultImpl.queryMetrics),
    queryProbes: failing(defaultImpl.queryProbes),
  }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  await screen.findByRole("heading", { name: "db-01" });
  fail = true;
  await act(async () => { await queryClient.refetchQueries(); });
  expect((await screen.findAllByRole("alert")).map((a) => a.textContent)).toEqual(["hub unreachable"]);
  expect(screen.getByRole("heading", { name: "db-01" })).toBeInTheDocument();
});

it("listNodes 从未成功但历史与流量已就绪时仍显示图表、流量卡与切窗按钮，只加错误横幅", async () => {
  renderWithAdmin({ ...defaultImpl, listNodes: async () => { throw new ConnectError("nodes down", Code.Unavailable); } },
    [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  expect(await screen.findByRole("alert")).toHaveTextContent("nodes down");
  expect(await screen.findAllByTestId("chart")).toHaveLength(12);
  expect(screen.getByRole("button", { name: "24h" })).toBeInTheDocument();
  fireEvent.click(screen.getByRole("tab", { name: "流量校正" }));
  expect(await screen.findByText("512 MiB / 1.0 GiB (50.0%)")).toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "节点 #7" })).toBeInTheDocument();
});

it("listNodes 挂起不阻挡历史图表，诊断 tab 显示加载中", async () => {
  let release!: (v: Awaited<ReturnType<typeof listNodes>>) => void;
  const pending = new Promise<Awaited<ReturnType<typeof listNodes>>>((resolve) => { release = resolve; });
  renderWithAdmin({ ...defaultImpl, listNodes: () => pending }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  expect(await screen.findAllByTestId("chart")).toHaveLength(12);
  fireEvent.click(screen.getByRole("tab", { name: "Agent 诊断" }));
  expect(screen.getByText("加载中…")).toBeInTheDocument();
  expect(screen.queryByText("主机名")).toBeNull();
  await act(async () => { release(await listNodes()); });
});

it("窗口每分钟前进后请求失败，图表与级别仍在并带横幅，不误报“非当前窗口”", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  let fail = false;
  const queryMetrics = vi.fn<NonNullable<AdminImpl["queryMetrics"]>>(async () => {
    if (fail) throw new ConnectError("history down", Code.Unavailable);
    return { level: "1m", stepS: 60, ts: [], series: [], coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } };
  });
  renderWithAdmin({ ...defaultImpl, queryMetrics, getTraffic: async () => ({ ...await getTraffic(), now: 1_757_000_000n + (fail ? 60n : 0n) }) }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  expect(await screen.findAllByTestId("chart")).toHaveLength(12);
  await screen.findByText(/级别 1m，每点 60 秒/);
  fail = true;
  // 下次流量轮询带来跨分钟的 hub now，换键后的历史请求失败。
  await act(async () => { await vi.advanceTimersByTimeAsync(60_000 + 100); });
  expect(await screen.findByRole("alert")).toHaveTextContent("history down");
  expect(screen.getAllByTestId("chart")).toHaveLength(12);
  expect(screen.getByText(/级别 1m，每点 60 秒/)).toBeInTheDocument();
  // 沿用的还是 24h 这个 range 自己的数据，只是这次刷新没成功；range 没变，不该报"看错窗口"，
  // 失败已经由上面的横幅表达。
  expect(screen.queryByText(/图表还不是/)).toBeNull();
});

it("切到另一个节点、新节点历史未返回时不显示上一个节点的图表与级别", async () => {
  let releaseNode8!: () => void;
  const node8Gate = new Promise<void>((resolve) => { releaseNode8 = resolve; });
  const listTwoNodes = async () => ({
    nodes: [
      { id: 7n, name: "db-07", public: false, note: "", sortOrder: 0, createdAt: 0n },
      { id: 8n, name: "db-08", public: false, note: "", sortOrder: 1, createdAt: 0n },
    ],
  });
  const queryMetrics = vi.fn<NonNullable<AdminImpl["queryMetrics"]>>(async (req) => {
    if (req.nodeId === 8n) await node8Gate;
    return req.nodeId === 8n ? { level: "5m", stepS: 300, ts: [], series: [], coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } } : { level: "1m", stepS: 60, ts: [], series: [], coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } };
  });
  const { router } = renderWithAdmin({ ...defaultImpl, listNodes: listTwoNodes, queryMetrics }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  await screen.findByRole("heading", { name: "db-07" });
  await screen.findByText(/级别 1m，每点 60 秒/);
  await act(async () => { await router.navigate("/nodes/8"); });
  expect(await screen.findByRole("heading", { name: "db-08" })).toBeInTheDocument();
  // 8 的历史还没回来：不能把 7 的"级别 1m"标签或图表当成 8 的显示，这段时间没有图表比显示错的更安全。
  expect(screen.queryByText(/级别 1m/)).toBeNull();
  expect(screen.queryAllByTestId("chart")).toHaveLength(0);
  await act(async () => { releaseNode8(); });
  expect(await screen.findByText(/级别 5m，每点 300 秒/)).toBeInTheDocument();
  expect(screen.getAllByTestId("chart")).toHaveLength(12);
});

it("告警事件 tab 内嵌该节点的事件列表，离开概览前不请求事件与名称", async () => {
  const listAlertEvents = vi.fn(async () => ({ events: [] }));
  const listAlertRules = vi.fn(async () => ({ rules: [] }));
  const listNotifyChannels = vi.fn(async () => ({ channels: [] }));
  renderWithAdmin({ ...defaultImpl, listAlertEvents, listAlertRules, listNotifyChannels }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  await screen.findByRole("heading", { name: "db-01" });
  expect(listAlertEvents).not.toHaveBeenCalled();
  expect(listAlertRules).not.toHaveBeenCalled();
  expect(listNotifyChannels).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("tab", { name: "告警事件" }));
  expect(await screen.findByText("没有告警事件。")).toBeInTheDocument();
  expect(listAlertEvents).toHaveBeenCalledWith(expect.objectContaining({ nodeId: 7n }), expect.anything());
  expect(listAlertRules).toHaveBeenCalledTimes(1);
  expect(listNotifyChannels).toHaveBeenCalledTimes(1);
});

it("事件 tab 的规则格显示带编号的规则名称", async () => {
  renderWithAdmin({ ...defaultImpl,
    listAlertEvents: async () => ({ events: [{ id: 1n, nodeId: 7n, ruleId: 3n, transition: "firing" }] }),
    listAlertRules: async () => ({ rules: [{ id: 3n, name: "CPU 过高" }] }),
    listNotifyChannels: async () => ({ channels: [] }),
  }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=events");
  expect(await screen.findByRole("cell", { name: "CPU 过高（#3）" })).toBeInTheDocument();
});

it.each(["listAlertRules", "listNotifyChannels"] as const)("事件名称查询 %s 失败只在事件 tab 显示错误", async (method) => {
  renderWithAdmin({ ...defaultImpl,
    listAlertEvents: async () => ({ events: [] }),
    listAlertRules: async () => ({ rules: [] }),
    listNotifyChannels: async () => ({ channels: [] }),
    [method]: async () => { throw new ConnectError("names unavailable", Code.Unavailable); },
  }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=events");
  expect(await screen.findByRole("alert")).toHaveTextContent("names unavailable");
  fireEvent.click(screen.getByRole("tab", { name: "概览" }));
  expect(screen.queryByRole("alert")).toBeNull();
});

it("同窗口同名任务各有一张探测图，标题带编号区分并链接对比页", async () => {
  renderWithAdmin({
    ...defaultImpl,
    queryProbes: async () => create(QueryProbesResponseSchema, { stepS: 60, series: [
      { taskId: 7n, kind: ProbeKind.ICMP, target: "1.1.1.1" },
      { taskId: 3n, kind: ProbeKind.ICMP, target: "1.1.1.1" },
    ] }),
  }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  await screen.findByRole("heading", { name: "ICMP 1.1.1.1 #7" });
  for (const id of [7, 3]) {
    const heading = screen.getByRole("heading", { name: `ICMP 1.1.1.1 #${id}` });
    expect(within(heading).getByRole("link")).toHaveAttribute("href", `/probes/${id}/compare`);
    expect(within(heading.parentElement!).getAllByTestId("chart")).toHaveLength(1);
  }
  expect(screen.getAllByTestId("chart")).toHaveLength(14);
});

it("状态头显示状态与最近上报、五段元信息、落后徽章及六格，编辑入口打开抽屉", async () => {
  renderWithAdmin({ ...defaultImpl,
    getSnapshot: async () => ({ now: 1_000n, boundAgentVersion: "v0.8.0", nodes: [{ id: 7n, online: true, lastSeenAt: 990n, metrics: { cpuPct: 42 } }] }),
    listNodes: async () => ({ nodes: [{ ...(await listNodes()).nodes[0], country: "JP", facts: { ...(await listNodes()).nodes[0].facts,
      agentVersion: "v0.7.0", network: { ipv4: { state: 1, address: "8.8.8.8" } } }, network: { ipv4: { state: 1, address: "8.8.8.8", source: 1 }, ipv6: {} } }] }),
  }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  const head = within(await screen.findByRole("group", { name: "节点状态" }));
  expect(await head.findByText("在线 · 最近上报 10 秒前")).toBeInTheDocument();
  expect(head.getByText(/8\.8\.8\.8 · — · db-01\.internal · 6\.1 · v0\.7\.0/)).toHaveClass("node-head-meta", "num");
  expect(head.getByTitle("国家 / 地区 JP")).toBeInTheDocument();
  expect(head.getByText("agent 低于 v0.8.0")).toHaveClass("badge-attention");
  expect(head.getAllByRole("group").map((g) => g.getAttribute("aria-label"))).toEqual(["CPU", "内存", "磁盘", "网络", "运行时长", "剩余天数"]);
  expect(head.getByRole("group", { name: "CPU" })).toHaveTextContent("42%");
  fireEvent.click(head.getByRole("button", { name: "编辑" }));
  expect(await screen.findByRole("heading", { name: "编辑节点 · db-01（#7）" })).toBeInTheDocument();
});

it.each([
  { maintenance: false, rented: false, online: true, lastSeenAt: 990n, label: "在线 · 最近上报 10 秒前" },
  { maintenance: false, rented: false, online: false, lastSeenAt: 990n, label: "离线 · 最近上报 10 秒前" },
  { maintenance: false, rented: false, online: false, lastSeenAt: undefined, label: "从未上报" },
  { maintenance: true, rented: false, online: true, lastSeenAt: 990n, label: "维护中 · 最近上报 10 秒前" },
  { maintenance: false, rented: true, online: true, lastSeenAt: 990n, label: "已出租 · 最近上报 10 秒前" },
])("状态头使用统一五态：$label；相同版本不显示落后", async ({ maintenance, rented, online, lastSeenAt, label }) => {
  renderWithAdmin({ ...defaultImpl,
    getSnapshot: async () => ({ now: 1_000n, boundAgentVersion: "v0.8.0", nodes: [{ id: 7n, online, lastSeenAt }] }),
    listNodes: async () => ({ nodes: [{ ...(await listNodes()).nodes[0], maintenance, rented, facts: { ...(await listNodes()).nodes[0].facts, agentVersion: "v0.8.0" } }] }),
  }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  const head = within(await screen.findByRole("group", { name: "节点状态" }));
  expect(await head.findByText(label)).toHaveClass("status-badge");
  expect(head.queryByText(/agent 低于/)).toBeNull();
});

it("tab 跟随 URL，非法值回概览；切换保留窗口且不重发历史查询", async () => {
  const queryMetrics = vi.fn(defaultImpl.queryMetrics);
  const { router, queryClient } = renderWithAdmin({ ...defaultImpl, queryMetrics }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=bogus");
  expect(await screen.findByRole("tab", { name: "概览" })).toHaveAttribute("aria-selected", "true");
  await screen.findAllByTestId("chart");
  await waitFor(() => expect(queryClient.isFetching()).toBe(0));
  fireEvent.click(screen.getByRole("button", { name: "6h" }));
  await waitFor(() => expect(queryMetrics).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(queryClient.isFetching()).toBe(0));
  fireEvent.click(screen.getByRole("tab", { name: "Agent 诊断" }));
  expect(router.state.location.search).toBe("?tab=diagnostics");
  expect(router.state.historyAction).toBe("REPLACE");
  expect(screen.getByRole("region", { name: "Agent 运行诊断" })).toBeInTheDocument();
  const tab = screen.getByRole("tab", { name: "Agent 诊断" });
  const panel = screen.getByRole("tabpanel", { name: "Agent 诊断" });
  expect(tab).toHaveAttribute("aria-controls", panel.id);
  expect(panel).toHaveAttribute("aria-labelledby", tab.id);
  fireEvent.click(screen.getByRole("tab", { name: "概览" }));
  expect(router.state.location.search).toBe("");
  expect(screen.getByRole("button", { name: "6h" })).toHaveAttribute("aria-pressed", "true");
  await waitFor(() => expect(queryClient.isFetching()).toBe(0));
  expect(queryMetrics).toHaveBeenCalledTimes(2);
});

it("诊断深链直接选中 Agent 诊断", async () => {
  renderWithAdmin(defaultImpl, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=diagnostics");
  expect(await screen.findByRole("tab", { name: "Agent 诊断" })).toHaveAttribute("aria-selected", "true");
  expect(await screen.findByRole("region", { name: "Agent 运行诊断" })).toBeInTheDocument();
});

it.each([false, true])("详情编辑保存等待回读，回读失败=%s 时保留草稿", async (fail) => {
  let saving = false;
  let release!: () => void;
  let started!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  const reading = new Promise<void>((resolve) => { started = resolve; });
  const updateNode = vi.fn(async () => { saving = true; return {}; });
  renderWithAdmin({ ...defaultImpl, updateNode,
    listNodes: async () => {
      if (saving) {
        started();
        await gate;
        if (fail) throw new ConnectError("readback unavailable", Code.Unavailable);
      }
      return { nodes: [{ ...(await listNodes()).nodes[0], trafficResetDay: 1 }] };
    },
  }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  await screen.findByRole("heading", { name: "db-01" });
  fireEvent.click(screen.getByRole("button", { name: "编辑" }));
  const input = screen.getByRole("textbox", { name: "名称 db-01（#7）" });
  fireEvent.change(input, { target: { value: "renamed" } });
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "保存" })); await reading; });
  expect(screen.getByRole("dialog")).toBeInTheDocument();
  await waitFor(() => expect(screen.getByRole("button", { name: "关闭抽屉" })).toBeDisabled());
  expect(screen.getByRole("textbox", { name: "名称 db-01（#7）" })).toBe(input);
  expect(input).toHaveValue("renamed");
  expect(updateNode).toHaveBeenCalledWith(expect.objectContaining({ id: 7n, name: "renamed" }), expect.anything());
  await act(async () => { release(); });
  if (fail) {
    await waitFor(() => expect(within(screen.getByRole("dialog")).getByText(/已保存，但回读失败：readback unavailable/)).toBeInTheDocument());
    const drawer = within(screen.getByRole("dialog"));
    expect(drawer.getByRole("textbox", { name: "名称 db-01（#7）" })).toBe(input);
    expect(input).toHaveValue("renamed");
    expect(drawer.getByRole("button", { name: "保存" })).toBeEnabled();
  } else {
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  }
});

// 详情页与节点页、续费页共用同一个编辑草稿；整体替换缺 rented 会把已出租清成 false。
it("详情编辑保存的请求带上 rented", async () => {
  const updateNode = vi.fn(async () => ({}));
  renderWithAdmin({ ...defaultImpl, updateNode,
    listNodes: async () => ({ nodes: [{ ...(await listNodes()).nodes[0], rented: true, trafficResetDay: 1 }] }),
  }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  await screen.findByRole("heading", { name: "db-01" });
  fireEvent.click(screen.getByRole("button", { name: "编辑" }));
  fireEvent.click(await screen.findByRole("button", { name: "保存" }));
  await waitFor(() => expect(updateNode).toHaveBeenCalledWith(expect.objectContaining({ id: 7n, rented: true }), expect.anything()));
});

describe("NodeDetail", () => {
  it.each(["UTC", "Asia/Tokyo"])("流量周期按 hub 时区 %s 显示", async (timezone) => {
    renderWithAdmin({ ...defaultImpl, listNodes, queryMetrics: async () => ({ level: "1m", stepS: 60, ts: [], series: [], coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } }),
      getTraffic: async () => ({ ...await getTraffic(), timezone }) },
      [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=traffic");
    const t = trafficOf(7n).traffic;
    const start = dateTime(t.periodStart, timezone);
    const next = dateTime(t.nextResetAt, timezone);
    expect(await screen.findByText(start)).toBeInTheDocument();
    expect(screen.getByText(`${next}（每月 1 日，${timezone}）`)).toBeInTheDocument();
  });

  it("流量卡显示周期与总量，并按 GiB 提交校正后刷新", async () => {
    const adjustTraffic = vi.fn(async () => ({ traffic: trafficOf(7n).traffic }));
    const traffic = vi.fn(getTraffic);
    renderWithAdmin({ ...defaultImpl, listNodes, queryMetrics: async () => ({ level: "1m", stepS: 60, ts: [], series: [], coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } }), getTraffic: traffic, adjustTraffic },
      [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=traffic");
    expect(await screen.findByText("512 MiB / 1.0 GiB (50.0%)")).toBeInTheDocument();
    expect(screen.getByText("↓ 10 GiB ↑ 5.0 GiB")).toBeInTheDocument();
    expect(screen.getByText(/每月 1 日/)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("本周期下行 (GiB)"), { target: { value: "2.5" } });
    fireEvent.change(screen.getByLabelText("本周期上行 (GiB)"), { target: { value: "0" } });
    fireEvent.click(screen.getByRole("button", { name: "校正本周期" }));
    await waitFor(() => expect(adjustTraffic).toHaveBeenCalledWith(expect.objectContaining({ nodeId: 7n, periodRx: 2_684_354_560n, periodTx: 0n }), expect.anything()));
    await waitFor(() => expect(traffic.mock.calls.length).toBeGreaterThanOrEqual(2));
  });

  it("校正输入不是非负数时按钮禁用", async () => {
    renderWithAdmin({ ...defaultImpl, listNodes, queryMetrics: async () => ({ level: "1m", stepS: 60, ts: [], series: [], coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } }), getTraffic },
      [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=traffic");
    await screen.findByText("512 MiB / 1.0 GiB (50.0%)");
    fireEvent.change(screen.getByLabelText("本周期下行 (GiB)"), { target: { value: "-1" } });
    expect(screen.getByRole("button", { name: "校正本周期" })).toBeDisabled();
    fireEvent.change(screen.getByLabelText("本周期下行 (GiB)"), { target: { value: "abc" } });
    expect(screen.getByRole("button", { name: "校正本周期" })).toBeDisabled();
    fireEvent.change(screen.getByLabelText("本周期下行 (GiB)"), { target: { value: "0" } });
    expect(screen.getByRole("button", { name: "校正本周期" })).toBeEnabled();
  });

  it.each(["1e308", "17179869184"])("校正输入 %s 超出字节范围时按钮禁用", async (value) => {
    renderWithAdmin({ ...defaultImpl, listNodes, queryMetrics: async () => ({ level: "1m", stepS: 60, ts: [], series: [], coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } }), getTraffic },
      [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=traffic");
    await screen.findByText("512 MiB / 1.0 GiB (50.0%)");
    fireEvent.change(screen.getByLabelText("本周期下行 (GiB)"), { target: { value } });
    expect(screen.getByRole("button", { name: "校正本周期" })).toBeDisabled();
  });

  it("流量首次失败时没有 hub 时间，不查历史但保留节点信息", async () => {
    const queryMetrics = vi.fn(async () => ({ level: "1m", stepS: 60, ts: [], series: [], coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } }));
    renderWithAdmin({ ...defaultImpl, listNodes, queryMetrics,
      getTraffic: async () => { throw new ConnectError("traffic unavailable", Code.Unavailable); } },
      [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
    expect(await screen.findByRole("alert")).toHaveTextContent(/^traffic unavailable$/);
    expect(queryMetrics).not.toHaveBeenCalled();
    expect(screen.queryAllByTestId("chart")).toHaveLength(0);
    expect(screen.getByRole("heading", { name: "db-01" })).toBeInTheDocument();
  });

  it("切窗请求挂起期间保留上一份图表", async () => {
    const response = { level: "1m", stepS: 60, ts: [], series: [], coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } };
    let release!: () => void;
    let started!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const pending = new Promise<void>((resolve) => { started = resolve; });
    const queryMetrics = vi.fn(async () => {
      if (queryMetrics.mock.calls.length > 1) { started(); await gate; }
      return response;
    });
    renderWithAdmin({ ...defaultImpl, getTraffic, listNodes, queryMetrics }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
    await screen.findByText(/级别 1m/);
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "7d" })); await pending; });
    try { expect(screen.queryAllByTestId("chart").length).toBe(12); }
    finally { await act(async () => { release(); }); }
  });

  it("同一窗口内的每分钟刷新挂起不误报“非当前窗口”，换窗口挂起才提示", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const response = { level: "1m", stepS: 60, ts: [], series: [], coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } };
    let release!: () => void;
    let started!: () => void;
    let gate = new Promise<void>((resolve) => { release = resolve; });
    let pending = new Promise<void>((resolve) => { started = resolve; });
    const queryMetrics = vi.fn(async () => {
      if (queryMetrics.mock.calls.length > 1) { started(); await gate; }
      return response;
    });
    let now = 1_757_000_000n;
    renderWithAdmin({ ...defaultImpl, getTraffic: async () => ({ ...await getTraffic(), now }), listNodes, queryMetrics }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
    await screen.findByText(/级别 1m，每点 60 秒/);

    // hub now 跨分钟换键但 range 没变：挂起期间不该报“非当前窗口”。
    now += 60n;
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000 + 100); });
    await pending;
    expect(screen.queryByText(/图表还不是/)).toBeNull();
    release();
    await waitFor(() => expect(queryMetrics.mock.calls.length).toBeGreaterThanOrEqual(2));
    expect(screen.queryByText(/图表还不是/)).toBeNull();

    // 换成另一个 range：挂起期间沿用的是 24h 的数据，该出现提示。
    gate = new Promise<void>((resolve) => { release = resolve; });
    pending = new Promise<void>((resolve) => { started = resolve; });
    fireEvent.click(screen.getByRole("button", { name: "7d" }));
    await pending;
    expect(screen.getByText(/图表还不是 7d 窗口的结果/).parentElement).toBe(rangeHeader());
    expect(screen.getByText(/级别 1m，每点 60 秒/).closest("header")).toBe(rangeHeader());
    release();
    await waitFor(() => expect(screen.queryByText(/图表还不是/)).toBeNull());
  });

  it("非数字节点路径不发查询并显示返回链接", async () => {
    const list = vi.fn(listNodes);
    const queryMetrics = vi.fn(async () => ({ level: "1m", stepS: 60, ts: [], series: [], coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } }));
    await act(async () => { renderWithAdmin({ ...defaultImpl, getTraffic, listNodes: list, queryMetrics }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/abc"); });
    expect(screen.getByRole("alert")).toHaveTextContent("节点 abc 不存在");
    expect(screen.getByRole("link", { name: "返回总览" })).toHaveAttribute("href", "/");
    expect(list).not.toHaveBeenCalled();
    expect(queryMetrics).not.toHaveBeenCalled();
  });
  it("按面板画图，单位随数据，显示 hub 选定的级别", async () => {
    const queryMetrics = vi.fn<NonNullable<AdminImpl["queryMetrics"]>>(async () => ({
      level: "5m", stepS: 300, ts: [],
      series: [{ name: "cpu", unit: "percent", samples: [] }, { name: "mem_used", unit: "bytes", samples: [] }],
      coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n },
    }));
    renderWithAdmin({ ...defaultImpl, getTraffic, listNodes, queryMetrics }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
    expect(await screen.findByRole("heading", { level: 1, name: "db-01" })).toBeInTheDocument();
    expect(await screen.findByText(/级别 5m，每点 300 秒/)).toBeInTheDocument();
    const charts = screen.getAllByTestId("chart");
    expect(charts).toHaveLength(12);
    expect(charts[7]).toHaveAttribute("data-labels", "下行均值,上行均值,下行峰值,上行峰值");
    expect(charts[7]).toHaveAttribute("data-unit", "bytes/s");
    expect(charts[0]).toHaveAttribute("data-unit", "percent");
    expect(charts[1]).toHaveAttribute("data-labels", "内存均值,内存峰值,交换均值");
    expect(charts[1]).toHaveAttribute("data-unit", "bytes");
    expect(charts[3]).toHaveAttribute("data-labels", "负载均值");
    expect(charts[4]).toHaveAttribute("data-labels", "按核负载均值");
    expect(charts[8]).toHaveAttribute("data-labels", "读均值,写均值");
    expect(charts[8]).toHaveAttribute("data-unit", "bytes/s");
    expect(charts[9]).toHaveAttribute("data-labels", "steal 均值,iowait 均值");
    expect(charts[9]).toHaveAttribute("data-unit", "percent");
    expect(screen.getByText(/db-01\.internal/)).toBeInTheDocument();
    const req = queryMetrics.mock.calls[0][0] as { nodeId: bigint; from: bigint; to: bigint; maxPoints: number };
    expect(req.nodeId).toBe(7n);
    expect(Number(req.to - req.from)).toBe(86400);
    expect(req.maxPoints).toBe(1000);
  });

  it.each([
    { label: "1h", seconds: 3600 },
    { label: "6h", seconds: 21600 },
    { label: "7d", seconds: 604800 },
    { label: "30d", seconds: 2592000 },
  ])("切换到 $label 重新查询", async ({ label, seconds }) => {
    const queryMetrics = vi.fn<NonNullable<AdminImpl["queryMetrics"]>>(async () => ({ level: "1m", stepS: 60, ts: [], series: [], coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } }));
    renderWithAdmin({ ...defaultImpl, getTraffic, listNodes, queryMetrics }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
    await screen.findByRole("heading", { level: 1, name: "db-01" });
    await screen.findByText(/级别 1m，每点 60 秒/);
    fireEvent.click(screen.getByRole("button", { name: label }));
    await waitFor(() => expect(queryMetrics.mock.calls.length).toBeGreaterThanOrEqual(2));
    const last = queryMetrics.mock.calls.at(-1)![0] as { from: bigint; to: bigint };
    expect(Number(last.to - last.from)).toBe(seconds);
  });

  it("不存在的节点给出返回链接", async () => {
    renderWithAdmin({ ...defaultImpl, getTraffic, listNodes, queryMetrics: async () => ({ level: "1m", stepS: 60, ts: [], series: [], coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } }) }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/99");
    expect(await screen.findByRole("alert")).toHaveTextContent("节点 99 不存在");
    expect(screen.getByRole("link", { name: "返回总览" })).toHaveAttribute("href", "/");
  });

  it.each(["nodes", "history"])("%s 请求失败时显示 hub 的错误正文", async (source) => {
    const fail = async () => { throw new ConnectError("data unavailable", Code.Unavailable); };
    renderWithAdmin({ ...defaultImpl,
      getTraffic,
      listNodes: source === "nodes" ? fail : listNodes,
      queryMetrics: source === "history" ? fail : async () => ({ level: "1m", stepS: 60, ts: [], series: [], coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } }),
    }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
    expect(await screen.findByRole("alert")).toHaveTextContent(/^data unavailable$/);
  });
});

// hub 不标注的序列（受限 token 看不到的任务；已删除的任务不出现在序列里）用编号称呼，也不给对比入口。
it("探测图每个任务一张图，未标注的任务用编号，且与指标查询共用同一窗口", async () => {
  const windows: { name: string; from: bigint; to: bigint }[] = [];
  renderWithAdmin({ ...defaultImpl,
    listNodes,
    queryMetrics: async (req) => { windows.push({ name: "metrics", from: req.from, to: req.to }); return defaultImpl.queryMetrics(); },
    queryProbes: async (req) => {
      windows.push({ name: "probes", from: req.from, to: req.to });
      return create(QueryProbesResponseSchema, { level: "1m", stepS: 60, series: [
        { taskId: 3n, kind: ProbeKind.ICMP, target: "1.1.1.1", samples: [{ ts: req.from, sent: 10, lost: 1, errors: 0, rttMeanUs: 9000 }] },
        { taskId: 9n, samples: [{ ts: req.from, sent: 10, lost: 0, errors: 0, rttMeanUs: 1000 }] },
      ] });
    },
  }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  const active = await screen.findByRole("heading", { name: "ICMP 1.1.1.1" });
  expect(within(active).getByRole("link")).toHaveAttribute("href", "/probes/3/compare");
  const unlabeled = screen.getByRole("heading", { name: "任务 #9" });
  expect(within(unlabeled).queryByRole("link")).toBeNull();
  expect(screen.getAllByTestId("chart")).toHaveLength(14);
  await waitFor(() => expect(windows.filter((w) => w.name === "probes")).toHaveLength(1));
  const m = windows.find((w) => w.name === "metrics")!;
  const p = windows.find((w) => w.name === "probes")!;
  expect([p.from, p.to]).toEqual([m.from, m.to]);
  expect(screen.queryByText(/窗口内没有探测结果/)).toBeNull();
});

it("窗口内没有探测结果时给出去向", async () => {
  renderWithAdmin({ ...defaultImpl,
    listNodes,
    queryMetrics: defaultImpl.queryMetrics,
    queryProbes: async () => create(QueryProbesResponseSchema, { level: "1m", stepS: 60, series: [] }),
  }, [{ path: "/nodes/:id", Component: NodeDetail }, { path: "/probes", element: <p>任务页</p> }], "/nodes/7");
  expect(await screen.findByText(/窗口内没有探测结果/)).toBeInTheDocument();
  expect(screen.queryByRole("heading", { name: "探测 · 丢包率" })).toBeNull();
  fireEvent.click(screen.getByRole("link", { name: "管理探测任务" }));
  expect(await screen.findByText("任务页")).toBeInTheDocument();
});

it("主机信息显示 ICMP 是否可用", async () => {
  renderWithAdmin({ ...defaultImpl, listNodes, queryMetrics: defaultImpl.queryMetrics,
    queryProbes: async () => create(QueryProbesResponseSchema, { stepS: 60 }) },
    [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=diagnostics");
  const dt = await screen.findByText("ICMP 探测");
  expect(dt.nextElementSibling).toHaveTextContent("不可用");
});

it("主机信息也显示 ICMP 可用", async () => {
  renderWithAdmin({ ...defaultImpl, listNodes: async () => {
    const response = await listNodes();
    response.nodes[0].facts.icmpAvailable = true;
    return response;
  } }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=diagnostics");
  const dt = await screen.findByText("ICMP 探测");
  expect(dt.nextElementSibling).toHaveTextContent(/^可用$/);
});

it("探测查询失败显示错误，不吞掉失败", async () => {
  renderWithAdmin({ ...defaultImpl, queryProbes: async () => { throw new ConnectError("probes unavailable", Code.Unavailable); } },
    [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  expect(await screen.findByRole("alert")).toHaveTextContent(/^probes unavailable$/);
});

it("切窗请求挂起时保留探测图与图例", async () => {
  let releaseProbes!: () => void;
  let started!: () => void;
  const probeGate = new Promise<void>((resolve) => { releaseProbes = resolve; });
  const pending = new Promise<void>((resolve) => { started = resolve; });
  const queryProbes = vi.fn<NonNullable<AdminImpl["queryProbes"]>>(async (req) => {
    if (queryProbes.mock.calls.length > 1) { started(); await probeGate; }
    return create(QueryProbesResponseSchema, { stepS: 60, series: [
      { taskId: 3n, kind: ProbeKind.ICMP, target: "1.1.1.1", samples: [{ ts: req.from, sent: 1, rttMeanUs: 1000 }] },
    ] });
  });
  renderWithAdmin({ ...defaultImpl, queryProbes }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  try {
    expect(await screen.findByRole("heading", { name: "ICMP 1.1.1.1" })).toBeInTheDocument();
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "7d" })); await pending; });
    expect(screen.getByRole("heading", { name: "ICMP 1.1.1.1" })).toBeInTheDocument();
    expect(screen.getAllByTestId("chart")).toHaveLength(13);
    expect(screen.getByText("RTT 均值")).toBeInTheDocument();
  } finally {
    await act(async () => { releaseProbes(); });
  }
});

it("主机名一格带上 hub 看到的来源地址；从未上报时不显示", async () => {
  renderWithAdmin({ ...defaultImpl, listNodes: async () => {
    const response = await listNodes();
    return { nodes: [{ ...response.nodes[0], lastSource: "2001:db8::7" }] };
  } }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=diagnostics");
  const dt = await screen.findByText("主机名");
  expect(dt.nextElementSibling).toHaveTextContent(/^db-01\.internal（来源 2001:db8::7）$/);
});

it("来源地址为空时主机名一格只有主机名", async () => {
  renderWithAdmin({ ...defaultImpl, listNodes }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=diagnostics");
  const dt = await screen.findByText("主机名");
  expect(dt.nextElementSibling).toHaveTextContent(/^db-01\.internal$/);
});

it("旧 Agent 的节点详情明确显示未提供诊断", async () => {
  renderWithAdmin(defaultImpl, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=diagnostics");
  expect(await screen.findByText("Agent 未提供诊断信息，请更新 Agent 后等待上报。")).toBeInTheDocument();
  expect(screen.queryByText("最近采集未报告失败")).not.toBeInTheDocument();
});

it("诊断每 10 秒更新，刷新失败保留最近诊断并显示错误", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  let fail = false;
  let degraded = false;
  const updatedAt = 1_790_000_000n;
  const nodes = vi.fn<NonNullable<AdminImpl["listNodes"]>>(async () => {
    if (fail) throw new ConnectError("diagnostics refresh failed", Code.Unavailable);
    const response = await listNodes();
    return { nodes: [{ ...response.nodes[0], factsUpdatedAt: updatedAt, facts: { ...response.nodes[0].facts,
      diagnostics: { netInclude: ["eth*"], netInterfaces: degraded ? [] : ["eth0"], netInterfacesTotal: degraded ? 0 : 1,
        reportIntervalMs: degraded ? 2000 : 1000, failedCollectors: degraded ? [CollectionComponent.NET] : [] },
    } }] };
  });
  renderWithAdmin({ ...defaultImpl, listNodes: nodes }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=diagnostics");
  await screen.findByRole("heading", { name: "db-01" });
  await screen.findByRole("region", { name: "Agent 运行诊断" });
  expect(screen.getByText("生效上报间隔").nextElementSibling).toHaveTextContent("1000 ms");
  expect(screen.getByText("诊断信息更新时间").nextElementSibling?.querySelector("time")).toHaveAttribute("dateTime", new Date(Number(updatedAt) * 1000).toISOString());
  degraded = true;
  await act(async () => { await vi.advanceTimersByTimeAsync(10_000 + 100); });
  expect(screen.getByText("生效上报间隔").nextElementSibling).toHaveTextContent("2000 ms");
  expect(screen.getByText("本次网络采集失败，接口清单不可用")).toBeInTheDocument();
  expect(screen.queryByText("eth0")).not.toBeInTheDocument();
  fail = true;
  await act(async () => { await vi.advanceTimersByTimeAsync(10_000 + 100); });
  expect(await screen.findByRole("alert")).toHaveTextContent("diagnostics refresh failed");
  expect(screen.getByText("生效上报间隔").nextElementSibling).toHaveTextContent("2000 ms");
  expect(screen.getByText("本次网络采集失败，接口清单不可用")).toBeInTheDocument();
});

it("返回节点列表在数据到达前就可用，点击回到节点列表", async () => {
  const { router } = renderWithAdmin({ ...defaultImpl, listNodes: () => new Promise<never>(() => {}) },
    [{ path: "/nodes/:id", Component: NodeDetail }, { path: "/nodes", Component: () => <h1>节点列表页</h1> }], "/nodes/7?tab=diagnostics");
  const back = await screen.findByRole("link", { name: "返回节点列表" });
  expect(screen.queryByRole("heading", { name: "db-01" })).not.toBeInTheDocument();
  fireEvent.click(back);
  expect(await screen.findByRole("heading", { name: "节点列表页" })).toBeInTheDocument();
  expect(router.state.location.pathname).toBe("/nodes");
  expect(router.state.location.search).toBe("");
});

it("从列表带着查询串进入时，返回链接还原同一组筛选，切 tab 后仍保留；state 不合法时回到不带筛选的列表", async () => {
  const { router } = renderWithAdmin(defaultImpl, [{ path: "/nodes/:id", Component: NodeDetail }], "/");
  await act(async () => { await router.navigate("/nodes/7", { state: { nodeListSearch: "?q=db&tag=prod" } }); });
  const back = await screen.findByRole("link", { name: "返回节点列表" });
  expect(back).toHaveAttribute("href", "/nodes?q=db&tag=prod");
  fireEvent.click(screen.getByRole("tab", { name: "流量校正" }));
  await waitFor(() => expect(router.state.location.search).toBe("?tab=traffic"));
  expect(screen.getByRole("link", { name: "返回节点列表" })).toHaveAttribute("href", "/nodes?q=db&tag=prod");
  for (const state of [{ nodeListSearch: "javascript:alert(1)" }, { nodeListSearch: 3 }, "?q=x", null]) {
    await act(async () => { await router.navigate("/nodes/7", { state }); });
    expect(screen.getByRole("link", { name: "返回节点列表" })).toHaveAttribute("href", "/nodes");
  }
});

