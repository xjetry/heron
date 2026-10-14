import { createContext, useContext } from "react";
import { createFlagRenderSupport, regionalIndicators, type FlagRenderSupport } from "../lib/flagEmoji";

// 国旗 emoji（公开卡片名称前）：只在访客浏览器真的能渲染该旗帜时放 emoji——Windows 上区域指示符
// 渲染成两个字母、部分系统对个别地区显示成方框，那里保持名称后的 CountryBadge 文字徽章。
// 判定实现见 lib/flagEmoji.ts；测试与截图等需要固定外观的地方用 FlagSupportProvider 注入判定。
const FlagSupportContext = createContext<FlagRenderSupport | null>(null);

export const FlagSupportProvider = FlagSupportContext.Provider;

// 默认判定在模块加载时探测一次（canvas），逐国家码缓存；canvas 不可用（jsdom、被禁用）时是
// null，所有码按不支持处理。模块级单例让所有卡片共享同一次探测。
const defaultSupport = createFlagRenderSupport();

function useFlagSupport(): FlagRenderSupport | null {
  return useContext(FlagSupportContext) ?? defaultSupport;
}

// 能渲染时返回该码的旗帜 emoji，否则空串；调用方据此决定文字徽章的去留。
export function useFlagEmoji(code: string): string {
  const support = useFlagSupport();
  if (code === "" || support === null || !support(code)) return "";
  return regionalIndicators(code);
}

// role="img" + aria-label / title 写清地区，emoji 字形本身对读屏没有意义。
export function CountryFlag({ code }: { code: string }) {
  const emoji = useFlagEmoji(code);
  if (emoji === "") return null;
  return <span className="country-flag" role="img" aria-label={`国家 / 地区 ${code}`} title={`国家 / 地区 ${code}`}>{emoji}</span>;
}
