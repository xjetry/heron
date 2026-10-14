import { act, render, renderHook, screen } from "@testing-library/react";
import { Suspense, startTransition, use, useState } from "react";
import { expect, it } from "vitest";
import { useRetained } from "./useRetained";

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((r) => { resolve = r; });
  return { promise, resolve };
}

type Q = { data: number | undefined; error: unknown };
type QWithIdentity = Q & { identity: string };

it("请求失败时沿用上一份成功数据，data 恢复后不再沿用", () => {
  const initialProps: Q = { data: 1, error: null };
  const { result, rerender } = renderHook((q: Q) => useRetained(q), { initialProps });
  expect(result.current).toEqual({ data: 1, error: null, stale: false });
  rerender({ data: undefined, error: "boom" });
  expect(result.current).toEqual({ data: 1, error: "boom", stale: true });
  rerender({ data: 2, error: null });
  expect(result.current).toEqual({ data: 2, error: null, stale: false });
});

it("identity 变化的这一帧起不沿用上一个对象的数据，直到新对象自己取到数据", () => {
  const initialProps: QWithIdentity = { data: 1, error: null, identity: "a" };
  const { result, rerender } = renderHook(({ data, error, identity }: QWithIdentity) => useRetained({ data, error }, identity), { initialProps });
  expect(result.current).toEqual({ data: 1, error: null, stale: false });
  // 换对象：新对象还没有自己的数据，不该沿用 "a" 的 1。
  rerender({ data: undefined, error: null, identity: "b" });
  expect(result.current).toEqual({ data: undefined, error: null, stale: false });
  // 同一个新对象继续挂起或失败：还是不沿用 "a" 的值。
  rerender({ data: undefined, error: "b failed", identity: "b" });
  expect(result.current).toEqual({ data: undefined, error: "b failed", stale: false });
  // "b" 自己的数据到达。
  rerender({ data: 2, error: null, identity: "b" });
  expect(result.current).toEqual({ data: 2, error: null, stale: false });
  // 现在 "b" 的刷新失败，沿用的是 "b" 的 2，不是 "a" 的 1。
  rerender({ data: undefined, error: "b refresh failed", identity: "b" });
  expect(result.current).toEqual({ data: 2, error: "b refresh failed", stale: true });
  // 换成 "c"，且这一帧就已经有自己的数据（例如缓存命中）：直接返回，同时把它记成 "c" 自己的值——
  // 不是"沿用了 b 的 2"，后面 "c" 刷新失败时沿用的应该是 99，不是 2。
  rerender({ data: 99, error: null, identity: "c" });
  expect(result.current).toEqual({ data: 99, error: null, stale: false });
  rerender({ data: undefined, error: "c refresh failed", identity: "c" });
  expect(result.current).toEqual({ data: 99, error: "c refresh failed", stale: true });
});

// react-router 的 RouterProvider 把路由状态更新包在 startTransition 里；transition 渲染中途挂起
// （子组件 use() 一个未决的 promise）时，React 保留旧内容不提交，promise 解决后从根重试这次渲染，未变的子树跳过。
// identity 若记在 ref 里，第一次（被丢弃的）渲染已经把 ref 写成了新值，重试时 ref 与新 identity 相等，
// changedIdentity 判成"没换"，沿用值就漏判成了上一个对象的——这是本用例要钉住不发生的情况。
it("带新 identity 的 transition 渲染被挂起丢弃后重试，不沿用上一个对象的数据", async () => {
  const gate = deferred();
  let setProps!: (p: { identity: string; data: number | undefined; suspend: boolean }) => void;

  function Suspender() {
    use(gate.promise);
    return null;
  }
  function Result({ identity, data }: { identity: string; data: number | undefined }) {
    const r = useRetained({ data, error: null }, identity);
    return <div data-testid="result">{identity}:{String(r.data)}</div>;
  }
  function Wrapper() {
    const [props, setP] = useState<{ identity: string; data: number | undefined; suspend: boolean }>({ identity: "a", data: 1, suspend: false });
    setProps = setP;
    return (
      <Suspense fallback={<div data-testid="result">loading</div>}>
        <Result identity={props.identity} data={props.data} />
        {props.suspend && <Suspender />}
      </Suspense>
    );
  }

  render(<Wrapper />);
  expect(screen.getByTestId("result")).toHaveTextContent("a:1");
  await act(async () => {
    startTransition(() => setProps({ identity: "b", data: undefined, suspend: true }));
  });
  // transition 挂起期间不提交新树，屏幕上仍是挂起前的内容。
  expect(screen.getByTestId("result")).toHaveTextContent("a:1");
  await act(async () => {
    gate.resolve();
    await gate.promise;
  });
  // 挂起解除、重试提交后，"b" 不该带着 "a" 的 1。
  expect(screen.getByTestId("result")).toHaveTextContent("b:undefined");
});

it("不传 identity 时行为不变：调用点本身就是身份，同一个列表换过滤条件属于这一种", () => {
  const initialProps: Q = { data: 1, error: null };
  const { result, rerender } = renderHook((q: Q) => useRetained(q), { initialProps });
  rerender({ data: undefined, error: null });
  expect(result.current).toEqual({ data: 1, error: null, stale: true });
});
