import { focusManager, QueryClient, QueryObserver } from "@tanstack/react-query";
import { act, waitFor } from "@testing-library/react";
import { expect, vi } from "vitest";

// 从实际入口取得的 client 上挂观察者，防止入口漏装共用策略而 helper 单测仍然通过。
export async function checkFocusRefresh(client: QueryClient) {
  const polled = vi.fn(async () => "poll");
  const once = vi.fn(async () => "once");
  const observers = [
    new QueryObserver(client, { queryKey: ["entry-focus-poll"], queryFn: polled, refetchInterval: 60_000 }),
    new QueryObserver(client, { queryKey: ["entry-focus-once"], queryFn: once }),
  ];
  const unsubscribe = observers.map((o) => o.subscribe(() => {}));
  try {
    await waitFor(() => expect(observers.every((o) => o.getCurrentResult().isSuccess)).toBe(true));
    expect(polled).toHaveBeenCalledTimes(1);
    expect(once).toHaveBeenCalledTimes(1);
    await act(async () => focusManager.setFocused(false));
    await act(async () => {
      focusManager.setFocused(true);
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(polled).toHaveBeenCalledTimes(2);
    expect(once).toHaveBeenCalledTimes(1);
  } finally {
    unsubscribe.forEach((stop) => stop());
    focusManager.setFocused(undefined);
    client.removeQueries({ queryKey: ["entry-focus-poll"] });
    client.removeQueries({ queryKey: ["entry-focus-once"] });
  }
}
