import { Code, ConnectError } from "@connectrpc/connect";
import { focusManager, onlineManager, QueryClient } from "@tanstack/react-query";
import { act, within } from "@testing-library/react";
import * as ReactDOM from "react-dom/client";
import { afterAll, beforeAll, expect, test, vi } from "vitest";
import { checkFocusRefresh } from "../test/focus";

// 入口用例验证传输与站点设置的应用；图表依赖的布局和 canvas 不由 jsdom 提供。
vi.mock("../components/Chart", () => ({ Chart: () => null }));

vi.mock("react-dom/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("react-dom/client")>();
  return { ...actual, createRoot: vi.fn(actual.createRoot) };
});

const fetches: { url: string; init?: RequestInit }[] = [];
let root: HTMLDivElement;
let reactRoot: ReactDOM.Root | undefined;
let queryClient: QueryClient | undefined;

function json(body: unknown) {
  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
}

beforeAll(async () => {
  vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    fetches.push({ url, init });
    if (url.includes("/heron.v1.PublicService/GetSite")) return json({ title: "机房状态", theme: "dark", accentColor: "#123abc" });
    return json({ now: "1000", nodes: [{ id: "3", name: "web-1", online: true }] });
  }));
  root = document.createElement("div");
  root.id = "root";
  document.body.append(root);
  window.history.pushState({}, "", "/");
  const createRoot = vi.mocked(ReactDOM.createRoot);
  const mount = vi.spyOn(QueryClient.prototype, "mount");
  await act(async () => {
    await import("./main.tsx");
  });
  const created = createRoot.mock.results[0];
  reactRoot = created?.type === "return" ? created.value : undefined;
  queryClient = mount.mock.instances[0] as QueryClient | undefined;
  mount.mockRestore();
});

afterAll(async () => {
  await act(async () => reactRoot?.unmount());
  root.remove();
  vi.unstubAllGlobals();
});

test("入口在 / 挂载公开总览并应用站点设置", async () => {
  expect(await within(root).findByRole("link", { name: "web-1" })).toBeInTheDocument();
  expect(await within(root).findByRole("link", { name: "机房状态" })).toBeInTheDocument();
  expect(document.documentElement.dataset.theme).toBe("dark");
  expect(document.documentElement.style.getPropertyValue("--accent")).toBe("#123abc");
});

test("入口只让轮询查询在回前台时立即刷新", async () => {
  await checkFocusRefresh(queryClient!);
});

// 生产 client 的重试谓词是 retry.ts 的白名单：节点不公开的 NotFound、限流的 ResourceExhausted 一次就交给页面，
// 网络与反代的瞬时错误（Unavailable）再试两次。
test.each([
  ["NotFound", 1, Code.NotFound],
  ["ResourceExhausted", 1, Code.ResourceExhausted],
  ["Unavailable", 3, Code.Unavailable],
] as const)("查询得到 %s 时共请求 %i 次", async (_, calls, code) => {
  const request = vi.fn(async () => {
    throw new ConnectError("request failed", code);
  });
  await act(async () => {
    await queryClient!.fetchQuery({ queryKey: ["retry", code], queryFn: request, retryDelay: 0 }).catch(() => {});
  });
  expect(request).toHaveBeenCalledTimes(calls);
});

// 公开请求用 GET（curl 同款的 Connect GET 形态），不带 cookie。
test("公开请求是不带凭据的 GET", () => {
  expect(fetches.length).toBeGreaterThanOrEqual(2);
  for (const { url, init } of fetches) {
    expect(url).toMatch(/^\/heron\.v1\.PublicService\/(GetSite|GetSnapshot|QueryMetrics)\?connect=v1&encoding=json&message=/);
    expect(init?.method).toBe("GET");
    expect(init?.credentials).toBe("omit");
  }
});

// 站点设置只在页面加载时取：断网重连时 react-query 重取已过期的查询，GetSite 无论过了多久都不在其列。
// 时钟拨到一天之后再重连，任何有限的 staleTime 都已过期。加载时的次数不写死：开发模式下 StrictMode 把挂载做两遍，
// 加载时取了两次（去掉 StrictMode 即为一次）。
test("网络恢复后不重取站点设置", async () => {
  const siteFetches = () => fetches.filter(({ url }) => url.includes("/GetSite?")).length;
  expect(await within(root).findByRole("link", { name: "机房状态" })).toBeInTheDocument();
  const loaded = siteFetches();
  expect(loaded).toBeGreaterThan(0);
  vi.setSystemTime(Date.now() + 24 * 60 * 60 * 1000);
  try {
    await act(async () => onlineManager.setOnline(false));
    await act(async () => onlineManager.setOnline(true));
    await act(async () => new Promise((resolve) => setTimeout(resolve, 50)));
  } finally {
    vi.useRealTimers();
  }
  expect(siteFetches()).toBe(loaded);
});

// GetSite 不轮询，生产 client 对它关闭聚焦重取；这里从真实入口钉住不发额外请求的结果。
test("窗口重新聚焦后不重取站点设置", async () => {
  const siteFetches = () => fetches.filter(({ url }) => url.includes("/GetSite?")).length;
  expect(await within(root).findByRole("link", { name: "机房状态" })).toBeInTheDocument();
  const loaded = siteFetches();
  vi.setSystemTime(Date.now() + 2 * 24 * 60 * 60 * 1000);
  try {
    await act(async () => focusManager.setFocused(false));
    await act(async () => focusManager.setFocused(true));
    await act(async () => new Promise((resolve) => setTimeout(resolve, 50)));
  } finally {
    vi.useRealTimers();
    focusManager.setFocused(undefined);
  }
  expect(siteFetches()).toBe(loaded);
});
