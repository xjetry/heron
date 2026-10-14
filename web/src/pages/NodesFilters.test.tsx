import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { Nodes } from "./Nodes";
import { chooseOption, selectTrigger } from "../test/select";

afterEach(() => vi.useRealTimers());

const nodes = [
  { id: 1n, name: "on", public: true, maintenance: false, tags: ["prod"], trafficResetDay: 1, facts: { agentVersion: "v0.8.0" }, billing: { daysLeft: 12 } },
  { id: 2n, name: "off", public: false, maintenance: false, tags: [], trafficResetDay: 1, facts: { agentVersion: "v0.7.0" } },
  { id: 3n, name: "maint", public: false, maintenance: true, tags: [], trafficResetDay: 1 },
  { id: 4n, name: "fresh", public: false, maintenance: false, tags: [], trafficResetDay: 1 },
];
const snapshot = { now: 1_000n, reportIntervalMs: 4000, hubVersion: "v0.9.0", boundAgentVersion: "v0.8.0",
  nodes: [{ id: 1n, name: "on", online: true, lastSeenAt: 990n, traffic: { periodRx: 1024n, periodTx: 2048n } }, { id: 2n, name: "off", online: false, lastSeenAt: 1n }, { id: 3n, name: "maint", online: true, lastSeenAt: 990n }] };
const impl = { listNodes: async () => ({ nodes }), getSnapshot: async () => snapshot, listTags: async () => ({ tags: [{ name: "prod", nodeCount: 1 }] }) } satisfies AdminImpl;
const render = (path = "/nodes", over: AdminImpl = {}) => renderWithAdmin({ ...impl, ...over }, [{ path: "/nodes", Component: Nodes }], path);
const rowNames = () => screen.getAllByRole("row").slice(1).map((row) => within(row).getByRole("link").textContent);

it("状态列合并快照与维护中：维护中压过在线，不在快照里的节点是状态未知；离线数与总览同一判定", async () => {
  render();
  await screen.findByRole("row", { name: /fresh/ });
  const status = (name: string) => within(screen.getByRole("row", { name: new RegExp(name) })).getAllByRole("cell")[4].textContent;
  expect([status("on"), status("off"), status("maint"), status("fresh")]).toEqual(["在线", "离线", "维护中", "状态未知"]);
  expect(screen.getByRole("row", { name: /fresh/ })).toHaveAttribute("data-status", "unknown");
});

it("URL 里的筛选落到控件并过滤列表；非法值被忽略；控件改动回写 URL 并清空选择", async () => {
  const { router } = render("/nodes?status=offline&expiring=yes");
  await screen.findByRole("row", { name: /off/ });
  expect(rowNames()).toEqual(["off"]);
  expect(selectTrigger("状态")).toHaveAccessibleName("状态 离线");
  expect(screen.getByRole("checkbox", { name: "只看 15 天内到期" })).not.toBeChecked();
  fireEvent.click(screen.getByRole("checkbox", { name: `选择 off（#2）` }));
  expect(screen.getByRole("toolbar", { name: "批量操作" })).toHaveTextContent("已选择 1 个节点");
  chooseOption("状态", "全部");
  expect(screen.queryByRole("toolbar", { name: "批量操作" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("checkbox", { name: "只看 15 天内到期" }));
  expect(router.state.location.search).toBe("?expiring=1");
  expect(rowNames()).toEqual(["on"]);
  expect(screen.queryByRole("toolbar", { name: "批量操作" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "清除筛选" }));
  expect(router.state.location.search).toBe("");
  expect(rowNames()).toEqual(["on", "off", "maint", "fresh"]);
});

it("落后筛选：绑定版本已知时只列落后节点；未知时结果为空并说明原因", async () => {
  render("/nodes?lagging=1");
  await screen.findByRole("row", { name: /off/ });
  expect(rowNames()).toEqual(["off"]);
  cleanup();
  render("/nodes?lagging=1", { getSnapshot: async () => { throw new Error("snapshot unavailable"); } });
  expect(await screen.findByRole("status")).toHaveTextContent("无法取得 hub 绑定的 agent 版本");
});

it("名称格：公开胶囊只在公开节点出现，落后徽章一行，标签列表只在有标签时渲染", async () => {
  render();
  const on = within(await screen.findByRole("row", { name: /^on/ }));
  expect(on.getByText("公开")).toBeInTheDocument();
  expect(on.getByRole("list", { name: "标签 on（#1）" })).toHaveTextContent("prod");
  const off = within(screen.getByRole("row", { name: /off/ }));
  expect(off.queryByText("公开")).not.toBeInTheDocument();
  expect(off.getByText("agent 低于 v0.8.0")).toHaveClass("badge-attention");
  expect(off.queryByRole("list", { name: /^标签/ })).not.toBeInTheDocument();
});

it("批量条只在有选择时出现，表头全选作用于当前结果", async () => {
  render();
  await screen.findByRole("row", { name: /fresh/ });
  expect(screen.queryByRole("toolbar", { name: "批量操作" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("checkbox", { name: "选择当前结果全部节点" }));
  expect(screen.getByRole("toolbar", { name: "批量操作" })).toHaveTextContent("已选择 4 个节点");
  fireEvent.click(within(screen.getByRole("toolbar", { name: "批量操作" })).getByRole("button", { name: "清除选择" }));
  expect(screen.queryByRole("toolbar", { name: "批量操作" })).not.toBeInTheDocument();
});

it("行菜单打开且武装了删除时列表轮询刷新：菜单不关、武装保持；节点消失则菜单随行卸载", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  let gone = false;
  let revision = 0;
  const listNodes = vi.fn(async () => ({ nodes: (gone ? nodes.slice(1) : nodes).map((node) => ({ ...node, note: `刷新 ${++revision}` })) }));
  render("/nodes", { listNodes });
  await screen.findByRole("row", { name: /^on/ });
  fireEvent.click(screen.getByRole("button", { name: "更多操作 on（#1）" }));
  fireEvent.click(screen.getByRole("menuitem", { name: "删除 on（#1）" }));
  expect(screen.getByRole("menuitem", { name: "确认删除 on（#1）" })).toBeInTheDocument();
  await act(async () => { await vi.advanceTimersByTimeAsync(10_000 + 100); });
  await waitFor(() => expect(listNodes.mock.calls.length).toBeGreaterThanOrEqual(2));
  expect(screen.getByRole("menuitem", { name: "确认删除 on（#1）" })).toBeInTheDocument();
  gone = true;
  await act(async () => { await vi.advanceTimersByTimeAsync(10_000 + 100); });
  await waitFor(() => expect(screen.queryByRole("row", { name: /^on/ })).not.toBeInTheDocument());
  expect(screen.queryByRole("menu")).not.toBeInTheDocument();
});

it("行菜单「移动到…」作用于单个节点，区间按全部节点计算", async () => {
  render();
  await screen.findByRole("row", { name: /^on/ });
  fireEvent.click(screen.getByRole("button", { name: "更多操作 on（#1）" }));
  fireEvent.click(screen.getByRole("menuitem", { name: "移动到… on（#1）" }));
  const dialog = await screen.findByRole("dialog");
  expect(dialog).toHaveTextContent("共 4 个节点，将移动其中的 1 个");
});

// 按列排序是视图级的：显示顺序不再是服务端全序，与筛选一样关闭拖动与上下移（它们保存完整排列），序号用全序名次。
it("sort=expiry：已过期在前、剩余天数升序、没有到期日的最后保持全序；表头标 aria-sort；拖动关闭；再点表头回到全序", async () => {
  const dated = [
    { ...nodes[0], position: 1, billing: { expiresOn: "2030-01-13", daysLeft: 12 } },
    { ...nodes[1], position: 2, billing: { expiresOn: "2029-12-28", daysLeft: -3 } },
    { ...nodes[2], position: 3 },
    { ...nodes[3], position: 4, billing: { expiresOn: "2030-02-10", daysLeft: 40 } },
  ];
  const { router } = render("/nodes?sort=expiry", { listNodes: async () => ({ nodes: dated }) });
  await screen.findByRole("row", { name: /fresh/ });
  expect(rowNames()).toEqual(["off", "on", "fresh", "maint"]);
  expect(screen.getByRole("columnheader", { name: "到期" })).toHaveAttribute("aria-sort", "ascending");
  expect(screen.getByRole("button", { name: "调整顺序 on（#1）" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "调整顺序 on（#1）" })).toHaveTextContent("1");
  expect(screen.getByText(/按列排序时不能用拖动或上下移/)).toBeInTheDocument();
  // 排序不是筛选：没有「清除筛选」，也不清空已选
  expect(screen.queryByRole("button", { name: "清除筛选" })).toBeNull();
  fireEvent.click(screen.getByRole("checkbox", { name: "选择 off（#2）" }));
  // 筛选行里有排序胶囊：窄屏下表头不可见，它是看得见的退出路径
  fireEvent.click(screen.getByRole("button", { name: "取消按到期排序" }));
  expect(router.state.location.search).toBe("");
  expect(rowNames()).toEqual(["on", "off", "maint", "fresh"]);
  expect(screen.getByRole("columnheader", { name: "到期" })).toHaveAttribute("aria-sort", "none");
  expect(screen.getByRole("button", { name: "调整顺序 on（#1）" })).toBeEnabled();
  expect(screen.getByRole("toolbar", { name: "批量操作" })).toHaveTextContent("已选择 1 个节点");
  expect(screen.queryByRole("button", { name: "取消按到期排序" })).toBeNull();
  // 表头按钮进入与退出
  fireEvent.click(within(screen.getByRole("columnheader", { name: "到期" })).getByRole("button", { name: "到期" }));
  expect(router.state.location.search).toBe("?sort=expiry");
  expect(screen.getByRole("button", { name: "取消按到期排序" })).toBeInTheDocument();
  fireEvent.click(within(screen.getByRole("columnheader", { name: "到期" })).getByRole("button", { name: "到期" }));
  expect(router.state.location.search).toBe("");
});
