// 主题开发指南随面板打包进 hub，与 hub 同版本；SDK 方法表与指标表由 Go 测试对照实现（internal/hub/web 与
// internal/hub/metric 的 *_test.go）。下载时把文中的 hub 地址占位换成浏览器当前 origin，与安装命令同一口径。
import skill from "../assets/heron-theme-skill.md?raw";

export const THEME_SKILL_FILENAME = "heron-theme-SKILL.md";

export function buildThemeSkill(origin: string): string {
  return skill.replaceAll("__HERON_HUB__", origin);
}
