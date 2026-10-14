import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { Component, type ReactNode } from "react";
import { screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import type { AlignedData } from "uplot";
import { PublicService } from "../gen/heron/v1/public_pb";
import { CoverageSummarySchema, type QueryMetricsRequest, QueryMetricsResponseSchema, QueryProbesResponseSchema } from "../gen/heron/v1/query_pb";
import { ProbeKind } from "../gen/heron/v1/types_pb";
import { NodeDetail } from "../pages/NodeDetail";
import { NodePage } from "../public/NodePage";
import { renderWithAdmin, renderWithService } from "../test/harness";

vi.mock("./Chart", () => ({
  Chart: ({ labels, unit, data, soft, bands }: { labels: string[]; unit: string; data: AlignedData; soft?: boolean[]; bands?: unknown[] }) => (
    <div data-testid="chart" data-labels={labels.join(",")} data-unit={unit}
      data-soft={JSON.stringify(soft ?? [])} data-bands={JSON.stringify(bands ?? [])}
      data-points={JSON.stringify(data.slice(1).map((column) => column.slice(0, 5)))} />
  ),
}));

afterEach(() => vi.restoreAllMocks());

// 替身 Chart 把入参序列化进 data-*；读回来只做 toEqual 比较，不需要具体类型。属性缺失时得到 null，比较随之失败。
const parseJson = (json: string | undefined): unknown => JSON.parse(json ?? "null");

const queryMetrics = async (req: QueryMetricsRequest) => {
  const start = Number(req.from) - Number(req.from) % 60;
  return create(QueryMetricsResponseSchema, {
    level: "1m", stepS: 60, ts: [0, 60, 120, 180].map((offset) => BigInt(start + offset)),
    series: [
      { name: "cpu", unit: "percent", samples: [{ n: 3, mean: 25, max: 92 }, { n: 1, mean: 0, max: 0 }, { n: 1, mean: 50 }, { n: 0, mean: 12, max: 99 }] },
      { name: "mem_used", unit: "bytes", samples: [{ n: 3, mean: 100, max: 300 }, { n: 1, mean: 0, max: 0 }, { n: 1, mean: 8 }, { n: 0, mean: 12, max: 99 }] },
      { name: "swap_used", unit: "bytes", samples: [{ n: 3, mean: 10, max: 30 }, { n: 1, mean: 0, max: 0 }, { n: 1, mean: 2 }, { n: 0, mean: 12, max: 99 }] },
      { name: "rx_bytes", unit: "bytes", samples: [{ n: 3, sum: 120 }, { n: 1, sum: 0 }, { n: 1, sum: 60 }, { n: 0, sum: 999 }] },
      { name: "tx_bytes", unit: "bytes", samples: [{ n: 3, sum: 180 }, { n: 1, sum: 0 }, { n: 1, sum: 60 }, { n: 0, sum: 999 }] },
      { name: "net_rx_bps", unit: "bytes/s", samples: [{ n: 3, mean: 4, max: 11 }, { n: 1, mean: 0, max: 0 }, { n: 1, mean: 6 }, { n: 0, mean: 7, max: 99 }] },
      { name: "net_tx_bps", unit: "bytes/s", samples: [{ n: 3, mean: 9, max: 17 }, { n: 1, mean: 0, max: 0 }, { n: 1, mean: 6 }, { n: 0, mean: 7, max: 99 }] },
      { name: "disk_read_bps", unit: "bytes/s", samples: [{ n: 3, mean: 512, max: 2048 }, { n: 1, mean: 0, max: 0 }, { n: 1, mean: 1536 }, { n: 0, mean: 99, max: 999 }] },
      { name: "disk_write_bps", unit: "bytes/s", samples: [{ n: 3, mean: 256, max: 1024 }, { n: 1, mean: 0, max: 0 }, { n: 1, mean: 768 }, { n: 0, mean: 99, max: 999 }] },
      { name: "cpu_steal_pct", unit: "percent", samples: [{ n: 3, mean: 1.5, max: 4 }, { n: 1, mean: 0, max: 0 }, { n: 1, mean: 2.5 }, { n: 0, mean: 9, max: 9 }] },
      { name: "cpu_iowait_pct", unit: "percent", samples: [{ n: 3, mean: 3.5, max: 8 }, { n: 1, mean: 0, max: 0 }, { n: 1, mean: 4.5 }, { n: 0, mean: 9, max: 9 }] },
      { name: "tcp_active_opens_ps", unit: "count/s", samples: [{ n: 3, mean: 2, max: 6 }, { n: 1, mean: 0, max: 0 }, { n: 1, mean: 1 }, { n: 0, mean: 9, max: 9 }] },
      { name: "tcp_passive_opens_ps", unit: "count/s", samples: [{ n: 3, mean: 4, max: 10 }, { n: 1, mean: 0, max: 0 }, { n: 1, mean: 3 }, { n: 0, mean: 9, max: 9 }] },
      { name: "tcp_out_segs_ps", unit: "count/s", samples: [{ n: 3, mean: 200 }, { n: 1, mean: 0 }, { n: 1, mean: 50 }, { n: 0, mean: 9 }] },
      { name: "tcp_retrans_segs_ps", unit: "count/s", samples: [{ n: 3, mean: 3 }, { n: 1, mean: 0 }, { n: 1, mean: 1 }, { n: 0, mean: 9 }] },
      { name: "load1", unit: "", samples: [{ n: 3, mean: 6.4 }, { n: 1, mean: 0 }, { n: 1, mean: 8 }, { n: 0, mean: 99 }] },
      { name: "load1_per_core", unit: "", samples: [{ n: 3, mean: 0.4 }, { n: 1, mean: 0 }, { n: 0, mean: 99 }, { n: 0, mean: 99 }] },
    ],
    // QueryMetrics 成功响应总是带 coverageSummary；图表用例不关心覆盖率，给一份无起点的汇总。
    coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n },
  });
};

it.each([
  { surface: "管理", networkPeaks: true },
  { surface: "公开", networkPeaks: true },
  { surface: "管理", networkPeaks: false },
  { surface: "公开", networkPeaks: false },
])("$surface 节点均值与峰值不混用（含网络峰值系列：$networkPeaks）", async ({ surface, networkPeaks }) => {
  vi.spyOn(Date, "now").mockReturnValue(86_400_000);
  const queries = {
    queryMetrics: async (req: QueryMetricsRequest) => {
      const response = await queryMetrics(req);
      if (!networkPeaks) response.series = response.series.filter((series) => !series.name.startsWith("net_"));
      return response;
    },
    queryProbes: async () => ({ level: "1m", stepS: 60, series: [] }),
  };
  if (surface === "管理") {
    renderWithAdmin({ ...queries, getSnapshot: async () => ({ nodes: [] }), listTags: async () => ({ tags: [] }), listNodes: async () => ({ nodes: [{ id: 7n, name: "node" }] }), getTraffic: async () => ({ nodes: [] }) },
      [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  } else {
    renderWithService(PublicService, { ...queries, getSnapshot: async () => ({ nodes: [{ id: 7n, name: "node" }] }) },
      [{ path: "/nodes/:id", Component: NodePage }], "/nodes/7");
  }
  await screen.findByText(/级别 1m，每点 60 秒/);
  const expectedPanels = [
    { title: "CPU", labels: "CPU 均值,CPU 峰值", unit: "percent", soft: [false, true], points: [[25, 0, 50, null, null], [92, 0, null, null, null]] },
    { title: "内存 / 交换", labels: "内存均值,内存峰值,交换均值", unit: "bytes", soft: [false, true, false], points: [[100, 0, 8, null, null], [300, 0, null, null, null], [10, 0, 2, null, null]] },
    { title: "网络", labels: "下行均值,上行均值,下行峰值,上行峰值", unit: "bytes/s", soft: [false, false, true, true], points: [
      [2, 0, 1, null, null], [3, 0, 1, null, null],
      networkPeaks ? [11, 0, null, null, null] : [null, null, null, null, null],
      networkPeaks ? [17, 0, null, null, null] : [null, null, null, null, null],
    ] },
    { title: "磁盘速率", labels: "读均值,写均值", unit: "bytes/s", soft: [false, false], points: [
      [512, 0, 1536, null, null], [256, 0, 768, null, null],
    ] },
    { title: "CPU steal / iowait", labels: "steal 均值,iowait 均值", unit: "percent", soft: [false, false], points: [
      [1.5, 0, 2.5, null, null], [3.5, 0, 4.5, null, null],
    ] },
    { title: "负载（1 分钟）", labels: "负载均值", unit: "", soft: [false], points: [[6.4, 0, 8, null, null]] },
    { title: "按核负载（1 分钟）", labels: "按核负载均值", unit: "", soft: [false], points: [[0.4, 0, null, null, null]] },
    { title: "TCP 建连", labels: "主动均值,被动均值,主动峰值,被动峰值", unit: "count/s", soft: [false, false, true, true], points: [
      [2, 0, 1, null, null], [4, 0, 3, null, null], [6, 0, null, null, null], [10, 0, null, null, null],
    ] },
    { title: "TCP 重传率", labels: "重传率", unit: "percent", soft: [false], points: [[1.5, null, 2, null, null]] },
  ];
  const panels = expectedPanels.map((expected) => {
    const panel = screen.getByRole("heading", { name: expected.title }).parentElement!;
    const chart = within(panel).getByTestId("chart");
    return { title: expected.title, labels: chart.dataset.labels, unit: chart.dataset.unit, soft: parseJson(chart.dataset.soft), points: parseJson(chart.dataset.points) };
  });
  expect(panels).toEqual(expectedPanels);
  const levelTip = screen.getAllByRole("tooltip").find((el) => el.textContent?.includes("级别 1m"));
  expect(levelTip).toHaveTextContent("级别 1m，每点 60 秒。峰值为每个图表时间桶内已采集样本的最大值");
});

it("公开节点页每个探测任务一张 RTT 图：均值实线，最小 / 最大浅线并在两者之间填充；标题链到对比页", async () => {
  vi.spyOn(Date, "now").mockReturnValue(86_400_000);
  renderWithService(PublicService, {
    getSnapshot: async () => ({ nodes: [{ id: 7n, name: "node", online: true, lastSeenAt: 1n }] }),
    queryMetrics,
    queryProbes: async () => create(QueryProbesResponseSchema, { level: "1m", stepS: 60, series: [
      { taskId: 3n, kind: ProbeKind.TCP, target: "example.com:443", samples: [{ ts: 86_340n, sent: 4, lost: 0, errors: 0, rttMeanUs: 20_000, rttMinUs: 10_000, rttMaxUs: 30_000 }] },
      { taskId: 4n, kind: ProbeKind.UNSPECIFIED, target: "", samples: [] },
    ] }),
  }, [{ path: "/nodes/:id", Component: NodePage }], "/nodes/7");
  const heading = await screen.findByRole("heading", { name: "TCP example.com:443" });
  expect(within(heading).getByRole("link")).toHaveAttribute("href", "/probes/3");
  const chart = within(heading.parentElement!).getByTestId("chart");
  expect(chart.dataset.labels).toBe("RTT 均值,最小,最大");
  expect(chart.dataset.soft).toBe("[false,true,true]");
  expect(chart.dataset.bands).toBe('[{"lower":1,"upper":2}]');
  expect(chart.dataset.unit).toBe("ms");
  // 未标注（已撤下）的任务只有编号，没有对比页链接。
  const gone = screen.getByRole("heading", { name: "任务 #4" });
  expect(within(gone).queryByRole("link")).toBeNull();
  expect(screen.queryByRole("heading", { name: "探测 · 丢包率" })).toBeNull();
});

// 覆盖率只随管理端显示：公开页复用同一组件但不传 showCoverage（默认不显示），即便响应里带着
// coverageSummary 也不渲染。QueryMetrics 成功响应总是带 coverageSummary（history.metrics）。
const COVERAGE_TEXT = /上报覆盖|尚无覆盖记录|无可观测区间/;

class CoverageErrorProbe extends Component<{ children: ReactNode }, { message: string }> {
  state = { message: "" };
  static getDerivedStateFromError(error: Error) { return { message: error.message }; }
  render() { return this.state.message ? <p role="alert">{this.state.message}</p> : this.props.children; }
}

function GuardedNodeDetail() {
  return <CoverageErrorProbe><NodeDetail /></CoverageErrorProbe>;
}

function renderHistory(surface: "管理" | "公开", summary?: MessageInitShape<typeof CoverageSummarySchema>, omitCoverage = false) {
  const queries = {
    queryMetrics: async (req: QueryMetricsRequest) => {
      const response = await queryMetrics(req);
      if (omitCoverage) response.coverageSummary = undefined;
      else if (summary) response.coverageSummary = create(CoverageSummarySchema, summary);
      return response;
    },
    queryProbes: async () => ({ level: "1m", stepS: 60, series: [] }),
  };
  if (surface === "管理") {
    renderWithAdmin({ ...queries, getSnapshot: async () => ({ nodes: [] }), listTags: async () => ({ tags: [] }), listNodes: async () => ({ nodes: [{ id: 7n, name: "node" }] }), getTraffic: async () => ({ nodes: [] }) },
      [{ path: "/nodes/:id", Component: omitCoverage ? GuardedNodeDetail : NodeDetail }], "/nodes/7");
  } else {
    renderWithService(PublicService, { ...queries, getSnapshot: async () => ({ nodes: [{ id: 7n, name: "node" }] }) },
      [{ path: "/nodes/:id", Component: NodePage }], "/nodes/7");
  }
}

it.each([
  {
    name: "正常：百分比向下截断到一位小数，未知时长按量级选单位",
    summary: { coverageStart: 1_700_000_000n, eligibleMinutes: 200n, observedMinutes: 150n, observedReportedMinutes: 100n },
    shown: "上报覆盖 66.6%，未知 50 分钟",
  },
  {
    name: "无未知分钟时不显示未知时长",
    summary: { coverageStart: 1_700_000_000n, eligibleMinutes: 150n, observedMinutes: 150n, observedReportedMinutes: 150n },
    shown: "上报覆盖 100.0%",
  },
  {
    name: "coverageStart 缺席：只说尚无覆盖记录，不断言从未上报",
    summary: {},
    shown: "尚无覆盖记录",
  },
  {
    name: "observedMinutes 为 0：无可观测区间，不出现 NaN/Infinity",
    summary: { coverageStart: 1_700_000_000n, eligibleMinutes: 60n },
    shown: "无可观测区间",
  },
])("管理端在工具栏上直接显示上报覆盖率，口径收进说明（$name）", async ({ summary, shown }) => {
  renderHistory("管理", summary);
  const tip = await screen.findByRole("button", { name: "上报覆盖率说明" });
  const note = tip.closest(".coverage-note")!;
  expect(note.closest(".detail-header")).not.toBeNull();
  expect(note.textContent?.replace(tip.parentElement!.textContent, "")).toBe(shown);
  expect(screen.getByText(/这是 hub 观测到的分钟里节点有上报的比例，不是在线率/)).toHaveAttribute("role", "tooltip");
  expect(screen.queryByText(/NaN|Infinity/)).not.toBeInTheDocument();
});

it("公开页不传 showCoverage（默认不显示）：响应带 coverageSummary 也不渲染", async () => {
  renderHistory("公开", { coverageStart: 1_700_000_000n, eligibleMinutes: 200n, observedMinutes: 150n, observedReportedMinutes: 100n });
  await screen.findByText(/级别 1m，每点 60 秒/);
  expect(screen.queryByText(COVERAGE_TEXT)).not.toBeInTheDocument();
});

it("管理端 QueryMetrics 缺 coverageSummary 时不把覆盖率藏起来", async () => {
  renderHistory("管理", undefined, true);
  expect(await screen.findByRole("alert")).toHaveTextContent("coverage_summary");
});
