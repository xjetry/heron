import { describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { create } from "@bufbuild/protobuf";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { ConnectError, Code } from "@connectrpc/connect";
import { AdminService, ListNodesResponseSchema, ListProbeTasksResponseSchema, SaveProbeTaskResponseSchema, DeleteProbeTaskResponseSchema, type SaveProbeTaskRequest } from "../gen/heron/v1/admin_pb";
import { ProbeKind } from "../gen/heron/v1/types_pb";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { ProbeTasks } from "./ProbeTasks";
import { expectEmptyState } from "../test/empty";

const nodes = create(ListNodesResponseSchema, { nodes: [{ id: 1n, name: "东京" }, { id: 2n, name: "法兰克福" }] });
const tasks = create(ListProbeTasksResponseSchema, { version: 9n, tasks: [
  { task: { id: 3n, kind: ProbeKind.TCP, target: "1.1.1.1:443", intervalS: 30, timeoutMs: 1000 }, nodeIds: [1n, 2n] },
] });
async function openCreate() {
  fireEvent.click(await screen.findByRole("button", { name: "新建探测任务" }));
  return screen.getByRole("form", { name: "新建探测任务" });
}
async function openRowAction(label: string, action: string) {
  fireEvent.click(await screen.findByRole("button", { name: `更多操作 ${label}` }));
  fireEvent.click(screen.getByRole("menuitem", { name: `${action} ${label}` }));
}

const routes = [{ path: "/probes", Component: ProbeTasks }];

it("编辑抽屉打开时列表刷新把这条任务删掉：抽屉仍在，保存被 hub 拒绝时原文显示在抽屉内", async () => {
  let gone = false;
  const { queryClient } = renderWithAdmin({
    listNodes: async () => nodes,
    listProbeTasks: async () => ({ tasks: gone ? [] : tasks.tasks }),
    saveProbeTask: async () => { throw new ConnectError("task 3 not found", Code.NotFound); },
  }, routes, "/probes");
  await openRowAction("1.1.1.1:443（#3）", "编辑");
  const dialog = screen.getByRole("dialog");
  fireEvent.change(within(dialog).getByLabelText("目标"), { target: { value: "draft:443" } });
  gone = true;
  await act(async () => { await queryClient.invalidateQueries(); });
  await screen.findByText("还没有探测任务。");
  expect(screen.getByRole("dialog")).toBe(dialog);
  expect(within(dialog).getByLabelText("目标")).toHaveValue("draft:443");
  fireEvent.click(within(dialog).getByRole("button", { name: "保存" }));
  expect(await within(dialog).findByRole("alert", {}, { timeout: 1000 })).toHaveTextContent("task 3 not found");
});

it("页头、分配摘要与菜单保留完整导航和类型契约", async () => {
  renderWithAdmin({
    listNodes: async () => nodes,
    listProbeTasks: async () => ({ tasks: [{
      task: { id: 8n, kind: ProbeKind.HTTP, target: "https://example.com/" },
      selectorTags: ["prod", "edge"], nodeIds: [1n],
    }] }),
  }, routes, "/probes");
  await screen.findByRole("heading", { name: "探测任务", level: 1 });
  expect(screen.getAllByRole("columnheader").map((cell) => cell.textContent)).toEqual(["排序", "类型", "目标", "间隔", "超时", "分配", "操作"]);
  expect(screen.getByText("标签：prod ∩ edge（当前 1）")).toHaveAttribute("title", "东京");
  fireEvent.click(screen.getByRole("button", { name: "更多操作 https://example.com/（#8）" }));
  expect(screen.getAllByRole("menuitem").map((item) => [item.textContent, item.getAttribute("href")])).toEqual([
    ["编辑", null], ["对比", "/probes/8/compare"], ["证书", "/probes/8/certs"], ["删除", null],
  ]);
  fireEvent.click(screen.getByRole("button", { name: "更多操作 https://example.com/（#8）" }));
  const form = await openCreate();
  expect(within(within(form).getByRole("radiogroup", { name: "类型" })).getAllByRole("radio").map((radio) => radio.getAttribute("aria-label"))).toEqual(["ICMP", "TCP", "HTTP", "DNS"]);
});

it("连续排序串行保存完整排列，最终回读顺序保留到刷新之后", async () => {
  let current = create(ListProbeTasksResponseSchema, { tasks: [1n, 2n, 3n].map((id) => ({ task: { id, target: `task-${id}`, kind: ProbeKind.ICMP } })) });
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  const writes: bigint[][] = [];
  const { queryClient } = renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => current,
    reorderProbeTasks: async ({ ids }) => {
      writes.push(ids);
      if (writes.length === 1) await gate;
      current = create(ListProbeTasksResponseSchema, { tasks: ids.map((id) => current.tasks.find((d) => d.task?.id === id)!) });
      return {};
    },
  }, routes, "/probes");
  const order = () => screen.getAllByRole("button", { name: /^上移 task-/ }).map((b) => b.getAttribute("aria-label"));
  fireEvent.click(await screen.findByRole("button", { name: "上移 task-3（#3）" }));
  await waitFor(() => expect(writes).toEqual([[1n, 3n, 2n]]));
  fireEvent.click(screen.getByRole("button", { name: "上移 task-3（#3）" }));
  expect(order()).toEqual(["上移 task-3（#3）", "上移 task-1（#1）", "上移 task-2（#2）"]);
  await act(async () => { release(); });
  await waitFor(() => expect(writes).toEqual([[1n, 3n, 2n], [3n, 1n, 2n]]));
  await waitFor(() => expect(screen.queryByText("正在保存并确认排序…")).toBeNull());
  await act(async () => { await queryClient.refetchQueries(); });
  expect(order()).toEqual(["上移 task-3（#3）", "上移 task-1（#1）", "上移 task-2（#2）"]);
});

it("探测任务刷新失败保留同一编辑表单与草稿", async () => {
  let fail = false;
  const { queryClient } = renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => {
    if (fail) throw new ConnectError("tasks refresh failed", Code.Unavailable);
    return tasks;
  } }, routes, "/probes");
  await openRowAction("1.1.1.1:443（#3）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 1.1.1.1:443（#3）" });
  fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "draft:443" } });
  fail = true;
  await act(async () => { await queryClient.refetchQueries(); });
  expect(await screen.findByRole("alert")).toHaveTextContent("tasks refresh failed");
  expect(screen.getByRole("form", { name: "编辑 1.1.1.1:443（#3）" })).toBe(form);
  expect(within(form).getByLabelText("目标")).toHaveValue("draft:443");
});

it("任务表格提供可聚焦滚动区域和操作列表头", async () => {
  renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => tasks }, routes, "/probes");
  const region = await screen.findByRole("region", { name: "探测任务管理" });
  expect(region).toHaveAttribute("tabindex", "0");
  expect(within(region).getByRole("columnheader", { name: "操作" })).toBeInTheDocument();
});

it("节点列表挂起时不渲染表单与任务表格", async () => {
  const { queryClient } = renderWithAdmin({
    listNodes: () => new Promise(() => {}),
    listProbeTasks: async () => tasks,
  }, routes, "/probes");
  const key = createConnectQueryKey({ schema: AdminService.method.listProbeTasks, cardinality: "finite" });
  await waitFor(() => expect(queryClient.getQueriesData({ queryKey: key })[0]?.[1]).toEqual(tasks));
  expect({
    loading: screen.queryByText("加载中…") !== null,
    forms: screen.queryAllByRole("form").length,
    rows: screen.queryAllByRole("row").length,
  }).toEqual({ loading: true, forms: 0, rows: 0 });
});

it("任务列表挂起时不渲染新建表单", async () => {
  const { queryClient } = renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: () => new Promise(() => {}) }, routes, "/probes");
  const key = createConnectQueryKey({ schema: AdminService.method.listNodes, cardinality: "finite" });
  await waitFor(() => expect(queryClient.getQueriesData({ queryKey: key })[0]?.[1]).toEqual(nodes));
  expect({
    loading: screen.queryByText("加载中…") !== null,
    forms: screen.queryAllByRole("form").length,
  }).toEqual({ loading: true, forms: 0 });
});

it("四种类型在选项与任务列表使用一致标签", async () => {
  renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => ({ tasks: [
    ...tasks.tasks,
    { task: { id: 4n, kind: ProbeKind.ICMP, target: "host" } },
    { task: { id: 5n, kind: ProbeKind.HTTP, target: "https://example.com/" } },
    { task: { id: 6n, kind: ProbeKind.DNS, target: "example.com", dnsServer: "1.1.1.1:53" } },
  ] }) }, routes, "/probes");
  await screen.findByText("1.1.1.1:443");
  const form = await openCreate();
  for (const label of ["ICMP", "TCP", "HTTP", "DNS"]) {
    expect(within(form).getByRole("radio", { name: label })).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: label })).toBeInTheDocument();
  }
});

it("目标约束与解析器输入随类型切换，dns_server 只对 DNS 任务提交", async () => {
  const saved: SaveProbeTaskRequest[] = [];
  renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => tasks,
    saveProbeTask: async (req) => { saved.push(req); return {}; },
  }, routes, "/probes");
  const form = await openCreate();
  // ICMP 默认：253，无解析器输入。
  expect(within(form).getByLabelText("目标")).toHaveAttribute("maxlength", "253");
  expect(within(form).queryByLabelText("解析器")).toBeNull();
  // HTTP：512，无解析器输入。
  fireEvent.click(within(form).getByRole("radio", { name: "HTTP" }));
  expect(within(form).getByLabelText("目标")).toHaveAttribute("maxlength", "512");
  expect(within(form).queryByLabelText("解析器")).toBeNull();
  // TCP：host:port 提示，无解析器输入。
  fireEvent.click(within(form).getByRole("radio", { name: "TCP" }));
  expect(within(form).getByLabelText("目标")).toHaveAttribute("placeholder", "host:port");
  expect(within(form).queryByLabelText("解析器")).toBeNull();
  // DNS：253 且出现解析器输入；提交带上 dns_server（首尾空白裁掉）。
  fireEvent.click(within(form).getByRole("radio", { name: "DNS" }));
  expect(within(form).getByLabelText("目标")).toHaveAttribute("maxlength", "253");
  fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "example.com" } });
  fireEvent.change(within(form).getByLabelText("解析器"), { target: { value: " [2001:4860:4860::8888]:53 " } });
  fireEvent.submit(form);
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].task).toMatchObject({ kind: ProbeKind.DNS, target: "example.com", dnsServer: "[2001:4860:4860::8888]:53" });
  // 非 DNS 任务不携带 dns_server（草稿残留也不提交）。
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull(), { timeout: 1000 });
  const next = await openCreate();
  fireEvent.click(within(next).getByRole("radio", { name: "DNS" }));
  fireEvent.change(within(next).getByLabelText("解析器"), { target: { value: "1.1.1.1:53" } });
  fireEvent.click(within(next).getByRole("radio", { name: "HTTP" }));
  fireEvent.change(within(next).getByLabelText("目标"), { target: { value: "https://example.com/" } });
  fireEvent.submit(next);
  await waitFor(() => expect(saved).toHaveLength(2));
  expect(saved[1].task?.dnsServer).toBe("");
});

it("创建成功后复位全部字段，下一次提交不沿用旧值", async () => {
  const saved: SaveProbeTaskRequest[] = [];
  renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => tasks,
    saveProbeTask: async (req) => { saved.push(req); return {}; },
  }, routes, "/probes");
  await screen.findByText("2 个指定节点");
  const form = await openCreate();
  fireEvent.click(within(form).getByRole("radio", { name: "TCP" }));
  fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "old:80" } });
  fireEvent.change(within(form).getByLabelText("间隔 (s)"), { target: { value: "10" } });
  fireEvent.change(within(form).getByLabelText("超时 (ms)"), { target: { value: "500" } });
  fireEvent.click(within(form).getByLabelText("东京（#1）"));
  fireEvent.submit(form);
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull(), { timeout: 1000 });
  const next = await openCreate();
  expect(within(next).getByRole("radio", { name: "ICMP" })).toBeChecked();
  expect(within(next).getByLabelText("间隔 (s)")).toHaveValue(60);
  expect(within(next).getByLabelText("超时 (ms)")).toHaveValue(1000);
  expect(within(next).getByLabelText("东京（#1）")).not.toBeChecked();
  fireEvent.change(within(next).getByLabelText("目标"), { target: { value: "new.example" } });
  fireEvent.submit(next);
  await waitFor(() => expect(saved).toHaveLength(2));
  expect(saved[1]).toMatchObject({ task: { id: 0n, kind: ProbeKind.ICMP, target: "new.example", intervalS: 60, timeoutMs: 1000 }, nodeIds: [] });
});

it("缺失 task 的条目不变成可编辑或可删除的任务", async () => {
  renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => ({ tasks: [{ nodeIds: [1n] }, ...tasks.tasks] }) }, routes, "/probes");
  expect(await screen.findByText("1.1.1.1:443")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "更多操作 1.1.1.1:443（#3）" }));
  expect({
    edits: screen.getAllByRole("menuitem", { name: /^编辑 / }).length,
    deletes: screen.getAllByRole("menuitem", { name: /^删除 / }).length,
  }).toEqual({ edits: 1, deletes: 1 });
});

it("节点查询失败可见", async () => {
  renderWithAdmin({ listNodes: async () => { throw new ConnectError("nodes unavailable", Code.Unavailable); }, listProbeTasks: async () => tasks }, routes, "/probes");
  expect(await screen.findByRole("alert")).toHaveTextContent(/^nodes unavailable$/);
  expect({
    forms: screen.queryAllByRole("form").length,
    rows: screen.queryAllByRole("row").length,
  }).toEqual({ forms: 0, rows: 0 });
});

it("提交剔除编辑期间从节点列表消失的分配", async () => {
  let current = nodes;
  const save = vi.fn<NonNullable<AdminImpl["saveProbeTask"]>>(async () => ({}));
  const { queryClient } = renderWithAdmin({ listNodes: async () => current, listProbeTasks: async () => tasks, saveProbeTask: save }, routes, "/probes");
  await screen.findByText("2 个指定节点");
  await openRowAction("1.1.1.1:443（#3）", "编辑");
  current = create(ListNodesResponseSchema, { nodes: [nodes.nodes[0]] });
  await act(async () => { await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listNodes, cardinality: "finite" }) }); });
  await waitFor(() => expect(within(screen.getByRole("form", { name: "编辑 1.1.1.1:443（#3）" })).queryByLabelText("法兰克福")).toBeNull());
  fireEvent.submit(screen.getByRole("form", { name: "编辑 1.1.1.1:443（#3）" }));
  await waitFor(() => expect(save).toHaveBeenCalled());
  expect(save.mock.calls[0][0].nodeIds).toEqual([1n]);
});

it("保存挂起时抽屉不可关闭，刷新完成才关闭抽屉", async () => {
  let releaseSave!: () => void;
  let releaseList!: () => void;
  const saveGate = new Promise<void>((r) => { releaseSave = r; });
  const listGate = new Promise<void>((r) => { releaseList = r; });
  const entries = [...tasks.tasks, create(ListProbeTasksResponseSchema, { tasks: [{ task: { id: 4n, kind: ProbeKind.ICMP, target: "b", intervalS: 60, timeoutMs: 1000 } }] }).tasks[0]];
  const listProbeTasks = vi.fn(async () => { if (listProbeTasks.mock.calls.length > 1) await listGate; return { tasks: entries }; });
  const save = vi.fn(async () => { await saveGate; entries[0] = { ...entries[0], task: { ...entries[0].task!, target: "changed:80" } }; return {}; });
  renderWithAdmin({ listNodes: async () => nodes, listProbeTasks, saveProbeTask: save }, routes, "/probes");
  await screen.findByText("1.1.1.1:443");
  await openRowAction("1.1.1.1:443（#3）", "编辑");
  const a = screen.getByRole("form", { name: "编辑 1.1.1.1:443（#3）" });
  fireEvent.change(within(a).getByLabelText("目标"), { target: { value: "changed:80" } });
  fireEvent.submit(a);
  try {
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    expect(screen.getByRole("button", { name: "关闭抽屉" })).toBeDisabled();
    expect(within(a).getByRole("button", { name: "取消" })).toBeDisabled();
    vi.useFakeTimers();
    await act(async () => { releaseSave(); await vi.runAllTimersAsync(); });
    expect(listProbeTasks).toHaveBeenCalledTimes(2);
    expect(a).toBeInTheDocument();
  } finally { vi.useRealTimers(); await act(async () => { releaseSave(); releaseList(); }); }
  expect(await screen.findByRole("cell", { name: "changed:80" })).toBeInTheDocument();
  expect(screen.queryByRole("form", { name: "编辑 1.1.1.1:443（#3）" })).toBeNull();
  expect(screen.queryByRole("dialog")).toBeNull();
});

it("同名任务经菜单打开第二条，保存的是第二条的 id", async () => {
  const same = create(ListProbeTasksResponseSchema, { version: 9n, tasks: [
    { task: { id: 7n, kind: ProbeKind.TCP, target: "same.example", intervalS: 30, timeoutMs: 1000 }, nodeIds: [1n] },
    { task: { id: 8n, kind: ProbeKind.TCP, target: "same.example", intervalS: 30, timeoutMs: 1000 }, nodeIds: [2n] },
  ] });
  const saved: SaveProbeTaskRequest[] = [];
  renderWithAdmin({
    listNodes: async () => nodes, listProbeTasks: async () => same,
    saveProbeTask: async (req) => { saved.push(req); return {}; },
  }, routes, "/probes");
  await screen.findAllByText("same.example");
  await openRowAction("same.example（#8）", "编辑");
  const second = screen.getByRole("form", { name: "编辑 same.example（#8）" });
  fireEvent.change(within(second).getByLabelText("目标"), { target: { value: "other.example" } });
  fireEvent.submit(second);
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].task).toMatchObject({ id: 8n, target: "other.example" });
});

it("最新操作清掉创建旧错误，编辑失败显示自己的正文", async () => {
  let rejectEdit = false;
  renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => tasks,
    saveProbeTask: async (req) => {
      if (req.task?.id === 0n) throw new ConnectError("create rejected", Code.InvalidArgument);
      if (rejectEdit) throw new ConnectError("edit rejected", Code.InvalidArgument);
      return {};
    },
  }, routes, "/probes");
  await screen.findByText("1.1.1.1:443");
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "x" } });
  fireEvent.submit(form);
  expect(await screen.findByRole("alert")).toHaveTextContent(/^create rejected$/);
  fireEvent.click(screen.getByRole("button", { name: "关闭抽屉" }));
  await openRowAction("1.1.1.1:443（#3）", "编辑");
  fireEvent.submit(screen.getByRole("form", { name: "编辑 1.1.1.1:443（#3）" }));
  await waitFor(() => expect(screen.queryByRole("form", { name: "编辑 1.1.1.1:443（#3）" })).toBeNull(), { timeout: 1000 });
  expect(screen.queryByRole("alert")).toBeNull();
  rejectEdit = true;
  await openRowAction("1.1.1.1:443（#3）", "编辑");
  fireEvent.submit(screen.getByRole("form", { name: "编辑 1.1.1.1:443（#3）" }));
  expect(await screen.findByRole("alert")).toHaveTextContent(/^edit rejected$/);
});


describe("ProbeTasks", () => {
  it("编辑保存挂起与失败保留草稿，成功后才退出", async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const saveProbeTask = vi.fn(async () => {
      await gate;
      if (saveProbeTask.mock.calls.length === 1) throw new ConnectError("task update rejected", Code.InvalidArgument);
      return create(SaveProbeTaskResponseSchema, { version: 10n });
    });
    renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => tasks, saveProbeTask }, routes, "/probes");
    await screen.findByText("1.1.1.1:443");
    await openRowAction("1.1.1.1:443（#3）", "编辑");
    const form = screen.getByRole("form", { name: "编辑 1.1.1.1:443（#3）" });
    fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "8.8.8.8:443" } });
    fireEvent.submit(form);
    try {
      await waitFor(() => expect(saveProbeTask).toHaveBeenCalledTimes(1));
      expect(screen.getByRole("form", { name: "编辑 1.1.1.1:443（#3）" })).toBeInTheDocument();
      expect(within(form).getByRole("button", { name: "保存" })).toBeDisabled();
    } finally { await act(async () => { release(); }); }
    expect(await screen.findByRole("alert")).toHaveTextContent(/^task update rejected$/);
    expect(within(screen.getByRole("form", { name: "编辑 1.1.1.1:443（#3）" })).getByLabelText("目标")).toHaveValue("8.8.8.8:443");
    fireEvent.submit(form);
    await waitFor(() => expect(saveProbeTask).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByRole("form", { name: "编辑 1.1.1.1:443（#3）" })).toBeNull());
  });

  it("没有任务时显示空状态", async () => {
    renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => create(ListProbeTasksResponseSchema, {}) }, routes, "/probes");
    expect(await screen.findByText("还没有探测任务。")).toBeInTheDocument();
  });
  it("列出任务与分配的节点名", async () => {
    renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => tasks }, routes, "/probes");
    expect(await screen.findByText("1.1.1.1:443")).toBeInTheDocument();
    expect(screen.getByText("2 个指定节点")).toHaveAttribute("title", "东京、法兰克福");
    expect(screen.getByRole("cell", { name: "TCP" })).toBeInTheDocument();
  });

  it("创建提交 id 0、完整字段与升序节点列表，并只失效任务列表", async () => {
    const saved: SaveProbeTaskRequest[] = [];
    const listNodes = vi.fn(async () => nodes);
    const listProbeTasks = vi.fn(async () => tasks);
    const { queryClient } = renderWithAdmin({
      listNodes, listProbeTasks,
      saveProbeTask: async (req) => { saved.push(req); return create(SaveProbeTaskResponseSchema, { version: 10n }); },
    }, routes, "/probes");
    await screen.findByText("2 个指定节点");
    const nodesKey = createConnectQueryKey({ schema: AdminService.method.listNodes, cardinality: "finite" });
    queryClient.setQueryData(nodesKey, nodes);
    const form = await openCreate();
    fireEvent.click(within(form).getByRole("radio", { name: "TCP" }));
    fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "1.1.1.1:80" } });
    fireEvent.change(within(form).getByLabelText("间隔 (s)"), { target: { value: "60" } });
    fireEvent.change(within(form).getByLabelText("超时 (ms)"), { target: { value: "800" } });
    fireEvent.click(within(form).getByLabelText("法兰克福（#2）"));
    fireEvent.click(within(form).getByLabelText("东京（#1）"));
    fireEvent.submit(form);
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(saved[0].task).toMatchObject({ id: 0n, kind: ProbeKind.TCP, target: "1.1.1.1:80", intervalS: 60, timeoutMs: 800 });
    expect(saved[0].nodeIds).toEqual([1n, 2n]);
    await waitFor(() => expect(listProbeTasks).toHaveBeenCalledTimes(2));
    expect(queryClient.getQueryState(nodesKey)?.isInvalidated).toBe(false);
    expect(listNodes).toHaveBeenCalledTimes(1);
  });

  it("原生约束不满足时不发请求", async () => {
    const saved: SaveProbeTaskRequest[] = [];
    renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => tasks,
      saveProbeTask: async (req) => { saved.push(req); return create(SaveProbeTaskResponseSchema, {}); },
    }, routes, "/probes");
    await screen.findByText("1.1.1.1:443");
    const form = await openCreate();
    fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "8.8.8.8" } });
    fireEvent.change(within(form).getByLabelText("间隔 (s)"), { target: { value: "4" } });
    fireEvent.submit(form);
    // 同一表单修正后立即可提交；若越限的请求漏出，它经由同一路径排在这个请求之前。
    fireEvent.change(within(form).getByLabelText("间隔 (s)"), { target: { value: "60" } });
    fireEvent.submit(form);
    await waitFor(() => expect(saved.at(-1)?.task?.intervalS).toBe(60));
    expect(saved).toHaveLength(1);
  });

  it("编辑提交整个任务与当前分配", async () => {
    const saved: SaveProbeTaskRequest[] = [];
    renderWithAdmin({
      listNodes: async () => nodes, listProbeTasks: async () => tasks,
      saveProbeTask: async (req) => { saved.push(req); return create(SaveProbeTaskResponseSchema, { version: 10n }); },
    }, routes, "/probes");
    await screen.findByText("1.1.1.1:443");
    await openRowAction("1.1.1.1:443（#3）", "编辑");
    const form = screen.getByRole("form", { name: "编辑 1.1.1.1:443（#3）" });
    fireEvent.change(within(form).getByLabelText("超时 (ms)"), { target: { value: "2000" } });
    fireEvent.click(within(form).getByLabelText("法兰克福（#2）"));
    fireEvent.submit(form);
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(saved[0].task).toMatchObject({ id: 3n, kind: ProbeKind.TCP, target: "1.1.1.1:443", intervalS: 30, timeoutMs: 2000 });
    expect(saved[0].nodeIds).toEqual([1n]);
  });

  it("删除需要二次确认", async () => {
    const remove = vi.fn<NonNullable<AdminImpl["deleteProbeTask"]>>(async () => create(DeleteProbeTaskResponseSchema, { version: 11n }));
    renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => tasks, deleteProbeTask: remove }, routes, "/probes");
    await screen.findByText("1.1.1.1:443");
    await openRowAction("1.1.1.1:443（#3）", "删除");
    expect(remove).not.toHaveBeenCalled();
    expect(screen.getByText("历史随后由维护任务分块清除")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("menuitem", { name: "确认删除 1.1.1.1:443（#3）" }));
    await waitFor(() => expect(remove).toHaveBeenCalledTimes(1));
    expect(remove.mock.calls[0][0]).toMatchObject({ id: 3n });
  });

  it("编辑往返撤销已武装的删除确认", async () => {
    renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => tasks }, routes, "/probes");
    await screen.findByText("1.1.1.1:443");
    await openRowAction("1.1.1.1:443（#3）", "删除");
    expect(screen.getByRole("menuitem", { name: "确认删除 1.1.1.1:443（#3）" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("menuitem", { name: "编辑 1.1.1.1:443（#3）" }));
    fireEvent.click(screen.getByRole("button", { name: "取消" }));
    fireEvent.click(screen.getByRole("button", { name: "更多操作 1.1.1.1:443（#3）" }));
    expect(screen.getByRole("menuitem", { name: "删除 1.1.1.1:443（#3）" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "确认删除 1.1.1.1:443（#3）" })).toBeNull();
  });

  it("服务端错误原文可见", async () => {
    renderWithAdmin({
      listNodes: async () => nodes, listProbeTasks: async () => tasks,
      saveProbeTask: async () => { throw new ConnectError("task.target: target for a TCP task must be host:port; got \"x\"", Code.InvalidArgument); },
    }, routes, "/probes");
    await screen.findByText("1.1.1.1:443");
    const form = await openCreate();
    fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "x" } });
    fireEvent.change(within(form).getByLabelText("间隔 (s)"), { target: { value: "60" } });
    fireEvent.change(within(form).getByLabelText("超时 (ms)"), { target: { value: "800" } });
    fireEvent.submit(form);
    expect(await screen.findByRole("alert")).toHaveTextContent("task.target: target for a TCP task must be host:port");
  });
});

describe("全部节点作用域", () => {
  const allTask = { task: { id: 5n, kind: ProbeKind.ICMP, target: "all.example", intervalS: 60, timeoutMs: 1000 }, allNodes: true, nodeIds: [1n, 2n] };
  const emptyTask = { task: { id: 6n, kind: ProbeKind.ICMP, target: "none.example", intervalS: 60, timeoutMs: 1000 }, nodeIds: [] };

  it("列表显示 hub 展开的节点；显式空分配显示未分配而不是全部节点", async () => {
    renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => ({ tasks: [allTask, emptyTask] }) }, routes, "/probes");
    const all = within((await screen.findByText("all.example")).closest("tr")!);
    const none = within(screen.getByText("none.example").closest("tr")!);
    expect(all.getByText("全部节点")).toHaveAttribute("title", "东京、法兰克福");
    expect(none.getByRole("cell", { name: "未分配" })).toBeInTheDocument();
    expect(none.queryByText(/全部节点/)).toBeNull();
  });

  it("选择全部节点后隐藏节点多选，提交 allNodes 且不带分配", async () => {
    const saved: SaveProbeTaskRequest[] = [];
    renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => tasks,
      saveProbeTask: async (req) => { saved.push(req); return {}; },
    }, routes, "/probes");
    const form = await openCreate();
    fireEvent.click(within(form).getByLabelText("东京（#1）"));
    fireEvent.click(within(form).getByRole("radio", { name: "全部节点" }));
    expect(within(form).queryByLabelText("东京（#1）")).toBeNull();
    fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "all.example" } });
    fireEvent.submit(form);
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(saved[0]).toMatchObject({ task: { id: 0n, target: "all.example" }, allNodes: true, nodeIds: [] });
  });

  it("编辑全部节点任务时切换指定节点，以当前展开的节点作为显式分配提交", async () => {
    const saved: SaveProbeTaskRequest[] = [];
    renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => ({ tasks: [allTask] }),
      saveProbeTask: async (req) => { saved.push(req); return {}; },
    }, routes, "/probes");
    await openRowAction("all.example（#5）", "编辑");
    const form = screen.getByRole("form", { name: "编辑 all.example（#5）" });
    const toggle = within(form).getByRole("radio", { name: "全部节点" });
    expect(toggle).toBeChecked();
    fireEvent.click(within(form).getByRole("radio", { name: "指定节点" }));
    expect(within(form).getByLabelText("东京（#1）")).toBeChecked();
    fireEvent.click(within(form).getByLabelText("东京（#1）"));
    fireEvent.submit(form);
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(saved[0]).toMatchObject({ task: { id: 5n }, allNodes: false, nodeIds: [2n] });
  });
});

it("编辑带上配置身份，清除指纹是单独的动作", async () => {
  const config = Uint8Array.from({ length: 16 }, () => 4);
  const saved: SaveProbeTaskRequest[] = [];
  renderWithAdmin({
    listNodes: async () => nodes,
    listProbeTasks: async () => ({ tasks: [{ task: { id: 8n, kind: ProbeKind.HTTP, target: "https://example.com/", intervalS: 60, timeoutMs: 1000, certSpkiSha256: Uint8Array.from({ length: 32 }, () => 7), configId: config }, nodeIds: [1n] }] }),
    saveProbeTask: async (req) => { saved.push(req); return {}; },
  }, routes, "/probes");
  await openRowAction("https://example.com/（#8）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 https://example.com/（#8）" });
  expect(within(form).getByLabelText<HTMLInputElement>("证书指纹").value).toMatch(/^sha256\/\//);
  fireEvent.click(within(form).getByRole("button", { name: "清除指纹" }));
  fireEvent.submit(form);
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].expectedConfigId).toEqual(config);
  expect(saved[0].certPin?.action.case).toBe("clear");
  expect(saved[0].task?.certSpkiSha256 ?? new Uint8Array()).toHaveLength(0);
});

it("前置条件失败提示刷新且不自动再提交", async () => {
  let calls = 0;
  renderWithAdmin({
    listNodes: async () => nodes,
    listProbeTasks: async () => ({ tasks: [{ task: { id: 8n, kind: ProbeKind.HTTP, target: "https://example.com/", intervalS: 60, timeoutMs: 1000, configId: Uint8Array.from({ length: 16 }, () => 1) }, nodeIds: [1n] }] }),
    saveProbeTask: async () => { calls += 1; throw new ConnectError("expected_config_id does not match", Code.FailedPrecondition); },
  }, routes, "/probes");
  await openRowAction("https://example.com/（#8）", "编辑");
  fireEvent.submit(screen.getByRole("form", { name: "编辑 https://example.com/（#8）" }));
  expect(await screen.findByText(/请刷新后再试/)).toBeInTheDocument();
  expect(calls).toBe(1);
});

it("只有 https 的 HTTP 任务有证书链接", async () => {
  renderWithAdmin({
    listNodes: async () => nodes,
    listProbeTasks: async () => ({ tasks: [
      { task: { id: 3n, kind: ProbeKind.TCP, target: "1.1.1.1:443", intervalS: 30, timeoutMs: 1000 }, nodeIds: [1n] },
      { task: { id: 8n, kind: ProbeKind.HTTP, target: "https://example.com/", intervalS: 60, timeoutMs: 1000 }, nodeIds: [1n] },
      { task: { id: 9n, kind: ProbeKind.HTTP, target: "http://example.com/", intervalS: 60, timeoutMs: 1000 }, nodeIds: [1n] },
    ] }),
  }, routes, "/probes");
  for (const label of ["1.1.1.1:443（#3）", "http://example.com/（#9）"]) {
    fireEvent.click(await screen.findByRole("button", { name: `更多操作 ${label}` }));
    expect(screen.queryByRole("menuitem", { name: /^证书/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: `更多操作 ${label}` }));
  }
  fireEvent.click(screen.getByRole("button", { name: "更多操作 https://example.com/（#8）" }));
  expect(screen.getByRole("menuitem", { name: "证书 https://example.com/（#8）" })).toHaveAttribute("href", "/probes/8/certs");
  expect(screen.getAllByRole("menuitem", { name: /^证书/ })).toHaveLength(1);
});

it("改成不能钉的种类时仍显示指纹，且不会自动清除", async () => {
  const pin = Uint8Array.from({ length: 32 }, () => 7);
  const saved: SaveProbeTaskRequest[] = [];
  renderWithAdmin({
    listNodes: async () => nodes,
    listProbeTasks: async () => ({ tasks: [{ task: { id: 8n, kind: ProbeKind.HTTP, target: "https://example.com/", intervalS: 60, timeoutMs: 1000, certSpkiSha256: pin }, nodeIds: [1n] }] }),
    saveProbeTask: async (req) => { saved.push(req); return {}; },
  }, routes, "/probes");
  await openRowAction("https://example.com/（#8）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 https://example.com/（#8）" });
  fireEvent.click(within(form).getByRole("radio", { name: "ICMP" }));
  expect(within(form).getByLabelText("证书指纹")).toBeInTheDocument();
  expect(within(form).getByRole("button", { name: "清除指纹" })).toBeInTheDocument();
  expect(within(form).getByText(/不能钉指纹/)).toBeInTheDocument();
  expect(within(form).getByLabelText<HTMLInputElement>("证书指纹").value).toMatch(/^sha256\/\//);
  fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "192.0.2.1" } });
  fireEvent.submit(form);
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].certPin?.action.case).toBe("setSpkiSha256");
  expect(saved[0].task?.kind).toBe(ProbeKind.ICMP);
});

it("没有探测任务时只有空态卡，不画只有表头的表", async () => {
  renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => ({ tasks: [] }) }, routes, "/probes");
  await expectEmptyState("还没有探测任务。", { region: "探测任务管理" });
});
