// 由 vite.config.ts 的 define 在构建与测试时从 styles.css 的 --accent 读出，styles.css 是唯一来源。
declare const __BUILT_IN_ACCENT__: string;

// BUILT_IN_ACCENT 是内置浅色主题的主色：外观页的取色器在主色为空时显示它。
export const BUILT_IN_ACCENT: string = __BUILT_IN_ACCENT__;
