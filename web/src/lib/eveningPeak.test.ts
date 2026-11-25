import { describe, expect, it } from "vitest";
import { eveningPeakFit, eveningPeakCoverage, sliceEveningPeak } from "./eveningPeak";

const at = (iso: string) => Date.parse(iso) / 1000;
const H = 3600;

describe("桶与晚高峰（hub 时区 20:00–24:00）的关系", () => {
  it.each([
    // Asia/Shanghai 固定 +08:00：12:00Z 是当地 20:00。
    { zone: "Asia/Shanghai", ts: "2026-11-20T11:00:00Z", step: H, want: "outside" }, // 19:00–20:00
    { zone: "Asia/Shanghai", ts: "2026-11-20T12:00:00Z", step: H, want: "inside" }, // 20:00–21:00
    { zone: "Asia/Shanghai", ts: "2026-11-20T15:00:00Z", step: H, want: "inside" }, // 23:00–24:00，右端开区间
    { zone: "Asia/Shanghai", ts: "2026-11-20T16:00:00Z", step: H, want: "outside" }, // 次日 00:00
    { zone: "Asia/Shanghai", ts: "2026-11-20T12:00:00Z", step: 4 * H, want: "inside" }, // 恰好整段
    { zone: "Asia/Shanghai", ts: "2026-11-20T15:00:00Z", step: 3 * H, want: "partial" }, // 23:00–02:00 跨日界
    { zone: "Asia/Shanghai", ts: "2026-11-20T09:00:00Z", step: 6 * H, want: "partial" }, // 17:00–23:00
    { zone: "Asia/Shanghai", ts: "2026-11-20T06:00:00Z", step: 6 * H, want: "outside" }, // 14:00–20:00
    { zone: "Asia/Shanghai", ts: "2026-11-19T16:00:00Z", step: 24 * H, want: "partial" }, // 整天
    // Asia/Kolkata +05:30：整点桶在当地是半点起止。
    { zone: "Asia/Kolkata", ts: "2026-11-20T14:00:00Z", step: H, want: "partial" }, // 19:30–20:30
    { zone: "Asia/Kolkata", ts: "2026-11-20T15:00:00Z", step: H, want: "inside" }, // 20:30–21:30
    { zone: "Asia/Kolkata", ts: "2026-11-20T18:00:00Z", step: H, want: "partial" }, // 23:30–00:30
    // America/Santiago 2026-04-05T03:00Z 从 -03 回拨到 -04：当地 4 月 4 日的 23:00–24:00 出现两次，这一天 25 小时，
    // 晚高峰从 23:00Z 持续到次日 04:00Z，共 5 小时。按固定 -03 或按"20:00 加 4 小时"算都会把第二个 23 点当成次日。
    { zone: "America/Santiago", ts: "2026-04-04T22:00:00Z", step: H, want: "outside" }, // 19:00–20:00 (-03)
    { zone: "America/Santiago", ts: "2026-04-05T02:00:00Z", step: H, want: "inside" }, // 23:00–24:00 (-03)
    { zone: "America/Santiago", ts: "2026-04-05T03:00:00Z", step: H, want: "inside" }, // 第二个 23:00–24:00 (-04)
    { zone: "America/Santiago", ts: "2026-04-05T02:00:00Z", step: 2 * H, want: "inside" }, // 跨回拨时刻
    { zone: "America/Santiago", ts: "2026-04-04T23:00:00Z", step: 5 * H, want: "inside" }, // 整段 5 小时
    { zone: "America/Santiago", ts: "2026-04-05T04:00:00Z", step: H, want: "outside" }, // 4 月 5 日 00:00 (-04)
    // 2026-09-06T04:00Z 从 -04 拨快到 -03：当地 9 月 5 日 24:00 直接成为 9 月 6 日 01:00，这一天 23 小时。
    { zone: "America/Santiago", ts: "2026-09-06T03:00:00Z", step: H, want: "inside" }, // 23:00–24:00 (-04)
    { zone: "America/Santiago", ts: "2026-09-06T04:00:00Z", step: H, want: "outside" }, // 01:00–02:00 (-03)
    { zone: "America/Santiago", ts: "2026-09-06T00:00:00Z", step: 5 * H, want: "partial" }, // 20:00 起 5 小时越过 24:00
  ])("$zone $ts 起 $step 秒 → $want", ({ zone, ts, step, want }) => {
    expect(eveningPeakFit(zone)(at(ts), step)).toBe(want);
  });

  it("只看 hub 时区，不看运行环境的本地时区", () => {
    // 同一个桶在 UTC 是 12:00–13:00，在上海是 20:00–21:00；判定只取参数里的时区。
    expect(eveningPeakFit("UTC")(at("2026-11-20T12:00:00Z"), H)).toBe("outside");
    expect(eveningPeakFit("Asia/Shanghai")(at("2026-11-20T12:00:00Z"), H)).toBe("inside");
  });
});

describe("窗口能否给出晚高峰读数", () => {
  const shanghai = eveningPeakFit("Asia/Shanghai");
  it("窗口里至少有一个整桶命中", () => {
    expect(eveningPeakCoverage(at("2026-11-20T00:00:00Z"), at("2026-11-21T00:00:00Z"), H, shanghai)).toBe("covered");
  });
  it("窗口经过晚高峰但步长大到没有整桶落在里面：要缩短窗口", () => {
    expect(eveningPeakCoverage(at("2026-11-01T00:00:00Z"), at("2026-11-21T00:00:00Z"), 6 * H, shanghai)).toBe("step-too-large");
  });
  it("窗口根本没有经过晚高峰：缩短窗口没有用", () => {
    // 上海 08:00–12:00。
    expect(eveningPeakCoverage(at("2026-11-20T00:00:00Z"), at("2026-11-20T04:00:00Z"), 60, shanghai)).toBe("outside-window");
  });
});

it("切片只留整桶落在晚高峰内的样本，跨界的桶整个丢掉", () => {
  const fit = eveningPeakFit("Asia/Kolkata");
  const samples = [
    { ts: BigInt(at("2026-11-20T14:00:00Z")), tag: "19:30" },
    { ts: BigInt(at("2026-11-20T15:00:00Z")), tag: "20:30" },
    { ts: BigInt(at("2026-11-20T17:00:00Z")), tag: "22:30" },
    { ts: BigInt(at("2026-11-20T18:00:00Z")), tag: "23:30" },
  ];
  expect(sliceEveningPeak(samples, H, fit).map((s) => s.tag)).toEqual(["20:30", "22:30"]);
});
