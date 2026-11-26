import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { fireEvent, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { AlignedData } from "uplot";
import { PublicService } from "../gen/heron/v1/public_pb";
import { ListProbeComparisonNodesResponseSchema, QueryMetricsResponseSchema, QueryProbeComparisonResponseSchema, QueryProbesResponseSchema, type ProbeSampleSchema } from "../gen/heron/v1/query_pb";
import { ProbeKind } from "../gen/heron/v1/types_pb";
import { NodeDetail } from "../pages/NodeDetail";
import { ProbeCompare } from "../pages/ProbeCompare";
import { NodePage } from "../public/NodePage";
import { PublicProbeCompare } from "../public/ProbeCompare";
import { renderWithAdmin, renderWithService } from "../test/harness";

vi.mock("./Chart", () => ({
  Chart: ({ unit, data }: { unit: string; data: AlignedData }) => (
    <div data-testid={`chart-${unit}`} data-plotted={JSON.stringify(data.slice(1).map((column) => (column as readonly (number | null)[]).filter((v) => v !== null)))} />
  ),
}));

// 进程原来的时区在每条用例之后还原，假时区不漏到别的用例。
const startTZ = process.env.TZ;
afterEach(() => {
  vi.restoreAllMocks();
  if (startTZ === undefined) delete process.env.TZ;
  else process.env.TZ = startTZ;
});

// hub 在 UTC，访客浏览器在上海（+08:00）。PEAK 是 hub 的 21:00、访客的 05:00；DAY 是 hub 的 13:00、访客的 21:00。
// 按访客本地时区切晚高峰的实现会把两者对调，汇总读数随之完全不同。
const HUB_NOW = Date.UTC(2023, 10, 14, 22, 13, 20) / 1000;
const DAY = Date.UTC(2023, 10, 14, 13) / 1000;
const PEAK = Date.UTC(2023, 10, 14, 21) / 1000;

function visitorInShanghai() {
  process.env.TZ = "Asia/Shanghai";
  // 假时区真的生效了：DAY 在访客本地是 21 点，否则这条用例区分不了两种时区。
  expect(new Date(DAY * 1000).getHours()).toBe(21);
}

const samples = (stepS: number): MessageInitShape<typeof ProbeSampleSchema>[] => [
  { ts: BigInt(DAY - (DAY % stepS)), sent: 100, lost: 0, errors: 0, rttMeanUs: 10_000, rttMinUs: 5_000, rttMaxUs: 15_000 },
  { ts: BigInt(PEAK - (PEAK % stepS)), sent: 100, lost: 10, errors: 0, rttMeanUs: 50_000, rttMinUs: 40_000, rttMaxUs: 90_000 },
];

const metrics = async () => create(QueryMetricsResponseSchema, { level: "1m", stepS: 60, coverageSummary: {} });

function renderNode(surface: "管理" | "公开", { stepS = 60, timeZone = "UTC", siteFails = false } = {}) {
  const queryProbes = async () => create(QueryProbesResponseSchema, { level: "1m", stepS, series: [
    { taskId: 3n, kind: ProbeKind.TCP, target: "example.com:443", samples: samples(stepS) },
  ] });
  const fail = async (): Promise<never> => { throw new ConnectError("down", Code.Unavailable); };
  if (surface === "管理") {
    renderWithAdmin({
      queryMetrics: metrics, queryProbes, getSnapshot: async () => ({ nodes: [] }), getNodeDisks: async () => ({}), listTags: async () => ({ tags: [] }),
      listNodes: async () => ({ nodes: [{ id: 7n, name: "node" }] }), getTraffic: async () => ({ nodes: [], now: BigInt(HUB_NOW), timezone: timeZone }),
      getSettings: siteFails ? fail : async () => ({ settings: {}, timezone: timeZone }),
    }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  } else {
    renderWithService(PublicService, {
      queryMetrics: metrics, queryProbes, getSnapshot: async () => ({ now: BigInt(HUB_NOW), nodes: [{ id: 7n, name: "node" }] }),
      getSite: siteFails ? fail : async () => ({ timezone: timeZone }),
    }, [{ path: "/nodes/:id", Component: NodePage }], "/nodes/7");
  }
}

function renderCompare(surface: "管理" | "公开", { stepS = 60, timeZone = "UTC" } = {}) {
  const list = async () => create(ListProbeComparisonNodesResponseSchema, { kind: ProbeKind.ICMP, target: "edge.example", nodeIds: [1n], maxNodesPerQuery: 8 });
  const query = async () => create(QueryProbeComparisonResponseSchema, { level: "1m", stepS, series: [{ nodeId: 1n, samples: samples(stepS) }] });
  if (surface === "管理") {
    renderWithAdmin({
      listProbeComparisonNodes: list, queryProbeComparison: query, listNodes: async () => ({ nodes: [{ id: 1n, name: "edge" }] }),
      getSnapshot: async () => ({ now: BigInt(HUB_NOW), nodes: [] }), getSettings: async () => ({ settings: {}, timezone: timeZone }),
    }, [{ path: "/probes/:id/compare", Component: ProbeCompare }], "/probes/9/compare");
  } else {
    renderWithService(PublicService, {
      listProbeComparisonNodes: list, queryProbeComparison: query,
      getSnapshot: async () => ({ now: BigInt(HUB_NOW), nodes: [{ id: 1n, name: "edge" }] }), getSite: async () => ({ timezone: timeZone }),
    }, [{ path: "/probes/:id", Component: PublicProbeCompare }], "/probes/9");
  }
}

const sliceNav = () => screen.getByRole("navigation", { name: "时段" });
const pick = async (label: string) => {
  const nav = await screen.findByRole("navigation", { name: "时段" });
  const button = within(nav).getByRole("button", { name: label });
  await vi.waitFor(() => expect(button).toBeEnabled());
  fireEvent.click(button);
};
const summaryOf = (title: string) => {
  const card = screen.getByRole("heading", { name: title }).closest<HTMLElement>(".chart-card")!;
  const list = within(card).getByRole("list", { name: "窗口汇总" });
  return within(list).getAllByRole("listitem").map((item) => item.textContent);
};
const compareRow = () => within(screen.getByRole("row", { name: "edge" })).getAllByRole("cell").slice(2).map((td) => td.textContent);

// 全时段：两桶合计，均值按成功数加权 (10×100 + 50×90) ÷ 190 ≈ 28.9 ms，丢包 10 ÷ 200。晚高峰：只有 hub 21:00 那一桶。
const ALL = ["28.9 ms", "5.00 ms", "90.0 ms", "5.0%"];
const EVENING = ["50.0 ms", "40.0 ms", "90.0 ms", "10%"];

describe.each(["管理", "公开"] as const)("%s端", (surface) => {
  it("单节点探测历史切到晚高峰后按 hub 时区重算汇总，图上只留整桶命中的点", async () => {
    visitorInShanghai();
    renderNode(surface);
    await screen.findByRole("heading", { name: "TCP example.com:443" });
    expect(summaryOf("TCP example.com:443")).toEqual(["均值 28.9 ms", "最小 5.00 ms", "最大 90.0 ms", "丢包率 5.0%"]);
    expect(within(sliceNav()).getByRole("button", { name: "全时段" })).toHaveAttribute("aria-pressed", "true");
    await pick("晚高峰");
    expect(within(sliceNav()).getByRole("button", { name: "晚高峰" })).toHaveAttribute("aria-pressed", "true");
    expect(summaryOf("TCP example.com:443")).toEqual(EVENING.map((v, i) => `${["均值", "最小", "最大", "丢包率"][i]} ${v}`));
    expect(JSON.parse(screen.getByTestId("chart-milliseconds").dataset.plotted!)).toEqual([[50], [40], [90]]);
    await pick("全时段");
    expect(summaryOf("TCP example.com:443")).toEqual(ALL.map((v, i) => `${["均值", "最小", "最大", "丢包率"][i]} ${v}`));
  });

  it("跨节点对比切到晚高峰后按 hub 时区重算汇总表", async () => {
    visitorInShanghai();
    renderCompare(surface);
    await screen.findByRole("row", { name: "edge" });
    expect(compareRow()).toEqual([...ALL, "0"]);
    await pick("晚高峰");
    expect(compareRow()).toEqual([...EVENING, "0"]);
    expect(JSON.parse(screen.getByTestId("chart-milliseconds").dataset.plotted!)).toEqual([[50]]);
  });

  it("步长大到没有整桶落在晚高峰内时提示缩短查询窗口，不拿跨界的桶充数", async () => {
    // 6 小时一桶：hub 的 18:00–24:00 跨了 20:00，没有一个桶整个在 20:00–24:00 里。
    renderNode(surface, { stepS: 6 * 3600 });
    await screen.findByRole("heading", { name: "TCP example.com:443" });
    await pick("晚高峰");
    expect(screen.getByText(/没有整段落在晚高峰.*缩短查询时间窗口/)).toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "窗口汇总" })).toBeNull();
  });

  it("对比页同样提示缩短查询窗口", async () => {
    renderCompare(surface, { stepS: 6 * 3600 });
    await screen.findByRole("row", { name: "edge" });
    await pick("晚高峰");
    expect(screen.getByText(/没有整段落在晚高峰.*缩短查询时间窗口/)).toBeInTheDocument();
    expect(screen.queryByRole("row", { name: "edge" })).toBeNull();
  });

  it("取不到 hub 时区时不提供晚高峰切换，也不退回访客本地时区", async () => {
    renderNode(surface, { siteFails: true });
    await screen.findByRole("heading", { name: "TCP example.com:443" });
    const evening = within(sliceNav()).getByRole("button", { name: "晚高峰" });
    await vi.waitFor(() => expect(evening).toHaveAttribute("title", expect.stringMatching(/取不到 Hub 时区/)));
    expect(evening).toBeDisabled();
  });
});

it("窗口没有经过 hub 时区的晚高峰：提示窗口里没有晚高峰时段，不让人去缩短窗口", async () => {
  // hub 在奥克兰（11 月是 +13:00）：1h 窗口 21:14–22:14Z 是当地次日 10:14–11:14。
  renderNode("公开", { timeZone: "Pacific/Auckland" });
  await screen.findByRole("heading", { name: "TCP example.com:443" });
  fireEvent.click(within(screen.getByRole("navigation", { name: "时间窗口" })).getByRole("button", { name: "1h" }));
  await pick("晚高峰");
  expect(await screen.findByText(/窗口内没有晚高峰时段/)).toBeInTheDocument();
  expect(screen.queryByText(/缩短查询时间窗口/)).toBeNull();
});
