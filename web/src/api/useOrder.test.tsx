import { act, renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { useOrder } from "./useOrder";

const defer = <T,>() => {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
};
const initial = [1n, 2n, 3n];

describe("useOrder", () => {
  it.each([
    [3n, 1n, "before", [3n, 1n, 2n]],
    [1n, 3n, "before", [2n, 1n, 3n]],
    [3n, 1n, "after", [1n, 3n, 2n]],
  ] as const)("节点 %s 插入 %s 的 %s，保留其余相对顺序", async (value, target, edge, expected) => {
    const save = vi.fn(async () => {});
    const reload = vi.fn(async () => expected);
    const { result } = renderHook(() => useOrder({ items: initial, id: (n) => n, enabled: true, save, reload }));
    await act(async () => result.current.move(value, { target, edge }));
    expect(save).toHaveBeenCalledExactlyOnceWith(expected);
    expect(result.current.items).toEqual(expected);
  });
  it("拖放插入与置顶置底共用串行保存，保持完整排列而非交换两行", async () => {
    const first = defer<void>();
    const save = vi.fn().mockImplementationOnce(() => first.promise).mockResolvedValue(undefined);
    const reload = vi.fn(async () => [3n, 2n, 1n]);
    const { result } = renderHook(() => useOrder({ items: initial, id: (n) => n, enabled: true, save, reload }));
    act(() => result.current.move(1n, { target: 3n, edge: "after" }));
    expect(result.current.items).toEqual([2n, 3n, 1n]);
    act(() => result.current.move(3n, "first"));
    expect(result.current.items).toEqual([3n, 2n, 1n]);
    expect(save).toHaveBeenCalledExactlyOnceWith([2n, 3n, 1n]);
    await act(async () => first.resolve());
    expect(save.mock.calls).toEqual([[[2n, 3n, 1n]], [[3n, 2n, 1n]]]);
    expect(result.current.pending).toBe(false);
  });

  it("置底保持其余节点相对顺序，无效或原地落点不发请求", async () => {
    const save = vi.fn(async () => {});
    const reload = vi.fn(async () => [2n, 3n, 1n]);
    const { result } = renderHook(() => useOrder({ items: initial, id: (n) => n, enabled: true, save, reload }));
    act(() => {
      result.current.move(1n, "first");
      result.current.move(1n, { target: 1n, edge: "after" });
      result.current.move(1n, { target: 99n, edge: "before" });
    });
    expect(save).not.toHaveBeenCalled();
    expect(result.current.confirmed).toBe(false);
    await act(async () => result.current.move(1n, "last"));
    expect(save).toHaveBeenCalledExactlyOnceWith([2n, 3n, 1n]);
    expect(result.current.confirmed).toBe(true);
  });
  it("连续移动从乐观顺序推导，串行写入且旧回读不能覆盖最终确认前的顺序", async () => {
    const first = defer<void>();
    const second = defer<void>();
    const read = defer<bigint[]>();
    const save = vi.fn().mockImplementationOnce(() => first.promise).mockImplementationOnce(() => second.promise);
    const reload = vi.fn(() => read.promise);
    const { result, rerender } = renderHook(({ items }) => useOrder({ items, id: (n) => n, enabled: true, save, reload }), { initialProps: { items: initial } });
    act(() => { result.current.move(1n, 1); result.current.move(1n, 1); });
    expect(result.current.items).toEqual([2n, 3n, 1n]);
    expect(save.mock.calls).toEqual([[[2n, 1n, 3n]]]);
    rerender({ items: [...initial] });
    expect(result.current.items).toEqual([2n, 3n, 1n]);
    await act(async () => { first.resolve(); });
    expect(save.mock.calls).toEqual([[[2n, 1n, 3n]], [[2n, 3n, 1n]]]);
    expect(reload).not.toHaveBeenCalled();
    await act(async () => { second.resolve(); });
    expect(result.current.pending).toBe(true);
    rerender({ items: [2n, 1n, 3n] });
    expect(result.current.items).toEqual([2n, 3n, 1n]);
    await act(async () => { read.resolve([2n, 3n, 1n]); });
    expect(result.current.pending).toBe(false);
    expect(result.current.items).toEqual([2n, 3n, 1n]);
    rerender({ items: [2n, 3n, 1n] });
    expect(result.current.items).toEqual([2n, 3n, 1n]);
  });

  it("最终回读中仍能累积移动，但回读完成前不发下一写", async () => {
    const read = defer<bigint[]>();
    const save = vi.fn(async () => {});
    const reload = vi.fn().mockImplementationOnce(() => read.promise).mockResolvedValue([2n, 3n, 1n]);
    const { result } = renderHook(() => useOrder({ items: initial, id: (n) => n, enabled: true, save, reload }));
    act(() => result.current.move(1n, 1));
    await waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
    act(() => result.current.move(1n, 1));
    expect(save).toHaveBeenCalledTimes(1);
    await act(async () => { read.resolve([2n, 1n, 3n]); });
    expect(save.mock.calls).toEqual([[[2n, 1n, 3n]], [[2n, 3n, 1n]]]);
    expect(result.current.items).toEqual([2n, 3n, 1n]);
  });

  it.each(["save", "reload"])("%s 失败后清空排队，回读失败时禁止新排序，恢复成功仍保留失败提示", async (failure) => {
    const first = defer<void>();
    const recovery = defer<bigint[]>();
    const save = vi.fn(() => first.promise);
    const reload = vi.fn();
    if (failure === "reload") reload.mockRejectedValueOnce(new Error("读取响应丢失"));
    reload.mockImplementationOnce(() => recovery.promise).mockResolvedValue(initial);
    const { result } = renderHook(() => useOrder({ items: initial, id: (n) => n, enabled: true, save, reload }));
    act(() => result.current.move(1n, 1));
    if (failure === "save") act(() => result.current.move(1n, 1));
    await act(async () => { if (failure === "save") first.reject(new Error("保存响应丢失")); else first.resolve(); });
    expect(result.current.blocked).toBe(true);
    act(() => result.current.move(3n, -1));
    expect(save).toHaveBeenCalledTimes(1);
    await act(async () => { recovery.reject(new Error("hub unavailable")); });
    expect(result.current.pending).toBe(false);
    expect(result.current.blocked).toBe(true);
    expect(String(result.current.error)).toContain("hub unavailable");
    expect(String(result.current.error)).toContain(failure === "save" ? "保存响应丢失" : "读取响应丢失");
    act(() => result.current.move(result.current.items[1], -1));
    expect(save).toHaveBeenCalledTimes(1);
    await act(async () => { result.current.recover(); });
    expect(result.current.blocked).toBe(false);
    expect(result.current.items).toEqual(initial);
    expect(result.current.error).not.toBeNull();
    expect(save).toHaveBeenCalledTimes(1);
  });

  it("完整列表成员变化取消未发送排列，不能继续用旧集合写入", async () => {
    const first = defer<void>();
    const save = vi.fn(() => first.promise);
    const reload = vi.fn(async () => [1n, 3n]);
    const { result, rerender } = renderHook(({ items }) => useOrder({ items, id: (n) => n, enabled: true, save, reload }), { initialProps: { items: initial } });
    act(() => { result.current.move(1n, 1); result.current.move(1n, 1); });
    rerender({ items: [1n, 3n] });
    await act(async () => { first.resolve(); });
    expect(save).toHaveBeenCalledTimes(1);
    expect(result.current.items).toEqual([1n, 3n]);
    expect(String(result.current.error)).toContain("列表成员已变化");
  });

  it("过滤或沿用子集不开放排序，恢复完整列表后才接受移动", async () => {
    const save = vi.fn(async () => {});
    const reload = vi.fn(async () => [2n, 1n, 3n]);
    const { result, rerender } = renderHook(({ enabled }) => useOrder({ items: initial, id: (n) => n, enabled, save, reload }), { initialProps: { enabled: false } });
    act(() => result.current.move(1n, 1));
    expect(save).not.toHaveBeenCalled();
    rerender({ enabled: true });
    await act(async () => { result.current.move(1n, 1); });
    expect(save).toHaveBeenCalledExactlyOnceWith([2n, 1n, 3n]);
  });

  it("卸载后不再回读或提交剩余排列", async () => {
    const first = defer<void>();
    const save = vi.fn(() => first.promise);
    const reload = vi.fn(async () => initial);
    const { result, unmount } = renderHook(() => useOrder({ items: initial, id: (n) => n, enabled: true, save, reload }));
    act(() => { result.current.move(1n, 1); result.current.move(1n, 1); });
    unmount();
    await act(async () => { first.resolve(); });
    expect(save).toHaveBeenCalledTimes(1);
    expect(reload).not.toHaveBeenCalled();
  });
});
