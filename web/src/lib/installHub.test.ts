import { afterEach, describe, expect, it, vi } from "vitest";
import { clearInstallHub, loadInstallHub, parseInstallHub, saveInstallHub } from "./installHub";

afterEach(() => {
  localStorage.clear();
  vi.restoreAllMocks();
});

describe("parseInstallHub", () => {
  it.each(["", "   ", "\t"])("%j 表示用当前域名", (text) => expect(parseInstallHub(text)).toBe(""));

  // 命令里写规范化后的 origin，与 location.origin 同一种写法。
  it.each([
    ["https://heron-panel.o1.pw:28080", "https://heron-panel.o1.pw:28080"],
    ["https://heron-panel.o1.pw:28080/", "https://heron-panel.o1.pw:28080"],
    ["HTTPS://Heron.Example:443", "https://heron.example"],
    ["http://hub.example:80", "http://hub.example"],
    ["http://10.0.0.1:8080", "http://10.0.0.1:8080"],
    ["http://127.1:8080", "http://127.0.0.1:8080"],
    ["http://[0:0:0:0:0:0:0:1]:8080", "http://[::1]:8080"],
    ["https://中文.example", "https://xn--fiq228c.example"],
    ["  https://hub.example  ", "https://hub.example"],
  ])("%j 规范化为 %s", (text, origin) => expect(parseInstallHub(text)).toBe(origin));

  it.each([
    "hub.example",
    "hub.example:28080",
    "ftp://hub.example",
    "http:hub.example",
    "https:///hub.example",
    "https:\\\\hub.example",
    "http://",
    "https://:8080",
    "https://hub.example/admin",
    "https://hub.example/admin/",
    "https://hub.example?",
    "https://hub.example?x=1",
    "https://hub.example#",
    "https://hub.example#a",
    "https://user@hub.example",
    "https://user:pw@hub.example",
    "https://hub.example:0",
    "https://hub.example:65536",
    "https://hub.ex ample",
    "https://hub.ex\tample",
    "https://hub.ex\nample",
    // WHATWG 的主机允许这些字符留在规范化结果里，原样进 shell 命令就是注入。
    "https://a$(reboot).example",
    "https://a;reboot.example",
    "https://a`id`.example",
    "https://a'b.example",
    "https://a&b.example",
    "https://a|b.example",
  ])("%j 不合法", (text) => expect(parseInstallHub(text)).toBeNull());
});

describe("installHub 存储", () => {
  it("存下的 origin 原样读回，清除后回到空", () => {
    expect(loadInstallHub()).toBe("");
    saveInstallHub("https://heron-panel.o1.pw:28080");
    expect(localStorage.getItem("heron-install-hub")).toBe("https://heron-panel.o1.pw:28080");
    expect(loadInstallHub()).toBe("https://heron-panel.o1.pw:28080");
    clearInstallHub();
    expect(localStorage.getItem("heron-install-hub")).toBeNull();
    expect(loadInstallHub()).toBe("");
  });

  it.each(["https://hub.example/admin", "not a url", "https://a$(id).example"])("存的值 %j 不合法时按空处理", (saved) => {
    localStorage.setItem("heron-install-hub", saved);
    expect(loadInstallHub()).toBe("");
  });

  it("存储被禁用时按空处理，写入与清除不抛错", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("denied"); });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("denied"); });
    vi.spyOn(Storage.prototype, "removeItem").mockImplementation(() => { throw new Error("denied"); });
    expect(loadInstallHub()).toBe("");
    expect(() => saveInstallHub("https://hub.example")).not.toThrow();
    expect(() => clearInstallHub()).not.toThrow();
  });
});
