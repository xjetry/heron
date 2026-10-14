// 图表系列色与 styles.css 的 --status-* 字面量保持 RGB 距离 >=70，由 seriesPalette.test.ts 核对。
// 六色用完循环，对比图的多条序列另靠悬停高亮分辨。
export const SERIES_PALETTE: readonly string[] = ["#3b82f6", "#06b6d4", "#d946ef", "#84cc16", "#fdba74", "#2dd4bf"];

// 浅线（峰值、最小 / 最大）取它前面最近一条实线的颜色：调用方把峰值紧跟在同指标的均值之后，颜色就成对；
// 第一条就是浅线时没有实线可跟，取首色。
export function seriesColors(soft: readonly boolean[]): string[] {
  let solidCount = 0;
  let current = SERIES_PALETTE[0];
  return soft.map((isSoft) => {
    if (!isSoft) current = SERIES_PALETTE[solidCount++ % SERIES_PALETTE.length];
    return current;
  });
}
