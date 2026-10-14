// 安装命令里的 --hub 默认取浏览器当前的 origin；有的主机连不到这个入口（如出站 443 被封），要经另一个入口连
// hub，运维在这里填一个覆盖地址。覆盖地址只存在这台浏览器的 localStorage，hub 不记录：它是"这个运维从哪台
// 机器装哪些节点"的偏好，不是 hub 的属性，hub 也无从验证某个入口对某台主机是否可达；节点实际连的地址写在
// agent 自己的配置里。

// 覆盖地址未加引号地拼进可复制的 shell 命令，所以只接受 origin 形态——http 或 https、有主机、可带 1–65535
// 的端口，不带路径、查询、片段与用户信息——并且命令里写的是 WHATWG URL 规范化后的 protocol + host（默认端口
// 省掉、IPv4 规范成点分十进制、IPv6 压缩、国际化域名转 punycode），与 lib/transport.ts 读 location.origin
// 的口径相同：--hub 与 --insecure-http 的判定因此看到同一种写法。
// 返回值：空串或全空白返回 ""，表示用当前域名；合法时返回规范化的 origin；其余返回 null。
export function parseInstallHub(text: string): string | null {
  const trimmed = text.trim();
  if (trimmed === "") return "";
  // 要求字面上以 scheme:// 加主机开头、内部不含空白：WHATWG 解析器会把 "http:host"、多余的斜杠、反斜杠、
  // 内嵌的制表与换行静默改写成另一个地址，命令里出现的就不是输入框里看到的那个。
  if (!/^https?:\/\/[^/\\]/i.test(trimmed) || /\s/.test(trimmed)) return null;
  let url: URL;
  try {
    url = new URL(trimmed);
  } catch {
    return null;
  }
  // href 恰为 origin 加根路径时，才没有路径、查询（含空查询 "?"）、片段与用户信息；结尾单个 "/" 与不写等价，允许。
  if (url.href !== `${url.origin}/`) return null;
  // WHATWG 的主机只排除少数分隔符，$ ( ) ; & ` ' 之类都能留在规范化结果里，原样进命令就是命令注入。
  // 所以主机只认字母、数字、连字符与点（国际化域名已转成这种 punycode 写法），或压缩后的 IPv6 字面量；
  // 端口 0 能解析但连不上，一并拒绝。
  if (!/^(?:[a-z0-9.-]+|\[[0-9a-f:]+\])$/.test(url.hostname) || url.port === "0") return null;
  return url.origin;
}

// 存规范化后的 origin；存储被禁用或存的值不合法时按没有覆盖处理，命令回到当前域名。
const storageKey = "heron-install-hub";

export function loadInstallHub(): string {
  try {
    const saved = localStorage.getItem(storageKey);
    return saved === null ? "" : (parseInstallHub(saved) ?? "");
  } catch {
    return "";
  }
}

export function saveInstallHub(origin: string) {
  try { localStorage.setItem(storageKey, origin); } catch { /* 存储被禁用时只影响下次打开的默认值。 */ }
}

export function clearInstallHub() {
  try { localStorage.removeItem(storageKey); } catch { /* 同上：没有存下来的覆盖地址，下次打开本就回到当前域名。 */ }
}
