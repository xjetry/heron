import { dateTime } from "../lib/format";

// 时刻：可见文字用 lib/format 的统一写法，机器可读的 dateTime 属性给 ISO 8601（UTC）。at 是 Unix 秒。
export function Timestamp({ at }: { at: bigint | number }) {
  return <time dateTime={new Date(Number(at) * 1000).toISOString()}>{dateTime(at)}</time>;
}
