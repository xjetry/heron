import { afterEach, expect, it } from "vitest";
import heronSvg from "../assets/heron.svg?raw";
import template from "../assets/heron-quick-node.user.js?raw";

afterEach(() => { document.documentElement.querySelectorAll(":scope > div").forEach((el) => el.remove()); });

type Part = [string, Record<string, string>];
const attrs = (el: Element) => Object.fromEntries([...el.attributes].filter((a) => a.name !== "xmlns").map((a) => [a.name, a.value]));

// 在 jsdom 里跑一遍脚本（GM 接口给空实现），取出它画在 shadow DOM 里的悬浮按钮。脚本像页面里的 <script> 一样由 jsdom
// 执行（vitest 的 jsdom 环境默认 runScripts: "dangerously"），GM 接口作为包裹函数的参数传入，不落到全局。
function runScript(): HTMLButtonElement {
  const before = new Set(document.documentElement.children);
  const script = document.createElement("script");
  script.textContent = `((GM_getValue, GM_setValue, GM_registerMenuCommand) => {\n${template}\n})(() => "", () => {}, () => {});`;
  document.head.appendChild(script);
  script.remove();
  const host = [...document.documentElement.children].find((el) => !before.has(el))!;
  return host.shadowRoot!.querySelector("button.fab")!;
}

// 悬浮按钮的标志与面板的 heron.svg 是两份物理分离的同一图形：脚本独立分发，不能引用面板资源。
it("悬浮按钮画的是 Heron 标志，图形与面板的 heron.svg 逐元素一致，没有文字", () => {
  const logo = new DOMParser().parseFromString(heronSvg, "image/svg+xml").documentElement;
  const expected: Part[] = [...logo.children].filter((el) => el.localName !== "style").map((el) => [el.localName, attrs(el)]);
  expect(expected.length).toBeGreaterThan(2);
  const fab = runScript();
  const svg = fab.querySelector("svg")!;
  expect(svg.namespaceURI).toBe("http://www.w3.org/2000/svg");
  expect(svg.getAttribute("viewBox")).toBe(logo.getAttribute("viewBox"));
  expect(svg.getAttribute("fill")).toBe(logo.getAttribute("fill"));
  expect([...svg.children].map((el): Part => [el.localName, attrs(el)])).toEqual(expected);
  expect(fab.textContent).toBe("");
  expect(fab).toHaveAttribute("aria-label", "添加 Heron 节点");
});

it("悬浮按钮的明暗配色与 heron.svg 的 .bird / .legs 一致", () => {
  const [light, dark] = [...heronSvg.matchAll(/\.bird \{ fill: (#[0-9a-f]+); \}/g)].map((m) => m[1]);
  expect([light, dark].every(Boolean)).toBe(true);
  expect(heronSvg).toMatch(new RegExp(`\\.legs \\{ stroke: ${light}; \\}[\\s\\S]*\\.legs \\{ stroke: ${dark}; \\}`));
  const css = template.slice(template.indexOf("const CSS"), template.indexOf("`;", template.indexOf("const CSS")));
  expect(css).toMatch(new RegExp(`\\.fab \\.bird \\{ fill: ${light}; \\}\\s*\\.fab \\.legs \\{ stroke: ${light}; \\}`));
  expect(css).toMatch(new RegExp(`prefers-color-scheme: dark[\\s\\S]*\\.fab \\.bird \\{ fill: ${dark}; \\}\\s*\\.fab \\.legs \\{ stroke: ${dark}; \\}`));
});
