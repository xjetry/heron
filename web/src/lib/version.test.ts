import { describe, expect, it } from "vitest";
import { isStableRelease, olderThan } from "./version";

describe("olderThan 按 semver 2.0 优先级比较", () => {
  it.each([
    ["v1.9.0", "v1.10.0"],
    ["v0.9.9", "v1.0.0"],
    ["v2.0.0", "v10.0.0"],
    ["v1.1.0-rc.1", "v1.1.1"],
    // 同一 MAJOR.MINOR.PATCH 下预发布低于正式版。
    ["v1.1.0-rc.1", "v1.1.0"],
    ["v1.1.0-alpha", "v1.1.0-beta"],
    ["v1.1.0-alpha.1", "v1.1.0-alpha.2"],
    // 纯数字标识符按数值比，不按字符串比。
    ["v1.1.0-alpha.2", "v1.1.0-alpha.10"],
    ["v1.1.0-alpha.99999999999999999999", "v1.1.0-alpha.100000000000000000000"],
    // 前缀全等时段数多者更高。
    ["v1.1.0-alpha", "v1.1.0-alpha.1"],
    // 数字标识符低于字母数字标识符。
    ["v1.1.0-alpha.1", "v1.1.0-alpha.beta"],
    // 含字母或连字符的标识符按 ASCII 比。
    ["v1.1.0-a-b", "v1.1.0-a-c"],
    ["v1.1.0-RC", "v1.1.0-rc"],
    // 构建元数据能解析且不参与比较。
    ["v1.2.3+build.5", "v1.2.4"],
    ["v1.1.0-rc.1+b9", "v1.1.0+b1"],
  ])("%s 落后于 %s", (current, target) => {
    expect(olderThan(current, target)).toBe(true);
  });

  it.each([
    ["v1.1.0", "v1.1.0"],
    ["v1.2.0", "v1.1.0"],
    // 目标为预发布、当前为对应的正式版：当前更高。
    ["v1.1.0", "v1.1.0-rc.1"],
    ["v1.1.0-beta", "v1.1.0-alpha"],
    ["v1.1.0-alpha.10", "v1.1.0-alpha.2"],
    ["v1.1.0-alpha.1", "v1.1.0-alpha"],
    // 只差构建元数据视为同一版本。
    ["v1.1.0+b1", "v1.1.0+b2"],
    ["v1.1.0+b2", "v1.1.0+b1"],
  ])("%s 不落后于 %s", (current, target) => {
    expect(olderThan(current, target)).toBe(false);
  });

  // 任一方不是合法的正式或预发布版本号就没有可比的次序，不标。
  it.each([
    ["dev", "v1.1.0"],
    ["v1.0.0", "dev"],
    ["v1.0.0", ""],
    [undefined, "v1.1.0"],
    ["", "v1.1.0"],
    ["1.0.0", "v1.1.0"],
    ["v1.0", "v1.1.0"],
    ["v01.0.0", "v1.1.0"],
    // 前导 0 不合法：宽松解析会把下面两对判成落后。
    ["v1.0.0", "v01.1.0"],
    ["v0.9.09", "v0.9.10"],
    ["v1.1.0-alpha.1", "v1.1.0-alpha.01"],
    ["v1.0.0-", "v1.1.0"],
    ["v1.0.0-01", "v1.1.0"],
    ["v1.0.0-a..b", "v1.1.0"],
    ["v1.0.0+", "v1.1.0"],
    ["v1.0.0+a_b", "v1.1.0"],
    ["v1.0.0 ", "v1.1.0"],
  ])("%s 对 %s 解析失败不标", (current, target) => {
    expect(olderThan(current, target)).toBe(false);
  });
});

describe("isStableRelease 与 hub 的 ValidVersion 同一口径", () => {
  it.each(["v0.5.4", "v1.0.0", "v10.20.30"])("%s 是正式版", (v) => {
    expect(isStableRelease(v)).toBe(true);
  });
  it.each(["v4294967295.0.0", "v0.4294967295.0"])("%s 在 uint32 上界内", (v) => {
    expect(isStableRelease(v)).toBe(true);
  });
  it.each(["", "dev", "v0.5.4-rc.1", "v0.5.4+b.1", "0.5.4", "v1.0", "v01.0.0", "v4294967296.0.0", "v0.0.99999999999"])("%s 不是正式版", (v) => {
    expect(isStableRelease(v)).toBe(false);
  });
});
