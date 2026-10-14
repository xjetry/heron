import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { currentScheme, useColorScheme } from "./colorScheme";

let systemDark = false;
let changed = () => {};

function stubSystem() {
  vi.stubGlobal("matchMedia", () => ({
    get matches() { return systemDark; },
    addEventListener: (_: string, listener: () => void) => { changed = listener; },
    removeEventListener: vi.fn(),
  }));
}

afterEach(() => {
  vi.unstubAllGlobals();
  delete document.documentElement.dataset.theme;
  systemDark = false;
});

it("data-theme 压过系统设置，没有或不认识时跟随系统", () => {
  stubSystem();
  expect(currentScheme()).toBe("light");
  systemDark = true;
  expect(currentScheme()).toBe("dark");
  document.documentElement.dataset.theme = "light";
  expect(currentScheme()).toBe("light");
  systemDark = false;
  document.documentElement.dataset.theme = "dark";
  expect(currentScheme()).toBe("dark");
  document.documentElement.dataset.theme = "auto";
  expect(currentScheme()).toBe("light");
});

it("useColorScheme 随系统与 data-theme 的变化更新", async () => {
  stubSystem();
  const { result, unmount } = renderHook(() => useColorScheme());
  // 先卸载再由 afterEach 撤掉 matchMedia 的替身：挂着的订阅会在删 data-theme 时再读一次系统设置。
  // 断言失败时也要卸载，否则那次读取抛出的未捕获异常会盖住真正的失败原因。
  try {
    expect(result.current).toBe("light");
    act(() => { systemDark = true; changed(); });
    expect(result.current).toBe("dark");
    await act(async () => { document.documentElement.dataset.theme = "light"; });
    await waitFor(() => expect(result.current).toBe("light"));
  } finally {
    unmount();
  }
});
