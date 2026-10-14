import { Component, type ReactNode } from "react";
import { expect, it } from "vitest";
import { screen, within } from "@testing-library/react";
import { create } from "@bufbuild/protobuf";
import { GetStorageStatsResponseSchema, WalFileObservationSchema, type WalFileObservation } from "../gen/heron/v1/admin_pb";
import { renderWithAdmin } from "../test/harness";
import { Storage } from "./Storage";
import { dateTime } from "../lib/format";

class StorageErrorProbe extends Component<{ children: ReactNode }, { message: string }> {
  state = { message: "" };
  static getDerivedStateFromError(error: Error) { return { message: error.message }; }
  render() { return this.state.message ? <p role="alert">{this.state.message}</p> : this.props.children; }
}

function GuardedStorage() {
  return <StorageErrorProbe><Storage /></StorageErrorProbe>;
}

const routes = [{ path: "/storage", Component: Storage }];
const at = (unix: bigint) => dateTime(unix);
const observedAt = 1_767_230_000n;
const limit = 16n << 20n;
const byteWal = (value: bigint) => create(WalFileObservationSchema, { observedAt, result: { case: "bytes" as const, value } });

const stats = create(GetStorageStatsResponseSchema, {
  dbBytes: 4096n,
  sqlObservedAt: 1_767_220_000n,
  cleanupPending: 0n,
  walSizeLimitBytes: limit,
  wal: byteWal(1n),
  tables: [{ name: "metric_1m", rows: 3n }],
  lastRollupAt: 1_767_225_000n,
  series: [
    // 数值本身远在阈值之外，但 hub 说不标红：面板只看结论，不自己拿数值重算。
    { table: "metric_1m", bucketS: 60, retentionS: 604_800n, oldestTs: 1n, oldestStale: false },
    { table: "metric_5m", bucketS: 300, retentionS: 2_592_000n, oldestTs: 1_767_000_000n, oldestStale: true, watermarkTs: 1_767_100_000n, watermarkStale: true },
    { table: "metric_1h", bucketS: 3600, retentionS: 31_536_000n, watermarkTs: 1_767_222_000n },
  ],
});

const row = async (table: string) => {
  const region = await screen.findByRole("region", { name: "时序表健康" });
  return within(within(region).getByRole("cell", { name: table }).closest("tr")!);
};

it("显示 SQL 统计时刻与完成后的复用窗口", async () => {
  renderWithAdmin({ getStorageStats: async () => stats }, routes, "/storage");
  expect(await screen.findByText(/^统计于 /)).toHaveTextContent(`统计于 ${at(1_767_220_000n)}；同一份 SQL 统计在算出后 60 秒内复用。`);
});

it("sql_observed_at 由 GetStorageStats 总是给出：缺席不是不显示时刻", async () => {
  renderWithAdmin({ getStorageStats: async () => create(GetStorageStatsResponseSchema, { dbBytes: 4096n, cleanupPending: 0n, walSizeLimitBytes: limit, wal: create(WalFileObservationSchema, { observedAt, result: { case: "bytes", value: 1n } }) }) }, [{ path: "/storage", Component: GuardedStorage }], "/storage");
  expect(await screen.findByRole("alert")).toHaveTextContent("sql_observed_at");
});

it("按 hub 给的 stale 标红，不按数值重算", async () => {
  renderWithAdmin({ getStorageStats: async () => stats }, routes, "/storage");
  const m1 = await row("metric_1m");
  expect(m1.getByText(at(1n))).not.toHaveClass("error");
  const m5 = await row("metric_5m");
  expect(m5.getByText(`${at(1_767_000_000n)}（超出保留期）`)).toHaveClass("error");
  expect(m5.getByText(`${at(1_767_100_000n)}（上卷滞后）`)).toHaveClass("error");
  const h1 = await row("metric_1h");
  expect(h1.getByText(at(1_767_222_000n))).not.toHaveClass("error");
});

it("缺失的读数按各自含义显示：空表、没有水位、从未成功", async () => {
  renderWithAdmin({ getStorageStats: async () => stats }, routes, "/storage");
  const m1 = await row("metric_1m");
  expect(m1.getByText("—")).toBeInTheDocument();
  const h1 = await row("metric_1h");
  expect(h1.getByText("空表")).toBeInTheDocument();
  expect(screen.getByText(/上次清理完成/)).toHaveTextContent(`上次清理完成：从未成功；上次上卷完成：${at(1_767_225_000n)}`);
});

it.each([3n, 0n])("显示待清理作业数 %s，0 也照实显示", async (pending) => {
  const response = create(GetStorageStatsResponseSchema, { dbBytes: 4096n, sqlObservedAt: 1_767_220_000n, cleanupPending: pending, walSizeLimitBytes: limit, wal: byteWal(1n) });
  renderWithAdmin({ getStorageStats: async () => response }, routes, "/storage");
  expect(await screen.findByText(/^待清理的已删除节点与任务：/)).toHaveTextContent(`待清理的已删除节点与任务：${pending}`);
});

it("cleanup_pending 由 GetStorageStats 总是给出：缺席不是不显示，也不当成 0", async () => {
  renderWithAdmin({ getStorageStats: async () => create(GetStorageStatsResponseSchema, { dbBytes: 4096n, sqlObservedAt: 1_767_220_000n, walSizeLimitBytes: limit, wal: create(WalFileObservationSchema, { observedAt, result: { case: "bytes", value: 1n } }) }) }, [{ path: "/storage", Component: GuardedStorage }], "/storage");
  expect(await screen.findByRole("alert")).toHaveTextContent("cleanup_pending");
});

// 逻辑大小固定为 4096（显示 4.0 KiB），避免和 WAL 的 0 B 混在同一段文本里。
// sql 时刻、待清理作业数与 WAL 上限是 GetStorageStats 总是给出的字段，这里一并带上。
const renderWal = (wal?: WalFileObservation) => {
  const response = create(GetStorageStatsResponseSchema, {
    dbBytes: 4096n, sqlObservedAt: 1_767_220_000n, cleanupPending: 0n, walSizeLimitBytes: limit,
  });
  if (wal) response.wal = wal;
  renderWithAdmin({ getStorageStats: async () => response }, routes, "/storage");
};

it("wal 由 GetStorageStats 总是给出：缺席不是不显示，也不当成无文件或 0 字节", async () => {
  renderWithAdmin({ getStorageStats: async () => create(GetStorageStatsResponseSchema, { dbBytes: 4096n, sqlObservedAt: 1_767_220_000n, cleanupPending: 0n, walSizeLimitBytes: limit }) }, [{ path: "/storage", Component: GuardedStorage }], "/storage");
  expect(await screen.findByRole("alert")).toHaveTextContent(/\.wal/);
  expect(screen.queryByText(/无 WAL 文件/)).toBeNull();
  expect(screen.queryByText(/0 B/)).toBeNull();
});

it("bytes 为 0 仍是文件长度，带观测时刻，不是无文件", async () => {
  renderWal(create(WalFileObservationSchema, { observedAt, result: { case: "bytes", value: 0n } }));
  const line = await screen.findByText(/^WAL 文件：/);
  expect(line.textContent).not.toContain("无 WAL 文件");
  expect(line).toHaveTextContent(`WAL 文件：0 B（观测于 ${at(observedAt)}）`);
  expect(screen.getByText(/不含 -wal 与 -shm 文件/)).toHaveTextContent("不是同一时刻读出的");
  // 长度旁边不出现危险或阈值判断；有长度的时候最容易被读成健康信号。
  expect(screen.queryByText(/越大越危险|超过|阈值/)).toBeNull();
});

it("absent 显示无 WAL 文件并带观测时刻", async () => {
  renderWal(create(WalFileObservationSchema, { observedAt, result: { case: "absent", value: true } }));
  const line = await screen.findByText(/^WAL 文件：无 WAL 文件/);
  expect(line).toHaveTextContent(`WAL 文件：无 WAL 文件（观测于 ${at(observedAt)}）`);
  expect(screen.getByText(/实际长度/)).toBeInTheDocument();
});

it("error 显示大小未知，不出现 0 字节，也不写成无文件", async () => {
  renderWal(create(WalFileObservationSchema, { observedAt, result: { case: "error", value: "permission denied" } }));
  const line = await screen.findByText(/^WAL 文件：/);
  expect(line.textContent).not.toContain("0 B");
  expect(line.textContent).not.toContain("0 字节");
  expect(line.textContent).not.toContain("无 WAL 文件");
  expect(line).toHaveTextContent(`WAL 文件：大小未知（permission denied）（观测于 ${at(observedAt)}）`);
  expect(screen.queryByText(/0 B/)).toBeNull();
  expect(screen.queryByText(/0 字节/)).toBeNull();
});

it("没有 result 分支时显示未知，不补成 0 字节", async () => {
  renderWal(create(WalFileObservationSchema, { observedAt }));
  expect(await screen.findByText(/数据库逻辑大小/)).toBeInTheDocument();
  const line = screen.queryByText(/^WAL 文件：/);
  expect(line).not.toBeNull();
  expect(line?.textContent).not.toContain("0 B");
  expect(line?.textContent).not.toContain("0 字节");
  expect(line?.textContent).not.toContain("无 WAL 文件");
  expect(line?.textContent).not.toContain("大小未知");
  expect(line).toHaveTextContent(`WAL 文件：未知（观测于 ${at(observedAt)}）`);
});

it("WAL 超过上限 4 倍时在观测行下给出提示，等于 4 倍不提示", async () => {
  const over = create(GetStorageStatsResponseSchema, {
    dbBytes: 4096n, sqlObservedAt: 1_767_220_000n, cleanupPending: 0n, walSizeLimitBytes: limit,
    wal: create(WalFileObservationSchema, { observedAt, result: { case: "bytes", value: 4n * limit + 1n } }),
  });
  renderWithAdmin({ getStorageStats: async () => over }, routes, "/storage");
  expect(await screen.findByText(/^当前 WAL 文件长度超过/)).toHaveTextContent(
    "当前 WAL 文件长度超过 64 MiB；文件长度不等于未落盘数据量，若持续增长请排查长读事务与写入负载。",
  );
});

it.each([
  ["等于 4 倍", { case: "bytes" as const, value: 4n * limit }, limit],
  ["absent", { case: "absent" as const, value: true }, limit],
  ["error", { case: "error" as const, value: "permission denied" }, limit],
])("WAL 提示不出现：%s", async (_name, result, walSizeLimitBytes) => {
  const response = create(GetStorageStatsResponseSchema, {
    dbBytes: 4096n, sqlObservedAt: 1_767_220_000n, cleanupPending: 0n, walSizeLimitBytes, wal: create(WalFileObservationSchema, { observedAt, result }),
  });
  renderWithAdmin({ getStorageStats: async () => response }, routes, "/storage");
  await screen.findByText(/^WAL 文件：/);
  expect(screen.queryByText(/当前 WAL 文件长度超过/)).toBeNull();
});

it("列出迁移前备份：路径、schema 版本、大小、修改时间，按 hub 给的顺序", async () => {
  const response = create(GetStorageStatsResponseSchema, {
    dbBytes: 4096n, sqlObservedAt: 1_767_220_000n, cleanupPending: 0n, walSizeLimitBytes: limit, wal: byteWal(1n),
    schemaBackups: [
      { path: "/var/lib/heron/heron.db.before-schema-40.3", schemaVersion: 40, bytes: 2048n, modifiedAt: 1_767_000_000n },
      { path: "/var/lib/heron/heron.db.before-schema-39.2", schemaVersion: 39, bytes: 1024n, modifiedAt: 1_768_000_000n },
    ],
  });
  renderWithAdmin({ getStorageStats: async () => response }, routes, "/storage");
  const region = await screen.findByRole("region", { name: "迁移前备份" });
  const rows = within(region).getAllByRole("row").slice(1).map((r) => within(r).getAllByRole("cell").map((c) => c.textContent));
  expect(rows).toEqual([
    ["/var/lib/heron/heron.db.before-schema-40.3", "40", "2.0 KiB", at(1_767_000_000n)],
    ["/var/lib/heron/heron.db.before-schema-39.2", "39", "1.0 KiB", at(1_768_000_000n)],
  ]);
  expect(screen.queryByText("迁移前备份：无")).toBeNull();
});

it("没有迁移前备份时一句说明", async () => {
  renderWithAdmin({ getStorageStats: async () => create(GetStorageStatsResponseSchema, { dbBytes: 4096n, sqlObservedAt: 1_767_220_000n, cleanupPending: 0n, walSizeLimitBytes: limit, wal: byteWal(1n) }) }, routes, "/storage");
  expect(await screen.findByText("迁移前备份：无")).toBeInTheDocument();
  expect(screen.queryByRole("region", { name: "迁移前备份" })).toBeNull();
});

it("列举备份失败时显示读取失败，不说没有备份", async () => {
  const response = create(GetStorageStatsResponseSchema, { dbBytes: 4096n, sqlObservedAt: 1_767_220_000n, cleanupPending: 0n, walSizeLimitBytes: limit, wal: byteWal(1n), schemaBackupsError: "open /var/lib/heron: permission denied" });
  renderWithAdmin({ getStorageStats: async () => response }, routes, "/storage");
  expect(await screen.findByText(/^无法读取迁移前备份：/)).toHaveTextContent("无法读取迁移前备份：open /var/lib/heron: permission denied");
  expect(screen.queryByText("迁移前备份：无")).toBeNull();
});

it("有长度且上限为 0 时不把 0 当成不下发而不提示", async () => {
  const response = create(GetStorageStatsResponseSchema, {
    dbBytes: 4096n, sqlObservedAt: 1_767_220_000n, cleanupPending: 0n, walSizeLimitBytes: 0n,
    wal: create(WalFileObservationSchema, { observedAt, result: { case: "bytes", value: 1n << 40n } }),
  });
  renderWithAdmin({ getStorageStats: async () => response }, [{ path: "/storage", Component: GuardedStorage }], "/storage");
  expect(await screen.findByRole("alert")).toHaveTextContent("wal_size_limit_bytes");
});

it("上限为 0 不再表示没有这一项：空列表仍是没有备份", async () => {
  const response = create(GetStorageStatsResponseSchema, {
    dbBytes: 4096n, sqlObservedAt: 1_767_220_000n, cleanupPending: 0n, walSizeLimitBytes: 0n,
    wal: create(WalFileObservationSchema, { observedAt, result: { case: "absent", value: true } }),
  });
  renderWithAdmin({ getStorageStats: async () => response }, routes, "/storage");
  expect(await screen.findByText("迁移前备份：无")).toBeInTheDocument();
});
