import { useEffect, useRef } from "react";

// 鼠标激活按钮不保证它获得焦点；调用方显式传入触发器，原生 dialog 负责背景 inert 与焦点约束。
// 弹窗与抽屉共用生命周期，关闭时统一把焦点交还给仍在页面上的触发器。
export function useNativeDialog(opener: HTMLElement) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const dialog = ref.current!;
    dialog.showModal();
    dialog.querySelector<HTMLElement>("[data-autofocus]")?.focus();
    return () => {
      dialog.close();
      if (opener.isConnected) opener.focus({ preventScroll: true });
    };
  }, [opener]);
  return ref;
}
