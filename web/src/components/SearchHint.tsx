// 搜索入口里的快捷键提示「输入 / 搜索…」。只是视觉提示，读屏由入口自己的可访问名称与 aria-keyshortcuts 说明。
export function SearchHint({ text }: { text: string }) {
  return <span className="search-hint" aria-hidden="true">输入 <kbd>/</kbd> {text}</span>;
}
