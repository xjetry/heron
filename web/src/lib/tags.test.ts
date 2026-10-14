import { describe, expect, it } from "vitest";
import { matchesTags, sameTag, withTag, withoutTag } from "./tags";

describe("tags", () => {
  it.each([
    ["db", "DB", true], [" db ", "Db", true], ["Σ", "ς", true], ["K", "k", true],
    ["ß", "ss", false], ["db", "d", false], ["a.b", "axb", false], ["客户A", "客户a", true],
    ["web server", "Web Server", true], ["web server", "webserver", false],
    // 首尾空白与 hub 的 strings.TrimSpace 同一字符集：U+0085 去掉，U+FEFF 不去。
    ["\u0085db\u0085", "db", true], ["\uFEFFdb", "db", false], ["\u3000db", "db", true],
  ])("sameTag(%j, %j) = %s", (a, b, same) => {
    expect(sameTag(a, b)).toBe(same);
  });

  it("withTag 去首尾空白、跳过空串与折叠后重复，保留已有写法", () => {
    expect(withTag(["db"], "  web ")).toEqual(["db", "web"]);
    expect(withTag(["db"], " web server ")).toEqual(["db", "web server"]);
    expect(withTag(["web server"], "WEB SERVER")).toEqual(["web server"]);
    expect(withTag(["db"], "DB")).toEqual(["db"]);
    expect(withTag(["db"], "   ")).toEqual(["db"]);
    expect(withTag([], "\u0085web\u0085")).toEqual(["web"]);
    expect(withTag([], "\uFEFFweb")).toEqual(["\uFEFFweb"]);
  });

  it("withoutTag 按折叠后的名字移除", () => {
    expect(withoutTag(["db", "web"], "DB")).toEqual(["web"]);
  });

  describe("matchesTags", () => {
    it.each(["all", "any"] as const)("空选择匹配一切，含没有标签的节点（%s）", (match) => {
      expect(matchesTags([], [], match)).toBe(true);
      expect(matchesTags(["db"], [], match)).toBe(true);
    });
    it("all 取交集，按折叠比较", () => {
      expect(matchesTags(["db", "web"], ["web", "db"], "all")).toBe(true);
      expect(matchesTags(["db"], ["web", "db"], "all")).toBe(false);
      expect(matchesTags(["DB"], ["db"], "all")).toBe(true);
      expect(matchesTags([], ["db"], "all")).toBe(false);
    });
    it("any 取并集，按折叠比较", () => {
      expect(matchesTags(["db"], ["web", "db"], "any")).toBe(true);
      expect(matchesTags(["DB"], ["web", "db"], "any")).toBe(true);
      expect(matchesTags(["lab"], ["web", "db"], "any")).toBe(false);
      expect(matchesTags([], ["db"], "any")).toBe(false);
    });
  });
});
