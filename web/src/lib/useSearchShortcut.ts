import { useEffect } from "react";

// ⌘K / Ctrl+K 与 / 唤起搜索。⌘K 不是可输入字符，焦点在输入框里也生效；/ 是可输入字符，只在焦点不在可编辑元素上、
// 没按修饰键、不在输入法组字时生效，否则会吞掉用户正要打的斜杠。onTrigger 变化时重新订阅，调用方传稳定的回调。
export function useSearchShortcut(onTrigger: () => void) {
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.isComposing) return;
      const commandK = (event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k";
      const slash = event.key === "/" && !event.metaKey && !event.ctrlKey && !event.altKey && !isEditable(event.target);
      if (!commandK && !slash) return;
      event.preventDefault();
      onTrigger();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onTrigger]);
}

// 供 aria-keyshortcuts 使用，与上面的判定同一组按键。
export const SEARCH_KEYSHORTCUTS = "/ Meta+K Control+K";

function isEditable(target: EventTarget | null): boolean {
  return target instanceof HTMLElement && (target.isContentEditable || target.matches("input, textarea, select"));
}
