// 内嵌字体：woff2 随前端产物打进 hub，同源加载（hub 的 CSP 是 default-src 'self'，见 internal/hub/web/web.go），
// 显示效果不取决于访客机器上装没装 Inter / JetBrains Mono。上游 CSS 按 unicode-range 分子集，浏览器只下载页面用到的子集。
// 中文没有内嵌（完整中文字体以 MB 计），由 styles.css 的 --font-ui / --font-mono 回退到系统中文字体。
import "@fontsource-variable/inter/wght.css";
import "@fontsource-variable/jetbrains-mono/wght.css";
