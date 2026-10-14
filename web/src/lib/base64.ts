// 标准 base64（带填充）。分块经 String.fromCharCode 展开：整个数组一次展开会超出参数个数上限。
export function toBase64(bytes: Uint8Array): string {
  let binary = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(binary);
}
