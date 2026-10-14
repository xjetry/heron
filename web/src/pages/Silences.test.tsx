import { expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { ListNodesResponseSchema, ListSilencesResponseSchema, SilenceKind, type SaveSilenceRequest } from "../gen/heron/v1/admin_pb";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { Silences } from "./Silences";
import { fillSegments, segmentsValue } from "../test/fields";
import { expectEmptyState } from "../test/empty";
import { stringContaining } from "../test/matchers";

const nodes = create(ListNodesResponseSchema, { nodes: [{ id: 1n, name: "东京" }, { id: 2n, name: "法兰克福" }] });
const silences = create(ListSilencesResponseSchema, { silences: [
  { silence: { id: 7n, name: "夜间维护", enabled: true, allNodes: true, kind: SilenceKind.DAILY, startHhmm: "22:00", endHhmm: "06:00", reason: "机房巡检", createdAt: 100n }, active: true },
  { silence: { id: 8n, name: "升级窗口", enabled: true, kind: SilenceKind.ONCE, fromAt: 1760000000n, untilAt: 1760086400n, nodeIds: [1n], createdAt: 100n }, active: false },
  { silence: { id: 9n, name: "停用的", enabled: false, allNodes: true, kind: SilenceKind.DAILY, startHhmm: "00:00", endHhmm: "23:59", createdAt: 100n }, active: true },
] });
const routes = [{ path: "/silences", Component: Silences }];
const base: AdminImpl = { listNodes: async () => nodes, listSilences: async () => silences };
const render = (impl: AdminImpl = {}) => renderWithAdmin({ ...base, ...impl }, routes, "/silences");

async function openCreate() {
  fireEvent.click(await screen.findByRole("button", { name: "新建维护静默" }));
  return within(screen.getByRole("dialog")).getByRole("form", { name: "新建维护静默" });
}

async function openRowAction(label: string, action: string) {
  fireEvent.click(await screen.findByRole("button", { name: `更多操作 ${label}` }));
  fireEvent.click(screen.getByRole("menuitem", { name: `${action} ${label}` }));
}

it("列出窗口、作用域与生效状态", async () => {
  render();
  const daily = within((await screen.findByRole("cell", { name: /夜间维护/ })).closest("tr")!);
  expect(daily.getByRole("cell", { name: "每日 22:00–06:00（hub 时区）" })).toBeInTheDocument();
  expect(daily.getByRole("cell", { name: "全部节点" })).toBeInTheDocument();
  expect(daily.getByRole("cell", { name: "生效中" })).toBeInTheDocument();
  expect(daily.getByText("机房巡检")).toBeInTheDocument();
  const once = within(screen.getByRole("cell", { name: "升级窗口" }).closest("tr")!);
  expect(once.getByRole("cell", { name: "1 个指定节点" })).toBeInTheDocument();
  expect(once.getByRole("cell", { name: "窗口外" })).toBeInTheDocument();
  const disabled = within(screen.getByRole("cell", { name: "停用的" }).closest("tr")!);
  expect(disabled.getByRole("cell", { name: "已停用" })).toBeInTheDocument();
});

it("每日静默只提交窗口字段，一次性静默只提交起止时间", async () => {
  const saved: SaveSilenceRequest[] = [];
  render({ saveSilence: async (req) => { saved.push(req); return {}; } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "夜间维护" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].silence).toMatchObject({
    name: "夜间维护", enabled: true, allNodes: true, nodeIds: [], selectorTags: [],
    kind: SilenceKind.DAILY, startHhmm: "22:00", endHhmm: "06:00", fromAt: 0n, untilAt: 0n,
  });
  // 创建成功后抽屉关闭，重新打开时使用空草稿。
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  const again = await openCreate();
  fireEvent.change(within(again).getByLabelText("名称"), { target: { value: "升级窗口" } });
  fireEvent.change(within(again).getByLabelText("类型"), { target: { value: String(SilenceKind.ONCE) } });
  fillSegments("开始", "2026-10-04T22:00", within(again));
  fillSegments("结束", "2026-10-05T06:00", within(again));
  fireEvent.click(within(again).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(saved).toHaveLength(2));
  expect(saved[1].silence).toMatchObject({
    kind: SilenceKind.ONCE, startHhmm: "", endHhmm: "",
    fromAt: BigInt(Math.floor(new Date("2026-10-04T22:00").getTime() / 1000)),
    untilAt: BigInt(Math.floor(new Date("2026-10-05T06:00").getTime() / 1000)),
  });
});

it("编辑保留完整载荷，删除两段确认后发出", async () => {
  const saved: SaveSilenceRequest[] = [];
  const deleted: bigint[] = [];
  render({
    saveSilence: async (req) => { saved.push(req); return {}; },
    deleteSilence: async (req) => { deleted.push(req.id); return {}; },
  });
  await openRowAction("夜间维护（#7）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 夜间维护（#7）" });
  expect(within(form).getByLabelText("名称")).toHaveValue("夜间维护");
  expect(segmentsValue("每日开始", within(form))).toBe("22:00");
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "夜间巡检" } });
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  // created_at 由 hub 在保存时保留原值，面板不回发。
  expect(saved[0].silence).toEqual({ ...silences.silences[0].silence, name: "夜间巡检", createdAt: 0n });
  await waitFor(() => expect(screen.queryByRole("form", { name: "编辑 夜间维护（#7）" })).toBeNull());
  await openRowAction("升级窗口（#8）", "删除");
  expect(deleted).toHaveLength(0);
  fireEvent.click(screen.getByRole("menuitem", { name: "确认删除 升级窗口（#8）" }));
  await waitFor(() => expect(deleted).toEqual([8n]));
});

it("动态标签作用域展示交集与当前节点数", async () => {
  render({ listSilences: async () => create(ListSilencesResponseSchema, { silences: [
    { silence: { id: 1n, name: "数据库维护", enabled: true, kind: SilenceKind.DAILY, startHhmm: "01:00", endHhmm: "02:00", selectorTags: ["db", "west"], nodeIds: [2n], createdAt: 100n }, active: false },
  ] }) });
  const row = within((await screen.findByRole("cell", { name: "数据库维护" })).closest("tr")!);
  expect(row.getByRole("cell", { name: "标签：db ∩ west（当前 1）" })).toBeInTheDocument();
});

// 页面的时间输入用本地墙钟，hub 按 Unix 秒判定；刷新不重置正在编辑的草稿。
it("编辑中的草稿不被周期刷新覆盖", async () => {
  vi.useFakeTimers();
  try {
    render();
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    fireEvent.click(screen.getByRole("button", { name: "更多操作 升级窗口（#8）" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "编辑 升级窗口（#8）" }));
    const form = screen.getByRole("form", { name: "编辑 升级窗口（#8）" });
    fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "尚未保存" } });
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(screen.getByRole("form", { name: "编辑 升级窗口（#8）" })).toBe(form);
    expect(within(form).getByLabelText("名称")).toHaveValue("尚未保存");
  } finally { vi.useRealTimers(); }
});

it("页头、五列表格与作用域单选使用统一契约", async () => {
  render();
  await screen.findByRole("heading", { name: "维护静默", level: 1 });
  const headers = screen.getAllByRole("columnheader").map((cell) => cell.textContent);
  const form = await openCreate();
  expect({
    headers,
    modes: within(form).getAllByRole("radio").map((radio) => radio.getAttribute("aria-label")),
    scope: within(form).getByRole("radiogroup", { name: "作用域方式" }).textContent,
  }).toEqual({
    headers: ["名称", "窗口", "作用域", "状态", "操作"],
    modes: ["全部节点", "动态标签选择器", "指定节点"],
    scope: stringContaining("只保存本次选中的节点，之后标签变化不会改变分配"),
  });
});

it("列表移除正在编辑的静默后保留草稿，保存失败原文留在抽屉", async () => {
  let removed = false;
  const { queryClient } = render({
    listSilences: async () => removed ? { silences: [] } : silences,
    saveSilence: async () => { throw new ConnectError("silence 8 not found", Code.NotFound); },
  });
  await openRowAction("升级窗口（#8）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 升级窗口（#8）" });
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "未保存的窗口" } });
  removed = true;
  await act(async () => { await queryClient.refetchQueries(); });
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  const dialog = screen.getByRole("dialog");
  const error = await within(dialog).findByRole("alert");
  expect({ form: within(dialog).getByRole("form"), name: within(form).getByLabelText<HTMLInputElement>("名称").value, error: error.textContent }).toEqual({ form, name: "未保存的窗口", error: "silence 8 not found" });
});

it("动态标签为空不发请求，选择标签后仍可保存", async () => {
  const saved: SaveSilenceRequest[] = [];
  render({ listTags: async () => ({ tags: [{ name: "prod", nodeCount: 1 }] }), saveSilence: async (req) => { saved.push(req); return {}; } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "标签维护" } });
  fireEvent.click(within(form).getByRole("radio", { name: "动态标签选择器" }));
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await act(async () => {});
  expect(saved).toHaveLength(0);
  fireEvent.click(within(form).getByRole("button", { name: /匹配标签/ }));
  fireEvent.click(await screen.findByRole("checkbox", { name: "prod" }));
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(saved.map((req) => req.silence?.selectorTags)).toEqual([["prod"]]));
});

it("没有维护静默时只有空态卡，不画只有表头的表", async () => {
  render({ listSilences: async () => ({ silences: [] }) });
  await expectEmptyState("还没有维护静默。", { region: "维护静默管理" });
});
