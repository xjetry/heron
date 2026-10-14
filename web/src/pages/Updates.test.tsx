import { expect, it } from "vitest";
import { Code, ConnectError } from "@connectrpc/connect";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { AdminService, type GetUpdatesResponse, GetUpdatesResponseSchema } from "../gen/heron/v1/admin_pb";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { Updates } from "./Updates";
import { expectEmptyState } from "../test/empty";

const targets = [
  { nodeId: 0n, status: { supported: true, version: "v0.2.0" } },
  { nodeId: 1n, status: { supported: true, version: "v0.2.0", source: "hub" } },
  { nodeId: 2n, status: { supported: false, version: "v0.2.0", reason: "OpenRC is unsupported" } },
  { nodeId: 3n, status: { supported: true, version: "v0.2.0", source: "github" } },
];
// hub v0.5.5 绑定 agent v0.5.4：东京 v0.5.3 落后，香港不支持，西雅图已在绑定版本。
const boundTargets = [
  { nodeId: 0n, status: { supported: true, version: "v0.5.5" } },
  { nodeId: 1n, status: { supported: true, version: "v0.5.3" } },
  { nodeId: 2n, status: { supported: false, version: "v0.5.3", reason: "OpenRC is unsupported" } },
  { nodeId: 3n, status: { supported: true, version: "v0.5.4" } },
];
function render(impl: AdminImpl = {}) {
  return renderWithAdmin({
    getUpdates: async (req) => ({ targets, latestVersion: req.checkLatest ? "v0.3.0" : "", boundAgentVersion: "v0.3.0" }),
    listNodes: async () => ({ nodes: [{ id: 1n, name: "东京" }, { id: 2n, name: "香港" }, { id: 3n, name: "西雅图" }] }),
    ...impl,
  }, [{ path: "/updates", Component: Updates }], "/updates");
}
async function check(version = "v0.3.0") {
  fireEvent.click(await screen.findByRole("button", { name: "检查官方新版本" }));
  await waitFor(() => expect(within(screen.getByRole("region", { name: "Hub" })).getByText("官方最新正式版").nextElementSibling).toHaveTextContent(version));
}

it("打开即自动检查一次官方新版本（恰一次），按实际更新能力禁用目标；按钮可再次手动检查", async () => {
  const checks: boolean[] = [];
  render({ getUpdates: async (req) => { checks.push(req.checkLatest); return { targets, latestVersion: req.checkLatest ? "v0.3.0" : "", boundAgentVersion: "v0.3.0" }; } });
  // 轮询的 GetUpdates({}) 不查官方：带 checkLatest 的调用恰有一次，就是打开页面时自动发的那次。
  // hub v0.2.0 落后官方最新 v0.3.0，自动检查填上目标后“更新 Hub”即可用，无需先点按钮。
  await waitFor(() => expect(checks.filter((latest) => latest)).toEqual([true]));
  expect(await screen.findByRole("button", { name: "更新 Hub" })).toBeEnabled();
  expect(within(screen.getByRole("region", { name: "Hub" })).getByText("官方最新正式版").nextElementSibling).toHaveTextContent("v0.3.0");
  expect(screen.getByRole("checkbox", { name: "选择 香港（#2）" })).toBeDisabled();
  expect(screen.getByText(/OpenRC is unsupported/)).toBeInTheDocument();
  const button = screen.getByRole("button", { name: "检查官方新版本" });
  await waitFor(() => expect(button).toBeEnabled());
  fireEvent.click(button);
  await waitFor(() => expect(checks.filter((latest) => latest)).toEqual([true, true]));
});

it("Hub 要求确认，只发送固定目标和版本，提交不冒充成功", async () => {
  const calls: unknown[] = [];
  render({ startUpdate: async (req) => { calls.push({ nodeId: req.nodeId, version: req.version }); return {}; } });
  await check(); fireEvent.click(screen.getByRole("button", { name: "更新 Hub" }));
  const modal = await screen.findByRole("dialog", { name: "确认更新 Hub" });
  expect(calls).toEqual([]);
  expect(within(modal).getByText(/短暂断连/)).toBeInTheDocument();
  fireEvent.click(within(modal).getByRole("button", { name: "确认更新" }));
  await screen.findByText(/Hub：更新任务已提交/);
  expect(calls).toEqual([{ nodeId: 0n, version: "v0.3.0" }]);
  expect(screen.queryByText("更新成功")).toBeNull();
  expect(within(screen.getByRole("region", { name: "Hub" })).getByText("官方最新正式版").nextElementSibling).toHaveTextContent("v0.3.0");
});

it("批量更新逐目标显示部分失败", async () => {
  const ids: bigint[] = [];
  render({ startUpdate: async (req) => { ids.push(req.nodeId); if (req.nodeId === 3n) throw new ConnectError("节点已在更新", Code.FailedPrecondition); return {}; } });
  await check();
  fireEvent.click(screen.getByRole("checkbox", { name: "选择 东京（#1）" }));
  fireEvent.click(screen.getByRole("checkbox", { name: "选择 西雅图（#3）" }));
  fireEvent.click(screen.getByRole("button", { name: "更新选中节点（2）" }));
  fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "确认更新" }));
  await screen.findByText(/西雅图：.*节点已在更新/);
  expect(screen.getByText(/东京：更新任务已提交/)).toBeInTheDocument();
  // 进度与结果在固定高度的弹窗里而不是页面上；弹窗随时可关，关闭后弹窗消失、结果不再占页面。
  const progress = screen.getByRole("dialog", { name: "提交更新任务" });
  expect(within(progress).getByRole("list", { name: "提交结果" }).children).toHaveLength(2);
  expect(within(progress).getByRole("button", { name: "关闭" })).toBeEnabled();
  fireEvent.click(within(progress).getByRole("button", { name: "关闭" }));
  await waitFor(() => expect(screen.queryByRole("dialog", { name: "提交更新任务" })).toBeNull());
  expect(ids).toEqual([1n, 3n]);
});

it("确认后立即关闭确认框，提交进度与结果显示在进度弹窗里；提交期间可关闭弹窗，提交继续、结果仍进表格", async () => {
  const ids: bigint[] = [];
  const releases: Array<() => void> = [];
  let current: TargetsInit = targets;
  render({
    getUpdates: async (req) => ({ targets: current, latestVersion: req.checkLatest ? "v0.3.0" : "", boundAgentVersion: "v0.3.0" }),
    startUpdate: (req) => new Promise((resolve) => { ids.push(req.nodeId); releases.push(() => {
      current = current.map((t) => t.nodeId === req.nodeId ? { ...t, status: { ...t.status, task: { id: `task-${req.nodeId}`, version: "v0.3.0", state: "succeeded" } } } : t) as TargetsInit;
      resolve({});
    }); }),
  });
  await check();
  fireEvent.click(screen.getByRole("checkbox", { name: "选择 东京（#1）" }));
  fireEvent.click(screen.getByRole("checkbox", { name: "选择 西雅图（#3）" }));
  fireEvent.click(screen.getByRole("button", { name: "更新选中节点（2）" }));
  fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "确认更新" }));
  // 第一个请求还没有返回，确认框已经关闭，取而代之的是进度弹窗。
  await waitFor(() => expect(ids).toEqual([1n]));
  expect(screen.queryByRole("dialog", { name: "确认更新节点" })).toBeNull();
  const progress = screen.getByRole("dialog", { name: "提交更新任务" });
  expect(within(progress).getByText("正在提交更新任务：0/2")).toBeInTheDocument();
  // 提交期间关闭按钮与 Esc 都可用；busy 仍锁住页面上的发起与勾选控件，同一批目标不会被重复提交。
  expect(within(progress).getByRole("button", { name: "关闭" })).toBeEnabled();
  expect(within(progress).getByRole("button", { name: "关闭弹窗" })).toBeEnabled();
  expect(screen.getByRole("button", { name: "更新选中节点（2）" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "检查官方新版本" })).toBeDisabled();
  expect(screen.getByRole("checkbox", { name: "选择 东京（#1）" })).toBeDisabled();
  // Esc 关闭只是收起弹窗，不取消逐个提交：弹窗消失后提交循环继续，busy 不解锁。
  fireEvent(progress, new Event("cancel", { cancelable: true }));
  await waitFor(() => expect(screen.queryByRole("dialog", { name: "提交更新任务" })).toBeNull());
  expect(screen.getByRole("button", { name: "更新选中节点（2）" })).toBeDisabled();
  await act(async () => { releases[0](); });
  await waitFor(() => expect(ids).toEqual([1n, 3n]));
  await act(async () => { releases[1](); });
  // 提交结束后 busy 解除；弹窗已关，结果在表格的任务状态里看（visibleTask 对本页发起的目标显示终态）。
  await waitFor(() => expect(screen.getByRole("button", { name: "检查官方新版本" })).toBeEnabled());
  expect(await within(screen.getByRole("row", { name: /东京/ })).findByText("更新成功")).toBeInTheDocument();
  expect(within(screen.getByRole("row", { name: /西雅图/ })).getByText("更新成功")).toBeInTheDocument();
});

it("进度弹窗的焦点落在关闭按钮上，结果到达只替换文字、不带走焦点", async () => {
  const releases: Array<() => void> = [];
  render({ startUpdate: () => new Promise((resolve) => { releases.push(() => resolve({})); }) });
  await check();
  fireEvent.click(screen.getByRole("checkbox", { name: "选择 东京（#1）" }));
  fireEvent.click(screen.getByRole("button", { name: "更新选中节点（1）" }));
  fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "确认更新" }));
  const progress = await screen.findByRole("dialog", { name: "提交更新任务" });
  const close = within(progress).getByRole("button", { name: "关闭" });
  await waitFor(() => expect(document.activeElement).toBe(close));
  await waitFor(() => expect(releases).toHaveLength(1));
  await act(async () => { releases[0](); });
  await within(progress).findByText(/东京：更新任务已提交/);
  expect(document.activeElement).toBe(close);
  // 逐条结果经 aria-live 播报给读屏，不靠移动焦点。
  expect(within(progress).getByRole("list", { name: "提交结果" })).toHaveAttribute("aria-live", "polite");
});

// 弹窗高度只由目标数决定（admin.css 的 .update-results 读 --rows）：打开时就为每个目标留一行占位，结果到达只替换占位。
it("进度弹窗打开时就按目标数留好每一行，结果逐条到达只替换占位，行数与高度依据不变", async () => {
  const releases: Array<() => void> = [];
  render({ startUpdate: () => new Promise((resolve) => { releases.push(() => resolve({})); }) });
  await check();
  fireEvent.click(screen.getByRole("checkbox", { name: "选择 东京（#1）" }));
  fireEvent.click(screen.getByRole("checkbox", { name: "选择 西雅图（#3）" }));
  fireEvent.click(screen.getByRole("button", { name: "更新选中节点（2）" }));
  fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "确认更新" }));
  const list = within(await screen.findByRole("dialog", { name: "提交更新任务" })).getByRole("list", { name: "提交结果" });
  const rows = () => [...list.children].map((li) => li.textContent);
  await waitFor(() => expect(releases).toHaveLength(1));
  expect(rows()).toEqual(["东京：等待提交…", "西雅图：等待提交…"]);
  expect(list.style.getPropertyValue("--rows")).toBe("2");
  await act(async () => { releases[0](); });
  await waitFor(() => expect(rows()[0]).toMatch(/^东京：更新任务已提交/));
  expect(rows()).toHaveLength(2);
  expect(rows()[1]).toBe("西雅图：等待提交…");
  await waitFor(() => expect(releases).toHaveLength(2));
  await act(async () => { releases[1](); });
  await waitFor(() => expect(rows()[1]).toMatch(/^西雅图：更新任务已提交/));
  expect(rows()).toHaveLength(2);
  expect(list.style.getPropertyValue("--rows")).toBe("2");
});

it("显示每个节点取产物的来源", async () => {
  render();
  const tokyo = await screen.findByRole("row", { name: /东京/ });
  expect.soft(within(tokyo).getAllByRole("cell")[3]).toHaveTextContent("经 hub 中转");
  expect.soft(within(screen.getByRole("row", { name: /西雅图/ })).getAllByRole("cell")[3]).toHaveTextContent("GitHub 直连");
  // 精确匹配每个标签：页面说明文字也提到"经 hub 中转"，子串正则会把它多算一个。
  expect(screen.getAllByText("经 hub 中转")).toHaveLength(1);
  expect(screen.getAllByText("GitHub 直连")).toHaveLength(1);
});

it("版本列对低于绑定版本的节点标落后徽章，已到目标时不标", async () => {
  render({ getUpdates: async () => ({ latestVersion: "", boundAgentVersion: "v0.8.0", targets: [
    { nodeId: 1n, status: { supported: true, version: "v0.7.0" } },
    { nodeId: 2n, status: { supported: true, version: "v0.8.0" } },
  ] }) });
  const rows = await screen.findAllByRole("row");
  expect.soft(within(rows[1]).queryByText("低于 v0.8.0")).toHaveClass("badge-attention");
  expect.soft(within(rows[2]).queryByText(/^低于/)).not.toBeInTheDocument();
  expect.soft(within(screen.getByRole("region", { name: "Hub" })).getByText("绑定的 agent 版本").nextElementSibling).toHaveTextContent("v0.8.0");
});

it.each(["", "v0.8.0-rc.1"])("绑定非正式版时不标落后徽章（%s）", async (boundAgentVersion) => {
  render({ getUpdates: async () => ({ targets, boundAgentVersion }) });
  await screen.findByRole("row", { name: /东京/ });
  expect(screen.queryAllByText(/^低于/)).toHaveLength(0);
});


type TargetsInit = NonNullable<Exclude<MessageInitShape<typeof GetUpdatesResponseSchema>, GetUpdatesResponse>["targets"]>;

it("仅排队任务可取消；本页取消的任务，回读到的已取消终态显示出来", async () => {
  const cancelled: unknown[] = [];
  let current: TargetsInit = [
    { nodeId: 1n, status: { supported: true, version: "v0.2.0", task: { id: "queued-task", version: "v0.3.0", state: "queued" } } },
    { nodeId: 3n, status: { supported: true, version: "v0.2.0", task: { id: "rollback-task", version: "v0.3.0", state: "rolled_back", error: "new process timeout" } } },
  ];
  render({ getUpdates: async () => ({ targets: current, boundAgentVersion: "v0.3.0" }), cancelUpdate: async (req) => {
    cancelled.push({ nodeId: req.nodeId, id: req.id });
    current = current.map((t) => t.nodeId === 1n ? { ...t, status: { ...t.status, task: { id: "queued-task", version: "v0.3.0", state: "cancelled" } } } : t) as TargetsInit;
    return {};
  } });
  fireEvent.click(await screen.findByRole("button", { name: "取消排队" }));
  await waitFor(() => expect(cancelled).toEqual([{ nodeId: 1n, id: "queued-task" }]));
  // 取消成功后页面重取状态：本页取消的东京显示已取消；西雅图的回滚是别处留下的历史，仍不显示。
  expect(await within(screen.getByRole("row", { name: /东京/ })).findByText("已取消")).toBeInTheDocument();
  expect(screen.queryByText("已回滚")).toBeNull();
  expect(screen.queryByText("new process timeout")).toBeNull();
});

// 终态任务是 hub 保存的历史，不是页面状态：首次进入看到的只有进行中的任务与版本对比（Updates.tsx 的 visibleTask）。
it("首次进入时不显示任务终态，带终态任务的机器按版本对比陈述；进行中的任务照常显示", async () => {
  render({ getUpdates: async () => ({ targets: [
    { nodeId: 0n, status: { supported: true, version: "v0.3.0", task: { id: "hub-task", version: "v0.3.0", state: "succeeded" } } },
    { nodeId: 1n, status: { supported: true, version: "v0.3.0", task: { id: "done-task", version: "v0.3.0", state: "succeeded" } } },
    { nodeId: 2n, status: { supported: true, version: "v0.2.0", task: { id: "failed-task", version: "v0.3.0", state: "failed", error: "read official release: context deadline exceeded" } } },
    { nodeId: 3n, status: { supported: true, version: "v0.2.0", task: { id: "running-task", version: "v0.3.0", state: "downloading" } } },
  ], latestVersion: "", boundAgentVersion: "v0.3.0" }) });
  expect(within(await screen.findByRole("row", { name: /东京/ })).getByText("已是目标版本")).toBeInTheDocument();
  expect(within(screen.getByRole("row", { name: /香港/ })).getByText("可以在线更新")).toBeInTheDocument();
  expect(within(screen.getByRole("row", { name: /西雅图/ })).getByText("下载并校验")).toBeInTheDocument();
  expect(screen.queryByText("更新成功")).toBeNull();
  expect(screen.queryByText("更新失败")).toBeNull();
  expect(screen.queryByText(/context deadline exceeded/)).toBeNull();
  // 失败过的香港仍可再次发起：终态不占用资格。
  expect(screen.getByRole("checkbox", { name: "选择 香港（#2）" })).toBeEnabled();
});

it("本页发起的更新，回读到的终态显示出来；别处的终态仍不显示", async () => {
  let current: TargetsInit = [
    { nodeId: 1n, status: { supported: true, version: "v0.2.0" } },
    { nodeId: 3n, status: { supported: true, version: "v0.2.0", task: { id: "old-task", version: "v0.3.0", state: "failed", error: "old failure" } } },
  ];
  const { queryClient } = render({
    getUpdates: async (req) => ({ targets: current, latestVersion: req.checkLatest ? "v0.3.0" : "", boundAgentVersion: "v0.3.0" }),
    startUpdate: async (req) => {
      current = current.map((t) => t.nodeId === req.nodeId ? { ...t, status: { ...t.status, task: { id: "new-task", version: "v0.3.0", state: "succeeded" } } } : t) as TargetsInit;
      return {};
    },
  });
  await check();
  fireEvent.click(screen.getByRole("checkbox", { name: "选择 东京（#1）" }));
  fireEvent.click(screen.getByRole("button", { name: "更新选中节点（1）" }));
  fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "确认更新" }));
  await screen.findByText(/东京：更新任务已提交/);
  await act(async () => { await queryClient.refetchQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.getUpdates, cardinality: "finite" }) }); });
  expect(await within(screen.getByRole("row", { name: /东京/ })).findByText("更新成功")).toBeInTheDocument();
  expect(within(screen.getByRole("row", { name: /西雅图/ })).getByText("可以在线更新")).toBeInTheDocument();
  expect(screen.queryByText("更新失败")).toBeNull();
  expect(screen.queryByText("old failure")).toBeNull();
});
const selectAll = () => screen.getByRole("checkbox", { name: "选择全部可更新节点" });
const row = (name: string) => screen.getByRole("checkbox", { name: `选择 ${name}` });

it("全选只选行上可勾的节点：部分选中为半选，全选后再点清空", async () => {
  render();
  await screen.findByRole("button", { name: "更新 Hub" });
  // 节点按绑定版本即可选，不需要先检查官方新版本。
  expect(selectAll()).toBeEnabled();
  expect(row("东京（#1）")).toBeEnabled();
  expect(selectAll()).not.toBeChecked();
  fireEvent.click(row("东京（#1）"));
  expect(selectAll()).toHaveAttribute("aria-checked", "mixed");
  expect((selectAll() as HTMLInputElement).indeterminate).toBe(true);
  fireEvent.click(selectAll());
  expect(selectAll()).toBeChecked();
  expect(row("东京（#1）")).toBeChecked();
  expect(row("西雅图（#3）")).toBeChecked();
  // 香港不支持在线更新，行上不可勾，全选也不能选中它。
  expect(row("香港（#2）")).not.toBeChecked();
  expect(screen.getByRole("button", { name: "更新选中节点（2）" })).toBeEnabled();
  fireEvent.click(selectAll());
  expect(selectAll()).not.toBeChecked();
  expect(row("东京（#1）")).not.toBeChecked();
  expect(row("西雅图（#3）")).not.toBeChecked();
  expect(screen.getByRole("button", { name: "更新选中节点（0）" })).toBeDisabled();
});

it("全选后提交按节点列表顺序逐个下发", async () => {
  const ids: bigint[] = [];
  render({ startUpdate: async (req) => { ids.push(req.nodeId); return {}; } });
  await check();
  fireEvent.click(selectAll());
  fireEvent.click(screen.getByRole("button", { name: "更新选中节点（2）" }));
  fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "确认更新" }));
  await screen.findByText(/西雅图：更新任务已提交/);
  expect(ids).toEqual([1n, 3n]);
});

it("轮询使已选节点失去更新资格时，计数与全选状态一起收缩", async () => {
  let current: TargetsInit = targets;
  const { queryClient } = render({ getUpdates: async (req) => ({ targets: current, latestVersion: req.checkLatest ? "v0.3.0" : "", boundAgentVersion: "v0.3.0" }) });
  await check();
  fireEvent.click(selectAll());
  expect(screen.getByRole("button", { name: "更新选中节点（2）" })).toBeEnabled();
  // 西雅图在别处已开始更新：有进行中的任务即不再可选。
  current = targets.map((t) => t.nodeId === 3n ? { ...t, status: { ...t.status, task: { id: "running", version: "v0.3.0", state: "dispatched" } } } : t);
  await act(async () => { await queryClient.refetchQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.getUpdates, cardinality: "finite" }) }); });
  await waitFor(() => expect(row("西雅图（#3）")).toBeDisabled());
  expect(screen.getByRole("button", { name: "更新选中节点（1）" })).toBeEnabled();
  // 仍可更新的只剩东京且已选中，全选框是勾选而不是半选。
  expect(selectAll()).toBeChecked();
  expect(selectAll()).toHaveAttribute("aria-checked", "true");
});

it("targets the bound agent version without checking latest", async () => {
  const calls: unknown[] = [];
  render({
    getUpdates: async (req) => ({ targets: boundTargets, latestVersion: req.checkLatest ? "v0.5.6" : "", boundAgentVersion: "v0.5.4" }),
    startUpdate: async (req) => { calls.push({ nodeId: req.nodeId, version: req.version }); return {}; },
  });
  // 不点“检查官方新版本”：东京 v0.5.3 落后于绑定 v0.5.4 可勾，西雅图已在绑定版本不可勾。
  await screen.findByRole("button", { name: "更新 Hub" });
  expect(screen.getByRole("button", { name: "更新选中节点（0）" })).toBeDisabled();
  expect(row("东京（#1）")).toBeEnabled();
  expect(row("西雅图（#3）")).toBeDisabled();
  fireEvent.click(row("东京（#1）"));
  fireEvent.click(screen.getByRole("button", { name: "更新选中节点（1）" }));
  const modal = await screen.findByRole("dialog", { name: "确认更新节点" });
  expect(modal).toHaveTextContent("更新到 v0.5.4");
  fireEvent.click(within(modal).getByRole("button", { name: "确认更新" }));
  await screen.findByText(/东京：更新任务已提交/);
  expect(calls).toEqual([{ nodeId: 1n, version: "v0.5.4" }]);
});

it.each(["", "v0.5.4-rc.1"])("does not offer node updates without a stable binding (%s)", async (bound) => {
  render({ getUpdates: async (req) => ({ targets, latestVersion: req.checkLatest ? "v0.5.6" : "", boundAgentVersion: bound }) });
  await screen.findByRole("button", { name: "更新 Hub" });
  expect(row("东京（#1）")).toBeDisabled();
  expect(row("西雅图（#3）")).toBeDisabled();
  expect(selectAll()).toBeDisabled();
  expect(screen.getByText(/这个 hub 没有绑定正式的 agent 版本/)).toBeInTheDocument();
});

it("hub update still targets the latest official version", async () => {
  const calls: unknown[] = [];
  render({
    getUpdates: async (req) => ({ targets: boundTargets, latestVersion: req.checkLatest ? "v0.5.6" : "", boundAgentVersion: "v0.5.4" }),
    startUpdate: async (req) => { calls.push({ nodeId: req.nodeId, version: req.version }); return {}; },
  });
  await check("v0.5.6"); // 官方最新 v0.5.6
  expect(screen.getByRole("button", { name: "更新 Hub" })).toBeEnabled();
  fireEvent.click(screen.getByRole("button", { name: "更新 Hub" }));
  const modal = await screen.findByRole("dialog", { name: "确认更新 Hub" });
  expect(modal).toHaveTextContent("更新到 v0.5.6");
  fireEvent.click(within(modal).getByRole("button", { name: "确认更新" }));
  await screen.findByText(/Hub：更新任务已提交/);
  expect(calls).toEqual([{ nodeId: 0n, version: "v0.5.6" }]);
  // 节点目标仍是绑定版本 v0.5.4，不跟随官方最新。
  expect(row("东京（#1）")).toBeEnabled();
  expect(screen.getByText(/目标版本 v0\.5\.4（hub 绑定的 agent 版本）/)).toBeInTheDocument();
});

it("已在目标版本、没有任务的机器显示已是目标版本，而不是可以在线更新", async () => {
  let releaseCheck: () => void = () => {};
  render({ getUpdates: async (req) => {
    if (req.checkLatest) await new Promise<void>((resolve) => { releaseCheck = resolve; });
    return { targets: boundTargets, latestVersion: req.checkLatest ? "v0.5.5" : "", boundAgentVersion: "v0.5.4" };
  } });
  // 西雅图 v0.5.4 = 绑定版本；东京 v0.5.3 落后。
  expect(within(await screen.findByRole("row", { name: /西雅图/ })).getByText("已是目标版本")).toBeInTheDocument();
  expect(within(screen.getByRole("row", { name: /东京/ })).getByText("可以在线更新")).toBeInTheDocument();
  // 打开页面即自动检查官方最新版；应答未到时 hub 目标未知，只陈述能力。
  expect(screen.queryAllByText("已是目标版本")).toHaveLength(1);
  await act(async () => { releaseCheck(); });
  // 检查后 hub v0.5.5 已是最新，同样显示已是目标版本。
  await waitFor(() => expect(screen.getAllByText("已是目标版本")).toHaveLength(2));
});

it("已登记的不支持原因写中文，未登记的照写原文", async () => {
  render({ getUpdates: async () => ({ targets: [
    { nodeId: 0n, status: { supported: true, version: "v0.5.5" } },
    { nodeId: 1n, status: { supported: false, reason: "agent has not reported online update support" } },
    { nodeId: 2n, status: { supported: false, version: "v0.5.3", reason: "OpenRC is unsupported" } },
  ], latestVersion: "", boundAgentVersion: "v0.5.4" }) });
  const row = (name: string) => within(screen.getByRole("checkbox", { name: `选择 ${name}` }).closest("tr")!);
  await screen.findByRole("checkbox", { name: "选择 东京（#1）" });
  expect(row("东京（#1）").getByText("不支持在线更新：agent 尚未上报在线更新能力（版本过旧，或刚连上 hub）")).toBeInTheDocument();
  expect(row("东京（#1）").queryByText(/has not reported/)).toBeNull();
  expect(row("香港（#2）").getByText("OpenRC is unsupported").tagName).toBe("CODE");
});

it("没有节点时节点更新只有空态卡，不画只有表头的表", async () => {
  render({ listNodes: async () => ({ nodes: [] }) });
  await expectEmptyState("还没有节点。", { region: "节点更新" });
});

it("节点全部已出租时不画更新表，空态说明它们没有 agent", async () => {
  render({
    listNodes: async () => ({ nodes: [{ id: 1n, name: "已租", rented: true, facts: { agentVersion: "v0.1.0" } }] }),
    getUpdates: async () => ({ targets: [{ nodeId: 0n, status: { supported: true, version: "v0.5.5" } }, { nodeId: 1n, status: { supported: true, version: "v0.1.0" } }], boundAgentVersion: "v0.5.4" }),
  });
  await expectEmptyState("没有可更新的节点：已出租的节点没有 agent，不参与在线更新。", { region: "节点更新" });
  expect(screen.queryByRole("table")).toBeNull();
});

// 绑定 v0.3.0：离线的 a 可更新，b 已到目标，c 可更新，d 不支持，e 从未上报，f 正在更新。
const rankedNodes = [{ id: 1n, name: "a" }, { id: 2n, name: "b" }, { id: 3n, name: "c" }, { id: 4n, name: "d" }, { id: 5n, name: "e" }, { id: 6n, name: "f" }];
const rankedTargets = [
  { nodeId: 0n, status: { supported: true, version: "v0.3.0" } },
  { nodeId: 1n, status: { supported: true, version: "v0.2.0" } },
  { nodeId: 2n, status: { supported: true, version: "v0.3.0" } },
  { nodeId: 3n, status: { supported: true, version: "v0.2.0" } },
  { nodeId: 4n, status: { supported: false, version: "v0.2.0", reason: "OpenRC is unsupported" } },
  { nodeId: 6n, status: { supported: true, version: "v0.2.0", task: { id: "t", version: "v0.3.0", state: "dispatched" } } },
];
const rankedSnapshot = {
  now: 1000n,
  nodes: [
    { id: 1n, online: false, lastSeenAt: 100n }, { id: 2n, online: true, lastSeenAt: 999n }, { id: 3n, online: true, lastSeenAt: 999n },
    { id: 4n, online: true, lastSeenAt: 999n }, { id: 5n, online: false }, { id: 6n, online: true, lastSeenAt: 999n },
  ],
};
const tableOrder = () => within(screen.getByRole("region", { name: "节点更新" })).getAllByRole("row").slice(1).map((r) => r.querySelector('td[data-label="节点"]')!.firstChild!.textContent);

it("节点按组排序：可更新与正在更新在前，再是已是目标版本，再是其余，离线与从未上报最后；组内按节点列表顺序，组间不画分隔", async () => {
  const ids: bigint[] = [];
  render({
    getUpdates: async () => ({ targets: rankedTargets, latestVersion: "", boundAgentVersion: "v0.3.0" }),
    listNodes: async () => ({ nodes: rankedNodes }),
    getSnapshot: async () => rankedSnapshot,
    startUpdate: async (req) => { ids.push(req.nodeId); return {}; },
  });
  await waitFor(() => expect(tableOrder()).toEqual(["c", "f", "b", "d", "a", "e"]));
  expect(screen.getByRole("region", { name: "节点更新" }).querySelectorAll("tbody")).toHaveLength(1);
  // 全选与提交按表格顺序：可更新的 c 在前，离线但可更新（任务会排队等上线）的 a 在后。
  fireEvent.click(selectAll());
  fireEvent.click(screen.getByRole("button", { name: "更新选中节点（2）" }));
  fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "确认更新" }));
  await screen.findByText(/a：更新任务已提交/);
  expect(ids).toEqual([3n, 1n]);
});

it("快照不可用时不知道谁离线：只按更新状态分组，页面照常可用", async () => {
  render({
    getUpdates: async () => ({ targets: rankedTargets, latestVersion: "", boundAgentVersion: "v0.3.0" }),
    listNodes: async () => ({ nodes: rankedNodes }),
    getSnapshot: async () => { throw new ConnectError("snapshot down", Code.Unavailable); },
  });
  await waitFor(() => expect(tableOrder()).toEqual(["a", "c", "f", "b", "d", "e"]));
  expect(screen.queryByRole("alert")).toBeNull();
  expect(row("a（#1）")).toBeEnabled();
});

// hub v0.11.2 绑定 agent v0.11.2；官方最新 v0.12.1 是只发 hub 的 release，它的 AGENT_VERSION 资产写 v0.12.0——跨过了
// v0.12.0 那次完整发布，绑定会变。五个节点：东京落后于新绑定，大阪已在新绑定，首尔比新绑定还新，开发构建与从未上报
// facts 的两台版本未知，各自计数，不计入落后数。
const crossTargets = [{ nodeId: 0n, status: { supported: true, version: "v0.11.2" } }];
const crossNodes = [
  { id: 1n, name: "东京", facts: { agentVersion: "v0.11.2" } },
  { id: 2n, name: "大阪", facts: { agentVersion: "v0.12.0" } },
  { id: 3n, name: "首尔", facts: { agentVersion: "v0.12.1" } },
  { id: 4n, name: "开发机", facts: { agentVersion: "dev" } },
  { id: 5n, name: "新机", facts: undefined },
];
function renderHubCheck(latestBound: string, bound = "v0.11.2") {
  return render({
    getUpdates: async (req) => ({ targets: crossTargets, latestVersion: req.checkLatest ? "v0.12.1" : "", latestBoundAgentVersion: req.checkLatest ? latestBound : "", boundAgentVersion: bound }),
    listNodes: async () => ({ nodes: crossNodes }),
  });
}
async function openHubConfirmation() {
  await check("v0.12.1");
  fireEvent.click(screen.getByRole("button", { name: "更新 Hub" }));
  return screen.findByRole("dialog", { name: "确认更新 Hub" });
}
const commitment = "hub 只承诺支持绑定版本的 agent，更新后请尽快更新节点。";

it("目标 release 绑定的 agent 版本与当前相同：确认框与原来一样，不陈述绑定变化", async () => {
  renderHubCheck("v0.11.2");
  const modal = await openHubConfirmation();
  expect(modal).toHaveTextContent("更新到 v0.12.1");
  expect(within(modal).getByText(/短暂断连/)).toBeInTheDocument();
  expect(modal).not.toHaveTextContent("绑定");
  expect(modal).not.toHaveTextContent("落后");
  expect(modal).not.toHaveTextContent(commitment);
});

it("已出租节点不在更新表里，全选与 hub 更新确认框的落后数都不含它", async () => {
  render({
    getUpdates: async (req) => ({
      targets: [
        { nodeId: 0n, status: { supported: true, version: "v0.11.2" } },
        { nodeId: 1n, status: { supported: true, version: "v0.5.3" } },
        { nodeId: 9n, status: { supported: true, version: "v0.1.0" } },
      ],
      latestVersion: req.checkLatest ? "v0.12.1" : "",
      latestBoundAgentVersion: req.checkLatest ? "v0.12.0" : "",
      boundAgentVersion: "v0.5.4",
    }),
    listNodes: async () => ({
      nodes: [
        { id: 1n, name: "东京", facts: { agentVersion: "v0.5.3" } },
        { id: 9n, name: "已租", rented: true, facts: { agentVersion: "v0.1.0" } },
      ],
    }),
  });
  await screen.findByRole("row", { name: /东京/ });
  expect(screen.queryByRole("row", { name: /已租/ })).toBeNull();
  expect(screen.queryByRole("checkbox", { name: /已租/ })).toBeNull();
  await waitFor(() => expect(screen.getByRole("button", { name: "更新 Hub" })).toBeEnabled());
  fireEvent.click(screen.getByRole("checkbox", { name: "选择全部可更新节点" }));
  expect(screen.getByRole("checkbox", { name: "选择 东京（#1）" })).toBeChecked();
  expect(screen.getByRole("button", { name: "更新选中节点（1）" })).toBeEnabled();
  fireEvent.click(screen.getByRole("button", { name: "更新 Hub" }));
  const modal = await screen.findByRole("dialog", { name: "确认更新 Hub" });
  // 东京 v0.5.3 落后于新绑定 v0.12.0；已租 v0.1.0 更旧，但没有 agent，不进分子也不进分母。
  expect(modal).toHaveTextContent("更新后将落后的节点：1 / 1");
});

it("跨过完整发布直升只发 hub 的版本：陈述新绑定、落后数与版本未知数，开发构建与未上报分开计", async () => {
  renderHubCheck("v0.12.0");
  const modal = await openHubConfirmation();
  expect(modal).toHaveTextContent("更新后 hub 绑定的 agent 版本将从 v0.11.2 变为 v0.12.0。");
  expect(modal).toHaveTextContent("更新后将落后的节点：1 / 5");
  expect(modal).toHaveTextContent("版本未知的节点：2 / 5（未上报版本 1，版本号无法比较 1），不计入落后数。");
  expect(modal).toHaveTextContent(commitment);
});

it("读不到目标 release 绑定的 agent 版本：按会变化处理，不给出落后数", async () => {
  renderHubCheck("");
  const modal = await openHubConfirmation();
  expect(modal).toHaveTextContent("无法读到目标版本绑定的 agent 版本，按会变化处理；将落后的节点数无法计算。");
  expect(modal).toHaveTextContent(commitment);
  expect(modal).not.toHaveTextContent(/\d+ \/ \d+/);
});

it("确认框打开后，轮询与节点列表的新数据不改变它的内容", async () => {
  let nodes = crossNodes;
  const { queryClient } = render({
    getUpdates: async (req) => ({ targets: crossTargets, latestVersion: req.checkLatest ? "v0.12.1" : "", latestBoundAgentVersion: req.checkLatest ? "v0.12.0" : "", boundAgentVersion: "v0.11.2" }),
    listNodes: async () => ({ nodes }),
  });
  const modal = await openHubConfirmation();
  const before = modal.textContent;
  expect(before).toContain("更新后将落后的节点：1 / 5");
  // 打开之后所有节点都升到了 v0.12.0，并多了一台未上报的节点；轮询的 GetUpdates({}) 不带官方版本信息。
  nodes = [...crossNodes.map((n) => ({ ...n, facts: { agentVersion: "v0.12.0" } })), { id: 6n, name: "又一台", facts: undefined }];
  await act(async () => {
    await queryClient.refetchQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.getUpdates, cardinality: "finite" }) });
    await queryClient.refetchQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listNodes, cardinality: "finite" }) });
  });
  await waitFor(() => expect(screen.getByRole("row", { name: /又一台/ })).toBeInTheDocument());
  expect(modal.textContent).toBe(before);
});
