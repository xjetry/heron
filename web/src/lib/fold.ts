// text 按字面量（转义正则元字符）、大小写不敏感地匹配。iu 模式按 Unicode 简单大小写折叠比较（σ 与 ς、K 与开尔文符号
// K 相同，ß 与 ss 不同），与 hub 比较标签名的 store.TagFold 同为简单折叠。whole 为真时要求整串相等，否则是子串。
export function literalPattern(text: string, whole: boolean): RegExp {
  const body = text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return new RegExp(whole ? `^(?:${body})$` : body, "iu");
}
