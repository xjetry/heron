import { create } from "@bufbuild/protobuf";
import { afterEach, expect, it } from "vitest";
import { PublicSiteSchema } from "../gen/heron/v1/public_pb";
import { applySite, DEFAULT_TITLE } from "./site";

afterEach(() => {
  document.head.innerHTML = "";
  document.documentElement.removeAttribute("style");
  delete document.documentElement.dataset.theme;
});

it("明暗、主色与标题写到文档上，撤销后恢复；auto 跟随系统", () => {
  const root = document.documentElement;
  const undo = applySite(create(PublicSiteSchema, { title: "机房", theme: "dark", accentColor: "#123abc" }));
  expect(root.dataset.theme).toBe("dark");
  expect(root.style.getPropertyValue("--accent")).toBe("#123abc");
  expect(document.title).toBe("机房");
  undo();
  expect(root.dataset.theme).toBeUndefined();
  expect(root.style.getPropertyValue("--accent")).toBe("");
  applySite(create(PublicSiteSchema, { theme: "auto" }));
  expect(root.dataset.theme).toBeUndefined();
  expect(document.title).toBe(DEFAULT_TITLE);
});

it("自定义 CSS 排在已有样式之后，撤销时移除", () => {
  const builtIn = document.createElement("link");
  builtIn.rel = "stylesheet";
  document.head.append(builtIn);
  const undo = applySite(create(PublicSiteSchema, { theme: "auto", customCss: "body { color: red }" }));
  const last = document.head.lastElementChild as HTMLStyleElement;
  expect(last.tagName).toBe("STYLE");
  expect(last.dataset.siteCss).toBe("");
  expect(last.textContent).toBe("body { color: red }");
  undo();
  expect(document.head.querySelector("style[data-site-css]")).toBeNull();
});

it("访客的明暗选择压过站点设置；撤销后 data-theme 清掉", () => {
  const root = document.documentElement;
  const undo = applySite(create(PublicSiteSchema, { theme: "dark" }), "light");
  expect(root.dataset.theme).toBe("light");
  undo();
  expect(root.dataset.theme).toBeUndefined();
  applySite(create(PublicSiteSchema, { theme: "dark" }), "auto");
  expect(root.dataset.theme).toBe("dark");
});

// hub 拒绝含 "</" 的 CSS；即便有这样的值到达，textContent 也不经 HTML 解析，产生不了任何元素。
// 判据是文档里的元素：jsdom 不加载图片，onerror 永远不触发，看脚本有没有执行证明不了什么。
it("CSS 按文本写入，不产生元素", () => {
  const css = `</style><img src=x onerror="alert(1)"><script>alert(2)</script>`;
  document.title = "页面自带的标题"; // 先有 <title>：applySite 设标题时不必新建它，新增的元素就只该有那个 <style>
  const before = [...document.querySelectorAll("*")];
  applySite(create(PublicSiteSchema, { theme: "auto", customCss: css }));
  const added = [...document.querySelectorAll("*")].filter((el) => !before.includes(el));
  expect(added.map((el) => el.tagName)).toEqual(["STYLE"]);
  expect(added[0].parentElement).toBe(document.head);
  expect(added[0].textContent).toBe(css);
});
