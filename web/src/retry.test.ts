import { Code, ConnectError } from "@connectrpc/connect";
import { expect, test } from "vitest";
import { nonRetryableCodes, retryableCodes, retryQuery } from "./retry";

// Code 的全部取值取自枚举本身：connect-es 新增的码没有归进两个集合之一就红。
const allCodes = Object.values(Code).filter((v): v is Code => typeof v === "number");

test("每个 Code 都被显式归入重试或不重试，恰好其一", () => {
  expect(allCodes.length).toBeGreaterThan(0);
  const unclassified = allCodes.filter((c) => !retryableCodes.has(c) && !nonRetryableCodes.has(c)).map((c) => Code[c]);
  const both = allCodes.filter((c) => retryableCodes.has(c) && nonRetryableCodes.has(c)).map((c) => Code[c]);
  expect(unclassified).toEqual([]);
  expect(both).toEqual([]);
  expect(retryableCodes.size + nonRetryableCodes.size).toBe(allCodes.length);
});

test("只重试网络与反代的瞬时错误", () => {
  expect([...retryableCodes].map((c) => Code[c]).sort()).toEqual(["Aborted", "DeadlineExceeded", "Unavailable", "Unknown"]);
});

test.each(allCodes.map((c) => [Code[c], c] as const))("%s：谓词与归类一致，且至多再试两次", (_, code) => {
  const err = new ConnectError("failed", code);
  expect(retryQuery(0, err)).toBe(retryableCodes.has(code));
  expect(retryQuery(1, err)).toBe(retryableCodes.has(code));
  expect(retryQuery(2, err)).toBe(false);
});

test("不是 ConnectError 的异常不重试", () => {
  expect(retryQuery(0, new Error("bug in a query function"))).toBe(false);
});
