import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { act, cleanup, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { PublicService } from "../gen/heron/v1/public_pb";
import { QueryProbesResponseSchema } from "../gen/heron/v1/query_pb";
import { ProbeKind } from "../gen/heron/v1/types_pb";
import { rangeHeader, renderWithService } from "../test/harness";
import { NodePage } from "./NodePage";
import { AddressDetectionState } from "../gen/heron/v1/types_pb";

vi.mock("../components/Chart", () => ({
  Chart: ({ labels }: { labels: string[] }) => <div data-testid="chart">{labels.map((l) => <span key={l}>{l}</span>)}</div>,
}));

afterEach(() => vi.useRealTimers());

const snapshot = async () => ({ now: 1_000n, nodes: [{ id: 7n, name: "edge-1", online: true, facts: { os: "Alpine 3.21", arch: "arm64", cpuModel: "Neoverse", cpuCores: 2 } }] });

it("公开节点的历史图表走 PublicService；首屏是实时状态头与六个现值格；每任务一张 RTT 图", async () => {
  const queryMetrics = vi.fn(async () => ({ level: "1m", stepS: 60, ts: [], series: [] }));
  const queryProbes = vi.fn(async () => create(QueryProbesResponseSchema, { level: "1m", stepS: 60, series: [{ taskId: 3n, kind: ProbeKind.TCP, target: "example.com:443" }] }));
  const live = async () => ({ now: 1_000n, nodes: [{ id: 7n, name: "edge-1", online: true, lastSeenAt: 998n, country: "DE", tags: ["edge", "eu"], publicRemark: "法兰克福",
    facts: { os: "Alpine 3.21", arch: "arm64", cpuModel: "Neoverse", cpuCores: 2, virtualization: "kvm" },
    metrics: { cpuPct: 42, memUsed: 512n * 1024n ** 2n, memTotal: 1024n ** 3n, diskUsed: 1024n ** 3n, diskTotal: 10n * 1024n ** 3n, netRxBps: 2048n, netTxBps: 1024n, uptimeS: 90_000n },
    billing: { expiresOn: "2030-01-01", daysLeft: 10 } }] });
  renderWithService(PublicService, { getSnapshot: live, queryMetrics, queryProbes }, [{ path: "/nodes/:id", Component: NodePage }], "/nodes/7");
  const head = (await screen.findByRole("heading", { level: 1, name: "edge-1" })).closest("header")!;
  expect(within(head).getByText("在线 · 最近上报 刚刚")).toHaveAttribute("data-status", "online");
  expect(within(head).getByText("法兰克福")).toBeInTheDocument();
  expect(within(head).getAllByText(/^(edge|eu)$/).map((el) => el.textContent)).toEqual(["edge", "eu"]);
  expect(within(screen.getByRole("group", { name: "CPU" })).getByText("42%")).toBeInTheDocument();
  expect(within(screen.getByRole("group", { name: "内存" })).getByText("50%")).toBeInTheDocument();
  expect(within(screen.getByRole("group", { name: "磁盘" })).getByText("10%")).toBeInTheDocument();
  expect(screen.getByRole("group", { name: "网络" })).toHaveTextContent("↓ 2.0 KiB/s ↑ 1.0 KiB/s");
  expect(screen.getByRole("group", { name: "运行时长" })).toHaveTextContent("1d 1h");
  expect(screen.getByRole("group", { name: "剩余天数" })).toHaveTextContent("10 天");
  expect(within(screen.getByRole("group", { name: "剩余天数" })).getByText("10").closest("[data-level]")).toHaveAttribute("data-level", "attention");
  expect(within(screen.getByRole("group", { name: "剩余天数" })).getByText("10")).toHaveClass("num");
  expect(await screen.findByRole("heading", { name: "TCP example.com:443" })).toBeInTheDocument();
  expect(screen.getAllByTestId("chart")).toHaveLength(13);
  for (const r of ["1h", "6h", "24h", "7d", "30d"]) expect(screen.getByRole("button", { name: r })).toBeInTheDocument();
  expect(within(head).getByText("Alpine 3.21").closest("dl")).toHaveClass("facts-inline");
  expect(within(head).getByText("Neoverse × 2")).toBeInTheDocument();
  expect(screen.queryByLabelText("执行环境")).not.toBeInTheDocument();
  expect((queryMetrics.mock.calls[0] as unknown[])[0]).toMatchObject({ nodeId: 7n, maxPoints: 1000 });
});

it("从未上报的节点：状态头写从未上报，六格都是破折号，没有系统信息卡", async () => {
  renderWithService(PublicService, { getSnapshot: async () => ({ now: 1_000n, nodes: [{ id: 7n, name: "fresh", online: false }] }), queryMetrics: async () => ({ level: "1m", stepS: 60, ts: [], series: [] }), queryProbes: async () => ({ level: "1m", stepS: 60, series: [] }) }, [{ path: "/nodes/:id", Component: NodePage }], "/nodes/7");
  await screen.findByRole("heading", { level: 1, name: "fresh" });
  expect(document.querySelector(".node-head .status-badge")).toHaveAttribute("data-status", "never");
  expect(screen.getByText("从未上报")).toBeInTheDocument();
  expect(screen.getAllByLabelText("无读数")).toHaveLength(6);
  expect(screen.queryByText("系统")).toBeNull();
});

it("快照里没有的节点说明不存在或未公开，也不去查历史", async () => {
  const queryMetrics = vi.fn(async () => ({ level: "1m", stepS: 60, ts: [], series: [] }));
  renderWithService(PublicService, { getSnapshot: snapshot, queryMetrics }, [{ path: "/nodes/:id", Component: NodePage }], "/nodes/9");
  expect(await screen.findByRole("alert")).toHaveTextContent("节点 9 不存在或未公开");
  expect(queryMetrics).not.toHaveBeenCalled();
});

it("系统信息行：主机信息缺失时整行不画，缺的单项不写", async () => {
  const nodes = [
    { id: 7n, name: "edge-1", online: true, facts: { os: "Alpine 3.21", arch: "arm64" } },
    { id: 9n, name: "bare", online: false, billing: { price: "3", currency: "USD", expiresOn: "2030-01-01", daysLeft: 20 } },
    // 从未上报但手填了 IPv4 的节点：facts 只带 network，系统信息行不画，双栈标记照画。
    { id: 11n, name: "pinned", online: false, facts: { network: { ipv4: { state: AddressDetectionState.AVAILABLE } } } },
  ];
  const getSnapshot = async () => ({ now: 1_000n, nodes });
  const queryMetrics = async () => ({ level: "1m", stepS: 60, ts: [], series: [] });
  const queryProbes = async () => ({ level: "1m", stepS: 60, series: [] });
  const show = async (id: number) => {
    renderWithService(PublicService, { getSnapshot, queryMetrics, queryProbes }, [{ path: "/nodes/:id", Component: NodePage }], `/nodes/${id}`);
    await screen.findByRole("heading", { level: 1 });
  };
  await show(7);
  const facts = screen.getByText("Alpine 3.21").closest("dl")!;
  expect(facts).toHaveClass("facts-inline");
  expect(within(facts).queryByText("CPU")).toBeNull();
  cleanup();
  await show(9);
  expect(document.querySelector("dl.facts-inline")).toBeNull();
  cleanup();
  await show(11);
  expect(document.querySelector("dl.facts-inline")).toBeNull();
  expect(screen.getByRole("group", { name: "公网出口" })).toHaveTextContent("IPv4");
});

it("窗口每分钟前进后请求失败，图表与级别仍在并带横幅，不误报“非当前窗口”", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  let fail = false;
  const queryMetrics = vi.fn(async () => {
    if (fail) throw new ConnectError("history down", Code.Unavailable);
    return { level: "1m", stepS: 60, ts: [], series: [] };
  });
  const queryProbes = vi.fn(async () => ({ level: "1m", stepS: 60, series: [] }));
  renderWithService(PublicService, { getSnapshot: async () => ({ ...await snapshot(), now: 1_000n + (fail ? 60n : 0n) }), queryMetrics, queryProbes }, [{ path: "/nodes/:id", Component: NodePage }], "/nodes/7");
  await screen.findByRole("heading", { level: 1, name: "edge-1" });
  expect(await screen.findAllByTestId("chart")).toHaveLength(12);
  await screen.findByText(/级别 1m，每点 60 秒/);
  fail = true;
  // 下次快照轮询带来跨分钟的 hub now，换键后的历史请求失败。
  await act(async () => { await vi.advanceTimersByTimeAsync(60_000 + 100); });
  expect(await screen.findByRole("alert")).toHaveTextContent("history down");
  expect(screen.getAllByTestId("chart")).toHaveLength(12);
  expect(screen.getByText(/级别 1m，每点 60 秒/).closest("header")).toBe(rangeHeader());
  // 沿用的还是 24h 这个 range 自己的数据，只是这次刷新没成功；range 没变，不该报"看错窗口"。
  expect(screen.queryByText(/图表还不是/)).toBeNull();
});

it("切到另一个节点、新节点历史未返回时不显示上一个节点的图表与级别", async () => {
  let releaseNode8!: () => void;
  const node8Gate = new Promise<void>((resolve) => { releaseNode8 = resolve; });
  const twoNodes = async () => ({ now: 1_000n, nodes: [
    { id: 7n, name: "edge-1", online: true },
    { id: 8n, name: "edge-2", online: true },
  ] });
  const queryMetrics = vi.fn(async (req: { nodeId: bigint }) => {
    if (req.nodeId === 8n) await node8Gate;
    return req.nodeId === 8n ? { level: "5m", stepS: 300, ts: [], series: [] } : { level: "1m", stepS: 60, ts: [], series: [] };
  });
  const queryProbes = async () => ({ level: "1m", stepS: 60, series: [] });
  const { router } = renderWithService(PublicService, { getSnapshot: twoNodes, queryMetrics, queryProbes }, [{ path: "/nodes/:id", Component: NodePage }], "/nodes/7");
  await screen.findByRole("heading", { level: 1, name: "edge-1" });
  await screen.findByText(/级别 1m，每点 60 秒/);
  await act(async () => { await router.navigate("/nodes/8"); });
  expect(await screen.findByRole("heading", { level: 1, name: "edge-2" })).toBeInTheDocument();
  // 8 的历史还没回来：不能把 7 的"级别 1m"标签或图表当成 8 的显示，这段时间没有图表比显示错的更安全。
  expect(screen.queryByText(/级别 1m/)).toBeNull();
  expect(screen.queryAllByTestId("chart")).toHaveLength(0);
  await act(async () => { releaseNode8(); });
  expect(await screen.findByText(/级别 5m，每点 300 秒/)).toBeInTheDocument();
  expect(screen.getAllByTestId("chart")).toHaveLength(12);
});

it("节点页标题带国家 / 地区徽章", async () => {
  const withCountry = async () => ({ now: 1_000n, nodes: [{ id: 7n, name: "edge-1", online: true, country: "DE" }] });
  renderWithService(PublicService, { getSnapshot: withCountry }, [{ path: "/nodes/:id", Component: NodePage }], "/nodes/7");
  const h1 = await screen.findByRole("heading", { level: 1 });
  expect(h1).toHaveTextContent(/^edge-1$/);
  expect(within(h1.closest("header")!).getByTitle("国家 / 地区 DE")).toHaveTextContent(/^DE$/);
});
