import { useEffect, useId, useState } from "react";

// 带自绘候选列表的输入框，代替原生 datalist（它的弹出列表是浏览器自己画的系统菜单）。焦点始终留在输入框里，
// 当前候选用 aria-activedescendant 指出：上下键移动，Enter 选中当前候选；没有当前候选时 Enter 交给 onEnter（照输入的
// 文字处理）。Esc 只收起候选列表：阻止默认动作与冒泡，所在的抽屉不随之关闭。点候选即选中；mousedown 阻止默认动作，
// 输入框不失焦（失焦会先收起列表，click 就落空了）。suggestions 由调用方按输入筛好，本组件只存开合与当前项。
export function SuggestInput({ id, label, value, onChange, suggestions, onPick, onEnter, placeholder }: {
  id?: string;
  label: string;
  value: string;
  onChange: (text: string) => void;
  suggestions: readonly string[];
  onPick: (suggestion: string) => void;
  onEnter: () => void;
  placeholder?: string;
}) {
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1);
  const listId = useId();
  const shown = open && suggestions.length > 0;
  // 候选随输入变化：当前项越界时视为没有当前项，不指向一个已经不在列表里的候选。
  const current = active < suggestions.length ? active : -1;
  const optionId = (i: number) => `${listId}-${i}`;
  useEffect(() => {
    if (shown && current >= 0) document.getElementById(optionId(current))?.scrollIntoView?.({ block: "nearest" });
  });
  const pick = (suggestion: string) => { onPick(suggestion); setActive(-1); };
  return (
    <div className="suggest">
      <input id={id} role="combobox" aria-label={label} aria-autocomplete="list" aria-expanded={shown} aria-controls={shown ? listId : undefined}
        aria-activedescendant={shown && current >= 0 ? optionId(current) : undefined} placeholder={placeholder} value={value}
        onChange={(event) => { onChange(event.target.value); setOpen(true); setActive(-1); }}
        onFocus={() => setOpen(true)} onBlur={() => { setOpen(false); setActive(-1); }}
        onKeyDown={(event) => {
          if (event.key === "ArrowDown" && suggestions.length > 0) { event.preventDefault(); setOpen(true); setActive(shown ? Math.min(current + 1, suggestions.length - 1) : 0); }
          else if (event.key === "ArrowUp" && shown) { event.preventDefault(); setActive(Math.max(current - 1, 0)); }
          else if (event.key === "Enter") { event.preventDefault(); if (shown && current >= 0) pick(suggestions[current]); else onEnter(); }
          else if (event.key === "Escape" && shown) { event.preventDefault(); event.stopPropagation(); setOpen(false); setActive(-1); }
        }} />
      {shown && (
        <ul id={listId} role="listbox" aria-label={label} className="select-popup suggest-popup">
          {suggestions.map((suggestion, i) => (
            <li key={suggestion} id={optionId(i)} role="option" aria-selected={i === current}
              onMouseDown={(event) => event.preventDefault()} onClick={() => pick(suggestion)}>{suggestion}</li>
          ))}
        </ul>
      )}
    </div>
  );
}
