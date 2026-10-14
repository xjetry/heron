import type { SVGProps } from "react";

const paths = {
  overview: "M3 3h7v7H3z M14 3h7v7h-7z M3 14h7v7H3z M14 14h7v7h-7z",
  server: "M4 3h16v7H4z M4 14h16v7H4z M7 6.5h.01 M7 17.5h.01 M11 6.5h6 M11 17.5h6",
  activity: "M3 12h4l3-8 4 16 3-8h4",
  bell: "M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9 M10 21h4",
  moon: "M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z",
  history: "M3 11a9 9 0 1 1 2.6 7.4 M3 4v7h7 M12 7v5l3 2",
  send: "m21 3-7 18-4-7-7-4 18-7z M10 14l11-11",
  key: "M14 4a6 6 0 1 1-5 9L3 19v3h4v-3h3l3-3 M17 7h.01",
  palette: "M12 3a9 9 0 1 0 0 18h1a2 2 0 0 0 1-4c-1-1 0-3 2-3h2a3 3 0 0 0 3-3 9 9 0 0 0-9-8 M7 10h.01 M10 6h.01 M15 6h.01 M18 10h.01",
  layers: "m12 3 10 5-10 5L2 8l10-5z M2 12l10 5 10-5 M2 16l10 5 10-5",
  database: "M20 6c0 2-4 3-8 3s-8-1-8-3 4-3 8-3 8 1 8 3z M4 6v12c0 2 4 3 8 3s8-1 8-3V6 M4 12c0 2 4 3 8 3s8-1 8-3",
  shield: "m12 3 8 3v6c0 5-8 9-8 9s-8-4-8-9V6l8-3z m-4 9 3 3 5-6",
  plus: "M12 5v14 M5 12h14",
  copy: "M9 9h12v12H9z M15 9V3H3v12h6",
  check: "m5 12 4 4L19 6",
  external: "M14 3h7v7 M21 3l-9 9 M10 3H4v17h17v-7",
  logout: "M10 3H4v18h6 M8 12h13 m-5-5 5 5-5 5",
  menu: "M4 6h16 M4 12h16 M4 18h16",
  // 三个点画成小圆环而不是零长线段：零长线段的点径等于描边宽度（1.6/24），18px 下不到 1.5px，几乎看不见。
  more: "M3.6 12a1.4 1.4 0 1 0 2.8 0a1.4 1.4 0 1 0 -2.8 0 M10.6 12a1.4 1.4 0 1 0 2.8 0a1.4 1.4 0 1 0 -2.8 0 M17.6 12a1.4 1.4 0 1 0 2.8 0a1.4 1.4 0 1 0 -2.8 0",
  close: "m6 6 12 12 M6 18 18 6",
  sun: "M16 12a4 4 0 1 1-8 0 4 4 0 0 1 8 0 M12 2v2 M12 20v2 M2 12h2 M20 12h2 M5 5l1 1 M18 18l1 1 M5 19l1-1 M18 6l1-1",
  search: "M17 10a7 7 0 1 1-14 0 7 7 0 0 1 14 0 m-2 5 6 6",
  edit: "m15 4 5 5 M4 15 16 3l5 5L9 20l-6 1 1-6z",
  calendar: "M4 5h16v16H4z M4 10h16 M8 3v4 M16 3v4 M8 14h3 M8 17h6",
  chevronUp: "m6 15 6-6 6 6",
  chevronLeft: "m15 6-6 6 6 6",
  chevronRight: "m9 6 6 6-6 6",
  grip: "M9 5h.01 M15 5h.01 M9 12h.01 M15 12h.01 M9 19h.01 M15 19h.01",
  chevronDown: "m6 9 6 6 6-6",
  globe: "M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0 M3 12h18 M12 3c5 5 5 13 0 18-5-5-5-13 0-18",
  arrowDown: "M12 3v18 m-6-6 6 6 6-6",
  arrowUp: "M12 21V3 m-6 6 6-6 6 6",
  // 显示器：明暗切换「跟随系统」档的图标。
  system: "M3 4h18v12H3z M8 20h8 M12 16v4",
} as const;

export type IconName = keyof typeof paths;

export function Icon({ name, ...props }: SVGProps<SVGSVGElement> & { name: IconName }) {
  // data-icon 写图标名：图标对读屏隐藏（aria-hidden），测试与主题样式按名字认它，不按 path 数据。
  return <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" data-icon={name} {...props}><path d={paths[name]} /></svg>;
}
