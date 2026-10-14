import "@testing-library/jest-dom/vitest";
import { cleanup, configure } from "@testing-library/react";
import { afterEach } from "vitest";

import { asyncUtilTimeout } from "./async-timeout";

// 本工程 globals: false，RTL 找不到全局 afterEach 就不会自动注册清理；这里显式注册。
afterEach(cleanup);

// 上界只为挂死时能结束，不参与被测性质。显式传入 timeout 的查找不受这里影响。
configure({ asyncUtilTimeout });

// jsdom 不实现原生 dialog 的顶层与焦点管理；这里仅模拟 open，焦点约束在真实浏览器验收。
if (typeof HTMLDialogElement !== "undefined") {
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute("open", ""); };
  HTMLDialogElement.prototype.close = function () { this.removeAttribute("open"); };
}

// jsdom 未提供媒体查询；默认宽屏浅色，窄屏用例自行替换。
if (typeof window !== "undefined" && typeof window.matchMedia !== "function") {
  window.matchMedia = (query: string) => ({ matches: query === "(min-width: 901px)", media: query, onchange: null, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, dispatchEvent: () => false }) as MediaQueryList;
}
