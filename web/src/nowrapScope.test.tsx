import { readFileSync } from "node:fs";
import { join } from "node:path";
import { screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { Layout } from "./components/Layout";
import { PublicLayout } from "./public/Layout";
import { PublicService } from "./gen/heron/v1/public_pb";
import { renderWithAdmin, renderWithService } from "./test/harness";

// 两端构建各自只有一份入口 CSS，由 index.html 直接 <link>，每一页都加载：公开端那份合并了
// styles.css、public.css 与 uPlot 的规则，管理端那份合并了 styles.css 与 uPlot。两端共用的是
// styles.css 与 uPlot 两份；uPlot 只含 .uplot 与 .u-* 选择器、没有 white-space 规则，碰不到公开页头部，
// 所以本测试只核对 styles.css 里的规则不外溢到公开页头部；public.css（单独给
// `.public-header` 及其子元素定样式，站点标题过长时在那里按省略号截断）与运行期注入的站点自定义 CSS
// 不共用给两端，不存在"外溢"这回事，因此不在覆盖范围内。面板专用的 nowrap 规则必须锁在 .panel-nav 上：
// 公开页头部（header.public-header）不带 .nav 类，但它的直接子元素仍用 styles.css 里两端共用的
// .brand、.muted 等基础类，styles.css 里任何一条 nowrap 规则，只要选择器能匹配到这些共用类或裸元素，
// 就会连带命中公开页头部，让头部的折行与截断不再只由 public.css 决定；因此要连 @media、@supports
// 等分组规则内的一起枚举逐条核对，不能只看顶层规则。
function nowrapSelectors(): string[] {
  const css = readFileSync(join(import.meta.dirname, "styles.css"), "utf8");
  const style = document.createElement("style");
  style.textContent = css;
  document.head.appendChild(style);
  // @media、@supports 等分组规则（CSSGroupingRule）把成员规则嵌在自己的 cssRules 里，不出现在
  // 样式表的顶层 cssRules 中；要展开成一条不分层级的规则列表才能不漏。
  const flatten = (rules: CSSRuleList): CSSRule[] =>
    Array.from(rules).flatMap((rule) => [rule, ...("cssRules" in rule ? flatten((rule as CSSGroupingRule).cssRules) : [])]);
  const selectors = flatten(style.sheet!.cssRules)
    .filter((rule): rule is CSSStyleRule => rule instanceof CSSStyleRule && rule.style.getPropertyValue("white-space") === "nowrap")
    .map((rule) => rule.selectorText);
  if (selectors.length === 0) throw new Error("styles.css: 没有找到 white-space: nowrap 的规则");
  return selectors;
}

it("styles.css 的每条 nowrap 规则（含分组规则内的）都不命中公开页头部或其直接子元素", async () => {
  const selectors = nowrapSelectors();

  renderWithService(PublicService, { getSite: async () => ({}) }, [{ path: "/", Component: PublicLayout }], "/");
  await screen.findAllByRole("link");
  const header = document.querySelector("header.public-header")!;
  const targets = [header, ...Array.from(header.children)];
  for (const selector of selectors) {
    for (const el of targets) {
      expect(el.matches(selector), `选择器「${selector}」命中了公开页头部的 <${el.tagName.toLowerCase()} class="${el.className}">`).toBe(false);
    }
  }

  renderWithAdmin({}, [{ path: "/", Component: Layout, children: [{ index: true, element: <p /> }] }], "/");
  // 正控制：面板导航链接确实命中其中一条 nowrap 规则，排除“选择器全部写错、两边都不命中”这种误判为通过的可能。
  const overview = screen.getByRole("link", { name: "总览" });
  expect(selectors.some((selector) => overview.matches(selector)), `没有任何 nowrap 选择器命中面板导航链接，已枚举的选择器：${selectors.join("、")}`).toBe(true);
});
