export type SchemeChoice = "auto" | "light" | "dark";
export const PUBLIC_SCHEME_KEY = "heron-public-scheme";
export const ADMIN_SCHEME_KEY = "heron-admin-scheme";
const CHOICES: readonly SchemeChoice[] = ["auto", "light", "dark"];
export const SCHEME_LABEL: Record<SchemeChoice, string> = { auto: "跟随系统", light: "浅色", dark: "深色" };

// 存储不可用时读失败按 auto；持久化失败不应阻断调用方在当前页面应用 choice。
export function readSchemeChoice(key: string): SchemeChoice {
  try {
    const value = localStorage.getItem(key);
    return CHOICES.includes(value as SchemeChoice) ? (value as SchemeChoice) : "auto";
  } catch {
    return "auto";
  }
}

export function writeSchemeChoice(key: string, choice: SchemeChoice): void {
  try {
    if (choice === "auto") localStorage.removeItem(key);
    else localStorage.setItem(key, choice);
  } catch {
    // 持久化是尽力而为，不改变调用方持有的当前选择。
  }
}

export function nextSchemeChoice(choice: SchemeChoice): SchemeChoice {
  return CHOICES[(CHOICES.indexOf(choice) + 1) % CHOICES.length];
}

// 访客显式选择 > 站点 light / dark > 系统；未指定时不写 data-theme，交给 CSS 与 currentScheme 跟随系统。
export function forcedTheme(siteTheme: string, choice: SchemeChoice): "light" | "dark" | undefined {
  if (choice !== "auto") return choice;
  return siteTheme === "light" || siteTheme === "dark" ? siteTheme : undefined;
}
