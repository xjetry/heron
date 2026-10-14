import type { DescMethodUnary } from "@bufbuild/protobuf";
import { useQuery } from "@connectrpc/connect-query";
import { skipToken } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";
import { useRetained } from "../api/useRetained";
import type { QueryMetricsRequestSchema, QueryMetricsResponseSchema, QueryProbesRequestSchema, QueryProbesResponseSchema } from "../gen/heron/v1/query_pb";
import { ProbeKind } from "../gen/heron/v1/types_pb";
import { coverageView } from "../lib/coverage";
import { rttMeanMs, rttMinMs, rttMaxMs, seriesLabels, toProbeTaskAligned } from "../lib/probes";
import { toAligned, unitOf, type SeriesSelection } from "../lib/series";
import { Chart } from "./Chart";
import { InfoTip } from "./InfoTip";

// 对比图与历史图用同一组窗口、同一个 maxPoints：同一段时间才会落到同一级。
export const HISTORY_MAX_POINTS = 1000;
export const RANGES = [
  { label: "1h", seconds: 3600 },
  { label: "6h", seconds: 6 * 3600 },
  { label: "24h", seconds: 86400 },
  { label: "7d", seconds: 7 * 86400 },
  { label: "30d", seconds: 30 * 86400 },
] as const;
export type HistoryRange = (typeof RANGES)[number];

function rangeStaleText(label: string): string {
  return `图表还不是 ${label} 窗口的结果，取到之后会更新`;
}

// now 由页面的 hub 轮询响应提供，浏览器墙钟不参与窗口计算。按 hub 分钟归一化右端，
// 同一分钟不换查询键，公开 GET 的 URL 也不因访客时钟或进入页面的秒数而分裂。
export function useTimeWindow(now: number) {
  const [range, setRange] = useState<HistoryRange>(RANGES[2]);
  const to = (Math.floor(now / 60) + 1) * 60;
  return { range, setRange, from: to - range.seconds, to };
}

export function RangeButtons({ range, setRange }: { range: HistoryRange; setRange: (range: HistoryRange) => void }) {
  return (
    <nav aria-label="时间窗口" className="segmented">
      {RANGES.map((r) => (
        <button key={r.label} type="button" onClick={() => setRange(r)} aria-pressed={r.label === range.label}>
          {r.label}
        </button>
      ))}
    </nav>
  );
}

// 级别与口径收进说明，窗口过期与刷新中仍是可见状态；调用方与 RangeButtons 共用 .row 头部的间距。
export function RangeStatus({ shown, stale, rangeLabel, updating = false, note = PEAK_NOTE }: {
  shown?: { level: string; stepS: number } | null; stale: boolean; rangeLabel: string; updating?: boolean; note?: ReactNode;
}) {
  return (
    <>
      {shown && <InfoTip label="口径说明">级别 {shown.level}，每点 {shown.stepS} 秒。{note}</InfoTip>}
      {stale && <span className="muted">{rangeStaleText(rangeLabel)}</span>}
      {shown && updating && <span className="muted">更新中</span>}
    </>
  );
}

export const PEAK_NOTE = "峰值为每个图表时间桶内已采集样本的最大值，不代表采样间隔内的瞬时最高值；缺少峰值时留空。";

// 指标名与 hub 的描述表一致；网络均值由累计字节增量除以桶宽，峰值取 agent 已测得的速率，
// 两者来源不同，不能以均值补峰值。可加量的数据单位是 bytes，图上速率统一指定为 bytes/s。
const PANELS: { title: string; selections: SeriesSelection[]; unit?: string }[] = [
  { title: "CPU", selections: [
    { name: "cpu", value: "mean", label: "CPU 均值" },
    { name: "cpu", value: "max", label: "CPU 峰值" },
  ] },
  { title: "内存 / 交换", selections: [
    { name: "mem_used", value: "mean", label: "内存均值" },
    { name: "mem_used", value: "max", label: "内存峰值" },
    { name: "swap_used", value: "mean", label: "交换均值" },
  ] },
  { title: "磁盘", selections: [{ name: "disk_used", value: "mean", label: "已用均值" }] },
  { title: "负载（1 分钟）", selections: [{ name: "load1", value: "mean", label: "负载均值" }] },
  // 按核负载约是 load1 除以核数，多核机器上比 load1 小一个数量级；一张图只有一根纵轴，放在一起会被压成贴底的
  // 平线，所以单独成图。
  { title: "按核负载（1 分钟）", selections: [{ name: "load1_per_core", value: "mean", label: "按核负载均值" }] },
  { title: "连接数", selections: [
    { name: "tcp", value: "mean", label: "TCP 均值" },
    { name: "udp", value: "mean", label: "UDP 均值" },
  ] },
  { title: "进程数", selections: [{ name: "procs", value: "mean", label: "进程均值" }] },
  { title: "网络", unit: "bytes/s", selections: [
    { name: "rx_bytes", value: "sum-rate", label: "下行均值" },
    { name: "tx_bytes", value: "sum-rate", label: "上行均值" },
    { name: "net_rx_bps", value: "max", label: "下行峰值" },
    { name: "net_tx_bps", value: "max", label: "上行峰值" },
  ] },
  // 磁盘速率与网络同为 bytes/s，轴按该轴的最大刻度统一选倍率（axisValues）；缺桶与 n=0 都是空洞。
  { title: "磁盘速率", unit: "bytes/s", selections: [
    { name: "disk_read_bps", value: "mean", label: "读均值" },
    { name: "disk_write_bps", value: "mean", label: "写均值" },
  ] },
  // steal 与 iowait 是 cpu_pct 之外的独立占比，各自一条线；固定 0–100 的百分比轴。
  { title: "CPU steal / iowait", unit: "percent", selections: [
    { name: "cpu_steal_pct", value: "mean", label: "steal 均值" },
    { name: "cpu_iowait_pct", value: "mean", label: "iowait 均值" },
  ] },
  // hub 对建连速率保留桶内均值与最大采样值，toAligned 按所选统计量取值、不互相补齐；因此主动、被动各画均值与峰值，避免短时风暴被均值抹平。
  { title: "TCP 建连", unit: "count/s", selections: [
    { name: "tcp_active_opens_ps", value: "mean", label: "主动均值" },
    { name: "tcp_passive_opens_ps", value: "mean", label: "被动均值" },
    { name: "tcp_active_opens_ps", value: "max", label: "主动峰值" },
    { name: "tcp_passive_opens_ps", value: "max", label: "被动峰值" },
  ] },
  // TCP 两列由同次采样产生，重传率须先各自求和再相除；toAligned 的 ratio 用 mean × n 还原和，并把缺读数或发出和为 0 的桶留为空洞。
  { title: "TCP 重传率", unit: "percent", selections: [
    { name: "tcp_retrans_segs_ps", over: "tcp_out_segs_ps", value: "ratio", label: "重传率" },
  ] },
];

// 两族历史查询在管理与公开两个服务上各有一份，请求与响应类型相同（query.proto）。图表按此共用，
// 调用方只决定查哪个服务；本文件不引用任何服务的生成代码，公开页因此能用它。
export type HistoryMethods = {
  queryMetrics: DescMethodUnary<typeof QueryMetricsRequestSchema, typeof QueryMetricsResponseSchema>;
  queryProbes: DescMethodUnary<typeof QueryProbesRequestSchema, typeof QueryProbesResponseSchema>;
};

// now 是 hub 时钟（秒）；undefined 表示还没取到。窗口右端只取 hub 时钟，没有它就没有合法窗口，此时两族查询都不发——
// 不变式由本 hook 承担，调用方不必为了"拿到 hub 时间再查"另拆一层组件，也就能把 hook 留在不随 tab 等局部 UI 卸载的位置。
export function useHistory(methods: HistoryMethods, nodeId: bigint, now: number | undefined) {
  const { range, setRange, from, to } = useTimeWindow(now ?? 0);
  const request = now === undefined ? skipToken : { nodeId, from: BigInt(from), to: BigInt(to), maxPoints: HISTORY_MAX_POINTS };
  // 窗口右端每分钟前进一次、切换 range 都会换查询键；换键期间或失败时图表与“级别…”标签不能都消失，也不能
  // 沿用别的节点的数据。指标图与“级别…”标签由 metrics.data 派生，探测图由 probes.data 派生：data 回到
  // undefined 时它们都不再渲染。用 useRetained 沿用本节点上一份成功数据直到当前键取到自己的数据为止，失败
  // 只由 error 表达；不用 keepPreviousData——它只在挂起期间补位，请求一失败 data 就回到 undefined。identity
  // 传 nodeId：切到另一个节点时丢掉上一个节点的沿用值，否则新节点还没有自己的数据时会把上一个节点的图表当成
  // 这个节点显示。
  const metrics = useRetained(useQuery(methods.queryMetrics, request), nodeId);
  const probes = useRetained(useQuery(methods.queryProbes, request), nodeId);
  // useRetained 的返回值是一个不带判别字段的普通对象（不像 useQuery 按 status 分支的联合类型），narrow
  // 一次 metrics.data 只窄化这一次属性访问，不会像窄化整个联合类型变量那样带进下面 map 的回调里；
  // 这里先取到本地变量，回调里用的是这个已经排除 undefined 的变量，不是再次访问 metrics.data。
  const charts = useMemo(() => {
    const data = metrics.data;
    return data ? PANELS.map((p) => ({
      ...p,
      labels: p.selections.map((selection) => selection.label),
      soft: p.selections.map((selection) => selection.value === "max"),
      data: toAligned(data, p.selections, from, to),
      unit: p.unit ?? unitOf(data, p.selections[0].name),
    })) : [];
  }, [metrics.data, from, to]);
  // 标签随序列下发（任务当前的种类与目标），与数据同一次响应到达，不另查任务列表。
  const probeTaskCharts = useMemo(() => {
    const data = probes.data;
    if (!data) return [];
    const titles = seriesLabels(data.series);
    return data.series.map((s, i) => ({
      taskId: s.taskId,
      kind: s.kind,
      title: titles[i],
      labels: ["RTT 均值", "最小", "最大"],
      soft: [false, true, true],
      bands: [{ lower: 1, upper: 2 }],
      unit: "ms" as const,
      data: toProbeTaskAligned(data, s.taskId, from, to, [rttMeanMs, rttMinMs, rttMaxMs]),
    }));
  }, [probes.data, from, to]);
  // stale 只说“这份数据不是当前查询键自己的”：窗口右端每分钟前进一次也会换键，请求还没回来的这一小段
  // 时间同样是 stale，但沿用的还是同一个 range，只晚了不到一分钟，不该报成“看错窗口”。这里另记一下
  // “当前沿用值最后一次确认属于哪个 range”：只有 metrics/probes 沿用中、且沿用值所属的 range 与当前
  // 选中的 range 不同——也就是真的切换过 range、新 range 还没有自己的数据——才算“看错窗口”。
  const [metricsRange, setMetricsRange] = useState(range);
  if (!metrics.stale && metrics.data !== undefined && metricsRange !== range) setMetricsRange(range);
  const [probesRange, setProbesRange] = useState(range);
  if (!probes.stale && probes.data !== undefined && probesRange !== range) setProbesRange(range);
  const rangeStale = (metrics.stale && metricsRange !== range) || (probes.stale && probesRange !== range);
  return { range, setRange, metrics, probes, charts, probeTaskCharts, rangeStale };
}

export type HistoryState = ReturnType<typeof useHistory>;

// showCoverage 默认不显示：本组件由管理端与公开页共用，覆盖率口径（hub 的观测与保留期、节点首报）
// 只在管理端展示；默认方向取"不显示"，新调用方忘记传参时覆盖率不会被带到公开页。
export function RangePicker({ history, showCoverage = false }: { history: HistoryState; showCoverage?: boolean }) {
  const { range, setRange, metrics, rangeStale } = history;
  // 查询还没回来时没有响应，不显示覆盖率。响应一旦回来，coverage_summary 由 history.metrics 总是填充。
  const coverage = showCoverage && metrics.data ? coverageView(metrics.data.coverageSummary) : undefined;
  return (
    <>
      <RangeButtons range={range} setRange={setRange} />
      {/* rangeStale 排除了“同一个 range 里晚了不到一分钟”的情况，只在真的换过 range 还没等到新 range
          自己的数据时才出现；不点出来，这里显示的级别与图表会被当成当前选中 range 的结果看。 */}
      <RangeStatus shown={metrics.data} stale={rangeStale} rangeLabel={range.label} />
      {/* 覆盖率取与图表同一次 QueryMetrics 响应的 coverageSummary，不另发请求。读数直接写在工具栏上，口径收进说明。 */}
      {coverage && (
        <span className="muted coverage-note">
          {coverage.kind === "no-start" && "尚无覆盖记录"}
          {coverage.kind === "no-observed" && "无可观测区间"}
          {coverage.kind === "rate" && <>上报覆盖 <span className="num">{coverage.percent}%</span>{coverage.unknown && <>，未知 {coverage.unknown}</>}</>}
          <InfoTip label="上报覆盖率说明">这是 hub 观测到的分钟里节点有上报的比例，不是在线率；hub 未运行、超出保留期等无法观测的时段计为未知。</InfoTip>
        </span>
      )}
    </>
  );
}

export function MetricCharts({ history }: { history: HistoryState }) {
  const { charts } = history;
  return (
    <div className="chart-grid">
      {charts.map((c) => (
        <div className="card chart-card" key={c.title}>
          <h2>{c.title}</h2>
          <Chart data={c.data} labels={c.labels} unit={c.unit} soft={c.soft} />
        </div>
      ))}
    </div>
  );
}

// 未标注任务没有可用的公开对比页，仅显示编号标题；其余任务的链接由调用方提供。
export function ProbeTaskCharts({ history, noProbes, titleLink }: { history: HistoryState; noProbes: ReactNode; titleLink: (taskId: bigint, title: string) => ReactNode }) {
  const { probeTaskCharts, probes } = history;
  if (!probes.data) return null;
  if (probes.data.series.length === 0) return <>{noProbes}</>;
  return (
    <div className="chart-grid">
      {probeTaskCharts.map((c) => (
        <div className="card chart-card" key={String(c.taskId)}>
          <h2>{c.kind === ProbeKind.UNSPECIFIED ? c.title : titleLink(c.taskId, c.title)}</h2>
          <Chart data={c.data} labels={c.labels} unit={c.unit} soft={c.soft} bands={c.bands} />
        </div>
      ))}
    </div>
  );
}
