import { describe, expect, it } from "vitest";
import { parseQuota, quotaInput, trafficDetail, trafficParts, trafficText } from "./traffic";
import { create } from "@bufbuild/protobuf";
import { TrafficQuotaMode, TrafficSchema } from "../gen/heron/v1/types_pb";

describe("quota conversion", () => {
  it("rounds decimal input once at whole bytes", () => {
    expect(parseQuota("0.001", "GiB")).toBe(1073742n);
    expect(parseQuota("0.001", "GB")).toBe(1000000n);
    expect(parseQuota("1", "TiB")).toBe(1n << 40n);
    expect(parseQuota("1", "TB")).toBe(1000000000000n);
    expect(parseQuota("", "GiB")).toBe(0n);
  });
  it("rejects rounding to zero and out of range instead of disabling quota", () => {
    for (const text of ["0", "0.00000000001", "4294967296", "-1", "NaN", "1e6"]) expect(parseQuota(text, "GiB")).toBeNull();
  });
  it("formats only the server billing numerator and percentage", () => {
    const t = create(TrafficSchema, { periodRx: 800n, periodTx: 200n, quotaBytes: 1000n, quotaUsedBytes: 200n, quotaUsedPct: 20 });
    expect(trafficText(t)).toBe("200 B / 1000 B (20.0%)");
    expect(trafficParts(t)).toEqual({ amount: "200 B / 1000 B", percent: "20.0%" });
    t.quotaBytes = 0n;
    expect(trafficText(t)).toBe("200 B");
    expect(trafficParts(t)).toEqual({ amount: "200 B" });
    expect(quotaInput(1n << 30n, "GiB")).toBe("1");
  });
  it("labels the numerator by the quota mode and falls back to sum without a quota", () => {
    const t = create(TrafficSchema, { quotaBytes: 1000n, quotaMode: TrafficQuotaMode.RX });
    expect(trafficDetail(t)).toBe("仅下载");
    t.quotaMode = TrafficQuotaMode.TX;
    expect(trafficDetail(t)).toBe("仅上传");
    t.quotaMode = TrafficQuotaMode.MAX;
    expect(trafficDetail(t)).toBe("下载与上传取大者");
    t.quotaMode = TrafficQuotaMode.SUM;
    expect(trafficDetail(t)).toBe("下载 + 上传");
    t.quotaBytes = 0n;
    t.quotaMode = TrafficQuotaMode.TX;
    expect(trafficDetail(t)).toBe("下载 + 上传 · 未设配额");
  });
});
