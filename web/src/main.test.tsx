import { Code, ConnectError } from "@connectrpc/connect";
import { QueryClient } from "@tanstack/react-query";
import { act, within } from "@testing-library/react";
import * as ReactDOM from "react-dom/client";
import { afterAll, beforeAll, expect, test, vi } from "vitest";
import { checkFocusRefresh } from "./test/focus";

// 入口用例验证路由与认证；图表依赖的布局和 canvas 不由 jsdom 提供。
vi.mock("./components/Chart", () => ({ Chart: () => null }));

vi.mock("react-dom/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("react-dom/client")>();
  return { ...actual, createRoot: vi.fn(actual.createRoot) };
});

let root: HTMLDivElement;
let reactRoot: ReactDOM.Root | undefined;
let queryClient: QueryClient | undefined;
let router: (typeof import("./App"))["router"];

beforeAll(async () => {
  // 总览随路由挂载请求快照；网络结果固定为非认证错误，避免干扰本用例的认证裁决。
  vi.stubGlobal("fetch", vi.fn(async () => new Response(
    JSON.stringify({ code: "unavailable", message: "snapshot unavailable" }),
    { status: 503, headers: { "Content-Type": "application/json" } },
  )));
  root = document.createElement("div");
  root.id = "root";
  root.textContent = "尚未挂载";
  document.body.append(root);
  window.history.pushState({}, "", "/admin/login");

  const createRoot = vi.mocked(ReactDOM.createRoot);
  const mount = vi.spyOn(QueryClient.prototype, "mount");
  await act(async () => {
    await import("./main.tsx");
  });
  const created = createRoot.mock.results[0];
  reactRoot = created?.type === "return" ? created.value : undefined;
  queryClient = mount.mock.instances[0] as QueryClient | undefined;
  mount.mockRestore();
  router = (await import("./App")).router;
});

afterAll(async () => {
  await act(async () => reactRoot?.unmount());
  queryClient?.clear();
  router?.dispose();
  root.remove();
  vi.unstubAllGlobals();
});

test("入口在 /admin/login 将登录页挂载到 root", () => {
  expect(within(root).getByLabelText("管理员密码")).toBeInTheDocument();
});

test("入口只让轮询查询在回前台时立即刷新", async () => {
  await checkFocusRefresh(queryClient!);
});

async function failRequest(kind: "query" | "mutation", code: Code) {
  const request = vi.fn(async () => {
    throw new ConnectError("request failed", code);
  });
  await act(async () => {
    await router.navigate("/");
    if (kind === "query") {
      await queryClient!.fetchQuery({ queryKey: [kind, code], queryFn: request, retryDelay: 0 }).catch(() => {});
    } else {
      await queryClient!.getMutationCache().build(queryClient!, { mutationFn: request }).execute(undefined).catch(() => {});
    }
  });
  return request;
}

test.each(["query", "mutation"] as const)("%s 的 Unauthenticated 回到登录页且不重试", async (kind) => {
  const request = await failRequest(kind, Code.Unauthenticated);
  expect(window.location.pathname).toBe("/admin/login");
  expect(request).toHaveBeenCalledTimes(1);
});

// 生产 client 的重试谓词是 retry.ts 的白名单：确定性错误一次就交给页面，瞬时错误查询再试两次，变更一律不重试。
test.each(["query", "mutation"] as const)("%s 的确定性错误不跳登录页也不重试", async (kind) => {
  const request = await failRequest(kind, Code.PermissionDenied);
  expect(window.location.pathname).toBe("/admin");
  expect(request).toHaveBeenCalledTimes(1);
});

test.each(["query", "mutation"] as const)("%s 的 Unavailable 不跳登录页，查询再试两次", async (kind) => {
  const request = await failRequest(kind, Code.Unavailable);
  expect(window.location.pathname).toBe("/admin");
  expect(request).toHaveBeenCalledTimes(kind === "query" ? 3 : 1);
});
