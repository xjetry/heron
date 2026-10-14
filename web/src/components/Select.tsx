import { useEffect, useId, useRef, useState, type KeyboardEvent } from "react";
import { Icon } from "./Icon";

export type SelectOption<T extends string> = { value: T; label: string };

// 自绘的单选下拉：触发按钮写「标签 当前值 ▾」，与筛选行上的地区 / 标签入口同一种外观；弹出的列表也自绘，不用原生
// select 的系统菜单。value 必须是 options 之一，本组件不存选择，只存开合。
// 给出 caption 时是表单字段：caption 作可见标题写在按钮上方，按钮只写当前值、撑满字段宽度，label 只作读屏名（与
// DateInput 的 label / caption 同一分工：表格或抽屉里 label 带上对象名，caption 只写字段名）。
// 焦点在列表里逐项移动（每个选项可聚焦）：打开时聚焦当前项，上下键 / Home / End 移动，Enter / 空格选中并收起，Esc 收起
// 不改值；两者都把焦点还给触发按钮。Tab 同样收起并把焦点放回触发按钮，但不拦默认动作，于是浏览器从触发按钮出发移动焦点，
// 落到下拉之后（Shift+Tab 时之前）的控件，与原生 select 一致。点外面只收起，不动焦点。
// Esc 只收起这一层：阻止默认动作与冒泡，抽屉与弹窗（原生 dialog 的 Esc 关闭）不随之关闭。
export function Select<T extends string>({ label, caption, value, options, onChange }: {
  label: string;
  caption?: string;
  value: T;
  options: readonly SelectOption<T>[];
  onChange: (next: T) => void;
}) {
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const list = useRef<HTMLUListElement>(null);
  const listId = useId();
  useEffect(() => {
    if (!open) return;
    list.current?.querySelector<HTMLElement>('[aria-selected="true"]')?.focus();
    const away = (event: PointerEvent) => {
      if (!root.current?.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("pointerdown", away);
    return () => document.removeEventListener("pointerdown", away);
  }, [open]);
  const close = () => { setOpen(false); trigger.current?.focus(); };
  const choose = (next: T) => {
    close();
    if (next !== value) onChange(next);
  };
  const onListKey = (event: KeyboardEvent<HTMLUListElement>) => {
    const items = Array.from(list.current?.querySelectorAll<HTMLElement>('[role="option"]') ?? []);
    const at = items.indexOf(document.activeElement as HTMLElement);
    switch (event.key) {
      case "ArrowDown": items[Math.min(at + 1, items.length - 1)]?.focus(); break;
      case "ArrowUp": items[Math.max(at - 1, 0)]?.focus(); break;
      case "Home": items[0]?.focus(); break;
      case "End": items[items.length - 1]?.focus(); break;
      case "Enter": case " ": if (at >= 0) choose(options[at].value); break;
      case "Escape": event.stopPropagation(); close(); break;
      case "Tab": close(); return;
      default: return;
    }
    event.preventDefault();
  };
  const current = options.find((option) => option.value === value);
  return (
    <div ref={root} className={caption === undefined ? "select" : "select select-field"}>
      {caption !== undefined && <span className="field-caption" aria-hidden="true">{caption}</span>}
      <button ref={trigger} type="button" className="select-trigger" aria-haspopup="listbox" aria-expanded={open} aria-controls={open ? listId : undefined}
        onClick={() => setOpen((o) => !o)}
        onKeyDown={(event) => { if (!open && (event.key === "ArrowDown" || event.key === "ArrowUp")) { event.preventDefault(); setOpen(true); } }}>
        <span className={caption === undefined ? "select-label" : "sr-only"}>{label}</span>{" "}<span className="select-value">{current?.label}</span>
        <Icon name="chevronDown" className="select-caret" width={14} height={14} />
      </button>
      {open && (
        <ul ref={list} id={listId} role="listbox" aria-label={label} className="select-popup" onKeyDown={onListKey}>
          {options.map((option) => (
            <li key={option.value} role="option" aria-selected={option.value === value} tabIndex={-1} onClick={() => choose(option.value)}>
              <Icon name="check" className="select-check" width={14} height={14} />
              {option.label}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
