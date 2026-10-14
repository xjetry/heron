import { useSyncExternalStore } from "react";

export type Scheme = "light" | "dark";

const systemDark = () => window.matchMedia("(prefers-color-scheme: dark)");

// 页面的明暗由两处决定：html 的 data-theme（公开页按站点设置写 light 或 dark）优先，没有时跟随系统。
// styles.css 的 color-scheme 规则按同一优先级写，图表读到的与 CSS 是同一个结果。
export function currentScheme(): Scheme {
  const forced = document.documentElement.dataset.theme;
  if (forced === "light" || forced === "dark") return forced;
  return systemDark().matches ? "dark" : "light";
}

function subscribe(onChange: () => void): () => void {
  const media = systemDark();
  media.addEventListener("change", onChange);
  const observer = new MutationObserver(onChange);
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  return () => {
    media.removeEventListener("change", onChange);
    observer.disconnect();
  };
}

export function useColorScheme(): Scheme {
  return useSyncExternalStore(subscribe, currentScheme);
}

// canvas 不认 light-dark() 与 var()：放一个临时元素，让浏览器把 CSS 值解析成具体颜色，再交给 uPlot。
// host 须已挂在文档里，解析才会带上它继承的 color-scheme。
export function resolveColor(host: HTMLElement, value: string): string {
  const probe = document.createElement("span");
  probe.style.color = value;
  host.append(probe);
  const color = getComputedStyle(probe).color;
  probe.remove();
  return color;
}
