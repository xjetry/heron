import { fireEvent, screen, within } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import { PublicService } from "../gen/heron/v1/public_pb";
import { renderWithService } from "../test/harness";
import { chooseOption, selectTrigger } from "../test/select";
import { PublicOverview } from "./Overview";
import { PUBLIC_VIEW_KEY } from "./prefs";

afterEach(() => { localStorage.clear(); });

const nodes = [
  {
    id: 1n, name: "香港家宽", online: true, lastSeenAt: 998n, sortOrder: 0, country: "HK",
    metrics: { cpuPct: 12, memUsed: 512n * 1024n ** 2n, memTotal: 1024n ** 3n, diskUsed: 1024n ** 3n, diskTotal: 10n * 1024n ** 3n, load1: 0.5, load5: 0.4, load15: 0.3, netRxBps: 1024n, netTxBps: 2048n, uptimeS: 90_061n },
    traffic: { periodRx: 1024n ** 3n, periodTx: 512n * 1024n ** 2n, quotaUsedBytes: 512n * 1024n ** 2n, quotaBytes: 1024n ** 3n, quotaUsedPct: 50 },
    billing: { expiresOn: "2026-10-30", daysLeft: 21 },
  },
  { id: 2n, name: "东京", online: true, lastSeenAt: 998n, sortOrder: 1, country: "JP", maintenance: true, metrics: { cpuPct: 90 } },
  { id: 3n, name: "离线机", online: false, lastSeenAt: 400n, sortOrder: 2, metrics: { cpuPct: 50, netRxBps: 9n, netTxBps: 9n } },
  { id: 4n, name: "新机", online: false, sortOrder: 3 },
];

async function renderList() {
  renderWithService(PublicService, { getSnapshot: async () => ({ now: 1000n, nodes, tags: [] }) }, [{ path: "/", Component: PublicOverview }], "/");
  await screen.findByRole("group", { name: "视图" });
  fireEvent.click(within(screen.getByRole("group", { name: "视图" })).getByRole("button", { name: "列表" }));
}
const rows = () => within(screen.getByRole("region", { name: "节点列表" })).getAllByRole("row").slice(1);

it("列表视图：与管理端总览同一组列（含运行时长），另加到期；离线与从未上报不折叠，全在同一张表里", async () => {
  await renderList();
  const table = screen.getByRole("region", { name: "节点列表" });
  expect(within(table).getAllByRole("columnheader").map((th) => th.textContent)).toEqual(["状态", "节点", "CPU", "内存", "磁盘", "负载", "网络", "本周期", "运行时长", "到期", "最近上报"]);
  expect(rows().map((r) => r.getAttribute("aria-label"))).toEqual(["香港家宽", "东京", "离线机", "新机"]);
  expect(document.querySelector("details.folded-nodes")).toBeNull();
  expect(screen.queryAllByRole("article")).toHaveLength(0);
});

it("一行的内容：状态点、名称链接与国家徽章、三条细条、负载、网速、本周期、到期、最近上报；缺读数画「–」", async () => {
  await renderList();
  const hk = within(screen.getByRole("row", { name: "香港家宽" }));
  expect(hk.getByRole("img", { name: "在线" })).toBeInTheDocument();
  expect(hk.getByRole("link", { name: "香港家宽" })).toHaveAttribute("href", "/nodes/1");
  expect(hk.getByTitle("国家 / 地区 HK")).toBeInTheDocument();
  expect(hk.getByRole("meter", { name: "CPU 12%" })).toBeInTheDocument();
  expect(hk.getByRole("meter", { name: "内存 50%" })).toBeInTheDocument();
  expect(hk.getByRole("meter", { name: "磁盘 10%" })).toBeInTheDocument();
  expect(hk.getByText("0.50 / 0.40 / 0.30")).toBeInTheDocument();
  expect(hk.getByText("↓ 1.0 KiB/s ↑ 2.0 KiB/s")).toBeInTheDocument();
  expect(hk.getByText("512 MiB / 1.0 GiB (50.0%)")).toBeInTheDocument();
  expect(hk.getByText("2026-10-30")).toBeInTheDocument();
  expect(hk.getByText("刚刚")).toBeInTheDocument();
  expect(within(screen.getByRole("row", { name: "东京" })).getByRole("img", { name: "维护中" })).toBeInTheDocument();
  const offline = within(screen.getByRole("row", { name: "离线机" }));
  expect(offline.getByRole("img", { name: "离线" })).toBeInTheDocument();
  expect(offline.getByText("10 分钟前")).toBeInTheDocument();
  const fresh = within(screen.getByRole("row", { name: "新机" }));
  expect(fresh.getByRole("img", { name: "从未上报" })).toBeInTheDocument();
  expect(fresh.getAllByLabelText("无读数")).toHaveLength(7);
  expect(fresh.getByText("从未上报", { selector: "td" })).toBeInTheDocument();
  expect(fresh.queryByTitle(/^国家/)).toBeNull();
});

it("运行时长一列：表头与单元格同名（手机按 data-label 布局），秒数按 duration 写成天时分，无读数画「–」", async () => {
  await renderList();
  const uptime = (name: string) => screen.getByRole("row", { name }).querySelector('td[data-label="运行时长"]');
  expect(uptime("香港家宽")).toHaveTextContent(/^1d 1h$/);
  expect(within(uptime("东京") as HTMLElement).getByLabelText("无读数")).toHaveTextContent("–");
  expect(within(uptime("新机") as HTMLElement).getByLabelText("无读数")).toBeInTheDocument();
});

it("列表与卡片共用排序选择；视图选择记到 localStorage", async () => {
  await renderList();
  expect(localStorage.getItem(PUBLIC_VIEW_KEY)).toBe("list");
  expect(selectTrigger("排序")).toHaveAccessibleName("排序 默认");
  chooseOption("排序", "CPU");
  expect(rows().map((r) => r.getAttribute("aria-label"))).toEqual(["东京", "离线机", "香港家宽", "新机"]);
  chooseOption("排序", "到期");
  expect(rows().map((r) => r.getAttribute("aria-label"))[0]).toBe("香港家宽");
  fireEvent.click(within(screen.getByRole("group", { name: "视图" })).getByRole("button", { name: "卡片" }));
  expect(selectTrigger("排序")).toHaveAccessibleName("排序 到期");
});
