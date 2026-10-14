import { useEffect, useEffectEvent, useRef, useState, type KeyboardEvent } from "react";
import { isDate } from "../lib/format";
import { addMonths, calendarStep, monthDays } from "../lib/ymd";
import { Icon } from "./Icon";

const WEEKDAYS = ["一", "二", "三", "四", "五", "六", "日"];

// 「今天」取浏览器本地日期：选日期是给人看的日历，与 hub 时区无关；需要 hub 日界的地方（到期剩余天数）由 hub 自己算。
function today(): string {
  const now = new Date();
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${String(now.getFullYear()).padStart(4, "0")}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`;
}

// 自绘月历：周一开头，6 行 42 格，前后月份的日子淡显但可选。方向键按日 / 周移动，PageUp / PageDown 换月，
// Home / End 到本周首尾，Esc 关闭并把焦点还给控件；点到弹层与所属控件之外也关闭，焦点留在用户点到的地方。
// value 不是合法日期时从今天开始。
export function Calendar({ label, value, onPick, onClose }: { label: string; value: string; onPick: (ymd: string) => void; onClose: (returnFocus: boolean) => void }) {
  const selected = isDate(value) ? value : "";
  const [focus, setFocus] = useState(() => selected || today());
  const keyboard = useRef(true);
  const root = useRef<HTMLDivElement>(null);
  // 监听只在挂载时注册一次；关闭时调用的是父组件这一次渲染给的 onClose，不是挂载那一刻的。
  const closeOutside = useEffectEvent(() => onClose(false));
  useEffect(() => {
    if (!keyboard.current) return;
    root.current?.querySelector<HTMLButtonElement>(`[data-ymd="${focus}"]`)?.focus();
  }, [focus]);
  useEffect(() => {
    // 所属控件（含打开弹层的按钮）之内的点击由控件自己处理，否则按钮的 mousedown 先关、click 又开。
    const owner = root.current?.closest(".date-box") ?? root.current;
    const onPointer = (event: PointerEvent) => { if (owner && !owner.contains(event.target as Node)) closeOutside(); };
    document.addEventListener("pointerdown", onPointer, true);
    return () => document.removeEventListener("pointerdown", onPointer, true);
  }, []);
  // 0001–9999 年之外 toISOString 会写成六位年份；翻到范围外时停在原地。
  const move = (next: string, byKeyboard: boolean) => { if (!isDate(next)) return; keyboard.current = byKeyboard; setFocus(next); };
  const onKeyDown = (event: KeyboardEvent) => {
    if (event.key === "Escape") { event.preventDefault(); event.stopPropagation(); onClose(true); return; }
    const next = calendarStep(event.key, focus);
    if (next === undefined || !(event.target instanceof HTMLButtonElement) || !event.target.dataset.ymd) return;
    event.preventDefault();
    move(next, true);
  };
  const [year, month] = focus.split("-");
  const days = monthDays(focus);
  const now = today();
  return (
    <div ref={root} className="calendar" role="dialog" aria-label={`选择${label}`} onKeyDown={onKeyDown}>
      <div className="calendar-head">
        <button type="button" className="icon-button" aria-label="上个月" onClick={() => move(addMonths(focus, -1), false)}><Icon name="chevronLeft" /></button>
        <span className="calendar-title" aria-live="polite">{Number(year)} 年 {Number(month)} 月</span>
        <button type="button" className="icon-button" aria-label="下个月" onClick={() => move(addMonths(focus, 1), false)}><Icon name="chevronRight" /></button>
      </div>
      <table className="calendar-grid">
        <thead><tr>{WEEKDAYS.map((w) => <th key={w} scope="col" abbr={`星期${w}`}>{w}</th>)}</tr></thead>
        <tbody>
          {Array.from({ length: 6 }, (_, row) => (
            <tr key={row}>
              {days.slice(row * 7, row * 7 + 7).map((ymd) => (
                <td key={ymd}>
                  {isDate(ymd) && <button type="button" data-ymd={ymd} aria-label={ymd} tabIndex={ymd === focus ? 0 : -1}
                    aria-pressed={ymd === selected} aria-current={ymd === now ? "date" : undefined}
                    data-outside={ymd.slice(0, 7) !== focus.slice(0, 7) || undefined}
                    onClick={() => onPick(ymd)}>{Number(ymd.slice(8))}</button>}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      <div className="calendar-foot"><button type="button" className="link" onClick={() => onPick(now)}>今天</button></div>
    </div>
  );
}
