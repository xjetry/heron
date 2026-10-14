import { expect, it } from "vitest";
import { liveById, liveStatus } from "./adminStatus";

const live = liveById([{ id: 1n, name: "a", online: true, lastSeenAt: 100n }, { id: 2n, name: "b", online: false, lastSeenAt: 50n }, { id: 3n, name: "c", online: false }] as never);

it("快照 + 维护中合成五态；维护中压过在线；从未上报看快照的 lastSeenAt", () => {
  expect(liveStatus({ maintenance: false, rented: false }, live.get(1n))).toBe("online");
  expect(liveStatus({ maintenance: true, rented: false }, live.get(1n))).toBe("maintenance");
  expect(liveStatus({ maintenance: false, rented: false }, live.get(2n))).toBe("offline");
  expect(liveStatus({ maintenance: false, rented: false }, live.get(3n))).toBe("never");
});

it("快照里没有这个节点：维护中仍是维护中，其余是未知而不是离线", () => {
  expect(liveStatus({ maintenance: true, rented: false }, undefined)).toBe("maintenance");
  expect(liveStatus({ maintenance: false, rented: false }, undefined)).toBeUndefined();
});

// 已出租由站长设置，不依赖快照；与维护中同一例外。
it("快照里没有该节点但已出租 → rented", () => {
  expect(liveStatus({ maintenance: false, rented: true }, undefined)).toBe("rented");
});
