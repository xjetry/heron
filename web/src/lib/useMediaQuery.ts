import { useSyncExternalStore } from "react";

// 状态墙在宽屏点方块切换详情、窄屏直接进节点页；组件分支与 public.css 的列表布局使用同一断点。
export const WIDE_QUERY = "(min-width: 901px)";

export function useMediaQuery(query: string): boolean {
  return useSyncExternalStore(
    (onChange) => {
      const media = window.matchMedia(query);
      media.addEventListener("change", onChange);
      return () => media.removeEventListener("change", onChange);
    },
    () => window.matchMedia(query).matches,
  );
}
