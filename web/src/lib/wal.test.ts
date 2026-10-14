import { create } from "@bufbuild/protobuf";
import { expect, it } from "vitest";
import { WalFileObservationSchema } from "../gen/heron/v1/admin_pb";
import { walObservationView, walOverLimitHint } from "./wal";

const observedAt = 1_767_230_000n;

it("wal 由 GetStorageStats 总是填充：缺席不是不显示", () => {
  expect(() => walObservationView(undefined)).toThrow(/\.wal/);
});

it("bytes 为 0 是文件存在且长度为 0，不是无文件", () => {
  const view = walObservationView(create(WalFileObservationSchema, {
    observedAt,
    result: { case: "bytes", value: 0n },
  }));
  expect("text" in view ? view.text : "").not.toContain("无 WAL 文件");
  expect(view.kind).not.toBe("absent");
  expect(view).toEqual({ kind: "bytes", observedAt, text: "0 B" });
});

it("bytes 非 0 只格式化实际长度，不附加判断", () => {
  const view = walObservationView(create(WalFileObservationSchema, {
    observedAt,
    result: { case: "bytes", value: 4096n },
  }));
  expect(view).toEqual({ kind: "bytes", observedAt, text: "4.0 KiB" });
});

it("absent 为 true → 无 WAL 文件", () => {
  const view = walObservationView(create(WalFileObservationSchema, {
    observedAt,
    result: { case: "absent", value: true },
  }));
  expect(view).toEqual({ kind: "absent", observedAt, text: "无 WAL 文件" });
});

it("error 是大小未知，不是 0 字节也不是无文件；错误文本只作原因", () => {
  const view = walObservationView(create(WalFileObservationSchema, {
    observedAt,
    result: { case: "error", value: "permission denied" },
  }));
  const text = "text" in view ? view.text : "";
  expect(text).not.toContain("0 B");
  expect(text).not.toContain("0 字节");
  expect(text).not.toContain("无 WAL 文件");
  expect(view).toEqual({ kind: "error", observedAt, text: "大小未知（permission denied）" });
});

it("error 为空串仍是大小未知，不补成 0", () => {
  const view = walObservationView(create(WalFileObservationSchema, {
    observedAt,
    result: { case: "error", value: "" },
  }));
  expect(view).toEqual({ kind: "error", observedAt, text: "大小未知" });
});

it("没有 result 分支 → 未知，不补零", () => {
  expect(walObservationView(create(WalFileObservationSchema, { observedAt }))).toEqual({
    kind: "unknown",
    observedAt,
    text: "未知",
  });
});

it("absent 不是 true → 未知，不当成无文件", () => {
  const view = walObservationView(create(WalFileObservationSchema, {
    observedAt,
    result: { case: "absent", value: false },
  }));
  expect(view).toEqual({ kind: "unknown", observedAt, text: "未知" });
});

it("未识别的 result 分支 → 未知，不补零", () => {
  const wal = create(WalFileObservationSchema, { observedAt });
  // 生成类型只有当前三个分支；运行时仍可能遇到更新的 hub。断言走的是赋值后的对象，不是类型。
  (wal as { result: { case: string; value: unknown } }).result = { case: "checkpointed", value: 0 };
  expect(walObservationView(wal)).toEqual({ kind: "unknown", observedAt, text: "未知" });
});

const limit = 16n << 20n;
const present = (value: bigint) => create(WalFileObservationSchema, { observedAt, result: { case: "bytes", value } });

it("WAL 长度等于上限的 4 倍不提示，多一个字节才提示，文案带 4 倍上限且不作诊断", () => {
  expect(walOverLimitHint(present(4n * limit), limit)).toBeUndefined();
  expect(walOverLimitHint(present(4n * limit + 1n), limit)).toBe(
    "当前 WAL 文件长度超过 64 MiB；文件长度不等于未落盘数据量，若持续增长请排查长读事务与写入负载。",
  );
});

it("2 倍、3 倍上限都不提示", () => {
  expect(walOverLimitHint(present(2n * limit + 1n), limit)).toBeUndefined();
  expect(walOverLimitHint(present(3n * limit + 1n), limit)).toBeUndefined();
});

it("没有长度的观测（absent、error、无分支）不提示，再大的上限倍数也不补", () => {
  expect(walOverLimitHint(create(WalFileObservationSchema, { observedAt, result: { case: "absent", value: true } }), limit)).toBeUndefined();
  expect(walOverLimitHint(create(WalFileObservationSchema, { observedAt, result: { case: "error", value: "permission denied" } }), limit)).toBeUndefined();
  expect(walOverLimitHint(create(WalFileObservationSchema, { observedAt }), limit)).toBeUndefined();
});

it("wal 缺席时超限提示不把缺席当成没有长度", () => {
  expect(() => walOverLimitHint(undefined, limit)).toThrow(/\.wal/);
});

it("wal_size_limit_bytes 由 hub 总是给出正数：0 不是不提示", () => {
  expect(() => walOverLimitHint(present(1n << 40n), 0n)).toThrow(/wal_size_limit_bytes/);
});
