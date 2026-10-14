import { expect, it } from "vitest";
import { toBase64 } from "./base64";

it("与 Node 的 base64 逐字节一致，跨分块边界也一样", () => {
  for (const n of [0, 1, 2, 3, 0x8000 - 1, 0x8000, 0x8000 + 1, 3 * 0x8000 + 7]) {
    const bytes = Uint8Array.from({ length: n }, (_, i) => (i * 131 + 7) & 0xff);
    expect(toBase64(bytes)).toBe(Buffer.from(bytes).toString("base64"));
  }
});
