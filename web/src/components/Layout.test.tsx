import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { GetStorageStatsResponseSchema, WalFileObservationSchema } from "../gen/heron/v1/admin_pb";
import { renderWithAdmin } from "../test/harness";
import { Layout } from "./Layout";
import { router as appRouter } from "../App";
import heron from "../assets/heron.svg";

vi.mock("./Chart", () => ({ Chart: () => null }));

const routes = [{ path: "/", Component: Layout }, { path: "/login", element: <h1>login</h1> }];

it("明暗切换保存在本机并在离开布局时恢复原页面配色", async () => {
  document.documentElement.dataset.theme = "light";
  const { router } = renderWithAdmin({}, routes, "/");
  try {
    const toggle = screen.getByRole("button", { name: "明暗切换" });
    fireEvent.click(toggle);
    fireEvent.click(toggle);
    expect(document.documentElement.dataset.theme).toBe("dark");
    expect(localStorage.getItem("heron-admin-scheme")).toBe("dark");
    await act(() => router.navigate("/login"));
    expect(document.documentElement.dataset.theme).toBe("light");
  } finally { localStorage.removeItem("heron-admin-scheme"); delete document.documentElement.dataset.theme; }
});

it("顶栏提供面包屑、公开页链接、GitHub 仓库、明暗切换与登出，侧栏没有页脚", () => {
  renderWithAdmin({}, [{ path: "/", Component: Layout, children: [{ index: true, element: <h1>home</h1> }] }], "/");
  const topbar = screen.getByRole("banner");
  expect(within(topbar).getByRole("button", { name: "搜索节点" })).toBeInTheDocument();
  expect(within(topbar).getByRole("navigation", { name: "位置" })).toHaveTextContent("工作台/总览");
  expect(within(topbar).getByRole("link", { name: "公开页 ↗" })).toHaveAttribute("href", "/");
  const github = within(topbar).getByRole("link", { name: "GitHub 仓库" });
  expect(github).toHaveAttribute("href", "https://github.com/xjetry/heron");
  expect(github).toHaveAttribute("target", "_blank");
  expect(github).toHaveAttribute("rel", "noreferrer");
  expect(within(topbar).getByRole("button", { name: "明暗切换" })).toBeInTheDocument();
  expect(within(topbar).getByRole("button", { name: "登出" })).toBeInTheDocument();
  expect(screen.queryByText("管理工作台")).toBeNull();
  expect(screen.queryByText("基础设施监控")).toBeNull();
  expect(screen.getByRole("navigation", { name: "主导航" })).toHaveClass("panel-nav");
  expect(screen.queryByRole("combobox", { name: "后台配色" })).toBeNull();
});

it("移动导航用单一模态抽屉，导航后关闭且更新当前位置", async () => {
  renderWithAdmin({}, [{ path: "/", Component: Layout, children: [{ path: "nodes", element: <h1>node page</h1> }] }], "/");
  fireEvent.click(screen.getByRole("button", { name: "打开导航" }));
  const drawer = screen.getByRole("dialog", { name: "导航" });
  fireEvent.click(within(drawer).getByRole("link", { name: "节点" }));
  expect(await screen.findByRole("heading", { name: "node page" })).toBeInTheDocument();
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(screen.getByRole("button", { name: "打开导航" })).toHaveAttribute("aria-expanded", "false");
});

it("管理导航显示 Heron 字标与装饰性鹭鸟图标", () => {
  renderWithAdmin({}, routes, "/");
  const brand = screen.getByText("Heron");
  expect(brand.querySelector("img")).toHaveAttribute("alt", "");
  expect(brand.querySelector("img")).toHaveAttribute("src", heron);
});

it("安全导航进入应用的会话管理页", async () => {
  renderWithAdmin({ listNodes: async () => ({ nodes: [] }), listSessions: async () => ({ sessions: [] }) }, appRouter.routes, "/nodes");
  const link = screen.getByRole("link", { name: "安全" });
  fireEvent.click(link);
  expect(await screen.findByRole("heading", { name: "安全" })).toBeInTheDocument();
});

it("探测任务导航进入应用的任务页路由", async () => {
  renderWithAdmin({ listNodes: async () => ({ nodes: [] }), listProbeTasks: async () => ({ tasks: [] }) }, appRouter.routes, "/nodes");
  const link = screen.getByRole("link", { name: "探测任务" });
  expect(link).toHaveAttribute("href", "/probes");
  fireEvent.click(link);
  expect(await screen.findByRole("heading", { name: "探测任务" })).toBeInTheDocument();
});

it("通知渠道导航进入应用的渠道页路由", async () => {
  renderWithAdmin({ listNodes: async () => ({ nodes: [] }), listNotifyChannels: async () => ({ channels: [] }) }, appRouter.routes, "/nodes");
  const link = screen.getByRole("link", { name: "通知渠道" });
  expect(link).toHaveAttribute("href", "/channels");
  fireEvent.click(link);
  expect(await screen.findByRole("heading", { name: "通知渠道" })).toBeInTheDocument();
});

it("API token 导航进入应用的 token 页路由", async () => {
  renderWithAdmin({ listNodes: async () => ({ nodes: [] }), listApiTokens: async () => ({ tokens: [] }) }, appRouter.routes, "/nodes");
  const link = screen.getByRole("link", { name: "API token" });
  expect(link).toHaveAttribute("href", "/tokens");
  fireEvent.click(link);
  expect(await screen.findByRole("heading", { name: "API token" })).toBeInTheDocument();
});

it("操作记录导航进入应用的操作记录页路由", async () => {
  renderWithAdmin({ listNodes: async () => ({ nodes: [] }), listOperations: async () => ({ operations: [] }) }, appRouter.routes, "/nodes");
  const link = screen.getByRole("link", { name: "操作记录" });
  expect(link).toHaveAttribute("href", "/operations");
  fireEvent.click(link);
  expect(await screen.findByRole("heading", { name: "操作记录" })).toBeInTheDocument();
});

it("外观导航进入应用的外观页路由", async () => {
  renderWithAdmin({ listNodes: async () => ({ nodes: [] }), getSettings: async () => ({ settings: { theme: "auto" } }) }, appRouter.routes, "/nodes");
  const link = screen.getByRole("link", { name: "外观" });
  expect(link).toHaveAttribute("href", "/appearance");
  fireEvent.click(link);
  expect(await screen.findByRole("heading", { name: "外观" })).toBeInTheDocument();
});

it("主题导航进入应用的主题页路由", async () => {
  renderWithAdmin({ listNodes: async () => ({ nodes: [] }), listThemes: async () => ({ themes: [], themeOrigin: "https://status.example.com" }) }, appRouter.routes, "/nodes");
  const link = screen.getByRole("link", { name: "主题" });
  expect(link).toHaveAttribute("href", "/themes");
  fireEvent.click(link);
  expect(await screen.findByRole("heading", { name: "主题" })).toBeInTheDocument();
});

it("存储导航进入应用的存储页路由", async () => {
  renderWithAdmin({ listNodes: async () => ({ nodes: [] }), getStorageStats: async () => create(GetStorageStatsResponseSchema, {
    sqlObservedAt: 1n, cleanupPending: 0n, walSizeLimitBytes: 16n << 20n,
    wal: create(WalFileObservationSchema, { observedAt: 1n, result: { case: "absent", value: true } }),
  }) }, appRouter.routes, "/nodes");
  const link = screen.getByRole("link", { name: "存储" });
  expect(link).toHaveAttribute("href", "/storage");
  fireEvent.click(link);
  expect(await screen.findByRole("heading", { name: "存储" })).toBeInTheDocument();
});

it("告警规则导航进入应用的规则页路由", async () => {
  renderWithAdmin({ listNodes: async () => ({ nodes: [] }), listNotifyChannels: async () => ({ channels: [] }),
    listProbeTasks: async () => ({ tasks: [] }), listAlertRules: async () => ({ rules: [], states: [] }) }, appRouter.routes, "/nodes");
  const link = screen.getByRole("link", { name: "告警规则" });
  expect(link).toHaveAttribute("href", "/alerts");
  fireEvent.click(link);
  expect(await screen.findByRole("heading", { name: "告警规则" })).toBeInTheDocument();
});

it("告警事件导航进入应用的事件页路由", async () => {
  renderWithAdmin({ listNodes: async () => ({ nodes: [] }), listNotifyChannels: async () => ({ channels: [] }),
    listAlertEvents: async () => ({ events: [] }) }, appRouter.routes, "/nodes");
  const link = screen.getByRole("link", { name: "告警事件" });
  expect(link).toHaveAttribute("href", "/events");
  fireEvent.click(link);
  expect(await screen.findByRole("heading", { name: "告警事件" })).toBeInTheDocument();
});

it("登出在导航前清空查询与变更缓存", async () => {
  const { queryClient, router } = renderWithAdmin({ logout: async () => ({}) }, routes, "/");
  queryClient.setQueryData(["previous-session"], "old data");
  await queryClient.getMutationCache().build(queryClient, { mutationFn: async () => "old token" }).execute(undefined);
  let cacheAtLogin: number[] = [];
  router.subscribe((state) => {
    if (state.location.pathname === "/login") cacheAtLogin = [queryClient.getQueryCache().getAll().length, queryClient.getMutationCache().getAll().length];
  });
  fireEvent.click(screen.getByRole("button", { name: "登出" }));
  await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
  expect(cacheAtLogin).toEqual([0, 0]);
});

it("登出失败显示错误并保留当前页面", async () => {
  const { router } = renderWithAdmin({ logout: async () => { throw new ConnectError("logout unavailable", Code.Unavailable); } }, routes, "/");
  fireEvent.click(screen.getByRole("button", { name: "登出" }));
  await waitFor(() => expect({ message: screen.queryByRole("alert")?.textContent, path: router.state.location.pathname }).toEqual({ message: "logout unavailable", path: "/" }));
});

it("导航后常驻布局不保留上一页的登出错误", async () => {
  const { router } = renderWithAdmin({ logout: async () => { throw new ConnectError("logout unavailable", Code.Unavailable); } }, [
    { path: "/", Component: Layout, children: [{ index: true, element: <h1>home</h1> }, { path: "nodes", element: <h1>nodes</h1> }] },
  ], "/");
  fireEvent.click(screen.getByRole("button", { name: "登出" }));
  await screen.findByRole("alert");
  await act(() => router.navigate("/nodes"));
  await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
});

it("标签页标题在有触发中告警时加数量前缀，无告警或仅待定时保持原标题，数量变化实时更新", async () => {
  const previous = document.title;
  document.title = "Heron";
  let states: { ruleId: bigint; nodeId: bigint; state: string }[] = [
    { ruleId: 1n, nodeId: 1n, state: "firing" },
    { ruleId: 2n, nodeId: 1n, state: "firing" },
    { ruleId: 3n, nodeId: 2n, state: "pending" },
  ];
  try {
    const { queryClient } = renderWithAdmin({ listAlertRules: async () => ({ rules: [], states }) }, routes, "/");
    await waitFor(() => expect(document.title).toBe("(2) Heron"));
    states = [{ ruleId: 1n, nodeId: 1n, state: "firing" }];
    await act(async () => { await queryClient.refetchQueries(); });
    await waitFor(() => expect(document.title).toBe("(1) Heron"));
    states = [{ ruleId: 3n, nodeId: 2n, state: "pending" }];
    await act(async () => { await queryClient.refetchQueries(); });
    await waitFor(() => expect(document.title).toBe("Heron"));
    states = [
      { ruleId: 1n, nodeId: 1n, state: "firing" },
      { ruleId: 2n, nodeId: 2n, state: "firing" },
      { ruleId: 3n, nodeId: 3n, state: "firing" },
    ];
    await act(async () => { await queryClient.refetchQueries(); });
    await waitFor(() => expect(document.title).toBe("(3) Heron"));
  } finally { document.title = previous; }
});

it("切换管理页面保持标签页告警数且与子页面共享查询，不重复请求 listAlertRules", async () => {
  const previous = document.title;
  document.title = "Heron";
  const listAlertRules = vi.fn(async () => ({
    rules: [],
    states: [{ ruleId: 1n, nodeId: 1n, state: "firing" }],
  }));
  try {
    renderWithAdmin({
      getSnapshot: async () => ({ now: 1000n, nodes: [] }),
      listNodes: async () => ({ nodes: [] }),
      listNotifyChannels: async () => ({ channels: [] }),
      listProbeTasks: async () => ({ tasks: [] }),
      listSilences: async () => ({ now: 1000n, silences: [] }),
      listAlertRules,
    }, appRouter.routes, "/");
    await waitFor(() => expect(document.title).toBe("(1) Heron"));
    expect(listAlertRules).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("link", { name: "告警规则" }));
    expect(await screen.findByRole("heading", { name: "告警规则" })).toBeInTheDocument();
    expect(document.title).toBe("(1) Heron");
    fireEvent.click(screen.getByRole("link", { name: "维护静默" }));
    expect(await screen.findByRole("heading", { name: "维护静默" })).toBeInTheDocument();
    expect(document.title).toBe("(1) Heron");
    expect(listAlertRules).toHaveBeenCalledTimes(1);
  } finally { document.title = previous; }
});

it("登出离开管理布局后立即恢复原标签页标题", async () => {
  const previous = document.title;
  document.title = "Heron";
  try {
    const { router } = renderWithAdmin({
      listAlertRules: async () => ({ rules: [], states: [{ ruleId: 1n, nodeId: 1n, state: "firing" }, { ruleId: 2n, nodeId: 1n, state: "firing" }] }),
      logout: async () => ({}),
    }, routes, "/");
    await waitFor(() => expect(document.title).toBe("(2) Heron"));
    fireEvent.click(screen.getByRole("button", { name: "登出" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    expect(document.title).toBe("Heron");
  } finally { document.title = previous; }
});
