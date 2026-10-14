import { afterEach, describe, expect, it, vi } from "vitest";
import { clearGhProxy, loadGhProxy, parseGhProxy, saveGhProxy } from "./installGhProxy";

afterEach(() => {
  localStorage.clear();
  vi.restoreAllMocks();
});

describe("parseGhProxy", () => {
  it.each(["", "   ", "\t"])("%j 表示直连 GitHub", (text) => expect(parseGhProxy(text)).toBe(""));

  // 主机部分与 parseInstallHub 同一规范写法；路径前缀原样保留，只去掉结尾的 "/"。
  it.each([
    ["https://ghfast.top", "https://ghfast.top"],
    ["https://ghfast.top/", "https://ghfast.top"],
    ["HTTPS://GhFast.Top:443/", "https://ghfast.top"],
    ["http://10.0.0.1:8080", "http://10.0.0.1:8080"],
    ["https://proxy.example:8443/gh", "https://proxy.example:8443/gh"],
    ["https://proxy.example/gh/raw_1.x~y-z/", "https://proxy.example/gh/raw_1.x~y-z"],
    ["https://proxy.example/.well-known", "https://proxy.example/.well-known"],
    ["https://中文.example/gh", "https://xn--fiq228c.example/gh"],
    ["  https://ghfast.top/gh  ", "https://ghfast.top/gh"],
  ])("%j 规范化为 %s", (text, proxy) => expect(parseGhProxy(text)).toBe(proxy));

  it.each([
    "ghfast.top",
    "ghfast.top/gh",
    "ftp://ghfast.top",
    "http:ghfast.top",
    "https:///ghfast.top",
    "https:\\\\ghfast.top",
    "https://",
    "https://:8080/gh",
    "https://ghfast.top?",
    "https://ghfast.top/?x=1",
    "https://ghfast.top/gh?x",
    "https://ghfast.top#a",
    "https://ghfast.top/gh#a",
    "https://user@ghfast.top/gh",
    "https://ghfast.top:0",
    "https://ghfast.top:65536/gh",
    "https://ghfast.top//gh",
    "https://ghfast.top/gh//",
    "https://ghfast.top/./gh",
    "https://ghfast.top/gh/..",
    "https://ghfast.top/gh\\x",
    "https://ghfast.top/g h",
    "https://ghfast.top/g\th",
    "https://ghfast.top/%2e%2e",
    // 未加引号进 shell 命令：这些字符出现在主机或路径里都是注入。
    "https://a$(reboot).example",
    "https://ghfast.top/$(reboot)",
    "https://ghfast.top/a;reboot",
    "https://ghfast.top/a`id`",
    "https://ghfast.top/a'b",
    "https://ghfast.top/a&b",
    "https://ghfast.top/a|b",
    "https://ghfast.top/a:b",
    "https://ghfast.top/a@b",
  ])("%j 不合法", (text) => expect(parseGhProxy(text)).toBeNull());
});

describe("installGhProxy 存储", () => {
  it("存下的代理原样读回，清除后回到空", () => {
    expect(loadGhProxy()).toBe("");
    saveGhProxy("https://proxy.example/gh");
    expect(localStorage.getItem("heron-install-gh-proxy")).toBe("https://proxy.example/gh");
    expect(loadGhProxy()).toBe("https://proxy.example/gh");
    clearGhProxy();
    expect(localStorage.getItem("heron-install-gh-proxy")).toBeNull();
    expect(loadGhProxy()).toBe("");
  });

  it.each(["https://ghfast.top/?x", "not a url", "https://ghfast.top/$(id)"])("存的值 %j 不合法时按空处理", (saved) => {
    localStorage.setItem("heron-install-gh-proxy", saved);
    expect(loadGhProxy()).toBe("");
  });

  it("存储被禁用时按空处理，写入与清除不抛错", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("denied"); });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("denied"); });
    vi.spyOn(Storage.prototype, "removeItem").mockImplementation(() => { throw new Error("denied"); });
    expect(loadGhProxy()).toBe("");
    expect(() => saveGhProxy("https://ghfast.top")).not.toThrow();
    expect(() => clearGhProxy()).not.toThrow();
  });
});
