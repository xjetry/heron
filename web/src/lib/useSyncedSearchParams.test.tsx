import { act, render, screen } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { expect, it } from "vitest";
import { reconcile, SYNC_KEY, useSyncedSearchParams, type SetSearchParams } from "./useSyncedSearchParams";

it("reconcile 按编号识别落地：划掉落地的与更早的；同值重复写入也逐次对上；未知编号或没有编号是外部改动", () => {
  expect(reconcile(["p:1", "p:2", "p:3"], "p:2")).toEqual({ inFlight: ["p:3"], external: false });
  expect(reconcile(["p:1", "p:2", "p:3"], "p:3")).toEqual({ inFlight: [], external: false });
  expect(reconcile(["p:1"], "q:1")).toEqual({ inFlight: [], external: true });
  expect(reconcile(["p:1"], undefined)).toEqual({ inFlight: [], external: true });
  expect(reconcile([], undefined)).toEqual({ inFlight: [], external: true });
});

function mount(path: string) {
  const handle: { params?: URLSearchParams; set?: SetSearchParams } = {};
  function Probe() {
    const [params, set] = useSyncedSearchParams();
    handle.params = params;
    handle.set = set;
    return <p data-testid="local">{params.toString()}</p>;
  }
  const router = createMemoryRouter([{ path: "/", Component: Probe }], { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return { router, handle, local: () => screen.getByTestId("local").textContent };
}

it("同一事件里连写依次叠加；URL 替换当前历史项并带上编号与调用方的 state", async () => {
  const { router, handle, local } = mount("/?a=1");
  expect(local()).toBe("a=1");
  act(() => {
    handle.set!((current) => { current.set("b", "2"); return current; });
    handle.set!((current) => { current.set("c", "3"); return current; }, { state: { from: "list" } });
  });
  // 内存路由的导航在这里同步落地，测不出 URL 落后的窗口；那一段由真实浏览器的 e2e 覆盖（admin-ui.spec.ts 的 URL 筛选用例）。
  expect(local()).toBe("a=1&b=2&c=3");
  await act(async () => {});
  expect(router.state.location.search).toBe("?a=1&b=2&c=3");
  expect(router.state.historyAction).toBe("REPLACE");
  expect(router.state.location.state).toMatchObject({ from: "list" });
  expect(typeof (router.state.location.state as Record<string, unknown>)[SYNC_KEY]).toBe("string");
  expect(local()).toBe("a=1&b=2&c=3");
});

it("外部导航以 URL 为准；值没变的写入不导航", async () => {
  const { router, handle, local } = mount("/?q=a");
  await act(async () => { await router.navigate("/?q=external"); });
  expect(local()).toBe("q=external");
  const key = router.state.location.key;
  act(() => handle.set!(new URLSearchParams("q=external")));
  await act(async () => {});
  expect(router.state.location.key).toBe(key);
});
