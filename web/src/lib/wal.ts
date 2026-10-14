import type { WalFileObservation } from "../gen/heron/v1/admin_pb";
import { bytes } from "./format";

// WAL 文件观测的显示口径，与 WalFileObservation / GetStorageStatsResponse.wal 的注释一致：
// GetStorageStats 总是返回 wal（internal/hub/api/settings.go 的 walObservationProto）。缺席不是无文件，也不是 0 字节。
// - bytes：stat 成功时的实际文件长度。0 表示文件存在且长度为 0，不得显示成无文件。
// - absent 且值为 true：stat 得到 ENOENT，文案是“无 WAL 文件”。
// - error：stat 的其他失败，大小未知；附上 hub 给的错误文本只说明原因，不得换成 0 或无文件。
// - 没有 result、absent 不是 true、或出现未识别的分支：显示“未知”，不补零。
// 长度只是 -wal 文件的字节数，不是未检查点的数据量；这里不附加阈值或危险判断，超限提示另由 walOverLimitHint 给出。
export type WalObservationView =
  | { kind: "bytes"; observedAt: bigint; text: string }
  | { kind: "absent"; observedAt: bigint; text: string }
  | { kind: "error"; observedAt: bigint; text: string }
  | { kind: "unknown"; observedAt: bigint; text: string };

export function walObservationView(wal: WalFileObservation | undefined): WalObservationView {
  if (!wal) throw new Error("GetStorageStats.wal is always set");
  const observedAt = wal.observedAt;
  const result = wal.result;
  switch (result?.case) {
    case "bytes":
      return { kind: "bytes", observedAt, text: bytes(result.value) };
    case "absent":
      // 协议只把 true 定义为 ENOENT。false 不是“无文件”，也不是 0。
      if (result.value !== true) return { kind: "unknown", observedAt, text: "未知" };
      return { kind: "absent", observedAt, text: "无 WAL 文件" };
    case "error":
      return { kind: "error", observedAt, text: result.value ? `大小未知（${result.value}）` : "大小未知" };
    default:
      return { kind: "unknown", observedAt, text: "未知" };
  }
}

// 超限提示的倍数：WAL 文件长度严格大于上限的这么多倍才提示。上限只在 WAL 重置时生效：长度远超它，
// 说明自从长到这么大之后还没有发生过重置；长读事务会挡住重置，大批写入会把文件撑大，单次读数区分不了是哪一种，
// 所以文案只建议排查这两处、不下结论。
export const walHintFactor = 4n;

// WAL 超限提示，口径：
// - 只认 present 观测（bytes 分支）：absent、error、未知都没有长度可比，不提示。
// - 上限 limit 取 hub 下发的 wal_size_limit_bytes（store 的 journal_size_limit，GetStorageStats 总是给出非零值），面板不另写常量。
//   有长度时 0 不是合法上限，不能当成“不提示”。
// - bytes > walHintFactor × limit 才提示，等于不提示。
// - 文案只陈述长度超过多少，不作诊断：文件长度不等于未落盘的数据量（见 walObservationView 的说明）。
export function walOverLimitHint(wal: WalFileObservation | undefined, limit: bigint): string | undefined {
  if (!wal) throw new Error("GetStorageStats.wal is always set");
  if (wal.result.case !== "bytes") return undefined;
  if (limit <= 0n) throw new Error("GetStorageStats.wal_size_limit_bytes is always a positive journal_size_limit");
  const threshold = walHintFactor * limit;
  if (wal.result.value <= threshold) return undefined;
  return `当前 WAL 文件长度超过 ${bytes(threshold)}；文件长度不等于未落盘数据量，若持续增长请排查长读事务与写入负载。`;
}
