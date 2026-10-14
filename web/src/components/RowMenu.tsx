import { Fragment, type KeyboardEvent, useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Link } from "react-router";
import { Icon } from "./Icon";

export type RowMenuItem = {
  label: string;
  onSelect?: (trigger: HTMLElement) => void;
  to?: string;
  // 随 to 一起交给 Link 的导航 state（例如节点列表的返回位置）。
  state?: unknown;
  danger?: boolean;
  disabled?: boolean;
  confirm?: string;
  note?: string;
};

// 首击只武装，再击才执行；武装只活在这次打开的菜单里，关闭时统一撤销。
export function RowMenu({ label, items }: { label: string; items: readonly RowMenuItem[] }) {
  const [open, setOpen] = useState(false);
  const [armed, setArmed] = useState<number | null>(null);
  const root = useRef<HTMLSpanElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  const [position, setPosition] = useState({ left: 0, top: 0 });
  const close = (refocus: boolean) => {
    setOpen(false);
    setArmed(null);
    if (refocus) trigger.current?.focus();
  };
  useEffect(() => {
    if (!open) return;
    menu.current?.querySelector<HTMLElement>("[role=menuitem]")?.focus();
    const away = (event: PointerEvent) => {
      if (!root.current?.contains(event.target as Node) && !menu.current?.contains(event.target as Node)) close(false);
    };
    document.addEventListener("pointerdown", away);
    return () => document.removeEventListener("pointerdown", away);
  }, [open]);
  // 表格自身会裁切溢出内容；菜单挂到 body，并在滚动或尺寸变化后保持与触发器对齐。
  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const anchor = trigger.current?.getBoundingClientRect();
      const popup = menu.current?.getBoundingClientRect();
      if (!anchor || !popup) return;
      setPosition({ left: Math.max(8, Math.min(anchor.right - popup.width, window.innerWidth - popup.width - 8)), top: Math.max(8, Math.min(anchor.bottom + 4, window.innerHeight - popup.height - 8)) });
    };
    place();
    window.addEventListener("resize", place);
    document.addEventListener("scroll", place, true);
    return () => { window.removeEventListener("resize", place); document.removeEventListener("scroll", place, true); };
  }, [open, armed]);
  const onKeyDown = (event: KeyboardEvent) => {
    const focusable = Array.from(menu.current?.querySelectorAll<HTMLElement>("[role=menuitem]") ?? []);
    const index = focusable.indexOf(document.activeElement as HTMLElement);
    const go = (i: number) => focusable[(i + focusable.length) % focusable.length]?.focus();
    if (event.key === "Escape") { event.preventDefault(); close(true); }
    else if (event.key === "ArrowDown") { event.preventDefault(); go(index + 1); }
    else if (event.key === "ArrowUp") { event.preventDefault(); go(index - 1); }
    else if (event.key === "Home") { event.preventDefault(); go(0); }
    else if (event.key === "End") { event.preventDefault(); go(focusable.length - 1); }
  };
  const select = (item: RowMenuItem, index: number) => {
    if (item.disabled) return;
    if (item.confirm && armed !== index) { setArmed(index); return; }
    const button = trigger.current!;
    close(false);
    item.onSelect?.(button);
  };
  return (
    <span ref={root} className="row-menu">
      <button ref={trigger} type="button" className="icon-button row-menu-trigger" aria-label={`更多操作 ${label}`} aria-haspopup="menu" aria-expanded={open}
        onClick={() => (open ? close(false) : setOpen(true))}><Icon name="more" /></button>
      {open && createPortal(
        <div ref={menu} className="row-menu-popup" role="menu" aria-label={`${label} 的操作`} style={position} onKeyDown={onKeyDown}>
          {/* 项是位置式的静态列表，用序号作 key：label 会随数据刷新改写（如启用 / 停用互换），按 label 建 key 会让那一项重挂、丢掉焦点与武装。 */}
          {items.map((item, index) => {
            const name = armed === index && item.confirm ? item.confirm : `${item.label} ${label}`;
            const text = armed === index && item.confirm ? item.confirm : item.label;
            const className = item.danger ? "danger" : undefined;
            if (item.to && !item.disabled) {
              return <Link key={index} role="menuitem" aria-label={name} className={className} to={item.to} state={item.state} onClick={() => close(false)}>{text}</Link>;
            }
            return (
              <Fragment key={index}>
                <button type="button" role="menuitem" aria-label={name} className={className} aria-disabled={item.disabled || undefined} onClick={(event) => { event.currentTarget.focus(); select(item, index); }}>{text}</button>
                {armed === index && item.note && <small className="row-menu-note">{item.note}</small>}
              </Fragment>
            );
          })}
          {armed !== null && <button type="button" role="menuitem" aria-label="取消" onClick={() => setArmed(null)}>取消</button>}
        </div>, document.body
      )}
    </span>
  );
}
