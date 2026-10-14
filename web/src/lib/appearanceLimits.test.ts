// @vitest-environment node
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, it } from "vitest";
import { LOGO_TYPES, MAX_CSS_BYTES, MAX_LOGO_BYTES, MAX_TITLE_BYTES, MAX_TITLE_CHARS, THEMES, sizeProblems } from "./appearance";
import { MAX_NOTIFY_CHANNELS } from "./alerts";

const settingsGo = readFileSync(resolve(dirname(fileURLToPath(import.meta.url)), "../../../internal/hub/api/settings.go"), "utf8");

// 常量写成 N 或 N << S；找不到就让用例红，而不是比对 undefined。
function goConst(name: string): number {
  const m = settingsGo.match(new RegExp(`\\b${name}\\s*=\\s*(\\d+)(?:\\s*<<\\s*(\\d+))?`));
  if (!m) throw new Error(`${name} not found in settings.go`);
  return Number(m[1]) * 2 ** Number(m[2] ?? 0);
}

function goStrings(name: string): string[] {
  const m = settingsGo.match(new RegExp(`\\b${name}\\s*=\\s*\\[\\]string\\{([^}]*)\\}`));
  if (!m) throw new Error(`${name} not found in settings.go`);
  return [...m[1].matchAll(/"([^"]*)"/g)].map((s) => s[1]);
}

it("上限与取值和 hub 的 settings.go 一致", () => {
  expect(MAX_TITLE_CHARS).toBe(goConst("maxTitleRunes"));
  expect(MAX_TITLE_BYTES).toBe(goConst("maxTitleBytes"));
  expect(MAX_LOGO_BYTES).toBe(goConst("maxLogoBytes"));
  expect(MAX_CSS_BYTES).toBe(goConst("maxCSSBytes"));
  expect([...THEMES]).toEqual(goStrings("themes"));
  expect([...LOGO_TYPES]).toEqual(goStrings("logoTypes"));
  expect(MAX_NOTIFY_CHANNELS).toBe(goConst("maxNotifyChannels"));
});

it("大小按 UTF-8 字节计，恰在上限时不报", () => {
  expect(sizeProblems("a".repeat(MAX_TITLE_BYTES), "a".repeat(MAX_LOGO_BYTES), "a".repeat(MAX_CSS_BYTES))).toEqual([]);
  expect(sizeProblems("", "a".repeat(MAX_LOGO_BYTES + 1), "")).toHaveLength(1);
  // 342 个"中"是 1026 字节，UTF-16 长度只有 342；21846 个是 65538 字节。
  expect(sizeProblems("中".repeat(342), "", "")).toEqual([`标题 1026 字节，上限 ${MAX_TITLE_BYTES} 字节（去掉控制字符与首尾空白之前计）。`]);
  const css = "中".repeat(21846);
  expect(sizeProblems("", "", css)).toEqual([`自定义 CSS 65538 字节，上限 ${MAX_CSS_BYTES} 字节（64 KiB）。`]);
});
