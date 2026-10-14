// agent 地址准入规则见 agentconfig.CheckHub 注释；https 总是接受；http 只在主机是 loopback IP 字面量时不需要
// --insecure-http。loopback 字面量指 127.0.0.0/8 与 ::1；localhost 之类的名字要经解析、结果不由地址本身决定，
// 不在豁免内。面板据此决定安装命令带不带 --insecure-http，判定与 agent 同一口径。
// 两边不一致时方向不对称：面板多带一个 --insecure-http 只是把放行写进配置；少带则 register 被 agent 拒绝。
// 所以这里只认最窄的两种写法，其余一律按需要放行处理。

// hostname 取 WHATWG URL 的 hostname：IPv4 已规范成点分十进制（127.1、0x7f.0.0.1 都变成 127.0.0.1），
// IPv6 带方括号且已压缩（[0:0:0:0:0:0:0:1] 变成 [::1]）。面板写进命令的 hub 地址是浏览器的 location.origin，
// 或运维设的覆盖地址经 lib/installHub.ts 的 parseInstallHub 取出的 URL origin，两者都是这份规范写法，agent
// 拿到的也是它。
export function isLoopbackIPLiteral(hostname: string): boolean {
  if (hostname === "[::1]") return true;
  const m = /^127\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(hostname);
  return m !== null && m.slice(1).every((octet) => Number(octet) <= 255);
}

// origin 为 http 且主机不是 loopback IP 字面量时，安装命令要带 --insecure-http。
export function needsInsecureHTTP(origin: string): boolean {
  const url = new URL(origin);
  return url.protocol === "http:" && !isLoopbackIPLiteral(url.hostname);
}
