import { expect, it } from "vitest";
import { liveById } from "./adminStatus";
import { attentionCards } from "./attention";

const nodes = [
  { id: 1n, name: "on", maintenance: false, facts: { agentVersion: "v0.8.0" }, billing: { daysLeft: 12 } },
  { id: 2n, name: "off", maintenance: false, facts: { agentVersion: "v0.7.0" }, billing: { daysLeft: -3 } },
  { id: 3n, name: "never", maintenance: false },
  { id: 4n, name: "maint", maintenance: true, facts: { agentVersion: "dev" }, billing: { daysLeft: 400 } },
  { id: 5n, name: "unknown", maintenance: false, facts: { agentVersion: "v0.6.0" } },
  // 已出租：剩 3 天且版本低于绑定，两张卡都不该把它算进去；离线卡也不变（状态是已出租，不是离线或从未上报）。
  { id: 6n, name: "leased", maintenance: false, rented: true, facts: { agentVersion: "v0.6.0" }, billing: { daysLeft: 3 } },
] as never[];
const live = liveById([{ id: 1n, online: true, lastSeenAt: 9n }, { id: 2n, online: false, lastSeenAt: 1n }, { id: 3n, online: false }, { id: 4n, online: true, lastSeenAt: 9n }] as never);
const states = [{ ruleId: 1n, nodeId: 1n, state: "firing" }, { ruleId: 1n, nodeId: 2n, state: "pending" }, { ruleId: 2n, nodeId: 1n, state: "firing" }] as never[];

it("四个数各按自己的谓词：离线不含未知与维护中，到期含已过期，落后只比绑定版本，触发按状态条数；已出租不进到期与落后", () => {
  const cards = attentionCards({ nodes, live, boundAgentVersion: "v0.8.0", states });
  expect(cards.map((c) => [c.key, c.count, c.note, c.to])).toEqual([
    ["offline", 1, "从未上报 1", "/nodes?status=offline"],
    ["expiring", 2, "已过期 1", "/nodes?expiring=1&sort=expiry"],
    ["lagging", 2, "低于 v0.8.0", "/nodes?lagging=1"],
    ["firing", 2, undefined, "/alerts?state=firing"],
  ]);
});

it("绑定版本未知时落后数是 null 并说明原因；告警状态未到时触发数是 null；没有节点时四个数都是 0", () => {
  const unknown = attentionCards({ nodes, live, boundAgentVersion: undefined, states: undefined });
  expect(unknown[2]).toMatchObject({ count: null, note: "无法取得 hub 绑定的 agent 版本" });
  expect(unknown[3].count).toBeNull();
  expect(attentionCards({ nodes: [], live: new Map(), boundAgentVersion: "v0.8.0", states: [] }).map((c) => c.count)).toEqual([0, 0, 0, 0]);
});
