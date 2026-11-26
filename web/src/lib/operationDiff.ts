// 操作记录的修改前 / 修改后是 store 按动作拍下的对象快照（JSON）：顶层按表名分组，每组是行对象的数组。展开成"路径 → 值"
// 后逐路径比较，只列出变了的路径，空数组与空对象不单独占一行——建出来的行显示为它的全部字段从无到有，删掉的行反过来。
export type FieldChange = { path: string; before?: string; after?: string };

function flatten(value: unknown, path: string, out: Map<string, string>) {
  if (Array.isArray(value)) {
    value.forEach((v, i) => flatten(v, `${path}[${i}]`, out));
    return;
  }
  if (value !== null && typeof value === "object") {
    for (const [k, v] of Object.entries(value)) flatten(v, path ? `${path}.${k}` : k, out);
    return;
  }
  out.set(path, JSON.stringify(value));
}

function parse(text: string): Map<string, string> | null {
  try {
    const out = new Map<string, string>();
    flatten(JSON.parse(text), "", out);
    return out;
  } catch {
    return null;
  }
}

// 两边任一不是 JSON（详细记录过了保留期被清空，或不是快照）时返回 null：调用方原样显示两段文本，不把"读不出差异"
// 显示成"没有变化"。
export function snapshotDiff(before: string, after: string): FieldChange[] | null {
  const b = parse(before);
  const a = parse(after);
  if (b === null || a === null) return null;
  const out: FieldChange[] = [];
  for (const [path, value] of b) {
    if (a.get(path) !== value) out.push({ path, before: value, after: a.get(path) });
  }
  for (const [path, value] of a) {
    if (!b.has(path)) out.push({ path, after: value });
  }
  return out;
}
