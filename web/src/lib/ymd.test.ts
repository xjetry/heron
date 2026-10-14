import { describe, expect, it } from "vitest";
import { addDays, addMonths, calendarStep, monthDays, weekdayOf } from "./ymd";

describe("ymd", () => {
  it("加减天数跨月跨年、闰年", () => {
    expect(addDays("2025-12-31", 1)).toBe("2026-01-01");
    expect(addDays("2028-03-01", -1)).toBe("2028-02-29");
    expect(addDays("0001-01-02", -1)).toBe("0001-01-01");
  });
  it("加月钳到月末", () => {
    expect(addMonths("2026-01-31", 1)).toBe("2026-02-28");
    expect(addMonths("2028-01-31", 1)).toBe("2028-02-29");
    expect(addMonths("2026-03-31", -1)).toBe("2026-02-28");
  });
  it("月历周一开头、42 格，首格是该月 1 日所在周的周一", () => {
    const days = monthDays("2026-01-17");
    expect(days).toHaveLength(42);
    expect(days[0]).toBe("2025-12-29");
    expect(weekdayOf(days[0])).toBe(0);
    expect(days.indexOf("2026-01-01")).toBe(3);
  });
  it("键盘步进", () => {
    expect(calendarStep("ArrowUp", "2026-01-08")).toBe("2026-01-01");
    expect(calendarStep("Home", "2026-01-08")).toBe("2026-01-05");
    expect(calendarStep("End", "2026-01-08")).toBe("2026-01-11");
    expect(calendarStep("PageUp", "2026-03-31")).toBe("2026-02-28");
    expect(calendarStep("Enter", "2026-01-08")).toBeUndefined();
  });
});
