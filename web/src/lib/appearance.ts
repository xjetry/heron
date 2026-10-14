// 外观的取值与上限，与 hub 的 internal/hub/api/settings.go 同值，由 appearanceLimits.test.ts 逐项对照。
// 约束的裁决在 hub：页面显示 hub 的错误原文，不另做一份校验。页面在提交前只查三个字段的字节数——
// 超过 hub 给 UpdateSettings 的解码预算（internal/hub/api/settings_budget.go 的 settingsBudget/maxSettingsBody）的请求在校验之前就被拒绝，
// 那时的错误说不出是哪个字段超了。
export const MAX_TITLE_CHARS = 64;
export const MAX_TITLE_BYTES = 1024;
export const MAX_LOGO_BYTES = 128 * 1024;
export const MAX_CSS_BYTES = 64 * 1024;
export const THEMES = ["auto", "light", "dark"] as const;
export const LOGO_TYPES = ["image/png", "image/jpeg", "image/webp", "image/svg+xml"] as const;

export type Theme = (typeof THEMES)[number];

// hub 按 UTF-8 字节计大小；JS 字符串的 length 是 UTF-16 码元数，非 ASCII 时两者不同。
const utf8Bytes = (s: string) => new TextEncoder().encode(s).length;

export function sizeProblems(title: string, logo: string, customCss: string): string[] {
  const out: string[] = [];
  const titleBytes = utf8Bytes(title);
  if (titleBytes > MAX_TITLE_BYTES) out.push(`标题 ${titleBytes} 字节，上限 ${MAX_TITLE_BYTES} 字节（去掉控制字符与首尾空白之前计）。`);
  const logoBytes = utf8Bytes(logo);
  if (logoBytes > MAX_LOGO_BYTES) out.push(`logo 编码为 data: URL 后 ${logoBytes} 字节，上限 ${MAX_LOGO_BYTES} 字节（128 KiB）。换一张更小的图片。`);
  const cssBytes = utf8Bytes(customCss);
  if (cssBytes > MAX_CSS_BYTES) out.push(`自定义 CSS ${cssBytes} 字节，上限 ${MAX_CSS_BYTES} 字节（64 KiB）。`);
  return out;
}
