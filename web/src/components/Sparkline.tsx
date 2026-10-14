// 迷你趋势线：只给网络速率用。null 是无读数，线在这里断开而不是连过去（与 Chart 的 spanGaps 关闭同一规则）；
// 恒为 0 是一条贴底的线，不是无读数。y 按整组有限值的最大值归一，没有坐标轴，所以只表达走势不表达量级。
export function Sparkline({ values, label, width = 120, height = 28 }: {
  values: readonly (number | null | undefined)[]; label: string; width?: number; height?: number;
}) {
  const finite = values.filter((v): v is number => typeof v === "number" && Number.isFinite(v));
  const max = finite.length > 0 ? Math.max(...finite) : 0;
  const step = values.length > 1 ? width / (values.length - 1) : 0;
  const segments: string[] = [];
  let pen = false;
  values.forEach((v, i) => {
    if (typeof v !== "number" || !Number.isFinite(v)) {
      pen = false;
      return;
    }
    const x = (i * step).toFixed(1);
    const y = (height - 1 - (max > 0 ? (v / max) * (height - 2) : 0)).toFixed(1);
    segments.push(`${pen ? "L" : "M"}${x} ${y}`);
    pen = true;
  });
  // 空态是文字不是图，不能放进下面那个 SVG：viewBox 按 preserveAspectRatio="none" 随容器拉伸，文字会跟着变形；
  // SVG <text> 的默认 fill 是黑色，不随主题。普通文本占同样的高度，颜色与其它「无读数」占位一致。
  if (segments.length === 0) {
    return <span className="sparkline sparkline-empty" role="img" aria-label={`${label}：无读数`} style={{ height, lineHeight: `${height}px` }}>无读数</span>;
  }
  return (
    <svg className="sparkline" role="img" aria-label={label} width={width} height={height} viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none">
      <path d={segments.join(" ")} fill="none" stroke="currentColor" strokeWidth="1.5" vectorEffect="non-scaling-stroke" />
    </svg>
  );
}
