import { BYTE_UNITS, formatUnit } from "./format";

const axisDigits = (minGap: number, scale: number) => Math.min(20, Math.max(0, Math.ceil(-Math.log10(minGap / scale))));

// 同一根轴使用同一倍率，精度至少能区分最小刻度间隔；表格的摘要精度不适用于坐标轴。
// 字节轴在算出显示位数后复核峰值：若峰值四舍五入后进位到 1024 且未到最大单位，整根轴一起升到下一单位，不把 1024 留在旧单位上。
export function axisValues(values: number[], unit: string): string[] {
  if (values.length < 2) return values.map((v) => formatUnit(v, unit));
  const gaps = values.slice(1).map((v, i) => Math.abs(v - values[i])).filter((v) => v > 0);
  if (gaps.length === 0) return values.map((v) => formatUnit(v, unit));
  const minGap = Math.min(...gaps);
  let scale = 1;
  let suffix = unit === "percent" ? "%" : unit === "milliseconds" ? " ms" : unit === "count/s" ? "/s" : "";
  const rate = unit === "bytes/s";
  let digits = axisDigits(minGap, scale);
  if (unit === "bytes" || rate) {
    const peak = Math.max(...values.map(Math.abs));
    let index = 0;
    while (peak / scale >= 1024 && index < BYTE_UNITS.length - 1) { scale *= 1024; index++; }
    digits = axisDigits(minGap, scale);
    while (Number((peak / scale).toFixed(digits)) >= 1024 && index < BYTE_UNITS.length - 1) {
      scale *= 1024;
      index++;
      digits = axisDigits(minGap, scale);
    }
    suffix = ` ${BYTE_UNITS[index]}${rate ? "/s" : ""}`;
  }
  return values.map((v) => `${(v / scale).toFixed(digits)}${suffix}`);
}
