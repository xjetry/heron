import { type ReactNode, useId } from "react";
import { useNativeDialog } from "./dialog";
import { Icon } from "./Icon";

type Variant = "modal" | "drawer";
const CLOSE_LABEL: Record<Variant, string> = { modal: "关闭弹窗", drawer: "关闭抽屉" };

export function Modal({ title, description, busy = false, onClose, children, className = "", opener, variant = "modal" }: {
  title: string; description?: string; busy?: boolean; onClose: () => void; children: ReactNode; className?: string; opener: HTMLElement; variant?: Variant;
}) {
  const ref = useNativeDialog(opener);
  const id = useId();
  return <dialog ref={ref} className={`${variant === "drawer" ? "drawer" : "admin-modal"} ${className}`} aria-labelledby={`${id}-title`} aria-describedby={description ? `${id}-description` : undefined}
    onCancel={(event) => { event.preventDefault(); if (!busy) onClose(); }}>
    <header className="modal-header">
      <div><h2 id={`${id}-title`}>{title}</h2>{description && <p id={`${id}-description`}>{description}</p>}</div>
      <button type="button" className="icon-button" aria-label={CLOSE_LABEL[variant]} disabled={busy} onClick={onClose}><Icon name="close" /></button>
    </header>
    {children}
  </dialog>;
}

// 新建与编辑使用右侧抽屉，确认类的小对话仍用居中弹窗。
export function Drawer(props: Omit<Parameters<typeof Modal>[0], "variant">) {
  return <Modal {...props} variant="drawer" />;
}
