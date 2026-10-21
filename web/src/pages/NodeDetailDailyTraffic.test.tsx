import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { MetricSampleSchema, MetricSeriesSchema, QueryMetricsResponseSchema, QueryProbesResponseSchema, type QueryMetricsRequest } from "../gen/heron/v1/query_pb";
import { TrafficQuotaMode } from "../gen/heron/v1/types_pb";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { NodeDetail } from "./NodeDetail";

vi.mock("../components/Chart", () => ({ Chart: () => <div /> }));

const MiB = 1024 ** 2;
const at = (iso: string) => Date.parse(iso) / 1000;
const periodStart = at("2025-09-01T00:00:00Z");
const now = at("2025-09-04T15:33:20Z");

// 本周期 09-01..09-04：09-03 没有任何点（无数据），09-04 是今天。上周期只有 08-15 一天有量。
const points = [
  { ts: at("2025-08-15T10:00:00Z"), rx: 900 * MiB, tx: 100 * MiB },
  { ts: at("2025-09-01T08:00:00Z"), rx: 256 * MiB, tx: 0 },
  { ts: at("2025-09-02T08:00:00Z"), rx: 384 * MiB, tx: 128 * MiB },
  { ts: at("2025-09-02T09:00:00Z"), rx: 128 * MiB, tx: 128 * MiB },
  { ts: at("2025-09-04T08:00:00Z"), rx: 128 * MiB, tx: 0 },
];
const daily = create(QueryMetricsResponseSchema, {
  level: "1h", stepS: 3600, ts: points.map((p) => BigInt(p.ts)),
  series: [
    create(MetricSeriesSchema, { name: "rx_bytes", unit: "bytes", samples: points.map((p) => create(MetricSampleSchema, { n: 1, sum: p.rx })) }),
    create(MetricSeriesSchema, { name: "tx_bytes", unit: "bytes", samples: points.map((p) => create(MetricSampleSchema, { n: 1, sum: p.tx })) }),
  ],
});
const empty = create(QueryMetricsResponseSchema, { level: "1m", stepS: 60, coverageSummary: { eligibleMinutes: 0n, observedMinutes: 0n, observedReportedMinutes: 0n } });

const impl = (queryMetrics: NonNullable<AdminImpl["queryMetrics"]>) => ({
  queryMetrics,
  listNodes: async () => ({ nodes: [{ id: 7n, name: "db-01", public: false, note: "", sortOrder: 0, createdAt: 0n }] }),
  // 本周期用量 1.5 GiB，按日合计 1152 MiB：差 384 MiB 未能归日。
  getTraffic: async () => ({ timezone: "UTC", now: BigInt(now), nodes: [{ nodeId: 7n, name: "db-01", traffic: {
    totalRx: 10n * 1024n ** 3n, totalTx: 0n, periodRx: 1024n ** 3n, periodTx: 512n * 1024n ** 2n,
    periodStart: BigInt(periodStart), nextResetAt: BigInt(at("2025-10-01T00:00:00Z")), resetDay: 1 } }] }),
  getSnapshot: async () => ({ nodes: [] }),
  listTags: async () => ({ tags: [] }),
  queryProbes: async () => create(QueryProbesResponseSchema, { level: "1m", stepS: 60 }),
}) satisfies AdminImpl;

const isDaily = (req: QueryMetricsRequest) => req.maxPoints === 2000;

it("流量 tab 按 hub 时区逐日列出本周期，标出今天、无数据与用量最高的三天，并给出未能归日的差额", async () => {
  const queryMetrics = vi.fn(async (req: QueryMetricsRequest) => (isDaily(req) ? daily : empty));
  renderWithAdmin(impl(queryMetrics), [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=traffic");
  const card = await screen.findByRole("region", { name: "每日流量" });
  const bars = within(card).getByRole("list", { name: "每日用量" });
  await waitFor(() => expect(within(bars).getAllByRole("listitem")).toHaveLength(4));
  expect(within(bars).getByLabelText("2025-09-02 下行 512 MiB 上行 256 MiB 合计 768 MiB")).toBeInTheDocument();
  expect(within(bars).getByLabelText("2025-09-03 无数据")).toBeInTheDocument();
  expect(within(bars).getByLabelText("2025-09-04（今天，统计中） 下行 128 MiB 上行 0 B 合计 128 MiB")).toBeInTheDocument();
  expect(within(card).getByText("用量最高（下载 + 上传）：09-02 768 MiB、09-01 256 MiB、09-04 128 MiB")).toBeInTheDocument();
  expect(within(card).getByText(/^按日合计比本周期的下行 \+ 上行少约 384 MiB：/)).toBeInTheDocument();

  const req = queryMetrics.mock.calls.map((c) => c[0]).find(isDaily)!;
  expect(req.nodeId).toBe(7n);
  expect(Number(req.from)).toBe(periodStart - 32 * 86_400);
  expect(Number(req.to)).toBe((Math.floor(now / 60) + 1) * 60);
});

it("悬停或聚焦某一天时读数行显示那天的下行与上行", async () => {
  renderWithAdmin(impl(async (req) => (isDaily(req) ? daily : empty)), [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=traffic");
  const card = await screen.findByRole("region", { name: "每日流量" });
  const day = await within(card).findByLabelText(/^2025-09-01 /);
  fireEvent.focus(day);
  expect(within(card).getByTestId("daily-readout")).toHaveTextContent("2025-09-01：下行 256 MiB，上行 0 B，合计 256 MiB");
  fireEvent.pointerEnter(within(card).getByLabelText(/^2025-09-02 /));
  expect(within(card).getByTestId("daily-readout")).toHaveTextContent("2025-09-02：下行 512 MiB，上行 256 MiB，合计 768 MiB");
});

it("切到上周期列出上一个重置日起的整月，不显示未能归日的差额", async () => {
  renderWithAdmin(impl(async (req) => (isDaily(req) ? daily : empty)), [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=traffic");
  const card = await screen.findByRole("region", { name: "每日流量" });
  await within(card).findByText(/用量最高（下载 \+ 上传）：09-02/);
  fireEvent.click(within(card).getByRole("button", { name: "上周期" }));
  const bars = within(card).getByRole("list", { name: "每日用量" });
  const items = within(bars).getAllByRole("listitem");
  expect(items).toHaveLength(31);
  expect(items[0]).toHaveAccessibleName(/^2025-08-01 /);
  expect(items[30]).toHaveAccessibleName(/^2025-08-31 /);
  expect(within(card).getByText("用量最高（下载 + 上传）：08-15 1000 MiB")).toBeInTheDocument();
  expect(within(card).queryByText(/^按日合计比本周期/)).toBeNull();
});

// 同一个 15:30Z 的点在 UTC 是 09-01、在东京是 09-02 00:30。两个时区期望不同，页面若改用浏览器时区，不论跑测试的机器
// 在哪个时区，至少有一例会红。
it.each([
  { timezone: "UTC", periodStart: at("2025-09-01T00:00:00Z"), day: "2025-09-01", other: "2025-09-02" },
  { timezone: "Asia/Tokyo", periodStart: at("2025-08-31T15:00:00Z"), day: "2025-09-02", other: "2025-09-01" },
])("按 hub 时区 $timezone 的日界归日", async ({ timezone, periodStart, day, other }) => {
  const ts = at("2025-09-01T15:30:00Z");
  const resp = create(QueryMetricsResponseSchema, {
    level: "1h", stepS: 3600, ts: [BigInt(ts)],
    series: [
      create(MetricSeriesSchema, { name: "rx_bytes", unit: "bytes", samples: [create(MetricSampleSchema, { n: 1, sum: 100 * MiB })] }),
      create(MetricSeriesSchema, { name: "tx_bytes", unit: "bytes", samples: [create(MetricSampleSchema, { n: 1, sum: 0 })] }),
    ],
  });
  renderWithAdmin({
    ...impl(async (req) => (isDaily(req) ? resp : empty)),
    getTraffic: async () => ({ timezone, now: BigInt(at("2025-09-03T03:00:00Z")), nodes: [{ nodeId: 7n, name: "db-01", traffic: {
      periodRx: 100n * 1024n ** 2n, periodStart: BigInt(periodStart), nextResetAt: BigInt(periodStart + 30 * 86_400), resetDay: 1 } }] }),
  }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=traffic");
  const card = await screen.findByRole("region", { name: "每日流量" });
  const bars = within(card).getByRole("list", { name: "每日用量" });
  await waitFor(() => expect(within(bars).getAllByRole("listitem")).toHaveLength(3));
  expect(within(bars).getByLabelText(`${day} 下行 100 MiB 上行 0 B 合计 100 MiB`)).toBeInTheDocument();
  expect(within(bars).getByLabelText(`${other} 无数据`)).toBeInTheDocument();
});

// 09-01 下载多、09-02 上传多：按合计、仅上传、取大者三种计入方式排出的先后各不相同。
it.each([
  { name: "下载 + 上传", mode: TrafficQuotaMode.SUM, text: "用量最高（下载 + 上传）：09-01 910 MiB、09-02 310 MiB" },
  { name: "仅上传", mode: TrafficQuotaMode.TX, text: "用量最高（仅上传）：09-02 300 MiB、09-01 10 MiB" },
  { name: "取大者", mode: TrafficQuotaMode.MAX, text: "用量最高（下载与上传取大者，按下载计）：09-01 900 MiB、09-02 10 MiB" },
])("用量最高的日子按配额的计入方式（$name）排名，与流量卡的本周期同一口径", async ({ mode, text }) => {
  const days = [{ ts: at("2025-09-01T08:00:00Z"), rx: 900 * MiB, tx: 10 * MiB }, { ts: at("2025-09-02T08:00:00Z"), rx: 10 * MiB, tx: 300 * MiB }];
  const resp = create(QueryMetricsResponseSchema, {
    level: "1h", stepS: 3600, ts: days.map((p) => BigInt(p.ts)),
    series: [
      create(MetricSeriesSchema, { name: "rx_bytes", unit: "bytes", samples: days.map((p) => create(MetricSampleSchema, { n: 1, sum: p.rx })) }),
      create(MetricSeriesSchema, { name: "tx_bytes", unit: "bytes", samples: days.map((p) => create(MetricSampleSchema, { n: 1, sum: p.tx })) }),
    ],
  });
  renderWithAdmin({
    ...impl(async (req) => (isDaily(req) ? resp : empty)),
    getTraffic: async () => ({ timezone: "UTC", now: BigInt(now), nodes: [{ nodeId: 7n, name: "db-01", traffic: {
      periodRx: 910n * 1024n ** 2n, periodTx: 310n * 1024n ** 2n, periodStart: BigInt(periodStart), nextResetAt: BigInt(at("2025-10-01T00:00:00Z")), resetDay: 1,
      quotaBytes: 1024n ** 4n, quotaMode: mode } }] }),
  }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=traffic");
  const card = await screen.findByRole("region", { name: "每日流量" });
  expect(await within(card).findByText(text)).toBeInTheDocument();
});

it("本周期往小校正过、按日合计反而多时，提示多出的部分", async () => {
  // 按日合计 1152 MiB，周期用量校正成 512 MiB。
  renderWithAdmin({
    ...impl(async (req) => (isDaily(req) ? daily : empty)),
    getTraffic: async () => ({ timezone: "UTC", now: BigInt(now), nodes: [{ nodeId: 7n, name: "db-01", traffic: {
      periodRx: 512n * 1024n ** 2n, periodStart: BigInt(periodStart), nextResetAt: BigInt(at("2025-10-01T00:00:00Z")), resetDay: 1 } }] }),
  }, [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7?tab=traffic");
  const card = await screen.findByRole("region", { name: "每日流量" });
  expect(await within(card).findByText(/^按日合计比本周期的下行 \+ 上行多约 640 MiB：/)).toBeInTheDocument();
});

it("概览 tab 不发每日流量的查询；它失败时错误进页面横幅，校正表单照常可用", async () => {
  const queryMetrics = vi.fn(async (req: QueryMetricsRequest) => {
    if (isDaily(req)) throw new ConnectError("daily down", Code.Unavailable);
    return empty;
  });
  renderWithAdmin(impl(queryMetrics), [{ path: "/nodes/:id", Component: NodeDetail }], "/nodes/7");
  await screen.findByRole("button", { name: "24h" });
  expect(queryMetrics.mock.calls.map((c) => c[0]).some(isDaily)).toBe(false);
  fireEvent.click(screen.getByRole("tab", { name: "流量" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("daily down");
  expect(screen.getByLabelText("本周期下行 (GiB)")).toBeInTheDocument();
});
