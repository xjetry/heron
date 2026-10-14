// @vitest-environment node
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, it } from "vitest";
import { ANSWERS_PER_NODE } from "./country";

const geoGo = readFileSync(resolve(dirname(fileURLToPath(import.meta.url)), "../../../internal/hub/geo/geo.go"), "utf8");

// 面板文案写的上界与 hub 实际保留的地址数一致；找不到常量就让用例红，而不是比对 undefined。
it("答案表的地址数与 hub 的 geo.go 一致", () => {
  const m = geoGo.match(/\banswersPerNode\s*=\s*(\d+)/);
  if (!m) throw new Error("answersPerNode not found in geo.go");
  expect(ANSWERS_PER_NODE).toBe(Number(m[1]));
});
