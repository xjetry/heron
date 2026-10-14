import { act, renderHook } from "@testing-library/react";
import { expect, it } from "vitest";
import { useLatestError } from "./useLatestError";

it("只展示最新操作的错误，较早操作迟到不覆盖", () => {
  const { result } = renderHook(useLatestError);
  let first: number;
  let second: number;
  act(() => { first = result.current.mutationOptions.onMutate(); });
  act(() => { result.current.mutationOptions.onError("first error", {}, first); });
  expect(result.current.error).toBe("first error");
  act(() => { second = result.current.mutationOptions.onMutate(); });
  expect(result.current.error).toBeNull();
  act(() => { result.current.mutationOptions.onError("late first error", {}, first); });
  expect(result.current.error).toBeNull();
  act(() => { result.current.mutationOptions.onError("second error", {}, second); });
  expect(result.current.error).toBe("second error");
});

it("isLatest 只认最后一次 onMutate 的序号", () => {
  const { result } = renderHook(useLatestError);
  expect(result.current.isLatest(undefined)).toBe(false);
  let first = 0;
  let second = 0;
  act(() => { first = result.current.mutationOptions.onMutate(); });
  expect(result.current.isLatest(first)).toBe(true);
  act(() => { second = result.current.mutationOptions.onMutate(); });
  expect(result.current.isLatest(first)).toBe(false);
  expect(result.current.isLatest(second)).toBe(true);
});
