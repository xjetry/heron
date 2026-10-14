import { useEffect, useId, useRef, useState } from "react";
import { literalPattern } from "../lib/fold";
import { Icon } from "./Icon";

export type MultiSelectOption = { value: string; label: string; count?: number };

// 标签与地区由数据动态产生：选项来自当前快照，选择集由调用方按快照规范化后传回，
// 本组件不存选择，只存开合与搜索词。已选以可移除胶囊显示，超过 foldAt 个折成「+N」。
// 搜索与标签判重同一口径（lib/fold 的折叠字面匹配）：输入 "db" 能找到 "DB"，输入 "a.b" 不会匹配 "aXb"。
export function MultiSelect({ label, options, selected, onChange, searchable = false, foldAt = 3 }: {
  label: string;
  options: readonly MultiSelectOption[];
  selected: readonly string[];
  onChange: (next: string[]) => void;
  searchable?: boolean;
  foldAt?: number;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const root = useRef<HTMLDivElement>(null);
  const listId = useId();
  useEffect(() => {
    if (!open) return;
    const away = (event: PointerEvent) => {
      if (!root.current?.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("pointerdown", away);
    return () => document.removeEventListener("pointerdown", away);
  }, [open]);
  const pattern = query === "" ? null : literalPattern(query, false);
  const shown = pattern ? options.filter((option) => pattern.test(option.label)) : options;
  const toggle = (value: string) => onChange(selected.includes(value) ? selected.filter((v) => v !== value) : [...selected, value]);
  const chips = selected.flatMap((value) => options.filter((option) => option.value === value));
  return (
    <div ref={root} className="multi-select" role="group" aria-label={label} onKeyDown={(event) => { if (event.key === "Escape") setOpen(false); }}>
      <button type="button" className="multi-select-trigger" aria-expanded={open} aria-controls={listId} onClick={() => setOpen((o) => !o)}>
        {label}{chips.length > 0 && <span className="num"> {chips.length}</span>}
        <Icon name="chevronDown" className="multi-select-caret" width={14} height={14} />
      </button>
      {chips.length > 0 && (
        <ul className="multi-select-chips">
          {chips.slice(0, foldAt).map((option) => (
            <li key={option.value} className="chip">
              {option.label}
              <button type="button" aria-label={`移除 ${option.label}`} onClick={() => toggle(option.value)}>×</button>
            </li>
          ))}
          {chips.length > foldAt && <li className="chip chip-more num">+{chips.length - foldAt}</li>}
        </ul>
      )}
      {open && (
        <div className="multi-select-popup">
          {searchable && <input type="search" aria-label={`搜索${label}`} value={query} onChange={(event) => setQuery(event.target.value)} autoFocus />}
          <ul id={listId}>
            {shown.map((option) => (
              <li key={option.value}>
                <label>
                  <input type="checkbox" aria-label={option.label} checked={selected.includes(option.value)} onChange={() => toggle(option.value)} />
                  <span>{option.label}</span>
                  {option.count !== undefined && <span className="num muted">{option.count}</span>}
                </label>
              </li>
            ))}
            {shown.length === 0 && <li className="muted">没有匹配的选项</li>}
          </ul>
          {chips.length > 0 && <button type="button" className="link" onClick={() => onChange([])}>清除</button>}
        </div>
      )}
    </div>
  );
}
