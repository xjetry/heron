import { describe, expect, it } from "vitest";
import { isLoopbackIPLiteral, needsInsecureHTTP } from "./transport";

describe("needsInsecureHTTP", () => {
  it.each([
    "http://127.0.0.1:8080",
    "http://127.9.9.9",
    "http://[::1]:8080",
    // 浏览器规范化之后仍是 127.0.0.0/8 与 ::1 的写法。
    "http://127.1:8080",
    "http://[0:0:0:0:0:0:0:1]",
  ])("%s 是 loopback IP 字面量，不带", (origin) => {
    expect(needsInsecureHTTP(origin)).toBe(false);
  });

  it.each([
    "http://localhost:8080",
    "http://10.0.0.1",
    "http://hub.example",
    "http://128.0.0.1",
    "http://127.0.0.1.example",
    // IPv4 映射的 loopback 不在 127.0.0.0/8 与 ::1 两种写法里：多带一个开关无害，少带会被 agent 拒绝。
    "http://[::ffff:127.0.0.1]",
    "http://[::2]",
  ])("%s 不是 loopback IP 字面量，要带", (origin) => {
    expect(needsInsecureHTTP(origin)).toBe(true);
  });

  it.each(["https://hub.example", "https://127.0.0.1", "https://localhost:8443", "https://[::1]"])("%s 是 https，永远不带", (origin) => {
    expect(needsInsecureHTTP(origin)).toBe(false);
  });
});

describe("isLoopbackIPLiteral", () => {
  it.each(["127.0.0.1", "127.255.255.255", "[::1]"])("%s 是", (host) => expect(isLoopbackIPLiteral(host)).toBe(true));
  it.each(["localhost", "127.0.0.256", "127.0.0", "126.0.0.1", "::1", "[::2]", ""])("%s 不是", (host) => expect(isLoopbackIPLiteral(host)).toBe(false));
});
