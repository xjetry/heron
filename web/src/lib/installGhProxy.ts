import { parseInstallHub } from "./installHub";

// GitHub 代理（gh_proxy）给连不上 GitHub 的主机用：这类代理把 "<代理>/<原始 GitHub 地址>" 转发到原始地址，安装命令据此
// 把安装脚本与 --base-url 的包下载地址都改成经代理的写法（拼法在 lib/installCommand.ts）。代理地址与 Hub 连接地址一样只存在
// 这台浏览器的 localStorage，hub 不记录：用不用代理、用哪个，是运维从哪台机器装哪些节点的偏好，不是 hub 的属性。

// 代理地址未加引号地拼进可复制的 shell 命令，严格度与 parseInstallHub 相同：scheme、主机与端口部分直接交给
// parseInstallHub（只收 http(s) origin，规范化写法相同），之后只允许一段路径前缀——每段由字母、数字与 . _ ~ - 组成，
// 不能是 "." 或 ".."（WHATWG 解析会把它们折叠掉，命令里出现的就不是输入框里看到的那个），段之间单个 "/"，结尾可有一个 "/"；
// 查询、片段、百分号转义与其余字符一律拒绝。
// 返回值：空串或全空白返回 ""，表示直连 GitHub；合法时返回规范化的 origin 加去掉结尾 "/" 的路径前缀；其余返回 null。
export function parseGhProxy(text: string): string | null {
  const trimmed = text.trim();
  if (trimmed === "") return "";
  const scheme = /^https?:\/\//i.exec(trimmed);
  const split = scheme === null ? -1 : trimmed.indexOf("/", scheme[0].length);
  const origin = parseInstallHub(split === -1 ? trimmed : trimmed.slice(0, split));
  if (!origin) return null;
  const path = split === -1 ? "" : trimmed.slice(split);
  if (!/^(?:\/(?!\.\.?(?:\/|$))[A-Za-z0-9._~-]+)*\/?$/.test(path)) return null;
  return origin + path.replace(/\/$/, "");
}

// 存规范化后的代理地址；存储被禁用或存的值不合法时按没有代理处理，命令回到直连 GitHub。
const storageKey = "heron-install-gh-proxy";

export function loadGhProxy(): string {
  try {
    const saved = localStorage.getItem(storageKey);
    return saved === null ? "" : (parseGhProxy(saved) ?? "");
  } catch {
    return "";
  }
}

export function saveGhProxy(proxy: string) {
  try { localStorage.setItem(storageKey, proxy); } catch { /* 存储被禁用时只影响下次打开的默认值。 */ }
}

export function clearGhProxy() {
  try { localStorage.removeItem(storageKey); } catch { /* 同上：没有存下来的代理地址，下次打开本就直连 GitHub。 */ }
}
