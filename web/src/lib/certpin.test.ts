import { expect, it } from "vitest";
import { formatPin, parsePin } from "./certpin";

const pin = Uint8Array.from({ length: 32 }, (_, i) => i + 1);

it("sha256// 与 32 字节互相转换", () => {
  expect(parsePin(formatPin(pin))).toEqual(pin);
});

it("base64 解不开时说明原因，而不是抛出解码异常", () => {
  expect(() => parsePin("sha256//!!!!")).toThrow(/base64 解不开/);
});

it("长度不是 32 字节时拒绝", () => {
  expect(() => parsePin(formatPin(Uint8Array.from({ length: 31 }, () => 1)))).toThrow(/恰为 32 字节/);
});
