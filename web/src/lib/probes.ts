import type { AlignedData } from "uplot";
import type { ProbeSample, ProbeSeries, QueryProbesResponse } from "../gen/heron/v1/query_pb";
import { ProbeKind } from "../gen/heron/v1/types_pb";
import { gridOf } from "./series";

export type ProbeValue = (s: ProbeSample) => number | null;

// hub 的 Registry.Save 经 probelimit.CheckTask 准入，类型标签与选项依赖该准入。
export const PROBE_KINDS: readonly { kind: ProbeKind; label: string }[] = [
  { kind: ProbeKind.ICMP, label: "ICMP" },
  { kind: ProbeKind.TCP, label: "TCP" },
  { kind: ProbeKind.HTTP, label: "HTTP" },
  { kind: ProbeKind.DNS, label: "DNS" },
];
export const kindLabel = (kind: ProbeKind): string => PROBE_KINDS.find((entry) => entry.kind === kind)!.label;

// 按种类的目标约束与 probelimit.CheckTask 同源：页面只用原生表单属性做提示，最终裁决在 hub。
export const targetRule = (kind: ProbeKind): { placeholder: string; maxLength: number } => {
  switch (kind) {
    case ProbeKind.TCP:
      return { placeholder: "host:port", maxLength: 253 };
    case ProbeKind.HTTP:
      return { placeholder: "https://example.com/path", maxLength: 512 };
    case ProbeKind.DNS:
      return { placeholder: "要解析的 DNS 名", maxLength: 253 };
    default:
      return { placeholder: "IP 或主机名", maxLength: 253 };
  }
};

// 与 hub 的 probelimit.IsHTTPSTarget 同一判据：HTTP 任务，scheme 不分大小写是 https（hub 用 url.Parse 解析，
// scheme 转小写后比较）。钉指纹、证书观测与证书到期规则都只对这类任务成立；各处都调用这一个函数，
// 不各写一份大小写敏感的前缀判断——那会让 "HTTPS://" 的任务在页面与 hub 上得到相反的结论。
export const isHTTPSTarget = (kind: ProbeKind, target: string): boolean => kind === ProbeKind.HTTP && /^https:\/\//i.test(target.trim());

// 丢包率只看超时：error 是本地无法发起（无 socket、解析失败），不是链路事实。
// hub 只返回 sent > 0 的点；这里仍显式守住除零，让不变式不依赖上游。
export const lossPercent: ProbeValue = (s) => (s.sent > 0 ? (s.lost / s.sent) * 100 : null);
// rtt 只在该点有成功探测时存在；没有就是空洞，不能画成 0。
export const rttMeanMs: ProbeValue = (s) => (s.rttMeanUs === undefined ? null : s.rttMeanUs / 1000);
export const rttMinMs: ProbeValue = (s) => (s.rttMinUs === undefined ? null : s.rttMinUs / 1000);
export const rttMaxMs: ProbeValue = (s) => (s.rttMaxUs === undefined ? null : s.rttMaxUs / 1000);

// 同一任务的取值共用时间网格，Chart 据此对齐均值与最小 / 最大之间的填充带。
export function toProbeTaskAligned(resp: QueryProbesResponse, taskId: bigint, from: number, to: number, values: readonly ProbeValue[]): AlignedData {
  const xs = gridOf(resp.stepS, from, to);
  const at = new Map<number, ProbeSample>();
  resp.series.find((s) => s.taskId === taskId)?.samples.forEach((s) => at.set(Number(s.ts), s));
  const columns = values.map((value) => xs.map((t) => {
    const s = at.get(t);
    return s === undefined ? null : value(s);
  }));
  return [xs, ...columns] as AlignedData;
}

// 每个任务一列，网格规则与指标图相同，缺 ts 为 null；同一窗口的探测图与指标图因此可以对齐比较。
export function toProbeAligned(resp: QueryProbesResponse, taskIds: bigint[], from: number, to: number, value: ProbeValue): AlignedData {
  const xs = gridOf(resp.stepS, from, to);
  const columns = taskIds.map((id) => {
    const at = new Map<number, ProbeSample>();
    resp.series.find((s) => s.taskId === id)?.samples.forEach((s) => at.set(Number(s.ts), s));
    return xs.map((t) => {
      const s = at.get(t);
      return s === undefined ? null : value(s);
    });
  });
  return [xs, ...columns] as AlignedData;
}

export function taskIdsOf(resp: QueryProbesResponse): bigint[] {
  return resp.series.map((s) => s.taskId);
}

// 序列自带任务的种类与目标（hub 查询时从任务清单读出）。kind 为 UNSPECIFIED 表示 hub 未标注，退回编号；
// 未标注的原因见 ProbeSeries 的注释：管理端是调用方看不到这个任务（受限 token 的范围之外），公开端是任务已从该节点撤下；
// 已删除的任务根本不出现在序列里，所以未标注不能当成"已删除"显示。
// hub 的任务准入只放行 PROBE_KINDS 里的种类（probelimit.CheckTask），所以其余取值只会是 UNSPECIFIED。
export function seriesLabel(s: ProbeSeries): string {
  return s.kind === ProbeKind.UNSPECIFIED ? `任务 #${s.taskId}` : `${kindLabel(s.kind)} ${s.target}`;
}

export function seriesLabels(series: readonly ProbeSeries[]): string[] {
  return disambiguate(series.map(seriesLabel), series.map((s) => s.taskId));
}

// 同一窗口可有配置不同却同名的任务；只给碰撞的标签追加编号，保留常见标签的简短形式。
export function disambiguate(labels: string[], ids: readonly bigint[]): string[] {
  const counts = new Map<string, number>();
  for (const label of labels) counts.set(label, (counts.get(label) ?? 0) + 1);
  return labels.map((label, i) => (counts.get(label)! > 1 ? `${label} #${ids[i]}` : label));
}
