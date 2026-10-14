import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { useTimeWindow } from "./History";
import { NodeDetail } from "../pages/NodeDetail";
import { NodePage } from "../public/NodePage";
import { ProbeCompare } from "../pages/ProbeCompare";
import { PublicProbeCompare } from "../public/ProbeCompare";
import { AdminService } from "../gen/heron/v1/admin_pb";
import { PublicService } from "../gen/heron/v1/public_pb";
import { renderWithService } from "../test/harness";

vi.mock("./Chart", () => ({ Chart: () => null }));
afterEach(() => vi.useRealTimers());

const NOW = 1_800_000_020;
const END = 1_800_000_060;

it.each([-8, 8])("窗口只随 hub 分钟变化，不随浏览器偏差 %i 小时变化", (hours) => {
  vi.useFakeTimers();
  vi.setSystemTime((NOW + hours * 3600) * 1000);
  const { result, rerender } = renderHook(({ now }) => useTimeWindow(now), { initialProps: { now: NOW } });
  expect({ from: result.current.from, to: result.current.to }).toEqual({ from: END - 86400, to: END });
  rerender({ now: NOW + 39 });
  expect(result.current.to).toBe(END);
  rerender({ now: NOW + 40 });
  expect(result.current.to).toBe(END + 60);
  act(() => { vi.advanceTimersByTime(3600_000); });
  expect(result.current.to).toBe(END + 60);
});

it.each([
  { name: "管理节点", service: AdminService, Component: NodeDetail, comparison: false },
  { name: "公开节点", service: PublicService, Component: NodePage, comparison: false },
  { name: "管理对比", service: AdminService, Component: ProbeCompare, comparison: true },
  { name: "公开对比", service: PublicService, Component: PublicProbeCompare, comparison: true },
])("$name 在 hub now 到达前不发历史请求，到达后使用它", async ({ service, Component, comparison }) => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime((NOW + 8 * 3600) * 1000);
  const windows: { from: bigint; to: bigint }[] = [];
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  let now = NOW;
  const time = async () => { await pending; return { now: BigInt(now), nodes: [{ id: 7n, name: "node" }] }; };
  const history = async (req: { from: bigint; to: bigint }) => {
    windows.push({ from: req.from, to: req.to });
    return { level: "1m", stepS: 60, coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } };
  };
  const impl = {
    getSnapshot: time, getTraffic: time,
    listNodes: async () => ({ nodes: [{ id: 7n, name: "node" }] }),
    listProbeComparisonNodes: async () => ({ nodeIds: [7n], maxNodesPerQuery: 1 }),
    queryMetrics: history, queryProbes: history, queryProbeComparison: history,
  };
  renderWithService(service, impl, [{ path: "/view/:id", Component }], "/view/7");
  await act(async () => { await vi.advanceTimersByTimeAsync(10); });
  expect(windows).toEqual([]);
  await act(async () => release());
  await waitFor(() => expect(windows).toHaveLength(comparison ? 1 : 2));
  expect(windows).toEqual(Array.from({ length: comparison ? 1 : 2 }, () => ({ from: BigInt(END - 86400), to: BigInt(END) })));
  now += 60;
  await act(async () => { await vi.advanceTimersByTimeAsync(10_100); });
  expect(windows.slice(comparison ? 1 : 2)).toEqual(Array.from({ length: comparison ? 1 : 2 }, () => ({ from: BigInt(END + 60 - 86400), to: BigInt(END + 60) })));
});
