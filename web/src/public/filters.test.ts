import { create } from "@bufbuild/protobuf";
import { expect, it } from "vitest";
import { PublicNodeSchema, type PublicNode } from "../gen/heron/v1/public_pb";
import { NODE_SORTS, COLOR_BYS, filterPublicNodes, groupByRegion, groupByTag, matchesSearch, NO_FILTERS, sortNodes, statusCounts, summarize, tileLevel, type NodeSort, type ColorBy, type PublicFilters } from "./filters";

const node = (init: Parameters<typeof create<typeof PublicNodeSchema>>[1]): PublicNode => create(PublicNodeSchema, init);
const nodes = [
  node({ id: 1n, name: "tokyo-1", country: "JP", online: true, lastSeenAt: 9n, tags: ["prod"], metrics: { cpuPct: 95, memUsed: 8n, memTotal: 10n, netRxBps: 100n, netTxBps: 10n }, traffic: { periodRx: 5n, periodTx: 5n } }),
  node({ id: 2n, name: "tokyo-2", country: "JP", online: false, lastSeenAt: 1n, tags: ["prod", "DB"], publicRemark: "联通 4837", metrics: { cpuPct: 10, netRxBps: 999n }, billing: { expiresOn: "2030-01-01", daysLeft: 12 } }),
  node({ id: 3n, name: "hk-1", country: "HK", online: true, lastSeenAt: 9n, maintenance: true, metrics: { cpuPct: 50, netRxBps: 1n, netTxBps: 1n }, traffic: { periodRx: 1n, periodTx: 0n } }),
  node({ id: 4n, name: "fresh", country: "", online: false }),
  node({ id: 5n, name: "hk-2", country: "HK", online: true, lastSeenAt: 9n, metrics: { cpuPct: 72 }, billing: { expiresOn: "2026-01-01", daysLeft: -3 } }),
];
const names = (list: readonly PublicNode[]) => list.map((n) => n.name);

it.each([
  [nodes[1], "db", true],
  [nodes[1], "4837", true],
  [nodes[1], "TOKYO", true],
  [nodes[1], "t.k", false],
  [nodes[3], "", true],
] as const)("搜索名称、标签、公开备注，按折叠大小写的字面匹配：%o / %s", (n, search, expected) => {
  expect(matchesSearch(n, search)).toBe(expected);
});

it.each<[PublicFilters, string[]]>([
  [NO_FILTERS, ["tokyo-1", "tokyo-2", "hk-1", "fresh", "hk-2"]],
  [{ ...NO_FILTERS, regions: ["JP", ""] }, ["tokyo-1", "tokyo-2", "fresh"]],
  [{ ...NO_FILTERS, tags: ["prod", "db"] }, ["tokyo-2"]],
  [{ ...NO_FILTERS, tags: ["prod", "db"], tagMatch: "any" }, ["tokyo-1", "tokyo-2"]],
  [{ ...NO_FILTERS, status: "online" }, ["tokyo-1", "hk-2"]],
  [{ ...NO_FILTERS, search: "hk", status: "online" }, ["hk-2"]],
  [{ ...NO_FILTERS, status: "offline" }, ["tokyo-2", "hk-1", "fresh"]],
  [{ ...NO_FILTERS, search: "hk", status: "offline" }, ["hk-1"]],
])("筛选取交集，地区取并集、标签默认取交集（可改并集）、在线排除维护中、离线是在线以外的全部：%o", (filters, expected) => {
  expect(names(filterPublicNodes(nodes, filters))).toEqual(expected);
});

it("已出租的节点即使 online 为真也归离线，不归在线：在线与离线互补", () => {
  const rented = node({ id: 6n, name: "rented-1", country: "JP", online: true, lastSeenAt: 9n, rented: true });
  const list = [...nodes, rented];
  expect(names(filterPublicNodes(list, { ...NO_FILTERS, status: "online" }))).not.toContain("rented-1");
  expect(names(filterPublicNodes(list, { ...NO_FILTERS, status: "offline" }))).toContain("rented-1");
  const online = filterPublicNodes(list, { ...NO_FILTERS, status: "online" });
  const offline = filterPublicNodes(list, { ...NO_FILTERS, status: "offline" });
  expect(online.length + offline.length).toBe(list.length);
  expect(online.filter((n) => offline.includes(n))).toEqual([]);
});

it("状态计数按其余筛选算、不看状态筛选本身，在线加离线等于全部", () => {
  expect(statusCounts(nodes, NO_FILTERS)).toEqual({ all: 5, online: 2, offline: 3 });
  expect(statusCounts(nodes, { ...NO_FILTERS, status: "offline" })).toEqual({ all: 5, online: 2, offline: 3 });
  expect(statusCounts(nodes, { ...NO_FILTERS, search: "hk", status: "online" })).toEqual({ all: 2, online: 1, offline: 1 });
  expect(statusCounts(nodes, { ...NO_FILTERS, regions: ["JP"] })).toEqual({ all: 2, online: 1, offline: 1 });
});

it.each<[NodeSort, string[]]>([
  ["default", ["tokyo-1", "tokyo-2", "hk-1", "fresh", "hk-2"]],
  ["expiry", ["hk-2", "tokyo-2", "tokyo-1", "hk-1", "fresh"]],
  ["cpu", ["tokyo-1", "hk-2", "hk-1", "tokyo-2", "fresh"]],
  ["traffic", ["tokyo-1", "hk-1", "tokyo-2", "fresh", "hk-2"]],
])("排序 %s 不修改输入，到期升序、读数降序且缺失最后", (sort, expected) => {
  expect(names(sortNodes(Object.freeze([...nodes]), sort))).toEqual(expected);
});

it("流量按完整 bigint 排序，不因超过安全整数丢失大小关系", () => {
  const smaller = node({ name: "small", traffic: { periodRx: 2n ** 53n, quotaUsedBytes: 2n ** 53n } });
  const larger = node({ name: "large", traffic: { periodRx: 2n ** 53n, periodTx: 1n, quotaUsedBytes: 2n ** 53n + 1n } });
  expect(names(sortNodes([smaller, larger], "traffic"))).toEqual(["large", "small"]);
});

it.each<[PublicNode, ColorBy, string | undefined]>([
  [nodes[0], "status", undefined],
  [nodes[0], "cpu", "critical"],
  [nodes[0], "memory", "attention"],
  [nodes[3], "cpu", undefined],
  [node({ metrics: { memTotal: 10n } }), "memory", undefined],
  [node({ metrics: { memUsed: 1n, memTotal: 0n } }), "memory", undefined],
  [node({ metrics: { memUsed: 0n, memTotal: 10n } }), "memory", "neutral"],
  [nodes[1], "expiry", "attention"],
  [nodes[4], "expiry", "critical"],
  [nodes[0], "expiry", "neutral"],
  // 已出租不上到期色：即使已过期也是中性。
  [node({ billing: { expiresOn: "2026-01-01", daysLeft: -1 }, rented: true }), "expiry", "neutral"],
])("着色依据 %o / %s：无使用量读数不给档位，无到期日为中性", (n, by, expected) => {
  expect(tileLevel(n, by)).toBe(expected);
});

it("地区分组按四态算在线数，在线数与总数相同按代码排序", () => {
  expect(groupByRegion(nodes).map((g) => [g.key, g.name, g.online, names(g.nodes)])).toEqual([
    ["HK", "香港", 1, ["hk-1", "hk-2"]],
    ["JP", "日本", 1, ["tokyo-1", "tokyo-2"]],
    ["", "未知", 0, ["fresh"]],
  ]);
});

it("地区按在线数降序优先于总数与代码，未知即使在线更多仍最后", () => {
  const input = [
    node({ country: "DE", online: true, lastSeenAt: 1n }),
    node({ country: "DE", lastSeenAt: 1n }),
    node({ country: "DE", lastSeenAt: 1n }),
    node({ country: "US", online: true, lastSeenAt: 1n }),
    node({ country: "US", online: true, lastSeenAt: 1n }),
    ...Array.from({ length: 4 }, () => node({ online: true, lastSeenAt: 1n })),
  ];
  expect(groupByRegion(input).map((g) => g.key)).toEqual(["US", "DE", ""]);
});

it("同在线数先按总数降序，再按代码", () => {
  const input = [
    node({ country: "DE", online: true, lastSeenAt: 1n }),
    node({ country: "US", online: true, lastSeenAt: 1n }),
    node({ country: "US", lastSeenAt: 1n }),
  ];
  expect(groupByRegion(input).map((g) => g.key)).toEqual(["US", "DE"]);
});

it("只有未知地区时只有一个组", () => {
  expect(groupByRegion([nodes[3]]).map((g) => [g.key, g.name])).toEqual([["", "未知"]]);
});

it("空快照没有地区组", () => {
  expect(groupByRegion([])).toEqual([]);
});

it("按标签分组：节点出现在它的每个标签组里，标签按折叠比较归组、组名取 hub 的写法，无标签最后", () => {
  expect(groupByTag(nodes, ["db", "prod"]).map((g) => [g.key, g.name, g.online, names(g.nodes)])).toEqual([
    ["tag:prod", "prod", 1, ["tokyo-1", "tokyo-2"]],
    ["tag:db", "db", 0, ["tokyo-2"]],
    ["untagged", "无标签", 1, ["hk-1", "fresh", "hk-2"]],
  ]);
});

it("标签组同在线数先按总数降序，再按 hub 的标签顺序；无标签即使在线更多仍最后；筛掉后没有节点的标签不成组", () => {
  const input = [
    node({ tags: ["b"], online: true, lastSeenAt: 1n }),
    node({ tags: ["a"], online: true, lastSeenAt: 1n }),
    node({ tags: ["c"], online: true, lastSeenAt: 1n }),
    node({ tags: ["c"], lastSeenAt: 1n }),
    ...Array.from({ length: 3 }, () => node({ online: true, lastSeenAt: 1n })),
  ];
  expect(groupByTag(input, ["b", "a", "c", "gone"]).map((g) => g.key)).toEqual(["tag:c", "tag:b", "tag:a", "untagged"]);
  expect(groupByTag([], ["a"])).toEqual([]);
});

it("汇总五态互斥，速率只累计在线与维护中，已出租不计入实时速率", () => {
  const rented = node({ id: 6n, name: "rented", online: true, lastSeenAt: 9n, rented: true, metrics: { netRxBps: 500n, netTxBps: 50n }, traffic: { periodRx: 7n, periodTx: 0n } });
  expect(summarize([...nodes, rented])).toEqual({ total: 6, counts: { online: 2, maintenance: 1, rented: 1, never: 1, offline: 1 }, rxBps: 101n, txBps: 11n, periodBytes: 18n });
});

it("离线节点本周期流量仍累计，旧速率不计入实时合计", () => {
  expect(summarize([node({ lastSeenAt: 1n, metrics: { netRxBps: 999n, netTxBps: 999n }, traffic: { periodRx: 2n, periodTx: 3n } })]))
    .toEqual({ total: 1, counts: { online: 0, maintenance: 0, rented: 0, never: 0, offline: 1 }, rxBps: 0n, txBps: 0n, periodBytes: 5n });
});

it("空快照汇总全为零", () => {
  expect(summarize([])).toEqual({ total: 0, counts: { online: 0, maintenance: 0, rented: 0, never: 0, offline: 0 }, rxBps: 0n, txBps: 0n, periodBytes: 0n });
});

it("排序与着色选项使用约定的值、标签及顺序", () => {
  expect([NODE_SORTS, COLOR_BYS]).toEqual([
    [{ value: "default", label: "默认" }, { value: "expiry", label: "到期" }, { value: "cpu", label: "CPU" }, { value: "traffic", label: "流量" }],
    [{ value: "status", label: "状态" }, { value: "cpu", label: "CPU" }, { value: "memory", label: "内存" }, { value: "expiry", label: "到期" }],
  ]);
});
