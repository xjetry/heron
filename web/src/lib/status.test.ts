import { expect, it } from "vitest";
import { EXPIRING_DAYS, expiryLevel, nodeStatus, STATUS_LABEL, STATUS_ORDER, usageLevel } from "./status";

it.each([
  [{ online: true, maintenance: false, rented: false, lastSeenAt: 10n }, "online"],
  [{ online: false, maintenance: false, rented: false, lastSeenAt: 10n }, "offline"],
  [{ online: false, maintenance: false, rented: false }, "never"],
  // 维护中优先：站长主动摘出的节点即便仍在上报也不算在线。
  [{ online: true, maintenance: true, rented: false, lastSeenAt: 10n }, "maintenance"],
  [{ online: false, maintenance: true, rented: false }, "maintenance"],
  // 已出租优先于一切：即使还有上报也显示"已出租"；与维护中互斥由 hub 保证，这里不假设。
  [{ online: true, maintenance: false, rented: true, lastSeenAt: 10n }, "rented"],
  [{ online: false, maintenance: false, rented: true }, "rented"],
  [{ online: true, maintenance: true, rented: true, lastSeenAt: 10n }, "rented"],
] as const)("nodeStatus(%o) = %s", (input, want) => {
  expect(nodeStatus(input)).toBe(want);
});

it("五种状态各有标签，顺序表覆盖全部五种且不重复", () => {
  expect(Object.keys(STATUS_LABEL).sort()).toEqual(["maintenance", "never", "offline", "online", "rented"]);
  expect([...STATUS_ORDER].sort()).toEqual(["maintenance", "never", "offline", "online", "rented"]);
  expect([...STATUS_ORDER]).toEqual(["online", "maintenance", "rented", "never", "offline"]);
  expect(STATUS_LABEL.never).toBe("从未上报");
  expect(STATUS_LABEL.rented).toBe("已出租");
});

it.each([
  [0, "neutral"], [69.9, "neutral"], [70, "attention"], [90, "attention"], [90.1, "critical"], [100, "critical"],
])("usageLevel(%d) = %s", (v, want) => {
  expect(usageLevel(v)).toBe(want);
});

it.each([
  [[undefined, false], "neutral"],
  [[16, false], "neutral"],
  [[15, false], "attention"],
  [[0, false], "attention"],
  [[-1, false], "critical"],
  // 已出租恒为中性：出租期间的续费不由 hub 上色，过期与临近一样。
  [[-1, true], "neutral"],
  [[3, true], "neutral"],
] as const)("expiryLevel(%j) = %s", ([d, rented], want) => {
  expect(expiryLevel(d, rented)).toBe(want);
});

// 「N 天内到期」卡片、筛选与到期上色共用这一个数：常量变了，边界必须跟着变。未出租才走这条边界。
it("EXPIRING_DAYS 是 15 且就是需关注的上边界", () => {
  expect(EXPIRING_DAYS).toBe(15);
  expect(expiryLevel(EXPIRING_DAYS, false)).toBe("attention");
  expect(expiryLevel(EXPIRING_DAYS + 1, false)).toBe("neutral");
});
