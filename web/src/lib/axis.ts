import { formatUnit } from "./format";

const byteUnits = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

// 同一根轴使用同一倍率，精度至少能区分最小刻度间隔；表格的摘要精度不适用于坐标轴。
export function axisValues(values: number[], unit: string): string[] {
  if (values.length < 2) return values.map((v) => formatUnit(v, unit));
  const gaps = values.slice(1).map((v, i) => Math.abs(v - values[i])).filter((v) => v > 0);
  if (gaps.length === 0) return values.map((v) => formatUnit(v, unit));
  let scale = 1;
  let suffix = unit === "percent" ? "%" : unit === "ms" ? " ms" : unit === "count/s" ? "/s" : "";
  const rate = unit === "bytes/s";
  if (unit === "bytes" || rate) {
    const peak = Math.max(...values.map(Math.abs));
    let index = 0;
    while (peak / scale >= 1024 && index < byteUnits.length - 1) { scale *= 1024; index++; }
    suffix = ` ${byteUnits[index]}${rate ? "/s" : ""}`;
  }
  const digits = Math.min(20, Math.max(0, Math.ceil(-Math.log10(Math.min(...gaps) / scale))));
  return values.map((v) => `${(v / scale).toFixed(digits)}${suffix}`);
}
