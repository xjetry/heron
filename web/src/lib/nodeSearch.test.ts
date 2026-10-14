import { describe, expect, it } from "vitest";
import { filterNodes } from "./nodeSearch";

const nodes = [
  { id: 1, name: "Web-Alpha", note: "", facts: { hostname: "frontend.internal" } },
  { id: 2, name: "db", note: "Customer-BETA", facts: { hostname: "sql.internal" } },
  { id: 3, name: "worker", note: "", facts: { hostname: "Compute-GAMMA.internal" } },
  { id: 4, name: "", note: "" },
];

describe("filterNodes", () => {
  it.each(["8.8.8", "2606:4700", "JP", "1.1.1"])("搜索地址或地区 %s 与名称共用过滤口径", (search) => {
    const match = { name: "tokyo", country: "JP", lastSource: "1.1.1.1", network: { ipv4: { address: "8.8.8.8" }, ipv6: { address: "2606:4700:4700::1111" } }, facts: { hostname: "host" } };
    expect(filterNodes([...nodes, match], search)).toEqual([match]);
  });
  // 地址按显示值搜：手填的地址能搜到；被手填取代的 agent 探测值只留在 facts 里，不参与。
  it("手填地址可搜，被取代的探测值不参与", () => {
    const pinned = { name: "edge", network: { ipv4: { address: "9.9.9.9" } }, facts: { hostname: "h", network: { ipv4: { address: "4.4.4.4" } } } };
    expect(filterNodes([...nodes, pinned], "9.9.9")).toEqual([pinned]);
    expect(filterNodes([...nodes, pinned], "4.4.4")).toEqual([]);
  });
  it.each([
    ["名称", "aLpHa", [nodes[0]]],
    ["备注", "r-bEtA", [nodes[1]]],
    ["主机名", "e-gAmMa", [nodes[2]]],
    ["无匹配", "absent", []],
    ["空输入", "", nodes],
    ["空白不等于空输入", " ", []],
  ])("%s 按子串过滤且保留顺序", (_field, search, expected) => {
    expect(filterNodes(nodes, search)).toEqual(expected);
  });

  it.each(["", "anything"])("零个节点输入 %j 返回空数组", (search) => {
    expect(filterNodes([], search)).toEqual([]);
  });

  it.each([
    ["Σ", "ς", true], ["ſ", "S", true], ["K", "k", true],
    ["İ", "i", false], ["ß", "ss", false],
  ])("Unicode 简单折叠 %s / %s = %s", (name, search, matches) => {
    const input = [{ name }];
    expect(filterNodes(input, search)).toEqual(matches ? input : []);
  });

  it.each([".", "[", "*", "\\", "(a|b)"])("搜索 %s 按字面量而非正则表达式匹配", (search) => {
    const literal = { name: `prefix${search}suffix` };
    expect(filterNodes([{ name: "unrelated" }, literal], search)).toEqual([literal]);
  });
});
