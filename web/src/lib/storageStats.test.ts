import { expect, it } from "vitest";
import { requireCleanupPending, requireSqlObservedAt } from "./storageStats";

it("sql_observed_at 由 GetStorageStats 总是给出：缺席不是不显示时刻", () => {
  expect(requireSqlObservedAt(1_767_220_000n)).toBe(1_767_220_000n);
  expect(() => requireSqlObservedAt(undefined)).toThrow(/sql_observed_at/);
});

it("cleanup_pending 由 GetStorageStats 总是给出：0 是没有作业，缺席不是不显示", () => {
  expect(requireCleanupPending(0n)).toBe(0n);
  expect(() => requireCleanupPending(undefined)).toThrow(/cleanup_pending/);
});
