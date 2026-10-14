// 指纹在面板上的形状与 curl --pinnedpubkey 相同，转换只发生在展示层：协议里仍是 32 字节。
const PREFIX = "sha256//";

export function formatPin(bytes?: Uint8Array): string {
  if (!bytes || bytes.length === 0) return "";
  let binary = "";
  for (const b of bytes) binary += String.fromCharCode(b);
  return PREFIX + btoa(binary);
}

export function parsePin(text: string): Uint8Array {
  const raw = text.trim();
  if (!raw.startsWith(PREFIX)) throw new Error("指纹须写成 sha256// 加 base64");
  let binary: string;
  try {
    binary = atob(raw.slice(PREFIX.length));
  } catch {
    throw new Error("指纹的 base64 解不开");
  }
  if (binary.length !== 32) throw new Error(`指纹须恰为 32 字节；解出 ${binary.length} 字节`);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out;
}
