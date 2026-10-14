import { afterEach, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { ListApiTokensResponseSchema, TokenPermission, type TokenGrant } from "../gen/heron/v1/admin_pb";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { ApiTokens } from "./ApiTokens";
import { expectEmptyState } from "../test/empty";

const tokens = create(ListApiTokensResponseSchema, { tokens: [
  { id: 1n, name: "ci", createdAt: 1_700_000_000n, lastUsedAt: 1_700_000_600n },
  { id: 2n, name: "laptop", createdAt: 1_700_000_000n },
] });
const routes = [{ path: "/tokens", Component: ApiTokens }];
const render = (impl: AdminImpl) => renderWithAdmin({ listApiTokens: async () => tokens, ...impl }, routes, "/tokens");

async function openCreate() {
  fireEvent.click(await screen.findByRole("button", { name: "新建 API token" }));
  return within(screen.getByRole("dialog")).getByRole("form", { name: "新建 API token" });
}

async function openRowAction(label: string, action: string) {
  fireEvent.click(await screen.findByRole("button", { name: `更多操作 ${label}` }));
  fireEvent.click(screen.getByRole("menuitem", { name: `${action} ${label}` }));
}

it("预授权只提交勾选的操作和指定节点", async () => {
  let grant: TokenGrant | undefined;
  render({
    listNodes: async () => ({ nodes: [{ id: 11n, name: "边缘节点" }] }),
    createApiToken: async (req) => { grant = req.grant; return { apiToken: { id: 3n, name: req.name }, token: "heron_at_new" }; },
  });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "writer" } });
  fireEvent.click(within(form).getByLabelText("监控配置"));
  fireEvent.click(within(form).getByLabelText("创建节点"));
  fireEvent.change(within(form).getByLabelText("节点范围"), { target: { value: "selected" } });
  fireEvent.click(await within(form).findByLabelText("边缘节点（#11）"));
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(grant).toBeDefined());
  expect(grant?.allNodes).toBe(false);
  expect(grant?.nodeIds).toEqual([11n]);
  expect(grant?.permissions).toEqual([TokenPermission.CONFIGURE, TokenPermission.CREATE]);
});

it("默认凭据仍为全站只读", async () => {
  let grant: TokenGrant | undefined;
  render({ createApiToken: async (req) => { grant = req.grant; return { apiToken: { id: 3n, name: req.name }, token: "heron_at_read" }; } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "reader" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(grant).toBeDefined());
  expect(grant?.allNodes).toBe(true);
  expect(grant?.permissions).toEqual([]);
  expect(grant?.nodeIds).toEqual([]);
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

it("恢复后同请求 ID 的不同身份回执保持独立", async () => {
  const errors = vi.spyOn(console, "error").mockImplementation(() => {});
  render({ listOperations: async () => ({ operations: [
    { id: "receipt-source", ownerId: 1n, requestId: "same-key", action: "create_node", resourceId: 1n, committedAt: 1n, afterJson: "source" },
    { id: "receipt-target", ownerId: 1n, requestId: "same-key", action: "create_node", resourceId: 1n, committedAt: 1n, afterJson: "target" },
  ] }) });
  await openRowAction("ci（#1）", "查看操作记录");
  expect(await screen.findByText("source")).toBeInTheDocument();
  expect(screen.getByText("target")).toBeInTheDocument();
  expect(errors.mock.calls.filter((call) => String(call[0]).includes("same key"))).toEqual([]);
});

it("列表区分用过与从未使用", async () => {
  render({});
  const laptop = (await screen.findByRole("cell", { name: "laptop" })).closest("tr")!;
  expect(within(laptop).getByText("从未使用")).toBeInTheDocument();
  const ci = screen.getByRole("cell", { name: "ci" }).closest("tr")!;
  expect(within(ci).queryByText("从未使用")).toBeNull();
});

it("创建后只显示一次明文并刷新列表", async () => {
  let lists = 0;
  const created: string[] = [];
  render({
    listApiTokens: async () => { lists++; return tokens; },
    createApiToken: async (req) => { created.push(req.name); return { apiToken: { id: 3n, name: req.name, createdAt: 1n }, token: "heron_at_abc" }; },
  });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "agent" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  expect(await within(screen.getByRole("dialog")).findByLabelText("API token agent（#3）")).toHaveTextContent("heron_at_abc");
  expect(created).toEqual(["agent"]);
  await waitFor(() => expect(lists).toBe(2));
  fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "完成" }));
  expect(screen.queryByRole("dialog")).toBeNull();
});

it("创建失败显示 hub 的错误原文", async () => {
  render({ createApiToken: async () => { throw new ConnectError("at most 100 API tokens may exist; delete an unused one first", Code.ResourceExhausted); } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "x" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  expect(await within(screen.getByRole("dialog")).findByRole("alert")).toHaveTextContent("at most 100 API tokens");
});

it("吊销需要确认，确认后按 id 删除", async () => {
  const deleted: bigint[] = [];
  render({ deleteApiToken: async (req) => { deleted.push(req.id); return {}; } });
  await openRowAction("laptop（#2）", "吊销");
  expect(deleted).toEqual([]);
  expect(screen.getByText("用它的请求立即失效")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("menuitem", { name: "确认吊销 laptop（#2）" }));
  await waitFor(() => expect(deleted).toEqual([2n]));
});

it("同名 token 的吊销按钮按 id 区分并删除正确行", async () => {
  const duplicateTokens = create(ListApiTokensResponseSchema, { tokens: [
    { id: 1n, name: "ci", createdAt: 1n },
    { id: 2n, name: "ci", createdAt: 2n },
  ] });
  const deleted: bigint[] = [];
  render({ listApiTokens: async () => duplicateTokens, deleteApiToken: async (req) => { deleted.push(req.id); return {}; } });
  await openRowAction("ci（#2）", "吊销");
  fireEvent.click(screen.getByRole("menuitem", { name: "确认吊销 ci（#2）" }));
  await waitFor(() => expect(deleted).toEqual([2n]));
});

it("关闭抽屉后明文不在页面上，再次打开是空白表单", async () => {
  render({
    listApiTokens: async () => create(ListApiTokensResponseSchema, { tokens: [
      { id: 3n, name: "agent", createdAt: 1n },
      { id: 4n, name: "other", createdAt: 1n },
    ] }),
    createApiToken: async (req) => ({ apiToken: { id: 3n, name: req.name, createdAt: 1n }, token: "heron_at_abc" }),
    deleteApiToken: async () => ({}),
  });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "agent" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  expect(await screen.findByLabelText("API token agent（#3）")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "关闭抽屉" }));
  await waitFor(() => expect(screen.queryByLabelText("API token agent（#3）")).toBeNull());
  const reopened = await openCreate();
  expect(within(reopened).getByLabelText("名称")).toHaveValue("");
});

it("说明技能文件的保存路径，以及 token 经环境变量交给 agent、不写进文件", async () => {
  render({});
  const integrations = within(await screen.findByRole("region", { name: "接入方式" }));
  expect(integrations.getByText(/~\/\.claude\/skills\/heron-hub\/SKILL\.md/)).toBeInTheDocument();
  expect(integrations.getByText(/token 不写进 SKILL\.md，由环境变量提供/)).toBeInTheDocument();
  expect(integrations.getByText(`HERON_HUB=${window.location.origin}`)).toBeInTheDocument();
  expect(integrations.getByText(`{"env": {"HERON_HUB": "${window.location.origin}", "HERON_TOKEN": "heron_at_…"}}`)).toBeInTheDocument();
  expect(integrations.queryByText(/入口卡片/)).toBeNull();
});

it("下载的 SKILL.md 就是 hub 下发的 guide", async () => {
  // waitFor 自己也靠定时器；时间要跟着真实时间走，否则点击永远等不到。
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const blobs: Blob[] = [];
  const revoke = vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  vi.spyOn(URL, "createObjectURL").mockImplementation((b) => { blobs.push(b as Blob); return "blob:skill"; });
  const clicked: HTMLAnchorElement[] = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) { clicked.push(this); });
  render({ getApiReference: async () => ({ guide: "---\nname: heron-hub\n---\n技能文件", files: [] }) });
  fireEvent.click(await screen.findByRole("button", { name: "下载 SKILL.md" }));
  await waitFor(() => expect(clicked).toHaveLength(1));
  expect(clicked[0].download).toBe("SKILL.md");
  // undici 的 Response 不认 jsdom 的 Blob，读出来是 "[object Blob]"；这个 jsdom 实现了 Blob.text。
  expect(await blobs[0].text()).toBe("---\nname: heron-hub\n---\n技能文件");
  expect(revoke).not.toHaveBeenCalled();
  await vi.advanceTimersByTimeAsync(60_000);
  expect(revoke).toHaveBeenCalledWith("blob:skill");
});

it("列表挂起时显示加载中", async () => {
  render({ listApiTokens: () => new Promise(() => {}) });
  expect(await screen.findByText("加载中…")).toBeInTheDocument();
});

it("复制的油猴脚本填好本站地址、不带 token", async () => {
  const writeText = vi.fn(async (_text: string) => {});
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  render({});
  fireEvent.click(await screen.findByRole("button", { name: "复制油猴脚本" }));
  await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
  const source = writeText.mock.calls[0][0];
  expect(source).toContain("==UserScript==");
  expect(source).toContain(`const BUILTIN_HUB = "${window.location.origin}";`);
  expect(source).toContain('const BUILTIN_TOKEN = "";');
  expect(source).not.toContain("__HERON_HUB__");
  expect(source).not.toContain("__HERON_TOKEN__");
});

it("创建带「创建节点」权限的 token 后可复制预填脚本", async () => {
  const writeText = vi.fn(async (_text: string) => {});
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  render({ createApiToken: async (req) => ({ apiToken: { id: 3n, name: req.name }, token: "heron_at_secret" }) });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "tamper" } });
  fireEvent.click(within(form).getByLabelText("创建节点"));
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  fireEvent.click(await screen.findByRole("button", { name: "复制油猴脚本（已填入此 token）" }));
  await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
  expect(writeText.mock.calls[0][0]).toContain('const BUILTIN_TOKEN = "heron_at_secret";');
});

it("没有「创建节点」权限时不给出预填按钮", async () => {
  render({ createApiToken: async (req) => ({ apiToken: { id: 3n, name: req.name }, token: "heron_at_read" }) });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "reader" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  expect(await screen.findByLabelText("API token reader（#3）")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "复制油猴脚本（已填入此 token）" })).toBeNull();
});

it("剪贴板被拒时退回手动复制弹窗", async () => {
  vi.stubGlobal("navigator", { clipboard: { writeText: async () => { throw new Error("denied"); } } });
  render({});
  fireEvent.click(await screen.findByRole("button", { name: "复制油猴脚本" }));
  const box = await screen.findByRole("textbox", { name: "油猴脚本源码" });
  expect((box as HTMLTextAreaElement).value).toContain("==UserScript==");
  // 关闭后焦点还给打开它的按钮。
  fireEvent.click(screen.getByRole("button", { name: "关闭弹窗" }));
  expect(screen.queryByRole("textbox", { name: "油猴脚本源码" })).toBeNull();
  expect(screen.getByRole("button", { name: "复制油猴脚本" })).toHaveFocus();
});

it("没有 token 时不画只有表头的空表，空态与接入方式各自成块", async () => {
  render({ listApiTokens: async () => ({ tokens: [] }) });
  await expectEmptyState("还没有 API token。", { region: "API token 管理" });
  expect(screen.queryByRole("columnheader")).toBeNull();
  const integrations = screen.getByRole("region", { name: "接入方式" });
  expect(within(integrations).getByRole("button", { name: "下载 SKILL.md" })).toBeInTheDocument();
  expect(within(integrations).getByRole("button", { name: "复制油猴脚本" })).toBeInTheDocument();
});
