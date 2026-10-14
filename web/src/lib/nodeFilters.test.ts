import { expect, it } from "vitest";
import { liveById } from "./adminStatus";
import { applyScope, applySort, canonicalTagFilter, facetsFromParams, isFaceted, isScoped, NO_FACETS, nodeFacetOptions, nodeListReturnPath, nodeListReturnState, paramsWithFacets, paramsWithRegions, paramsWithScope, paramsWithSearch, paramsWithSort, paramsWithTagFilter, paramsWithTagMatch, regionsFromParams, scopeFromParams, searchFromParams, sortFromParams, tagFilterFromParams, tagMatchFromParams, type NodeFacets, type ScopeFilters } from "./nodeFilters";

it("URL 参数解析：合法值进入筛选，非法值被忽略而不是变成空列表", () => {
  expect(scopeFromParams(new URLSearchParams("status=offline&expiring=1&lagging=1"))).toEqual({ status: "offline", expiring: true, lagging: true });
  expect(scopeFromParams(new URLSearchParams("status=foo&expiring=yes&lagging=0"))).toEqual({ status: null, expiring: false, lagging: false });
  expect(isScoped(scopeFromParams(new URLSearchParams("")))).toBe(false);
});

it("回写只动自己的三个键，其余参数保留；空值删键", () => {
  const next = paramsWithScope(new URLSearchParams("tab=x&status=offline"), { status: null, expiring: true, lagging: false });
  expect(next.toString()).toBe("tab=x&expiring=1");
});

const nodes = [
  { id: 1n, name: "on", country: "JP", tags: ["db"], maintenance: false, facts: { agentVersion: "v0.7.0" }, billing: { daysLeft: 5 } },
  { id: 2n, name: "off", country: "HK", tags: ["DB", "web"], maintenance: false, facts: { agentVersion: "v0.8.0" } },
  { id: 3n, name: "unknown", country: "", tags: ["web"], maintenance: false },
  { id: 4n, name: "maint", country: "JP", tags: [], maintenance: true },
] as never[];
const live = liveById([{ id: 1n, online: true, lastSeenAt: 1n }, { id: 2n, online: false, lastSeenAt: 1n }, { id: 4n, online: true, lastSeenAt: 1n }] as never);

it("按状态 / 到期 / 落后过滤；未知状态只在不按状态筛时出现；筛选可叠加搜索", () => {
  const names = (scope: Parameters<typeof applyScope>[3], search = "") => applyScope(nodes, live, "v0.8.0", scope, search, NO_FACETS).map((n: { name: string }) => n.name);
  expect(names({ status: null, expiring: false, lagging: false })).toEqual(["on", "off", "unknown", "maint"]);
  expect(names({ status: "offline", expiring: false, lagging: false })).toEqual(["off"]);
  expect(names({ status: "maintenance", expiring: false, lagging: false })).toEqual(["maint"]);
  expect(names({ status: null, expiring: true, lagging: false })).toEqual(["on"]);
  expect(names({ status: null, expiring: false, lagging: true })).toEqual(["on"]);
  expect(names({ status: "online", expiring: true, lagging: true }, "on")).toEqual(["on"]);
  expect(applyScope(nodes, live, undefined, { status: null, expiring: false, lagging: true }, "", NO_FACETS)).toEqual([]);
});

it("到期与落后筛选不列已出租节点", () => {
  const withRented = [...nodes, { id: 5n, name: "leased", country: "JP", tags: ["db"], maintenance: false, rented: true, facts: { agentVersion: "v0.7.0" }, billing: { daysLeft: 3 } }] as never[];
  const names = (scope: ScopeFilters) => applyScope(withRented, live, "v0.8.0", scope, "", NO_FACETS).map((n: { name: string }) => n.name);
  expect(names({ status: null, expiring: true, lagging: false })).toEqual(["on"]);
  expect(names({ status: null, expiring: false, lagging: true })).toEqual(["on"]);
});

const noScope: ScopeFilters = { status: null, expiring: false, lagging: false };
const faceted = (facets: Partial<NodeFacets>, search = "", scope = noScope) =>
  applyScope(nodes, live, "v0.8.0", scope, search, { ...NO_FACETS, ...facets }).map((n: { name: string }) => n.name);

it("地区之间取并集，未知地区是空码；空选择不过滤", () => {
  expect(faceted({ regions: ["JP", "HK"] })).toEqual(["on", "off", "maint"]);
  expect(faceted({ regions: [""] })).toEqual(["unknown"]);
  expect(faceted({ regions: [] })).toEqual(["on", "off", "unknown", "maint"]);
});

it("标签默认同时满足、可切满足任一，名字按折叠比较；空选择不过滤", () => {
  expect(faceted({ tags: { kind: "tags", names: ["db", "web"] } })).toEqual(["off"]);
  expect(faceted({ tags: { kind: "tags", names: ["db", "web"] }, tagMatch: "any" })).toEqual(["on", "off", "unknown"]);
  expect(faceted({ tags: { kind: "tags", names: ["db"] }, tagMatch: "all" })).toEqual(["on", "off"]);
  expect(faceted({ tags: { kind: "tags", names: [] }, tagMatch: "any" })).toEqual(["on", "off", "unknown", "maint"]);
});

it("无标签只留没有任何标签的节点；地区、标签与搜索、状态彼此取交集", () => {
  expect(faceted({ tags: { kind: "untagged" } })).toEqual(["maint"]);
  expect(faceted({ tags: { kind: "untagged" }, regions: ["HK"] })).toEqual([]);
  expect(faceted({ regions: ["JP"], tags: { kind: "tags", names: ["db"] } })).toEqual(["on"]);
  expect(faceted({ regions: ["JP", "HK"] }, "o")).toEqual(["on", "off"]);
  expect(faceted({ regions: ["JP", "HK"] }, "", { status: "offline", expiring: false, lagging: false })).toEqual(["off"]);
});

it("地区与匹配方式在 URL 里往返：地区可重复、未知写空值、小写换大写、非法与重复丢弃；匹配方式缺省同时满足、不写进 URL", () => {
  expect(regionsFromParams(new URLSearchParams("region=jp&region=&region=JP&region=JPN&region=1A&region=hk"))).toEqual(["JP", "", "HK"]);
  expect(paramsWithRegions(new URLSearchParams("q=a&region=US"), ["JP", ""]).toString()).toBe("q=a&region=JP&region=");
  expect(regionsFromParams(paramsWithRegions(new URLSearchParams(), ["JP", ""]))).toEqual(["JP", ""]);
  expect(tagMatchFromParams(new URLSearchParams("match=any"))).toBe("any");
  for (const raw of ["", "match=all", "match=ANY", "match=or"]) expect(tagMatchFromParams(new URLSearchParams(raw))).toBe("all");
  expect(paramsWithTagMatch(new URLSearchParams("q=a"), "any").toString()).toBe("q=a&match=any");
  expect(paramsWithTagMatch(new URLSearchParams("q=a&match=any"), "all").toString()).toBe("q=a");
});

it("地区与标签整组读写：标签按清单换写法；清空只删自己的键，其余参数保留", () => {
  const params = new URLSearchParams("status=offline&region=JP&tag=DB&match=any");
  const facets = facetsFromParams(params, ["db"]);
  expect(facets).toEqual({ regions: ["JP"], tags: { kind: "tags", names: ["db"] }, tagMatch: "any" });
  expect(paramsWithFacets(params, facets).toString()).toBe("status=offline&match=any&region=JP&tag=db");
  expect(paramsWithFacets(new URLSearchParams("status=offline&region=JP&untagged=1&match=any"), NO_FACETS).toString()).toBe("status=offline");
  expect(isFaceted(NO_FACETS)).toBe(false);
  expect(isFaceted({ ...NO_FACETS, tagMatch: "any" })).toBe(false);
  expect([{ regions: [""] }, { tags: { kind: "untagged" } }, { tags: { kind: "tags", names: ["db"] } }].map((f) => isFaceted({ ...NO_FACETS, ...f } as NodeFacets))).toEqual([true, true, true]);
});

it("面板选项按全部节点计数；已选却不在选项里的地区与标签列在最后、可取消", () => {
  const options = nodeFacetOptions(nodes, ["db", "web", "idle"], { ...NO_FACETS, regions: ["US", "JP"], tags: { kind: "tags", names: ["gone"] } });
  expect(options.regions).toEqual([
    { value: "HK", label: "香港", count: 1 }, { value: "JP", label: "日本", count: 2 }, { value: "", label: "未知", count: 1 },
    { value: "US", label: "美国", count: 0 },
  ]);
  expect(options.tags).toEqual([
    { value: "db", label: "db", count: 2 }, { value: "web", label: "web", count: 2 }, { value: "idle", label: "idle", count: 0 },
    { value: "gone", label: "gone", count: 0 },
  ]);
  // 清单未到：只有已选的标签，计数照样按节点算。
  expect(nodeFacetOptions(nodes, undefined, { ...NO_FACETS, tags: { kind: "tags", names: ["web"] } }).tags).toEqual([{ value: "web", label: "web", count: 2 }]);
  expect(nodeFacetOptions(nodes, undefined, { ...NO_FACETS, tags: { kind: "untagged" } }).tags).toEqual([]);
});

it("标签过滤与搜索词在 URL 里往返；无标签优先于 tag，空的 tag 丢弃；写入时保留其他参数", () => {
  expect(tagFilterFromParams(new URLSearchParams("tag=db&tag=&tag=prod"))).toEqual({ kind: "tags", names: ["db", "prod"] });
  expect(tagFilterFromParams(new URLSearchParams("tag=db&untagged=1"))).toEqual({ kind: "untagged" });
  expect(tagFilterFromParams(new URLSearchParams("untagged=yes"))).toEqual({ kind: "tags", names: [] });
  const base = new URLSearchParams("status=offline&tag=old&untagged=1");
  expect(paramsWithTagFilter(base, { kind: "tags", names: ["a", "b"] }).toString()).toBe("status=offline&tag=a&tag=b");
  expect(paramsWithTagFilter(base, { kind: "untagged" }).toString()).toBe("status=offline&untagged=1");
  expect(searchFromParams(new URLSearchParams("q=%E9%A6%99%E6%B8%AF"))).toBe("香港");
  expect(paramsWithSearch(new URLSearchParams("q=x&status=offline"), "").toString()).toBe("status=offline");
  expect(searchFromParams(paramsWithSearch(new URLSearchParams(), "a&b=c"))).toBe("a&b=c");
});

it("URL 里的标签按清单换成清单写法并去重，清单没有的原样保留；清单未到或无标签过滤时原样返回", () => {
  expect(canonicalTagFilter({ kind: "tags", names: ["DB", "db", "gone"] }, ["db", "prod"])).toEqual({ kind: "tags", names: ["db", "gone"] });
  expect(canonicalTagFilter({ kind: "tags", names: ["DB"] }, undefined)).toEqual({ kind: "tags", names: ["DB"] });
  expect(canonicalTagFilter({ kind: "untagged" }, ["db"])).toEqual({ kind: "untagged" });
});

it("返回节点列表的路径只认以 ? 开头的字符串 state，其余回到不带筛选的列表", () => {
  expect(nodeListReturnPath(nodeListReturnState("?q=a&tag=db"))).toBe("/nodes?q=a&tag=db");
  for (const state of [nodeListReturnState(""), { nodeListSearch: "//evil.example" }, { nodeListSearch: 1 }, "?q=a", null, undefined]) {
    expect(nodeListReturnPath(state)).toBe("/nodes");
  }
});


it("排序参数：只认 sort=expiry，其余当没写；回写只动 sort 键", () => {
  expect(sortFromParams(new URLSearchParams("sort=expiry"))).toBe("expiry");
  expect(sortFromParams(new URLSearchParams("sort=name"))).toBeNull();
  expect(sortFromParams(new URLSearchParams(""))).toBeNull();
  expect(paramsWithSort(new URLSearchParams("tab=x&sort=expiry"), null).toString()).toBe("tab=x");
  expect(paramsWithSort(new URLSearchParams("tab=x"), "expiry").toString()).toBe("tab=x&sort=expiry");
});

it("按到期排序：已过期在前、剩余天数升序，没有剩余天数的排最后并保持原顺序；不排序时原样返回", () => {
  const dated = [
    { name: "a", billing: { daysLeft: 12 } },
    { name: "b" },
    { name: "c", billing: { daysLeft: -3 } },
    { name: "d", billing: { expiresOn: "2030-01-01" } },
    { name: "e", billing: { daysLeft: 0 } },
  ] as never[];
  const names = (sort: Parameters<typeof applySort>[1]) => applySort(dated, sort).map((n: { name: string }) => n.name);
  expect(names("expiry")).toEqual(["c", "e", "a", "b", "d"]);
  expect(names(null)).toEqual(["a", "b", "c", "d", "e"]);
});
