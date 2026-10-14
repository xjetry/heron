import { create } from "@bufbuild/protobuf";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, expect, it, vi } from "vitest";
import { FlagSupportProvider } from "../components/CountryFlag";
import { PublicNodeSchema, PublicService } from "../gen/heron/v1/public_pb";
import { BillingCycle } from "../gen/heron/v1/types_pb";
import { TrafficQuotaMode } from "../gen/heron/v1/types_pb";
import { renderWithService } from "../test/harness";
import { PublicOverview } from "./Overview";
import { NodeCard } from "./NodeCard";
import { chooseOption } from "../test/select";

afterEach(() => { vi.useRealTimers(); localStorage.clear(); });

// 每条进度条一行：标签、百分比，有绝对量时再跟一行用量 / 总量（Bar 自带的可访问名称不算可见文字）。
function meterRows(card: HTMLElement) {
  return [...card.querySelectorAll(".node-meter")].map((row) => [...row.querySelectorAll(":scope > span:not(.meter-missing)")].map((span) => span.textContent));
}

async function renderCards(nodes: object[]) {
  renderWithService(PublicService, { getSnapshot: async () => ({ now: 1000n, nodes, tags: [] }) }, [{ path: "/", Component: PublicOverview }], "/");
  await screen.findByRole("group", { name: "视图" });
  fireEvent.click(screen.getByRole("button", { name: "卡片" }));
}

it("卡片：名称、国家、状态徽章；系统图标 · 虚拟化 · 架构；三条进度条（CPU 带核心数，内存与磁盘带用量 / 总量与百分比）；↓↑ 速率；底部本周期、累计流量、费用、到期，运行时长在最后一行", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date(2031, 0, 1));
  await renderCards([{
    id: 1n, name: "香港家宽", online: true, lastSeenAt: 998n, country: "HK", tags: ["家宽"], publicRemark: "联通 4837",
    facts: { os: "Debian 13", arch: "amd64", virtualization: "kvm", cpuCores: 2 },
    metrics: { cpuPct: 0, memUsed: 0n, memTotal: 1024n ** 3n, diskUsed: 9n * 1024n ** 3n, diskTotal: 10n * 1024n ** 3n, uptimeS: 90000n, netRxBps: 0n, netTxBps: 1024n },
    traffic: { totalRx: 300n * 1024n ** 3n, totalTx: 5n * 1024n ** 4n, periodRx: 1024n ** 3n, periodTx: 2n * 1024n ** 3n, quotaUsedBytes: 2n * 1024n ** 3n, quotaBytes: 4n * 1024n ** 3n, quotaUsedPct: 50 },
    billing: { price: "12.50", currency: "USD", billingCycle: BillingCycle.MONTHLY, expiresOn: "2026-10-01", daysLeft: 4 },
  }]);
  const card = within(screen.getByRole("article", { name: "香港家宽" }));
  expect.soft(card.getByRole("link", { name: "香港家宽" })).toHaveAttribute("href", "/nodes/1");
  expect.soft(card.getByTitle("国家 / 地区 HK")).toBeInTheDocument();
  expect.soft(card.getByText("在线")).toHaveAttribute("data-status", "online");
  expect.soft(card.getByText("联通 4837")).toBeInTheDocument();
  // os 不再出文字：系统行只剩图标与 虚拟化 · 架构；图标的可访问名与悬停提示是完整 os 字符串；运行时长在底行。
  const meta = card.getByText("kvm · amd64").closest(".node-card-meta")!;
  expect.soft(meta.querySelector("svg.os-icon")).toHaveAttribute("data-os", "debian");
  expect.soft(card.getByRole("img", { name: "Debian 13" })).toBeInTheDocument();
  expect.soft(meta.textContent).not.toMatch(/运行|1d 1h/);
  expect.soft(card.getByRole("meter", { name: "CPU 2 核 (0.0%)" })).toHaveAttribute("aria-valuenow", "0");
  expect.soft(card.getByRole("meter", { name: "内存 0 B / 1.0 GiB (0.0%)" })).toHaveAttribute("aria-valuenow", "0");
  expect.soft(card.getByRole("meter", { name: "磁盘 9.0 GiB / 10 GiB (90%)" })).toHaveAttribute("data-level", "attention");
  expect.soft(meterRows(screen.getByRole("article", { name: "香港家宽" }))).toEqual([["CPU", "0.0%", "2 核"], ["内存", "0.0%", "0 B / 1.0 GiB"], ["磁盘", "90%", "9.0 GiB / 10 GiB"]]);
  expect.soft(card.getByRole("group", { name: "网络速率" })).toHaveTextContent("↓ 0 B/s ↑ 1.0 KiB/s");
  expect.soft(card.getByText("本周期").nextElementSibling).toHaveTextContent("2.0 GiB / 4.0 GiB (50.0%)下载 + 上传");
  expect.soft(card.getByText("累计流量").nextElementSibling).toHaveTextContent(/^↓ 300 GiB ↑ 5\.0 TiB$/);
  expect.soft(card.getByText("费用").nextElementSibling).toHaveTextContent("US$12.50 / 月");
  expect.soft(card.getByText("到期").nextElementSibling).toHaveTextContent("2026-10-01");
  expect.soft(card.getByText("剩 4 天").closest(".expiry")).toHaveAttribute("data-level", "attention");
  const foot = card.getByText("本周期").closest("dl") as HTMLElement;
  expect.soft([...foot.querySelectorAll("dt")].map((dt) => dt.textContent)).toEqual(["本周期", "累计流量", "费用", "到期", "运行时长"]);
  expect.soft(foot.lastElementChild).toHaveTextContent(/^1d 1h$/);
});

it("没填费用与到期、没有运行时长读数时底行写破折号；已过期写已过期天数并标玫红；无读数不画进度条", async () => {
  await renderCards([
    { id: 1n, name: "plain", online: true, lastSeenAt: 998n },
    { id: 2n, name: "lapsed", online: true, lastSeenAt: 998n, billing: { expiresOn: "2026-09-24", daysLeft: -3 } },
  ]);
  const plain = within(screen.getByRole("article", { name: "plain" }));
  expect.soft(plain.getByText("费用").nextElementSibling).toHaveTextContent("–");
  expect.soft(plain.getByText("到期").nextElementSibling).toHaveTextContent("–");
  expect.soft(plain.getByText("运行时长").nextElementSibling).toHaveTextContent(/^–$/);
  expect.soft(plain.queryAllByRole("meter")).toHaveLength(0);
  // 没有主机信息（也没有双栈标记）时系统行整行不画。
  expect.soft(plain.queryByText("系统未知")).toBeNull();
  expect.soft(screen.getByRole("article", { name: "plain" }).querySelector(".node-card-meta")).toBeNull();
  const lapsed = within(screen.getByRole("article", { name: "lapsed" }));
  expect.soft(lapsed.getByText("已过期 3 天").closest(".expiry")).toHaveAttribute("data-level", "critical");
});

it("内存与磁盘的百分比按 used / total 计；总量为 0 或缺失、用量缺失时不画条，写破折号而不是 0%", async () => {
  await renderCards([
    { id: 1n, name: "quarter", online: true, lastSeenAt: 998n, metrics: { memUsed: 1n * 1024n ** 3n, memTotal: 4n * 1024n ** 3n, diskUsed: 3n * 1024n ** 3n, diskTotal: 4n * 1024n ** 3n } },
    { id: 2n, name: "zero-total", online: true, lastSeenAt: 998n, metrics: { cpuPct: 5, memUsed: 1024n, memTotal: 0n, diskUsed: 0n } },
    { id: 3n, name: "no-used", online: true, lastSeenAt: 998n, metrics: { cpuPct: 5, diskTotal: 4n * 1024n ** 3n } },
  ]);
  const quarter = within(screen.getByRole("article", { name: "quarter" }));
  expect.soft(quarter.getByRole("meter", { name: "内存 1.0 GiB / 4.0 GiB (25%)" })).toHaveAttribute("aria-valuenow", "25");
  expect.soft(quarter.getByRole("meter", { name: "磁盘 3.0 GiB / 4.0 GiB (75%)" })).toHaveAttribute("aria-valuenow", "75");
  const zero = within(screen.getByRole("article", { name: "zero-total" }));
  expect.soft(zero.getAllByRole("meter").map((m) => m.getAttribute("aria-label"))).toEqual(["CPU 5.0%"]);
  expect.soft(meterRows(screen.getByRole("article", { name: "zero-total" }))).toEqual([["CPU", "5.0%"], ["内存", "–"], ["磁盘", "–"]]);
  const noUsed = within(screen.getByRole("article", { name: "no-used" }));
  expect.soft(noUsed.getAllByRole("meter").map((m) => m.getAttribute("aria-label"))).toEqual(["CPU 5.0%"]);
  expect.soft(meterRows(screen.getByRole("article", { name: "no-used" }))).toEqual([["CPU", "5.0%"], ["内存", "–"], ["磁盘", "–"]]);
});

it("累计流量：零用量写 0 B，traffic 缺失时不出这一行", async () => {
  await renderCards([
    { id: 1n, name: "fresh", online: true, lastSeenAt: 998n, traffic: {} },
    { id: 2n, name: "none", online: true, lastSeenAt: 998n },
  ]);
  const fresh = within(screen.getByRole("article", { name: "fresh" }));
  expect.soft(fresh.getByText("累计流量").nextElementSibling).toHaveTextContent(/^↓ 0 B ↑ 0 B$/);
  const none = within(screen.getByRole("article", { name: "none" }));
  expect.soft(none.queryByText("累计流量")).toBeNull();
  expect.soft([...none.getByText("本周期").closest("dl")!.querySelectorAll("dt")].map((dt) => dt.textContent)).toEqual(["本周期", "费用", "到期", "运行时长"]);
});

it("只有在线与维护中出卡片；离线与从未上报列在网格下方默认展开的表格里（名称、地区、状态、最后上报）", async () => {
  await renderCards([
    { id: 1n, name: "on", online: true, lastSeenAt: 998n, country: "JP" },
    { id: 2n, name: "maint", online: false, lastSeenAt: 900n, maintenance: true },
    { id: 3n, name: "off", online: false, lastSeenAt: 400n, country: "HK" },
    { id: 4n, name: "fresh", online: false },
  ]);
  expect.soft(screen.getAllByRole("article").map((a) => a.getAttribute("aria-label"))).toEqual(["on", "maint"]);
  expect.soft(within(screen.getByRole("article", { name: "maint" })).getByText("维护中")).toHaveAttribute("data-status", "maintenance");
  const folded = screen.getByText("离线、从未上报与已出租 · 2").closest("details")!;
  expect.soft(folded).toHaveAttribute("open");
  const rows = within(folded).getAllByRole("row").slice(1).map((row) => within(row).getAllByRole("cell").map((cell) => cell.textContent));
  expect.soft(rows).toEqual([["off", "HK", "离线", "10 分钟前"], ["fresh", "", "从未上报", "–"]]);
  expect.soft(within(folded).getByRole("link", { name: "off" })).toHaveAttribute("href", "/nodes/3");
});

it("已出租且仍在上报的节点不出卡片，进紧凑表并带「已出租」徽章", async () => {
  await renderCards([
    { id: 1n, name: "on", online: true, lastSeenAt: 998n },
    { id: 2n, name: "leased", online: true, lastSeenAt: 998n, rented: true, country: "US", metrics: { cpuPct: 40 } },
    { id: 3n, name: "off", online: false, lastSeenAt: 400n },
  ]);
  expect(screen.getAllByRole("article").map((a) => a.getAttribute("aria-label"))).toEqual(["on"]);
  const folded = screen.getByText("离线、从未上报与已出租 · 2").closest("details")!;
  const rows = within(folded).getAllByRole("row").slice(1).map((row) => within(row).getAllByRole("cell").map((cell) => cell.textContent));
  expect(rows).toEqual([["leased", "US", "已出租", "刚刚"], ["off", "", "离线", "10 分钟前"]]);
  expect(within(folded).getByText("已出租")).toHaveAttribute("data-status", "rented");
});

it("访客收起离线分组后，轮询带来新数据也不会把它再展开", async () => {
  let now = 1000n;
  const { queryClient } = renderWithService(PublicService, { getSnapshot: async () => ({ now, nodes: [
    { id: 1n, name: "on", online: true, lastSeenAt: now - 2n },
    { id: 3n, name: "off", online: false, lastSeenAt: 400n },
  ], tags: [] }) }, [{ path: "/", Component: PublicOverview }], "/");
  const folded = () => screen.getByText("离线、从未上报与已出租 · 1").closest("details")!;
  expect(await screen.findByText("10 分钟前")).toBeInTheDocument();
  expect(folded()).toHaveAttribute("open");
  // 访客点 summary 收起时，浏览器只改 DOM 上的 open。
  folded().open = false;
  now = 1060n;
  await act(async () => { await queryClient.refetchQueries(); });
  expect(await screen.findByText("11 分钟前")).toBeInTheDocument();
  expect(folded()).not.toHaveAttribute("open");
});

it("卡片排序：默认顺序与到期 / CPU / 流量排序", async () => {
  await renderCards([
    { id: 1n, name: "a", online: true, lastSeenAt: 1n, traffic: { periodRx: 1n, periodTx: 2n, quotaUsedBytes: 3n }, metrics: { cpuPct: 10 }, billing: { expiresOn: "2030-01-01", daysLeft: 30 } },
    { id: 2n, name: "b", online: true, lastSeenAt: 1n, traffic: { periodRx: 20n, periodTx: 10n, quotaUsedBytes: 30n }, metrics: { cpuPct: 90 }, billing: { expiresOn: "2030-01-01", daysLeft: 5 } },
  ]);
  const order = () => screen.getAllByRole("article").map((a) => a.getAttribute("aria-label"));
  expect.soft(order()).toEqual(["a", "b"]);
  chooseOption("排序", "CPU");
  expect.soft(order()).toEqual(["b", "a"]);
  chooseOption("排序", "到期");
  expect.soft(order()).toEqual(["b", "a"]);
  chooseOption("排序", "流量");
  expect.soft(order()).toEqual(["b", "a"]);
  chooseOption("排序", "默认");
  expect.soft(order()).toEqual(["a", "b"]);
});

it("流量副标题跟随节点的配额口径，未设配额时沿用下载 + 上传", async () => {
  await renderCards([
    { id: 1n, name: "只收", online: true, lastSeenAt: 998n, traffic: { periodRx: 300n, periodTx: 700n, quotaUsedBytes: 300n, quotaBytes: 1000n, quotaMode: TrafficQuotaMode.RX, quotaUsedPct: 30 } },
    { id: 2n, name: "无配额", online: true, lastSeenAt: 998n, traffic: { periodRx: 300n, periodTx: 700n, quotaUsedBytes: 1000n, quotaMode: TrafficQuotaMode.TX } },
  ]);
  const rx = within(await screen.findByRole("article", { name: "只收" }));
  expect(rx.getByText("300 B / 1000 B (30.0%)")).toHaveClass("num");
  expect(rx.getByText("仅下载")).not.toHaveClass("num");
  const none = within(screen.getByRole("article", { name: "无配额" }));
  expect(none.getByText("1000 B")).toHaveClass("num");
  expect(none.getByText("下载 + 上传 · 未设配额")).not.toHaveClass("num");
});

// 国旗判定通过 FlagSupportProvider 注入（jsdom 的 canvas 不可用，默认路径永远判不支持、走文字徽章，
// 上面"名称、国家、状态徽章"用例的 getByTitle("国家 / 地区 HK") 钉的就是这条默认路径）。
it("国家：能渲染该旗帜时名称前是 emoji、无文字胶囊；渲染不了退回名称后的文字徽章", () => {
  const init = { id: 1n, name: "香港家宽", online: true, lastSeenAt: 998n, country: "HK" } as const;
  const renderCard = (support: (code: string) => boolean) => render(
    <FlagSupportProvider value={support}>
      <MemoryRouter><NodeCard node={create(PublicNodeSchema, init)} now={1000} /></MemoryRouter>
    </FlagSupportProvider>,
  );
  const { unmount } = renderCard(() => true);
  const flag = screen.getByRole("img", { name: "国家 / 地区 HK" });
  expect(flag).toHaveTextContent("🇭🇰");
  expect(flag).toHaveAttribute("title", "国家 / 地区 HK");
  const link = screen.getByRole("link", { name: "香港家宽" });
  expect(flag.compareDocumentPosition(link) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  expect(document.querySelector(".country-badge")).toBeNull();
  unmount();
  renderCard(() => false);
  expect(screen.queryByRole("img", { name: "国家 / 地区 HK" })).toBeNull();
  expect(screen.getByText("HK")).toHaveClass("country-badge");
});
