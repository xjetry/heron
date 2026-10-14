// 国旗 emoji 可渲染性判定（公开卡片名称前的 CountryFlag 用）：Windows 把区域指示符画成两个字母、
// 部分系统对个别地区画成方框（同一张卡片在不同访客那里长得不一样），所以不按平台一刀切，
// 而是用 canvas 把该旗帜画出来逐码判定、结果缓存：必须是"彩色单字形"才算能渲染——
// 字母对的宽度等于两个区域指示符分开渲染的宽度（连字成一个字形后明显变窄，实测 macOS
// chromium / webkit 恰为一半），方框与字母都是单色。canvas 拿不到 2d 上下文（jsdom、被禁用的
// 环境）时整个判定不可用，调用方一律按不支持处理。

export type FlagRenderSupport = (code: string) => boolean;

// 两位 ASCII 国家码转区域指示符对（"HK" → 🇭🇰）；不是两位字母返回空串。
export function regionalIndicators(code: string): string {
  if (!/^[A-Za-z]{2}$/.test(code)) return "";
  const up = code.toUpperCase();
  return String.fromCodePoint(0x1f1e6 + up.charCodeAt(0) - 65, 0x1f1e6 + up.charCodeAt(1) - 65);
}

const FONT = "16px sans-serif";
// 彩色判定：任一像素三通道最大差超过阈值才算有色；黑字与方框经抗锯齿后各通道仍相等。
const COLOR_DIFF = 16;
// 单字形判定：旗帜连字后宽度明显小于两个指示符分开渲染之和；字母对与方框对则等宽。
const SINGLE_GLYPH_RATIO = 0.8;

// 返回逐码判定函数；canvas 不可用返回 null（判不出，按全部不支持处理）。
export function createFlagRenderSupport(): FlagRenderSupport | null {
  let ctx: CanvasRenderingContext2D | null = null;
  try {
    const canvas = document.createElement("canvas");
    canvas.width = 40;
    canvas.height = 28;
    ctx = canvas.getContext("2d", { willReadFrequently: true });
  } catch {
    return null;
  }
  if (ctx === null) return null;
  const cache = new Map<string, boolean>();
  return (code) => {
    const key = code.toUpperCase();
    const hit = cache.get(key);
    if (hit !== undefined) return hit;
    const verdict = judge(ctx, key);
    cache.set(key, verdict);
    return verdict;
  };
}

function judge(ctx: CanvasRenderingContext2D, code: string): boolean {
  const flag = regionalIndicators(code);
  if (flag === "") return false;
  ctx.font = FONT;
  const separate = [...flag].reduce((width, char) => width + ctx.measureText(char).width, 0);
  if (separate === 0 || ctx.measureText(flag).width >= separate * SINGLE_GLYPH_RATIO) return false;
  const { width, height } = ctx.canvas;
  ctx.clearRect(0, 0, width, height);
  ctx.fillStyle = "#000";
  ctx.textBaseline = "top";
  ctx.fillText(flag, 0, 0);
  const { data } = ctx.getImageData(0, 0, width, height);
  for (let i = 0; i < data.length; i += 4) {
    if (data[i + 3] === 0) continue;
    if (Math.max(Math.abs(data[i] - data[i + 1]), Math.abs(data[i + 1] - data[i + 2]), Math.abs(data[i] - data[i + 2])) > COLOR_DIFF) return true;
  }
  return false;
}
