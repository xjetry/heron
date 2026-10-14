import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { Code, ConnectError } from "@connectrpc/connect";
import { Outlet } from "react-router";
import { expect, it, vi } from "vitest";
import { renderWithAdmin } from "../test/harness";
import { QuickSearch } from "./QuickSearch";

const nodes = [{ id: 1n, name: "web-01", note: "customer" }, { id: 2n, name: "db-01", facts: { hostname: "db.internal" } }, { id: 3n, name: "edge" }];
const routes = [{ path: "/", element: <><QuickSearch /><Outlet /></>, children: [{ path: "nodes/:id", element: <p>详情</p> }] }];

it("⌘K 打开，输入过滤，方向键 + Enter 进入节点详情；打开前不请求节点列表", async () => {
  const listNodes = vi.fn(async () => ({ nodes }));
  const { router } = renderWithAdmin({ listNodes }, routes, "/");
  // 内存 transport 异步派发，请先让挂载产生的微任务完成，避免把尚未送达误判为未发请求。
  await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  expect(listNodes).not.toHaveBeenCalled();
  fireEvent.keyDown(window, { key: "k", metaKey: true });
  const dialog = await screen.findByRole("dialog", { name: "搜索节点" });
  const box = within(dialog).getByRole("searchbox", { name: "名称、IP、地区、备注或主机名" });
  expect(box).toHaveFocus();
  await waitFor(() => expect(listNodes).toHaveBeenCalledTimes(1));
  fireEvent.change(box, { target: { value: "db" } });
  await waitFor(() => expect(within(dialog).getAllByRole("option").map((o) => o.textContent)).toEqual(["db-01"]));
  fireEvent.change(box, { target: { value: "" } });
  await waitFor(() => expect(within(dialog).getAllByRole("option")).toHaveLength(3));
  fireEvent.keyDown(box, { key: "ArrowDown" });
  expect(within(dialog).getAllByRole("option")[1]).toHaveAttribute("aria-selected", "true");
  fireEvent.keyDown(box, { key: "Enter" });
  await waitFor(() => expect(router.state.location.pathname).toBe("/nodes/2"));
  expect(screen.queryByRole("dialog")).toBeNull();
});

it("点击顶栏按钮也能打开；没有匹配时说明；Escape 关闭并回焦", async () => {
  renderWithAdmin({ listNodes: async () => ({ nodes }) }, routes, "/");
  const trigger = screen.getByRole("button", { name: "搜索节点" });
  expect(trigger).toHaveClass("quick-search-trigger");
  expect(Array.from(trigger.querySelectorAll("kbd")).map((k) => k.textContent)).toEqual(["/", "⌘K"]);
  expect(trigger).toHaveTextContent("输入 / 搜索节点");
  expect(trigger).toHaveAttribute("aria-keyshortcuts", "/ Meta+K Control+K");
  fireEvent.click(trigger);
  const box = await screen.findByRole("searchbox");
  fireEvent.change(box, { target: { value: "nothing" } });
  expect(await screen.findByText("没有匹配的节点。")).toBeInTheDocument();
  fireEvent(screen.getByRole("dialog"), new Event("cancel", { cancelable: true }));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(trigger).toHaveFocus();
});

it("列表请求失败时在对话框内显示错误，输入框仍在", async () => {
  renderWithAdmin({ listNodes: async () => { throw new ConnectError("nodes down", Code.Unavailable); } }, routes, "/", { retry: false });
  fireEvent.click(screen.getByRole("button", { name: "搜索节点" }));
  const dialog = screen.getByRole("dialog", { name: "搜索节点" });
  expect(await within(dialog).findByRole("alert")).toHaveTextContent(/nodes down/);
  expect(within(dialog).getByRole("searchbox")).toBeInTheDocument();
});

it("Ctrl+K 打开后直接 Enter 进入第一条", async () => {
  const { router } = renderWithAdmin({ listNodes: async () => ({ nodes }) }, routes, "/");
  fireEvent.keyDown(window, { key: "k", ctrlKey: true });
  await screen.findAllByRole("option");
  fireEvent.keyDown(screen.getByRole("searchbox"), { key: "Enter" });
  await waitFor(() => expect(router.state.location.pathname).toBe("/nodes/1"));
  // 快捷键打开的对话框关闭后，焦点归还给顶栏按钮，而不是落到 body。
  expect(screen.getByRole("button", { name: "搜索节点" })).toHaveFocus();
});

it("只列前八条，方向键停在边界，重复快捷键保留查询与当前选中", async () => {
  renderWithAdmin({ listNodes: async () => ({ nodes: Array.from({ length: 10 }, (_, i) => ({ id: BigInt(i + 1), name: `node-${i + 1}` })) }) }, routes, "/");
  fireEvent.click(screen.getByRole("button", { name: "搜索节点" }));
  const list = await screen.findByRole("listbox", { name: "匹配的节点" });
  expect(within(list).getAllByRole("option").map((option) => option.textContent)).toEqual(Array.from({ length: 8 }, (_, i) => `node-${i + 1}`));
  const box = screen.getByRole("searchbox");
  fireEvent.keyDown(box, { key: "ArrowUp" });
  expect(within(list).getAllByRole("option")[0]).toHaveAttribute("aria-selected", "true");
  for (let i = 0; i < 10; i++) fireEvent.keyDown(box, { key: "ArrowDown" });
  expect(within(list).getAllByRole("option")[7]).toHaveAttribute("aria-selected", "true");
  fireEvent.keyDown(box, { key: "ArrowUp" });
  expect(within(list).getAllByRole("option")[6]).toHaveAttribute("aria-selected", "true");
  fireEvent.change(box, { target: { value: "node" } });
  expect(within(list).getAllByRole("option")[0]).toHaveAttribute("aria-selected", "true");
  fireEvent.keyDown(box, { key: "ArrowDown" });
  fireEvent.keyDown(window, { key: "K", metaKey: true });
  expect(box).toHaveValue("node");
  expect(within(list).getAllByRole("option")[1]).toHaveAttribute("aria-selected", "true");
  expect(screen.getAllByRole("dialog")).toHaveLength(1);
});

it("尚未到达或为空的结果不响应方向键与 Enter，结果到达后仍可直接进入第一条", async () => {
  let respond!: (value: { nodes: typeof nodes }) => void;
  const response = new Promise<{ nodes: typeof nodes }>((resolve) => { respond = resolve; });
  const { router } = renderWithAdmin({ listNodes: () => response }, routes, "/");
  fireEvent.click(screen.getByRole("button", { name: "搜索节点" }));
  const box = screen.getByRole("searchbox");
  fireEvent.keyDown(box, { key: "ArrowDown" });
  fireEvent.keyDown(box, { key: "Enter" });
  expect(router.state.location.pathname).toBe("/");
  await act(async () => respond({ nodes }));
  await screen.findAllByRole("option");
  fireEvent.keyDown(box, { key: "Enter" });
  await waitFor(() => expect(router.state.location.pathname).toBe("/nodes/1"));
});

it("无匹配时 Enter 不导航，关闭重开清空查询并回到第一条", async () => {
  const { router } = renderWithAdmin({ listNodes: async () => ({ nodes }) }, routes, "/");
  const trigger = screen.getByRole("button", { name: "搜索节点" });
  fireEvent.click(trigger);
  await screen.findAllByRole("option");
  const box = screen.getByRole("searchbox");
  fireEvent.change(box, { target: { value: "nothing" } });
  fireEvent.keyDown(box, { key: "ArrowDown" });
  fireEvent.keyDown(box, { key: "ArrowUp" });
  fireEvent.keyDown(box, { key: "Enter" });
  expect(router.state.location.pathname).toBe("/");
  fireEvent(screen.getByRole("dialog"), new Event("cancel", { cancelable: true }));
  fireEvent.click(trigger);
  expect(screen.getByRole("searchbox")).toHaveValue("");
  await screen.findAllByRole("option");
  fireEvent.keyDown(screen.getByRole("searchbox"), { key: "Enter" });
  await waitFor(() => expect(router.state.location.pathname).toBe("/nodes/1"));
});

it("点击结果链接进入对应节点详情并关闭面板", async () => {
  const { router } = renderWithAdmin({ listNodes: async () => ({ nodes }) }, routes, "/");
  fireEvent.click(screen.getByRole("button", { name: "搜索节点" }));
  const link = await screen.findByRole("link", { name: "db-01" });
  expect(link).toHaveAttribute("href", "/admin/nodes/2");
  fireEvent.click(link);
  await waitFor(() => expect(router.state.location.pathname).toBe("/nodes/2"));
  expect(screen.queryByRole("dialog")).toBeNull();
});

it("/ 在焦点不在可编辑元素上时打开；在输入框里、带修饰键或输入法组字时不打开", async () => {
  renderWithAdmin({ listNodes: async () => ({ nodes }) }, [{ path: "/", element: <><QuickSearch /><input aria-label="别处的输入框" /></> }], "/");
  const other = screen.getByRole("textbox", { name: "别处的输入框" });
  for (const init of [{ target: other }, { target: window, ctrlKey: true }, { target: window, altKey: true }, { target: window, isComposing: true }]) {
    const { target, ...modifiers } = init;
    const event = new KeyboardEvent("keydown", { key: "/", bubbles: true, cancelable: true, ...modifiers });
    fireEvent(target, event);
    expect(event.defaultPrevented, JSON.stringify(modifiers)).toBe(false);
    expect(screen.queryByRole("dialog")).toBeNull();
  }
  const slash = new KeyboardEvent("keydown", { key: "/", bubbles: true, cancelable: true });
  fireEvent(document.body, slash);
  expect(slash.defaultPrevented).toBe(true);
  expect(within(await screen.findByRole("dialog", { name: "搜索节点" })).getByRole("searchbox")).toHaveFocus();
});

