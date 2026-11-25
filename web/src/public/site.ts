import { useQuery } from "@connectrpc/connect-query";
import { PublicService, type PublicSite } from "../gen/heron/v1/public_pb";
import { forcedTheme, type SchemeChoice } from "../lib/scheme";

// 公开页的站点设置：PublicLayout 与需要站点时区的页面共用这一个查询（同一查询键、同一选项），一次页面加载只取一次。
// 只在页面加载时取，之后不再重取：已打开的页面刷新后才看到改动，刷新时浏览器还可能再用最多 5 分钟的缓存
// （hub 对 GetSite 下发 max-age=300）。断网重连与重新挂载默认只重取已过期查询，staleTime: Infinity 阻止这两类重取。
export function useSite() {
  return useQuery(PublicService.method.getSite, {}, { staleTime: Infinity });
}

// hub 实际生效的时区（GetSite.timezone）。公开页按它切日历时段，不按访客浏览器的时区；没取到时为 undefined。
export function useSiteTimeZone(): string | undefined {
  return useSite().data?.timezone || undefined;
}

// 站点设置里标题为空时用的标题；面板的外观页拿它作占位提示。
export const DEFAULT_TITLE = "服务器状态";

// applySite 把站点设置应用到文档。明暗按 forcedTheme 合成（访客选择 > 站点设置 > 跟随系统）写到 html 的 data-theme，
// styles.css 的 color-scheme 规则按它切换；主色覆盖 --accent，自定义 CSS 放进 head 末尾的 <style>。
// "排在全部内置样式之后"要求调用时内置样式都已在 head 里，由两件事保证：公开入口引用的 CSS 全部由 Vite 打成
// index.html 里的静态 <link>，公开包也没有按需加载的 chunk（两者由 importScan.test 对公开构建的产物核对）；
// applySite 又只在 Layout 的 effect 里运行，页面已挂载。以后若改成按需加载且带 CSS 的路由，那份 CSS 会在导航时
// 追加到 head 末尾、排到自定义 CSS 之后。
// CSS 经 textContent 写入，不经 HTML 解析；hub 另外拒绝含 "</" 的值，那是给把它内联进 HTML 的消费者的约束。
// 返回撤销函数：设置变化时先撤掉上一份再应用新的。
export function applySite(site: PublicSite, choice: SchemeChoice = "auto"): () => void {
  const root = document.documentElement;
  const theme = forcedTheme(site.theme, choice);
  if (theme) root.dataset.theme = theme;
  else delete root.dataset.theme;
  if (site.accentColor) root.style.setProperty("--accent", site.accentColor);
  else root.style.removeProperty("--accent");
  document.title = site.title || DEFAULT_TITLE;
  const style = document.createElement("style");
  style.dataset.siteCss = "";
  style.textContent = site.customCss;
  document.head.append(style);
  return () => {
    style.remove();
    delete root.dataset.theme;
    root.style.removeProperty("--accent");
  };
}
