import { expect, it } from "vitest";
import { createFlagRenderSupport, regionalIndicators } from "./flagEmoji";

it("两位 ASCII 国家码转区域指示符对；其他输入返回空串", () => {
  expect(regionalIndicators("HK")).toBe("🇭🇰");
  expect(regionalIndicators("jp")).toBe("🇯🇵");
  expect(regionalIndicators("")).toBe("");
  expect(regionalIndicators("H")).toBe("");
  expect(regionalIndicators("HKG")).toBe("");
  expect(regionalIndicators("1A")).toBe("");
});

// jsdom 的 canvas 拿不到 2d 上下文：判不出就按全部不支持处理（卡片退回名称后的文字徽章）。
// 彩色单字形的正向判定在真实浏览器里由截图脚本（TASK_DIR/scratch/shot.mjs）验收。
it("canvas 不可用时判定不可用", () => {
  expect(createFlagRenderSupport()).toBeNull();
});
