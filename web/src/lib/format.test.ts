import { describe, expect, it } from "vitest";
import { ago, bytes, dateTime, day, duration, formatScaled, formatUnit, percent } from "./format";

describe("format", () => {
  it("ms 按舍入后的量级取位数，门槛处不出现位数跳变", () => {
    expect(formatUnit(0.4567, "milliseconds")).toBe("0.46 ms");
    expect(formatUnit(9.994, "milliseconds")).toBe("9.99 ms");
    expect(formatUnit(9.995, "milliseconds")).toBe("10.0 ms");
    expect(formatUnit(12.34, "milliseconds")).toBe("12.3 ms");
    expect(formatUnit(99.94, "milliseconds")).toBe("99.9 ms");
    expect(formatUnit(99.95, "milliseconds")).toBe("100 ms");
    expect(formatUnit(250.7, "milliseconds")).toBe("251 ms");
    expect(formatUnit(0, "milliseconds")).toBe("0.00 ms");
    expect(formatUnit(-0.004, "milliseconds")).toBe("0.00 ms");
    expect(formatUnit(-9.995, "milliseconds")).toBe("-10.0 ms");
    expect(formatUnit(Number.NaN, "milliseconds")).toBe("NaN ms");
  });
  it("bytes 按四舍五入后的值决定单位与位数，进位到 1024 即换单位", () => {
    expect(bytes(0)).toBe("0 B");
    expect(bytes(0n)).toBe("0 B");
    expect(bytes(-0)).toBe("0 B");
    expect(bytes(-0.4)).toBe("0 B");
    expect(bytes(1023)).toBe("1023 B");
    expect(bytes(1023.4)).toBe("1023 B");
    expect(bytes(1023.6)).toBe("1.0 KiB");
    expect(bytes(1024)).toBe("1.0 KiB");
    expect(bytes(9.94 * 1024)).toBe("9.9 KiB");
    expect(bytes(9.95 * 1024)).toBe("10 KiB");
    expect(bytes(1048575)).toBe("1.0 MiB");
    expect(bytes(1023.7 * 1024 ** 2)).toBe("1.0 GiB");
    expect(bytes(15n * 1024n * 1024n)).toBe("15 MiB");
    expect(bytes(3.5 * 1024 ** 3)).toBe("3.5 GiB");
    expect(bytes(1023.9 * 1024 ** 5)).toBe("1024 PiB");
    expect(bytes(-1023.4)).toBe("-1023 B");
    expect(bytes(-1023.6)).toBe("-1.0 KiB");
    expect(bytes(-1048575)).toBe("-1.0 MiB");
    expect(bytes(Number.NaN)).toBe("NaN B");
  });
  it("formatScaled 支持十进制基数进位换单位", () => {
    const spec = { base: 1000, units: ["", "k", "M"], tiers: (i: number) => (i > 0 ? [{ below: 10, digits: 1 }] : []) };
    expect(formatScaled(999.4, spec)).toBe("999 ");
    expect(formatScaled(999.6, spec)).toBe("1.0 k");
    expect(formatScaled(999_999, spec)).toBe("1.0 M");
    expect(formatScaled(-999.6, spec)).toBe("-1.0 k");
  });
  it("percent 按舍入后的值取位数，覆盖边界、负数、0 与 NaN", () => {
    expect(percent(0)).toBe("0.0%");
    expect(percent(-0)).toBe("0.0%");
    expect(percent(-0.04)).toBe("0.0%");
    expect(percent(3.14159)).toBe("3.1%");
    expect(percent(9.94)).toBe("9.9%");
    expect(percent(9.95)).toBe("10%");
    expect(percent(9.994)).toBe("10%");
    expect(percent(9.995)).toBe("10%");
    expect(percent(42.6)).toBe("43%");
    expect(percent(-5)).toBe("-5.0%");
    expect(percent(-9.95)).toBe("-10%");
    expect(percent(-42.6)).toBe("-43%");
    expect(percent(Number.NaN)).toBe("NaN%");
  });
  it("duration 取最大的两个单位，负数不产生正值回绕", () => {
    expect(duration(0)).toBe("0m");
    expect(duration(59)).toBe("0m");
    expect(duration(3661)).toBe("1h 1m");
    expect(duration(90000n)).toBe("1d 1h");
    expect(duration(-59)).toBe("0m");
    expect(duration(-120)).toBe("-2m");
    expect(duration(Number.NaN)).toBe("NaN");
  });
  it("ago 以 hub 的 now 为基准且不出现负数或小数秒", () => {
    expect(ago(1000, 1002)).toBe("刚刚");
    expect(ago(1000, 1030)).toBe("30 秒前");
    expect(ago(1000, 1059.6)).toBe("59 秒前");
    expect(ago(1000, 1000 + 7200)).toBe("2 小时前");
    expect(ago(2000, 1000)).toBe("刚刚");
  });
  it("formatUnit 按单位分派并复用同一舍入与进位规则", () => {
    expect(formatUnit(50, "percent")).toBe("50%");
    expect(formatUnit(9.995, "percent")).toBe("10%");
    expect(formatUnit(2048, "bytes")).toBe("2.0 KiB");
    expect(formatUnit(1023.6, "bytes")).toBe("1.0 KiB");
    expect(formatUnit(2048, "bytes/s")).toBe("2.0 KiB/s");
    expect(formatUnit(1023.6, "bytes/s")).toBe("1.0 KiB/s");
    expect(formatUnit(1048575, "bytes/s")).toBe("1.0 MiB/s");
    expect(formatUnit(0, "bytes/s")).toBe("0 B/s");
    expect(formatUnit(Number.NaN, "bytes/s")).toBe("NaN B/s");
    expect(formatUnit(2.4, "count")).toBe("2");
    expect(formatUnit(-0.4, "count")).toBe("0");
    expect(formatUnit(Number.NaN, "count")).toBe("NaN");
    expect(formatUnit(3.456, "count/s")).toBe("3.46/s");
    expect(formatUnit(9.994, "count/s")).toBe("9.99/s");
    expect(formatUnit(9.995, "count/s")).toBe("10.0/s");
    expect(formatUnit(99.94, "count/s")).toBe("99.9/s");
    expect(formatUnit(99.95, "count/s")).toBe("100/s");
    expect(formatUnit(1234.4, "count/s")).toBe("1234/s");
    expect(formatUnit(Number.NaN, "count/s")).toBe("NaN/s");
    expect(formatUnit(1.23456, "")).toBe("1.23");
    expect(formatUnit(-0.001, "")).toBe("0.00");
    expect(formatUnit(Number.NaN, "")).toBe("NaN");
  });
  it("dateTime 与 day 固定为 YYYY-MM-DD HH:mm:ss / YYYY-MM-DD，24 小时制，按给定时区", () => {
    expect(dateTime(1_800_000_000, "UTC")).toBe("2027-01-15 08:00:00");
    expect(dateTime(1_800_000_000n, "Asia/Tokyo")).toBe("2027-01-15 17:00:00");
    expect(dateTime(1_700_000_005, "America/New_York")).toBe("2023-11-14 17:13:25");
    expect(dateTime(86_400, "UTC")).toBe("1970-01-02 00:00:00");
    expect(day(1_800_000_000, "UTC")).toBe("2027-01-15");
    expect(day(1_800_000_000, "Pacific/Kiritimati")).toBe("2027-01-15");
    expect(day(1_800_000_000 - 9 * 3600, "UTC")).toBe("2027-01-14");
  });
  it("dateTime 无效时间返回空串", () => {
    expect(dateTime(Number.NaN)).toBe("");
  });
});
