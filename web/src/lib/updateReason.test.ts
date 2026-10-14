// @vitest-environment node
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, it } from "vitest";
import { UPDATE_REASONS, updateReasonText } from "./updateReason";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
const sources = ["internal/update/protocol.go", "internal/hub/updates/manager.go", "internal/hub/api/updates.go", "internal/agent/client/update.go"];
const literals = new Set(sources.flatMap((file) => [...readFileSync(resolve(root, file), "utf8").matchAll(/\bReason:\s*"([^"]+)"/g)].map((m) => m[1])));

// 两个方向都钉：Go 新增的原文没有译文会在界面上露出英文；表里留着 Go 已删的原文说明表过期了。
it("每条 Go 里的 Reason 字面量都有译文", () => {
  expect(literals.size).toBeGreaterThan(0);
  expect([...literals].filter((r) => !(r in UPDATE_REASONS))).toEqual([]);
});

it("译文表里的每条原文都还在 Go 源码里", () => {
  expect(Object.keys(UPDATE_REASONS).filter((r) => !literals.has(r))).toEqual([]);
});

it("未登记的原文没有译文，原型链上的键也不算登记", () => {
  expect(updateReasonText("local updater protocol is incompatible")).toBe("本机更新器协议不兼容");
  expect(updateReasonText("dial unix /run/heron: no such file")).toBeUndefined();
  expect(updateReasonText("constructor")).toBeUndefined();
});
