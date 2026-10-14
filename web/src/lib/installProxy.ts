// 连不上 GitHub 与 CDN 的主机，首次安装可经 SSH 反代借用运维本机的代理：-R 把本地反代端口转发到目标主机
// 的同一端口，登录后的 shell 里三个代理变量都指向它，安装脚本的下载经此出网。两侧用同一个端口号，变量里写的
// 才是转发过来的那个端口；http 与 socks5h 指向同一端口，所以这个端口要同时接受两种协议（混合端口）。
// 端口只进 SSH 反代参数，不进安装命令。
// 油猴脚本（assets/heron-quick-node.user.js）独立分发、不能引用这里，另有一份同样的拼法与默认端口，由它的测试逐字对照。

export const defaultProxyPort = 7890;

// 端口要拼进可复制的 shell 命令：只认不带前导零的十进制 1–65535，其余一律视为不合法、不出命令，输入框里的
// 内容因此改不了命令的结构。
export function parseProxyPort(text: string): number | null {
  if (!/^[1-9]\d{0,4}$/.test(text)) return null;
  const port = Number(text);
  return port <= 65535 ? port : null;
}

export function sshProxyArgs(port: number): string {
  const at = `127.0.0.1:${port}`;
  return `-t -R ${at}:${at} 'export http_proxy=http://${at}; export https_proxy=http://${at}; export all_proxy=socks5h://${at}; exec $SHELL -l'`;
}

// 端口按浏览器记住，下次打开沿用；存储被禁用或记住的值不合法时回到默认端口。
const storageKey = "heron-install-proxy-port";

export function loadProxyPort(): string {
  try {
    const saved = localStorage.getItem(storageKey);
    return saved !== null && parseProxyPort(saved) !== null ? saved : String(defaultProxyPort);
  } catch {
    return String(defaultProxyPort);
  }
}

export function saveProxyPort(port: number) {
  try { localStorage.setItem(storageKey, String(port)); } catch { /* 存储被禁用时只影响下次打开的默认值。 */ }
}
