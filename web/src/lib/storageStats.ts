// GetStorageStats（internal/hub/api/settings.go）总是写入这两项，与 SQL 统计同一只读事务：
// sql_observed_at 是事务开始时的 hub 墙钟，cleanup_pending 是尚未完成的清理作业数（0 是没有作业）。
// 缺席不是“不显示”，也不是 0。

export function requireSqlObservedAt(value: bigint | undefined): bigint {
  if (value === undefined) throw new Error("GetStorageStats.sql_observed_at is always set");
  return value;
}

export function requireCleanupPending(value: bigint | undefined): bigint {
  if (value === undefined) throw new Error("GetStorageStats.cleanup_pending is always set");
  return value;
}
