import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AddressFamilies } from "../components/AddressFamilies";
import { PublicService } from "../gen/heron/v1/public_pb";
import { AddressDetectionState } from "../gen/heron/v1/types_pb";
import { renderWithService } from "../test/harness";
import { NodePage } from "./NodePage";
import { PublicOverview } from "./Overview";
import { PUBLIC_VIEW_KEY } from "./prefs";

vi.mock("../components/Chart", () => ({ Chart: () => null }));

afterEach(() => { localStorage.clear(); });

const { AVAILABLE, UNSUPPORTED, FAILED } = AddressDetectionState;
const shown = () => screen.queryByRole("group", { name: "公网出口" })?.textContent ?? null;

// 只有 AVAILABLE 画标记；不支持、探测失败、还没探测（地址族缺失）、agent 不报（network 缺失）都不画。
it.each([
  ["双栈都可用", { ipv4: { state: AVAILABLE }, ipv6: { state: AVAILABLE } }, "IPv4IPv6"],
  ["IPv6 探测失败", { ipv4: { state: AVAILABLE }, ipv6: { state: FAILED } }, "IPv4"],
  ["只有 IPv6", { ipv4: { state: UNSUPPORTED }, ipv6: { state: AVAILABLE } }, "IPv6"],
  ["都不可用", { ipv4: { state: UNSUPPORTED }, ipv6: { state: FAILED } }, null],
  ["首轮探测之前", {}, null],
  ["agent 不报", undefined, null],
] as const)("%s", (_, network, want) => {
  render(<AddressFamilies network={network as never} />);
  expect(shown()).toBe(want);
});

// 公开页四处显示国家徽章的地方都跟着画双栈标记：卡片、列表、状态墙详情面板、节点页。每处同时钉一个只画 IPv4 的节点，
// 不会把别的节点或不可用的族画出来。
const dual = { id: 1n, name: "双栈", online: true, lastSeenAt: 998n, sortOrder: 0, country: "JP",
  facts: { os: "Debian 13", network: { ipv4: { state: AVAILABLE }, ipv6: { state: AVAILABLE } } } };
const v4only = { id: 2n, name: "只有 v4", online: true, lastSeenAt: 998n, sortOrder: 1, country: "JP",
  facts: { os: "Debian 13", network: { ipv4: { state: AVAILABLE }, ipv6: { state: FAILED } } } };
// 从未上报但手填了 IPv4 的节点：facts 只带 network（PublicNode.facts），标记照画，详情面板不画"系统"一栏。
const pinnedOnly = { id: 3n, name: "只有手填", online: false, sortOrder: 2, facts: { network: { ipv4: { state: AVAILABLE } } } };
const families = (scope: HTMLElement) => within(scope).queryByRole("group", { name: "公网出口" })?.textContent ?? null;

async function overview(view: string) {
  localStorage.setItem(PUBLIC_VIEW_KEY, view);
  renderWithService(PublicService, { getSnapshot: async () => ({ now: 1000n, nodes: [dual, v4only, pinnedOnly], tags: [] }), queryMetrics: async () => ({ ts: [], series: [] }) },
    [{ path: "/", Component: PublicOverview }], "/");
  await screen.findByRole("group", { name: "视图" });
}

it("卡片视图", async () => {
  await overview("cards");
  expect(families(screen.getByRole("article", { name: "双栈" }))).toBe("IPv4IPv6");
  expect(families(screen.getByRole("article", { name: "只有 v4" }))).toBe("IPv4");
});

it("列表视图", async () => {
  await overview("list");
  const row = (name: string) => screen.getByRole("link", { name }).closest("tr")!;
  expect(families(row("双栈"))).toBe("IPv4IPv6");
  expect(families(row("只有 v4"))).toBe("IPv4");
});

it("状态墙的详情面板", async () => {
  await overview("wall");
  const panel = () => screen.getByRole("complementary", { name: "节点详情" });
  expect(within(panel()).getByRole("heading", { name: "双栈" })).toBeInTheDocument();
  expect(families(panel())).toBe("IPv4IPv6");
  fireEvent.click(screen.getByRole("link", { name: "只有 v4" }));
  expect(within(panel()).getByRole("heading", { name: "只有 v4" })).toBeInTheDocument();
  expect(families(panel())).toBe("IPv4");
  expect(within(panel()).getByText("系统")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("link", { name: "只有手填" }));
  expect(within(panel()).getByRole("heading", { name: "只有手填" })).toBeInTheDocument();
  expect(families(panel())).toBe("IPv4");
  expect(within(panel()).queryByText("系统")).toBeNull();
});

it("节点页", async () => {
  renderWithService(PublicService, { getSnapshot: async () => ({ now: 1000n, nodes: [v4only] }), queryMetrics: async () => ({ ts: [], series: [] }), queryProbes: async () => ({ series: [] }) },
    [{ path: "/nodes/:id", Component: NodePage }], "/nodes/2");
  const head = (await screen.findByRole("heading", { level: 1, name: "只有 v4" })).closest("header")!;
  expect(families(head)).toBe("IPv4");
});
