import { expect, it } from "vitest";
import { laggingNode } from "./lagging";

it.each([
  [{ rented: false, facts: { agentVersion: "v0.7.0" } }, "v0.8.0", true],
  [{ rented: true, facts: { agentVersion: "v0.7.0" } }, "v0.8.0", false], // 没有 agent 就没有"落后"
  [{ rented: false, facts: { agentVersion: "v0.8.0" } }, "v0.8.0", false],
  [{ rented: false }, "v0.8.0", false], // 版本未知不算落后
  [{ rented: false, facts: { agentVersion: "v0.7.0" } }, undefined, false], // 绑定未知谁都判不出
])("laggingNode(%o, %s) = %s", (node, bound, want) => {
  expect(laggingNode(node as never, bound)).toBe(want);
});
