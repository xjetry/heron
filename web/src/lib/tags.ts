import { literalPattern } from "./fold";

// 去首尾空白用的字符集与 hub 的 strings.TrimSpace 相同：Unicode 的 White_Space 属性（unicode.IsSpace 的定义）。
// 不用 String.prototype.trim：它按 ECMAScript 的 WhiteSpace 与 LineTerminator 去，比 White_Space 多 U+FEFF、少 U+0085。
// 用它时，经 API 建的 "\uFEFFdb" 在 hub 是另一个标签，页面却会把它与 db 判成同一个。
const edgeSpace = /^\p{White_Space}+|\p{White_Space}+$/gu;
const trimTag = (raw: string) => raw.replace(edgeSpace, "");

// 标签名在 hub 按去首尾空白后的简单折叠比较（db 与 DB 是同一个标签）；页面判重用同一口径（去空白见 trimTag，折叠见
// literalPattern），免得同一个标签在草稿里出现两次。
export function sameTag(a: string, b: string): boolean {
  return literalPattern(trimTag(a), true).test(trimTag(b));
}

// 把 raw 加进 tags：去首尾空白后为空或与已有的同名（折叠后）时原样返回，已有的写法优先。
export function withTag(tags: readonly string[], raw: string): string[] {
  const name = trimTag(raw);
  if (name === "" || tags.some((t) => sameTag(t, name))) return [...tags];
  return [...tags, name];
}

export function withoutTag(tags: readonly string[], name: string): string[] {
  return tags.filter((t) => !sameTag(t, name));
}

// 多选标签的两种口径：all 要求节点带有所选的每一个标签（交集），any 带其中任一个即可（并集）。
export type TagMatch = "all" | "any";

// 空选择不过滤——空条件匹配一切。这一分支显式写出：all 靠 every 对空数组恒真会碰巧对，any 的 some 对空数组恒假，
// 方向正好相反。
export function matchesTags(nodeTags: readonly string[], selected: readonly string[], match: TagMatch): boolean {
  if (selected.length === 0) return true;
  const has = (s: string) => nodeTags.some((t) => sameTag(t, s));
  return match === "all" ? selected.every(has) : selected.some(has);
}
