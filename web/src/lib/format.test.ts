import { describe, expect, it } from "vitest";
import { ago, bytes, dateTime, day, duration, formatUnit, percent } from "./format";

describe("format", () => {
  it("ms 按量级取位数", () => {
    expect(formatUnit(0.4567, "ms")).toBe("0.46 ms");
    expect(formatUnit(12.34, "ms")).toBe("12.3 ms");
    expect(formatUnit(250.7, "ms")).toBe("251 ms");
  });
  it("bytes 用二进制单位，小数只在个位数时出现", () => {
    expect(bytes(0)).toBe("0 B");
    expect(bytes(1023)).toBe("1023 B");
    expect(bytes(1024)).toBe("1.0 KiB");
    expect(bytes(15n * 1024n * 1024n)).toBe("15 MiB");
    expect(bytes(3.5 * 1024 ** 3)).toBe("3.5 GiB");
  });
  it("percent 小于 10 保留一位小数", () => {
    expect(percent(3.14159)).toBe("3.1%");
    expect(percent(42.6)).toBe("43%");
  });
  it("duration 取最大的两个单位", () => {
    expect(duration(59)).toBe("0m");
    expect(duration(3661)).toBe("1h 1m");
    expect(duration(90000n)).toBe("1d 1h");
  });
  it("ago 以 hub 的 now 为基准且不出现负数", () => {
    expect(ago(1000, 1002)).toBe("刚刚");
    expect(ago(1000, 1030)).toBe("30 秒前");
    expect(ago(1000, 1000 + 7200)).toBe("2 小时前");
    expect(ago(2000, 1000)).toBe("刚刚");
  });
  it("formatUnit 按单位分派，未知单位保留两位小数", () => {
    expect(formatUnit(50, "percent")).toBe("50%");
    expect(formatUnit(2048, "bytes")).toBe("2.0 KiB");
    expect(formatUnit(2048, "bytes/s")).toBe("2.0 KiB/s");
    expect(formatUnit(0, "bytes/s")).toBe("0 B/s");
    expect(formatUnit(2.4, "count")).toBe("2");
    expect(formatUnit(3.456, "count/s")).toBe("3.46/s");
    expect(formatUnit(1234.4, "count/s")).toBe("1234/s");
    expect(formatUnit(1.23456, "")).toBe("1.23");
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
