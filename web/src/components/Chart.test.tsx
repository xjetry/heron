import { act, render, screen, waitFor, within, fireEvent } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import uPlot, { type AlignedData, type Options } from "uplot";
import { formatChartTimes, isolatedPointIndices } from "../lib/chartMarks";
import { currentScheme } from "../lib/colorScheme";
import { Chart, COMPACT_WIDTH, X_TICK_SPACE } from "./Chart";

const plots = vi.hoisted(() => [] as { options: Options; data: AlignedData; setData: ReturnType<typeof vi.fn>; setSeries: ReturnType<typeof vi.fn>; cursor: { idx: number | null }; width: number; destroy: ReturnType<typeof vi.fn> }[]);
// 与真实 uPlot 一样：构造时把根节点 append 进宿主，destroy 时移除，DOM 里的相对位置才与浏览器一致。
vi.mock("uplot", () => ({ default: class {
  setData = vi.fn();
  setSeries = vi.fn();
  cursor = { idx: null as number | null };
  width = 600;
  setSize = vi.fn();
  root = Object.assign(document.createElement("div"), { className: "uplot" });
  destroy = vi.fn(() => this.root.remove());
  constructor(public options: Options, public data: AlignedData, host?: HTMLElement) {
    host?.appendChild(this.root);
    plots.push(this);
  }
} }));

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  delete document.documentElement.dataset.theme;
  plots.length = 0;
});

// jsdom 不解析 var() 与 light-dark()：按探针元素上写的值，给出浏览器在当前明暗下会解析出的颜色。
function stubResolvedColors() {
  const real = window.getComputedStyle.bind(window);
  vi.spyOn(window, "getComputedStyle").mockImplementation((el, pseudo) => {
    const dark = currentScheme() === "dark";
    const resolved: Record<string, string> = {
      "var(--muted)": dark ? "rgb(156, 163, 175)" : "rgb(107, 114, 128)",
      "var(--line)": dark ? "rgb(42, 47, 58)" : "rgb(229, 231, 235)",
    };
    const value = (el as HTMLElement).style?.color;
    return value in resolved ? ({ color: resolved[value] } as CSSStyleDeclaration) : real(el, pseudo);
  });
}

it("数据原地更新；系统或 data-theme 改变明暗时，用最新数据和解析后的配色重建", async () => {
  let dark = false;
  let changed = () => {};
  vi.stubGlobal("matchMedia", () => ({ get matches() { return dark; }, addEventListener: (_: string, listener: () => void) => { changed = listener; }, removeEventListener: vi.fn() }));
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  stubResolvedColors();
  const light = ["rgb(107, 114, 128)", "rgb(229, 231, 235)", false];
  const darkColors = ["rgb(156, 163, 175)", "rgb(42, 47, 58)", false];
  const { rerender, unmount } = render(<Chart data={[[0], [1]]} labels={["cpu"]} unit="count" />);
  // 断言失败时也先卸载：挂着的订阅会在 afterEach 撤掉 matchMedia 替身后再读一次系统设置，抛出的未捕获异常盖住真正的失败原因。
  try {
    const colors = () => plots.at(-1)!.options.axes!.map((axis) => [axis.stroke, axis.grid?.stroke, axis.ticks?.show]);
    expect(colors()).toEqual([light, light]);
    // 图例由组件自绘，线段色样代替默认方块。
    expect(plots[0].options.legend?.show).toBe(false);
    const next: AlignedData = [[0], [2]];
    rerender(<Chart data={next} labels={["cpu"]} unit="count" />);
    expect(plots).toHaveLength(1);
    expect(plots[0].setData).toHaveBeenLastCalledWith(next);
    act(() => { dark = true; changed(); });
    expect(colors()).toEqual([darkColors, darkColors]);
    expect(plots.at(-1)!.data).toEqual(next);
    // 站点设置强制浅色：html 的 data-theme 压过系统的深色。
    await act(async () => { document.documentElement.dataset.theme = "light"; });
    await waitFor(() => expect(colors()).toEqual([light, light]));
    expect(plots.at(-1)!.data).toEqual(next);
  } finally {
    unmount();
  }
});

// 本组件只给序列写颜色字符串；读回时先确认它确实是字符串，再拼成填充色。
function strokeOf(series: uPlot.Series | undefined): string {
  if (typeof series?.stroke !== "string") throw new Error(`series stroke is not a color string: ${typeof series?.stroke}`);
  return series.stroke;
}

function mountChart(props: Parameters<typeof Chart>[0]) {
  vi.stubGlobal("matchMedia", () => ({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() }));
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  return render(<Chart {...props} />);
}

it("全是 null 时给出图级无读数提示，恒为 0 不提示", () => {
  const { rerender, unmount } = mountChart({
    data: [[0, 60], [null, null], [null, null]],
    labels: ["CPU 均值", "CPU 峰值"],
    unit: "percent",
  });
  try {
    expect(screen.getByText("窗口内没有读数")).toBeInTheDocument();
    rerender(<Chart data={[[0, 60], [0, 0], [0, 0]]} labels={["CPU 均值", "CPU 峰值"]} unit="percent" />);
    expect(screen.queryByText(/没有读数/)).not.toBeInTheDocument();
  } finally {
    unmount();
  }
});

it("只有部分序列全是 null 时点名这些序列，有读数的序列不出现在提示里", () => {
  const { unmount } = mountChart({
    data: [[0, 60], [1, null], [null, null]],
    labels: ["CPU 均值", "CPU 峰值"],
    unit: "percent",
  });
  try {
    expect(screen.getByText("CPU 峰值：窗口内没有读数")).toBeInTheDocument();
    expect(screen.queryByText("窗口内没有读数")).not.toBeInTheDocument();
    expect(screen.getByText("CPU 峰值：窗口内没有读数")).not.toHaveTextContent("CPU 均值");
  } finally {
    unmount();
  }
});

it("无读数提示在图的上方，与提示和图谁先出现无关", () => {
  const { container, rerender, unmount } = mountChart({ data: [[0, 60], [1, 2]], labels: ["cpu"], unit: "count" });
  const hintPrecedesPlot = () => {
    const root = container.querySelector(".uplot");
    if (!root) throw new Error("图没有挂进宿主");
    return Boolean(screen.getByText("窗口内没有读数").compareDocumentPosition(root) & Node.DOCUMENT_POSITION_FOLLOWING);
  };
  try {
    // 图先建好，提示后出现。
    rerender(<Chart data={[[0, 60], [null, null]]} labels={["cpu"]} unit="count" />);
    expect(hintPrecedesPlot()).toBe(true);
    // 提示已在，图因标签变化重建。
    rerender(<Chart data={[[0, 60], [null, null]]} labels={["mem"]} unit="count" />);
    expect(plots).toHaveLength(2);
    expect(hintPrecedesPlot()).toBe(true);
  } finally {
    unmount();
  }
});

it("横轴：刻度间距 80px，刻度数写在 data-x-ticks，窄图只写时分；点过滤只留孤立读数", () => {
  const { container, unmount } = mountChart({ data: [[0, 60], [1, 2]], labels: ["cpu"], unit: "count" });
  try {
    const opts = plots.at(-1)!.options;
    const axis = opts.axes?.[0];
    expect(axis?.space).toBe(X_TICK_SPACE);
    if (typeof axis?.values !== "function") throw new Error("横轴 values 不是函数");
    const t0 = Date.parse("2026-10-06T22:00:00+08:00") / 1000;
    const ticks = [t0, t0 + 3600, t0 + 7200];
    const wide = axis.values({ width: 1200 } as uPlot, ticks, 0, 80, 3600);
    expect(wide).toEqual(formatChartTimes(ticks, { incrSec: 3600 }));
    expect(container.querySelector("[data-x-ticks]")).toHaveAttribute("data-x-ticks", "3");
    const narrow = axis.values({ width: COMPACT_WIDTH - 1 } as uPlot, ticks, 0, 80, 3600);
    expect(narrow).toEqual(formatChartTimes(ticks, { incrSec: 3600, compact: true }));
    expect(narrow.join(" ")).not.toMatch(/\d{2}-\d{2}/);
    expect(opts.axes?.[0]?.grid).toMatchObject({ width: 1 });
    const filter = opts.series?.[1]?.points?.filter;
    if (typeof filter !== "function") throw new Error("points.filter 不是函数");
    const sandwiched = [1, 2, null, 5, null, 3, 4];
    expect(filter({ data: [[], sandwiched] } as unknown as uPlot, 1, true)).toEqual(isolatedPointIndices(sandwiched));
  } finally {
    unmount();
  }
});

it("图例无悬停显示每条序列的最新读数，悬停显示该时刻的值；点击一项隐藏该序列", () => {
  const { unmount } = mountChart({ data: [[0, 60, 120], [1, 2, null], [null, 5, 6]], labels: ["CPU 均值", "CPU 峰值"], unit: "percent", soft: [false, true] });
  try {
    const legend = within(screen.getByRole("list", { name: "图例" }));
    expect(legend.getByText("最新")).toBeInTheDocument();
    expect(legend.getByRole("button", { name: /CPU 均值/ })).toHaveTextContent("2.0%");
    expect(legend.getByRole("button", { name: /CPU 峰值/ })).toHaveTextContent("6.0%");
    const plot = plots.at(-1)!;
    plot.cursor.idx = 0;
    act(() => { plot.options.hooks!.setCursor![0]!(plot as unknown as uPlot); });
    expect(legend.getByText(formatChartTimes([0])[0])).toBeInTheDocument();
    expect(legend.getByRole("button", { name: /CPU 均值/ })).toHaveTextContent("1.0%");
    expect(legend.getByRole("button", { name: /CPU 峰值/ })).toHaveTextContent("–");
    fireEvent.click(legend.getByRole("button", { name: /CPU 峰值/ }));
    expect(plot.setSeries).toHaveBeenLastCalledWith(2, { show: false });
    expect(legend.getByRole("button", { name: /CPU 峰值/ })).toHaveAttribute("aria-pressed", "false");
    // 浅线与它前面的实线同色、更细。
    expect(plot.options.series?.[2]?.stroke).toBe(plot.options.series?.[1]?.stroke);
    expect(plot.options.series?.[2]?.width).toBeLessThan(plot.options.series?.[1]?.width as number);
  } finally {
    unmount();
  }
});

it("受控隐藏集合与悬停回调：隐藏传给 uPlot，焦点序列回报 labels 下标，legend=false 时不画图例", () => {
  const onFocus = vi.fn();
  const { rerender, unmount } = mountChart({ data: [[0], [1], [2]], labels: ["a", "b"], unit: "ms", legend: false, hidden: new Set([1]), onFocus });
  try {
    const plot = plots.at(-1)!;
    expect(screen.queryByRole("list", { name: "图例" })).toBeNull();
    expect(plot.setSeries).toHaveBeenCalledWith(2, { show: false });
    rerender(<Chart data={[[0], [1], [2]]} labels={["a", "b"]} unit="ms" legend={false} hidden={new Set()} onFocus={onFocus} />);
    expect(plot.setSeries).toHaveBeenLastCalledWith(2, { show: true });
    // uPlot 运行时在悬停时给 setSeries 钩子传 { focus: true }，d.ts 把 opts 声明成 Series，这里按运行时形状传。
    const focus = { focus: true } as unknown as uPlot.Series;
    plot.options.hooks!.setSeries![0]!(plot as unknown as uPlot, 1, focus);
    expect(onFocus).toHaveBeenLastCalledWith(0);
    plot.options.hooks!.setSeries![0]!(plot as unknown as uPlot, null, focus);
    expect(onFocus).toHaveBeenLastCalledWith(null);
    // 自己调用 setSeries 切换显示时 uPlot 也会触发同一个钩子，这不是悬停。
    onFocus.mockClear();
    plot.options.hooks!.setSeries![0]!(plot as unknown as uPlot, 1, { show: false });
    expect(onFocus).not.toHaveBeenCalled();
  } finally {
    unmount();
  }
});

it("填充带按 labels 下标换算成 uPlot 序列下标，取上界序列的颜色", () => {
  const { unmount } = mountChart({ data: [[0], [1], [0], [2]], labels: ["均值", "最小", "最大"], unit: "ms", soft: [false, true, true], bands: [{ lower: 1, upper: 2 }] });
  try {
    const opts = plots.at(-1)!.options;
    expect(opts.bands).toEqual([{ series: [3, 2], fill: `${strokeOf(opts.series[3])}33` }]);
  } finally {
    unmount();
  }
});

// 何时重建只由标签、单位、尺寸、明暗、浅线与填充带决定；数组每次渲染都是新对象，同值的新数组不算变化。
// 建图读的是最近一次提交的输入：重建时用最新的数据、隐藏集合与悬停回调。
it("标签变化时用最新输入重建；隐藏集合、数据与同值的新数组只原地更新，不重建", () => {
  const first = vi.fn<(index: number | null) => void>();
  const latest = vi.fn<(index: number | null) => void>();
  const props = { unit: "ms", legend: false } as const;
  const { rerender, unmount } = mountChart({ ...props, data: [[0], [1], [2]], labels: ["a", "b"], soft: [false, true], bands: [{ lower: 1, upper: 0 }], hidden: new Set<number>(), onFocus: first });
  try {
    expect(plots).toHaveLength(1);
    const plot = plots[0];
    // 同一个集合对象一直传下去：隐藏集合没变，重建后的图只能靠建图时读到的最新集合隐藏第 2 条线。
    const hidden = new Set([1]);
    rerender(<Chart {...props} data={[[0], [1], [2]]} labels={["a", "b"]} soft={[false, true]} bands={[{ lower: 1, upper: 0 }]} hidden={hidden} onFocus={latest} />);
    expect(plots).toHaveLength(1);
    expect(plot.setSeries).toHaveBeenLastCalledWith(2, { show: false });
    const next: AlignedData = [[0], [3], [4]];
    rerender(<Chart {...props} data={next} labels={["a", "b"]} soft={[false, true]} bands={[{ lower: 1, upper: 0 }]} hidden={hidden} onFocus={latest} />);
    expect(plots).toHaveLength(1);
    expect(plot.setData).toHaveBeenLastCalledWith(next);
    // 建图之后才换的悬停回调，也是 uPlot 钩子调到的那一个。
    plot.options.hooks!.setSeries![0]!(plot as unknown as uPlot, 1, { focus: true } as unknown as uPlot.Series);
    expect(latest).toHaveBeenLastCalledWith(0);
    expect(first).not.toHaveBeenCalled();

    rerender(<Chart {...props} data={next} labels={["a", "c"]} soft={[false, true]} bands={[{ lower: 1, upper: 0 }]} hidden={hidden} onFocus={latest} />);
    expect(plots).toHaveLength(2);
    expect(plot.destroy).toHaveBeenCalled();
    const rebuilt = plots[1];
    expect(rebuilt.options.series.map((s) => s.label)).toEqual(["时间", "a", "c"]);
    expect(rebuilt.data).toBe(next);
    expect(rebuilt.setSeries).toHaveBeenCalledWith(2, { show: false });
  } finally {
    unmount();
  }
});

it("浅线标记或填充带的取值变化时重建", () => {
  const { rerender, unmount } = mountChart({ data: [[0], [1], [2]], labels: ["a", "b"], unit: "ms", soft: [false, false] });
  try {
    rerender(<Chart data={[[0], [1], [2]]} labels={["a", "b"]} unit="ms" soft={[false, true]} />);
    expect(plots).toHaveLength(2);
    expect(plots[1].options.series[2].width).toBeLessThan(plots[1].options.series[1].width as number);
    rerender(<Chart data={[[0], [1], [2]]} labels={["a", "b"]} unit="ms" soft={[false, true]} bands={[{ lower: 1, upper: 0 }]} />);
    expect(plots).toHaveLength(3);
    expect(plots[2].options.bands).toEqual([{ series: [1, 2], fill: `${strokeOf(plots[2].options.series[1])}33` }]);
  } finally {
    unmount();
  }
});
