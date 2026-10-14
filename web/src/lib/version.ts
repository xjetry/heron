// semver 2.0 带 v 前缀的版本号：数字段不带前导 0；预发布是点分标识符，纯数字的不带前导 0；
// "+" 之后的构建元数据只校验形状，不参与比较。
const SEMVER =
  /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/;

type Version = { core: [string, string, string]; pre: string[] };

function parse(v: string): Version | null {
  const m = SEMVER.exec(v);
  return m ? { core: [m[1], m[2], m[3]], pre: m[4] ? m[4].split(".") : [] } : null;
}

const isNumeric = (id: string) => /^\d+$/.test(id);

// 数字串不带前导 0（由 SEMVER 保证），所以先比长度再逐字符比就是按数值比，不受 Number 的精度上限影响。
function compareNumeric(a: string, b: string): number {
  if (a.length !== b.length) return a.length - b.length;
  return a < b ? -1 : a > b ? 1 : 0;
}

// 标识符只含 [0-9A-Za-z-]，JS 字符串比较按 UTF-16 码元，在 ASCII 范围内即 ASCII 序。
function compareIdentifier(a: string, b: string): number {
  const an = isNumeric(a), bn = isNumeric(b);
  if (an && bn) return compareNumeric(a, b);
  if (an !== bn) return an ? -1 : 1;
  return a < b ? -1 : a > b ? 1 : 0;
}

function compare(a: Version, b: Version): number {
  for (let i = 0; i < 3; i++) {
    const c = compareNumeric(a.core[i], b.core[i]);
    if (c !== 0) return c;
  }
  // 同一 MAJOR.MINOR.PATCH 下，正式版高于任何预发布。
  if (a.pre.length === 0 || b.pre.length === 0) return b.pre.length - a.pre.length;
  for (let i = 0; i < Math.min(a.pre.length, b.pre.length); i++) {
    const c = compareIdentifier(a.pre[i], b.pre[i]);
    if (c !== 0) return c;
  }
  return a.pre.length - b.pre.length;
}

// 带 v 前缀的合法 semver。与 olderThan 用同一个 parse：v1.0 这种少一段的不算。
export function isRelease(v: string): boolean {
  return parse(v) !== null;
}

// 当前版本按 semver 2.0 优先级低于目标版本才算落后；更高或相同不标。任一方解析不了（dev、空、格式不对）
// 就没有可比的次序，不标。节点与 hub 绑定的 agent 版本比，hub 与官方最新版比，都用它。
export function olderThan(current: string | undefined, target: string): boolean {
  const a = current ? parse(current) : null;
  const t = parse(target);
  return a !== null && t !== null && compare(a, t) < 0;
}

// 正式版：vMAJOR.MINOR.PATCH，无预发布、无构建元数据，每段不超过 uint32——与 hub 的 update.ValidVersion 同一
// 口径（它按 uint32 解析每段）。节点在线更新只接受正式版，绑定版本不是正式版时面板不提供节点更新。
const UINT32_MAX = "4294967295";
export function isStableRelease(v: string): boolean {
  const p = parse(v);
  return p !== null && p.pre.length === 0 && !v.includes("+") && p.core.every((n) => compareNumeric(n, UINT32_MAX) <= 0);
}
