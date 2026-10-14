import { ResourceScope, ScopeKind, ScopeNote, type ExecutionScope as Scope } from "../gen/heron/v1/types_pb";
import { bytes } from "../lib/format";
import { Timestamp } from "./Timestamp";

const KINDS: Partial<Record<ScopeKind, string>> = {
  [ScopeKind.HOST]: "整台主机",
  [ScopeKind.CGROUP_NAMESPACE]: "容器或 guest 的 cgroup",
  [ScopeKind.CGROUP_V1_LEGACY]: "旧读法，范围未区分",
  [ScopeKind.IDENTIFY_FAILED]: "识别失败",
};

const SCOPES: Partial<Record<ResourceScope, string>> = {
  [ResourceScope.HOST]: "整台主机",
  [ResourceScope.ENVIRONMENT]: "所在环境",
  [ResourceScope.UNKNOWN]: "无法确定",
  [ResourceScope.LEGACY]: "旧读法，范围未区分",
};

const NOTES: Partial<Record<ScopeNote, string>> = {
  [ScopeNote.CONTAINER_SIGNAL_ON_HOST_ROOT]: "在容器中运行，读数来自整台主机",
  [ScopeNote.MOUNTINFO_UNREADABLE]: "/proc/self/mountinfo 读不了，文件来源无法确定",
  [ScopeNote.LOADAVG_NOT_PROCFS]: "loadavg 不是 procfs，负载范围无法确定",
  [ScopeNote.CPUINFO_NOT_PROCFS]: "cpuinfo 不是 procfs，主机核数无从得知",
  [ScopeNote.MEMORY_CONTROLLER_MISSING]: "环境的 memory 控制器缺失或读不出",
  [ScopeNote.SWAP_ACCOUNTING_MISSING]: "环境的 swap 记账缺失或读不出",
  [ScopeNote.PROC_STAT_NOT_PROCFS]: "/proc/stat 不是 procfs，CPU 缺读数",
  [ScopeNote.MEMINFO_UNUSABLE]: "meminfo 的来源不可用",
  [ScopeNote.CPU_CONTROLLER_UNREADABLE]: "环境的 CPU 控制器读不出",
};

function cores(n: number): string {
  return `${n} 核`;
}

// 范围未知时协议不给容量，只显示范围。范围已知而容量缺席是"读不出"，不是 0：swap 的 0 是已知值（没有 swap），
// 照常显示。
function resourceText<T>(scope: ResourceScope, label: string, value: T | undefined, format: (v: T) => string): string {
  const text = SCOPES[scope] ?? "未识别";
  if (scope === ResourceScope.UNKNOWN) return text;
  return value === undefined ? `${text}，${label}读不出` : `${text}，${label} ${format(value)}`;
}

export function ExecutionScope({ execution, updatedAt }: { execution?: Scope; updatedAt?: bigint }) {
  const notes = (execution?.notes ?? []).flatMap((note) => {
    const text = NOTES[note];
    return text ? [text] : [];
  });
  return <section className="card" aria-label="执行环境">
    <h2>执行环境</h2>
    {!execution ? <p className="muted">未上报</p> : <>
      <p className="muted">采样来源取自最近保存的 Facts，可能滞后于实时指标。容量是可见上限，不是机器的实际容量。</p>
      <dl className="facts">
        <dt>保存时间</dt><dd>{updatedAt && updatedAt > 0n ? <Timestamp at={updatedAt} /> : "未知"}</dd>
        <dt>整体范围</dt><dd>{KINDS[execution.kind] ?? "未识别"}</dd>
        <dt>CPU</dt><dd>{resourceText(execution.cpu, "可见上限", execution.cpuEffectiveCores, cores)}</dd>
        <dt>内存</dt><dd>{resourceText(execution.memory, "可见上限", execution.memoryLimitBytes, bytes)}</dd>
        <dt>swap</dt><dd>{resourceText(execution.swap, "可见上限", execution.swapLimitBytes, bytes)}</dd>
        {/* load_cores 是按核负载的分母（主机核数），不是负载的上限。 */}
        <dt>负载</dt><dd>{resourceText(execution.load, "按核负载的分母", execution.loadCores, cores)}</dd>
        {notes.length > 0 && <><dt>说明</dt><dd>{notes.join("；")}</dd></>}
      </dl>
    </>}
  </section>;
}
