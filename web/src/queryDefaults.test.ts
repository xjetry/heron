import { focusManager, QueryClient, QueryObserver } from "@tanstack/react-query";
import { afterEach, expect, it, vi } from "vitest";
import { queryDefaults } from "./queryDefaults";

afterEach(() => { focusManager.setFocused(undefined); vi.useRealTimers(); });

it.each([undefined, false, 0, 2000, (): number => 2000, (): false => false] as const)("仅正在轮询的查询回前台立即刷新：%s", async (refetchInterval) => {
  vi.useFakeTimers();
  const client = new QueryClient({ defaultOptions: { queries: queryDefaults } });
  client.mount();
  const fetch = vi.fn(async () => "data");
  const observer = new QueryObserver(client, { queryKey: ["focus"], queryFn: fetch, refetchInterval });
  const unsubscribe = observer.subscribe(() => {});
  await vi.advanceTimersByTimeAsync(0);
  expect(fetch).toHaveBeenCalledTimes(1);
  focusManager.setFocused(false);
  focusManager.setFocused(true);
  await vi.advanceTimersByTimeAsync(0);
  const interval = typeof refetchInterval === "function" ? refetchInterval() : refetchInterval;
  expect(fetch).toHaveBeenCalledTimes(interval ? 2 : 1);
  unsubscribe();
  client.unmount();
  client.clear();
});
