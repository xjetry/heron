import { type CSSProperties, useEffect, useEffectEvent, useRef, useState } from "react";
import uPlot, { type AlignedData, type Options } from "uplot";
import "uplot/dist/uPlot.min.css";
import { formatUnit } from "../lib/format";
import { axisValues } from "../lib/axis";
import { formatChartTimes, isolatedPointIndices, readingGap, type ReadingGap } from "../lib/chartMarks";
import { resolveColor, useColorScheme } from "../lib/colorScheme";
import { seriesColors } from "../lib/seriesPalette";

// 窄于这个宽度的图按手机刻度：横轴只写时分。
export const COMPACT_WIDTH = 480;
// 相邻刻度至少相隔的像素：uPlot 据此从画布宽度算刻度数，节点页与对比页都不各自设刻度。
export const X_TICK_SPACE = 80;

type Column = readonly (number | null | undefined)[];

function yColumns(data: AlignedData): Column[] {
  const cols: Column[] = [];
  for (let i = 1; i < data.length; i++) cols.push(data[i] as Column);
  return cols;
}

// 文案与「窗口内没有探测结果」同一语气。图级与序列级分开：全部没有读数时不把每条序列的名字再列一遍。
function gapMessage(gap: ReadingGap): string | null {
  if (gap.kind === "chart") return "窗口内没有读数";
  if (gap.kind === "series") return `${gap.labels.join("、")}：窗口内没有读数`;
  return null;
}

function isReading(v: number | null | undefined): v is number {
  return typeof v === "number" && Number.isFinite(v);
}

function lastReadingIndex(col: Column): number | null {
  for (let i = col.length - 1; i >= 0; i--) if (isReading(col[i])) return i;
  return null;
}

type ChartProps = {
  data: AlignedData;
  labels: string[];
  unit: string;
  height?: number;
  soft?: readonly boolean[];
  bands?: readonly { lower: number; upper: number }[];
  legend?: boolean;
  hidden?: ReadonlySet<number>;
  onHiddenChange?: (next: ReadonlySet<number>) => void;
  onFocus?: (index: number | null) => void;
};

// spanGaps 关闭：null 是无读数，线在这里必须断开而不是把两侧连起来。
// 时间不传时区：formatChartTimes 与 uPlot 的刻度对齐都用浏览器本地时区，不引入 hub 时区。
// 图例由本组件自绘：无悬停时每条序列显示它最后一个有限读数，悬停时显示光标所在时刻的值；点击一项隐藏该序列。
// hidden 受控时由调用方持有集合（对比页的表格开关），否则组件自管。
// Chart 由容器宽度和 X_TICK_SPACE 算横轴刻度，COMPACT_WIDTH 以下只写时分；节点页与对比页不各自给数量。
// null 读数由共享的间断规则保留为空洞；图例无悬停时取最后一个有限值，悬停时取同一时刻的值。
export function Chart({ data, labels, unit, height = 180, soft, bands = [], legend = true, hidden, onHiddenChange, onFocus }: ChartProps) {
  const el = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const key = labels.join("|");
  const softFlags = labels.map((_, i) => soft?.[i] ?? false);
  const softKey = softFlags.map((s) => (s ? 1 : 0)).join("");
  const bandKey = bands.map((b) => `${b.lower}-${b.upper}`).join("|");
  const colors = seriesColors(softFlags);
  const scheme = useColorScheme();
  const gap = gapMessage(readingGap(yColumns(data), labels));
  const [cursorIdx, setCursorIdx] = useState<number | null>(null);
  const [ownHidden, setOwnHidden] = useState<ReadonlySet<number>>(() => new Set());
  const shownHidden = hidden ?? ownHidden;
  // 悬停回调由 uPlot 在图的生命周期里随时触发，调的是最近一次提交的 onFocus，不是建图那一刻的。
  const reportFocus = useEffectEvent((index: number | null) => onFocus?.(index));
  // 建图读的是最近一次提交的全部输入（数据、标签、配色、填充带、隐藏集合）；何时重建只由下面 effect 的
  // 依赖决定。数组每次渲染都是新对象，不能直接做依赖，否则每次渲染都重建，所以依赖是由它们派生的字符串 key。
  const createPlot = useEffectEvent((host: HTMLDivElement): uPlot => {
    const axisColor = resolveColor(host, "var(--muted)");
    const gridColor = resolveColor(host, "var(--line)");
    // 刻度画在 canvas 上，不继承 CSS：字体从 --font-mono 取，与页面里的 .num 读数同一个等宽栈。
    const axisFont = `11px ${getComputedStyle(host).getPropertyValue("--font-mono").trim() || "monospace"}`;
    const axisStyle = { stroke: axisColor, font: axisFont, grid: { stroke: gridColor, width: 1 }, ticks: { show: false } };
    const opts: Options = {
      width: host.clientWidth || 600,
      height,
      legend: { show: false },
      cursor: { focus: { prox: 16 } },
      focus: { alpha: 0.25 },
      scales: { x: { time: true }, y: unit === "percent" ? { range: [0, 100] } : {} },
      axes: [
        {
          ...axisStyle,
          space: X_TICK_SPACE,
          // 横轴只有一行 11px 刻度；uPlot 默认给横轴留 50px，刻度与图例之间空出一截，压到 30px 把高度还给绘图区。
          size: 30,
          // 刻度文字画在 canvas 上，DOM 里看不到；把刻度数写到宿主上，e2e 才能核对间距规则真的起了作用。
          values: (u, splits, _axisIdx, _foundSpace, foundIncr) => {
            host.dataset.xTicks = String(splits.length);
            return formatChartTimes(splits, { incrSec: foundIncr, compact: u.width < COMPACT_WIDTH });
          },
        },
        { ...axisStyle, size: 80, values: (_u, vals) => axisValues(vals, unit) },
      ],
      // uPlot 的 bands 用序列下标（0 是时间轴），本组件对外用 labels 下标；填充取上界序列的颜色加 20% 透明度。
      bands: bands.map((b) => ({ series: [b.upper + 1, b.lower + 1], fill: `${colors[b.upper]}33` })),
      series: [
        { label: "时间" },
        ...labels.map((label, i) => ({
          label,
          stroke: colors[i],
          width: softFlags[i] ? 1 : 1.5,
          alpha: softFlags[i] ? 0.55 : 1,
          spanGaps: false,
          // 默认点填充是白色，浅色背景上只剩细描边。填成与线相同的颜色，孤立读数才是一粒看得见的实心点。
          // filter 必须返回数组：返回 null 时 uPlot 会把可见范围内的每个点都画出来。
          points: {
            show: true,
            fill: colors[i],
            filter: (u: uPlot, seriesIdx: number) => isolatedPointIndices(u.data[seriesIdx] as Column),
          },
        })),
      ],
      hooks: {
        setCursor: [(u) => setCursorIdx(u.cursor.idx ?? null)],
        // 悬停高亮经 cursor.focus 触发 setSeries 且 opts 带 focus；本组件自己调 setSeries 切显示时 opts 只有 show。
        setSeries: [(_u, idx, seriesOpts: { focus?: boolean; show?: boolean }) => {
          if (!("focus" in seriesOpts)) return;
          reportFocus(idx == null ? null : idx - 1);
        }],
      },
    };
    const u = new uPlot(opts, data, host);
    for (const i of shownHidden) u.setSeries(i + 1, { show: false });
    return u;
  });
  useEffect(() => {
    const host = el.current;
    if (!host) return;
    plot.current = createPlot(host);
    const ro = new ResizeObserver(() => plot.current?.setSize({ width: host.clientWidth, height }));
    ro.observe(host);
    // canvas 不会在字体到达后自己重画：内嵌字体晚于首帧加载完时，就绪后重算一次坐标轴，刻度不停在回退字体上。
    let mounted = true;
    void document.fonts?.ready.then(() => { if (mounted) plot.current?.redraw(false, true); });
    return () => {
      mounted = false;
      ro.disconnect();
      plot.current?.destroy();
      plot.current = null;
    };
    // 标签、单位、尺寸、明暗、浅线与填充带改变才重建（配色由浅线标记决定，随 softKey 变）；数据与隐藏集合变化
    // 由下面两个 effect 原地应用到现有的图上，不重建。
  }, [key, unit, height, scheme, softKey, bandKey]);
  useEffect(() => {
    plot.current?.setData(data);
  }, [data]);
  const seriesCount = labels.length;
  useEffect(() => {
    const u = plot.current;
    if (!u) return;
    for (let i = 0; i < seriesCount; i++) u.setSeries(i + 1, { show: !shownHidden.has(i) });
  }, [shownHidden, seriesCount]);
  const toggle = (i: number) => {
    const next = new Set(shownHidden);
    if (next.has(i)) next.delete(i);
    else next.add(i);
    if (hidden === undefined) setOwnHidden(next);
    onHiddenChange?.(next);
  };
  const xs = data[0] as Column;
  // uPlot 把自己的根节点 append 进宿主。提示若也放在宿主里，图建好之后才出现的提示会被 React 追加到
  // uPlot 根之后，位置就随提示与图谁先出现而变；所以宿主只交给 uPlot，提示放在宿主之前、由 CSS 叠到图区中央。
  return (
    <div className="chart">
      <div className="chart-plot">
        {gap && <p className="muted chart-gap">{gap}</p>}
        <div ref={el} />
      </div>
      {legend && labels.length > 0 && (
        <ul className="chart-legend" aria-label="图例">
          {cursorIdx == null || !isReading(xs[cursorIdx]) ? <li className="legend-time">最新</li> : <li className="legend-time num">{formatChartTimes([xs[cursorIdx]])[0]}</li>}
          {labels.map((label, i) => {
            const col = data[i + 1] as Column;
            const at = cursorIdx ?? lastReadingIndex(col);
            const v = at == null ? null : col[at];
            const off = shownHidden.has(i);
            return (
              <li key={label}>
                <button type="button" aria-pressed={!off} style={{ "--series": colors[i] } as CSSProperties} onClick={() => toggle(i)}>
                  <span className={softFlags[i] ? "legend-swatch soft" : "legend-swatch"} aria-hidden="true" />
                  {label}
                  <span className="num">{isReading(v) ? formatUnit(v, unit) : "–"}</span>
                </button>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
