import type { AlignedData } from "uplot";
import type { ProbeSample, ProbeSeries, QueryProbesResponse } from "../gen/heron/v1/query_pb";
import { DnsRecordType, HttpStatusPolicy, ProbeIpFamily, ProbeKind, type ProbeTask } from "../gen/heron/v1/types_pb";
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

// 探测地址族只对 ICMP、TCP、HTTP 适用，DNS 用记录类型；与 probelimit.CheckTask 的适用范围同源，页面只提交适用的那一项。
export const PROBE_FAMILIES: readonly { family: ProbeIpFamily; label: string }[] = [
  { family: ProbeIpFamily.UNSPECIFIED, label: "自动" },
  { family: ProbeIpFamily.IPV4, label: "IPv4" },
  { family: ProbeIpFamily.IPV6, label: "IPv6" },
];
export const DNS_RECORD_TYPES: readonly { type: DnsRecordType; label: string }[] = [
  { type: DnsRecordType.UNSPECIFIED, label: "A" },
  { type: DnsRecordType.AAAA, label: "AAAA" },
];
export const familyApplies = (kind: ProbeKind): boolean => kind !== ProbeKind.DNS;

// HTTP 状态码判据只对 HTTP 适用，与 probelimit.CheckTask 同源。严格 2xx 排第一：管理端新建 HTTP 任务默认选它，
// 协议缺省的 < 400 会把反代跳去登录页或维护页的 3xx 也算作正常，只留给存量任务与明确要它的人。
export const HTTP_STATUS_POLICIES: readonly { policy: HttpStatusPolicy; label: string }[] = [
  { policy: HttpStatusPolicy.SUCCESS, label: "2xx" },
  { policy: HttpStatusPolicy.UNSPECIFIED, label: "< 400" },
  { policy: HttpStatusPolicy.EXACT, label: "指定状态码" },
];
// 精确状态码的取值范围，与 probelimit 的 MinHTTPStatus、MaxHTTPStatus 同源。
export const HTTP_STATUS_RANGE = { min: 100, max: 599 } as const;
const statusPolicyLabel = (task: ProbeTask): string => task.httpStatusPolicy === HttpStatusPolicy.EXACT
  ? String(task.httpExpectedStatus)
  : HTTP_STATUS_POLICIES.find((entry) => entry.policy === task.httpStatusPolicy)?.label ?? `判据 ${task.httpStatusPolicy}`;

// 列表里的类型：缺省（自动地址族、A 记录）只写种类，非缺省值跟在后面。HTTP 的状态码判据总是写出来：缺省的 < 400
// 恰是会把跳转当正常的那一种，标出来才看得出哪些存量任务还在用它。
export const taskTypeLabel = (task: ProbeTask): string => {
  const extra = task.kind === ProbeKind.DNS
    ? (task.dnsRecordType !== DnsRecordType.UNSPECIFIED ? DNS_RECORD_TYPES.find((entry) => entry.type === task.dnsRecordType)?.label : undefined)
    : (task.ipFamily !== ProbeIpFamily.UNSPECIFIED ? PROBE_FAMILIES.find((entry) => entry.family === task.ipFamily)?.label : undefined);
  return [kindLabel(task.kind), extra, task.kind === ProbeKind.HTTP ? statusPolicyLabel(task) : undefined].filter(Boolean).join(" · ");
};

// 用一个地址替换目标里的主机，生成该种类的目标写法（DNS 的目标是名字，不经这里）。TCP 保留原目标的端口，没有端口时
// 留出冒号由人补上；IPv6 按 probelimit 的规范形式加方括号（net.JoinHostPort）。HTTP 只换主机，协议、端口、路径照旧，
// 原目标不是 http(s) URL 时给出 http://地址/。
export function targetWithAddress(kind: ProbeKind, current: string, address: string, v6: boolean): string {
  const host = v6 ? `[${address}]` : address;
  switch (kind) {
    case ProbeKind.TCP: {
      const text = current.trim();
      const port = (text.startsWith("[") ? /^\[[^\]]*\]:(\d*)$/.exec(text) : /^[^:]*:(\d*)$/.exec(text))?.[1] ?? "";
      return `${host}:${port}`;
    }
    case ProbeKind.HTTP: {
      try {
        const url = new URL(current.trim());
        if (url.protocol === "http:" || url.protocol === "https:") {
          url.hostname = host;
          return url.toString();
        }
      } catch {
        // 不是绝对 URL：按新目标生成。
      }
      return `http://${host}/`;
    }
    default:
      return address;
  }
}

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

// 丢包率与 hub 的 ProbeBucket.LossPct 同一口径：分子只计超时，分母是有效测量数 sent − errors。error 是本机
// 没能发起测量（缺地址族、无 socket、解析失败），不是可达性事实，既不算丢也不算通——进分母会把链路真实的丢包
// 按本机错误的比例稀释掉。全是 error 的点（含 sent 为 0）没有丢包率，返回 null 而不是 0%——画成 0% 会把本机
// 失去地址的时段显示成链路完好。单点与窗口汇总都经这一个函数。
export const lossOf = (sent: number, lost: number, errors: number): number | null => (sent > errors ? (lost / (sent - errors)) * 100 : null);
export const lossPercent: ProbeValue = (s) => lossOf(s.sent, s.lost, s.errors);
// rtt 只在该点有成功探测时存在；没有就是空洞，不能画成 0。
export const rttMeanMs: ProbeValue = (s) => (s.rttMeanUs === undefined ? null : s.rttMeanUs / 1000);
export const rttMinMs: ProbeValue = (s) => (s.rttMinUs === undefined ? null : s.rttMinUs / 1000);
export const rttMaxMs: ProbeValue = (s) => (s.rttMaxUs === undefined ? null : s.rttMaxUs / 1000);

// 一组样本在整个窗口上的汇总：单节点探测历史的每个任务、跨节点对比的每个节点都经它算，晚高峰切片只换喂进来的样本。
// 每个桶的成功数可能不同，窗口均值按成功数加权，不能简单平均桶均值；最小 / 最大取各桶的最值。
// ProbeSample 的 sent 包含超时与本地错误；丢包率按 lossOf 的口径（全是错误时没有丢包率），错误单独累计。
// 没有成功探测时三个 RTT 都缺。
export type ProbeSummary = { mean: number | null; min: number | null; max: number | null; lossPercent: number | null; errors: number };

export function summarizeProbes(samples: readonly ProbeSample[]): ProbeSummary {
  let sent = 0, lost = 0, errors = 0, weighted = 0, ok = 0;
  let min: number | null = null, max: number | null = null;
  for (const s of samples) {
    sent += s.sent;
    lost += s.lost;
    errors += s.errors;
    const succeeded = s.sent - s.lost - s.errors;
    if (s.rttMeanUs !== undefined && succeeded > 0) {
      weighted += (s.rttMeanUs / 1000) * succeeded;
      ok += succeeded;
    }
    if (s.rttMinUs !== undefined) min = min === null ? s.rttMinUs / 1000 : Math.min(min, s.rttMinUs / 1000);
    if (s.rttMaxUs !== undefined) max = max === null ? s.rttMaxUs / 1000 : Math.max(max, s.rttMaxUs / 1000);
  }
  return { mean: ok > 0 ? weighted / ok : null, min, max, lossPercent: lossOf(sent, lost, errors), errors };
}

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
