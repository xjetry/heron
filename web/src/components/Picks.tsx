import { toggled, withId } from "../lib/ids";

// 节点与渠道的多选共用；列表刷新后已删除对象的勾选仍留在集合里，由调用方提交时与当前列表求交。
export function Picks({ legend, items, selected, onChange, max }: {
  legend: string; items: readonly { id: bigint; name: string }[]; selected: ReadonlySet<bigint>; onChange: (next: Set<bigint>) => void;
  max?: number;
}) {
  const full = max !== undefined && items.filter((it) => selected.has(it.id)).length >= max;
  return (
    <fieldset className="picks">
      <legend>{legend}</legend>
      {max !== undefined && items.length > max && <p className="muted">最多选 {max} 个渠道</p>}
      {items.length === 0 && <span className="muted">没有可选项。</span>}
      {items.map((it) => (
        <label key={String(it.id)}><input type="checkbox" checked={selected.has(it.id)} disabled={full && !selected.has(it.id)} onChange={() => onChange(toggled(selected, it.id))} />{withId(it.name, it.id)}</label>
      ))}
    </fieldset>
  );
}
