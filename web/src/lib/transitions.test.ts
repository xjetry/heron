// @vitest-environment node
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, it } from "vitest";
import { TRANSITIONS } from "./alerts";

const alertGo = readFileSync(resolve(dirname(fileURLToPath(import.meta.url)), "../../../internal/hub/store/alert.go"), "utf8");

// store.Transition 的每个常量都写成 `Name Transition = "value"`；取出全部取值，与面板的标签表逐值比对。两个方向都要
// 成立：Go 新增的取值没有标签，面板把它原样显示；表里留着 Go 已删的取值，说明表与 hub 已经脱节。
it("TRANSITIONS 与 hub 的 store.Transition 常量逐值一致", () => {
  const values = [...alertGo.matchAll(/\bTransition\w*\s+Transition\s*=\s*"([^"]+)"/g)].map((m) => m[1]);
  expect(values).toContain("firing");
  expect(Object.keys(TRANSITIONS).sort()).toEqual([...values].sort());
});
