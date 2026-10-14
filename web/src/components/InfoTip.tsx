import { type ReactNode, useId, useState } from "react";

// aria-describedby 始终指向 DOM 中的正文；CSS 只控制浮层是否显示。
export function InfoTip({ label = "说明", children }: { label?: string; children: ReactNode }) {
  const id = useId();
  const [pinned, setPinned] = useState(false);
  const [transient, setTransient] = useState(false);
  // 点击只切换固定状态，不能把此前由聚焦或悬停展开的正文立即关闭。
  const open = pinned || transient;
  return (
    <span className="info-tip" data-open={open || undefined} onMouseEnter={() => setTransient(true)} onMouseLeave={() => setTransient(false)}>
      <button type="button" aria-label={label} aria-expanded={open} aria-describedby={id} onClick={() => setPinned((p) => !p)}
        onFocus={() => setTransient(true)} onBlur={() => setTransient(false)} onKeyDown={(event) => {
          if (event.key === "Escape") {
            setPinned(false);
            setTransient(false);
          }
        }}>ⓘ</button>
      <span role="tooltip" id={id} className="info-tip-body">{children}</span>
    </span>
  );
}
