import { type ReactNode, useState } from "react";
import { CopyableText } from "./CopyableText";
import { isRelease } from "../lib/version";
import { installCommand, loadUpdateSource, saveUpdateSource, type UpdateSource } from "../lib/installCommand";
import { clearGhProxy, loadGhProxy, parseGhProxy, saveGhProxy } from "../lib/installGhProxy";
import { defaultProxyPort, loadProxyPort, parseProxyPort, saveProxyPort, sshProxyArgs } from "../lib/installProxy";
import { clearInstallHub, loadInstallHub, parseInstallHub, saveInstallHub } from "../lib/installHub";

// 四个安装参数都是选填，都只存在这台浏览器的 localStorage、hub 不记录，所有用到本组件的入口（添加节点、换 token、注册窗口）
// 与节点列表的"复制安装命令"读同一份记住的值；命令由 lib/installCommand.ts 的 installCommand 拼出，这里只负责输入与展示。
// origin 是浏览器当前的 origin，Hub 连接地址留空时用它；生效地址也是判定要不要 --insecure-http 的依据。
// Hub 连接地址或 GitHub 代理不合法时不出任何命令（含 SSH 反代参数），输入框的内容因此改不了命令的结构；本地反代端口只影响
// SSH 反代参数，不合法时只是不出那段参数。
// registerKey 是注册窗口的 key 或指定节点的安装凭据；注册后由 agent 保存另行签发的运行 token。
// boundAgentVersion 是 hub 绑定的 agent 版本：命令装上的是它，不是 hub 自己的版本；正式 hub 的绑定
// 版本由发版判定保证存在，空或未知只是兜底显示。
export function InstallCommands({ hubVersion, boundAgentVersion, origin, registerKey, reRegister = false, banner }: { hubVersion: string; boundAgentVersion: string; origin: string; registerKey: string; reRegister?: boolean; banner?: ReactNode }) {
  const [portText, setPortText] = useState(loadProxyPort);
  const [hubText, setHubText] = useState(loadInstallHub);
  const [updateSource, setUpdateSource] = useState(loadUpdateSource);
  const [ghProxyText, setGhProxyText] = useState(loadGhProxy);
  const port = parseProxyPort(portText);
  const override = parseInstallHub(hubText);
  const ghProxy = parseGhProxy(ghProxyText);
  const command = override === null || ghProxy === null
    ? null
    : installCommand({ hubVersion, boundAgentVersion, registerKey, reRegister, options: { hub: override || origin, updateSource, ghProxy } });
  const editPort = (text: string) => {
    setPortText(text);
    const parsed = parseProxyPort(text);
    if (parsed !== null) saveProxyPort(parsed);
  };
  const editHub = (text: string) => {
    setHubText(text);
    const parsed = parseInstallHub(text);
    if (parsed === "") clearInstallHub();
    else if (parsed !== null) saveInstallHub(parsed);
  };
  const editUpdateSource = (source: UpdateSource) => {
    setUpdateSource(source);
    saveUpdateSource(source);
  };
  const editGhProxy = (text: string) => {
    setGhProxyText(text);
    const parsed = parseGhProxy(text);
    if (parsed === "") clearGhProxy();
    else if (parsed !== null) saveGhProxy(parsed);
  };
  return (
    <>
      {banner}
      <p className="muted">以下安装参数均为选填，只保存在这台浏览器，hub 不记录。</p>
      <label className="inline">本地反代端口<input aria-label="本地反代端口" inputMode="numeric" placeholder={String(defaultProxyPort)} value={portText} onChange={(e) => editPort(e.target.value)} /></label>
      <p className="muted">默认 {defaultProxyPort}；只用于下面的 SSH 反代参数，不进安装命令。</p>
      {port === null && <p role="alert" className="error">本地反代端口须为 1–65535 的整数</p>}
      <label className="inline">Hub 连接地址<input aria-label="Hub 连接地址" type="url" placeholder={origin} value={hubText} onChange={(e) => editHub(e.target.value)} /></label>
      <p className="muted">留空使用当前站点地址；agent 要经备用入口连 hub 时（如出站 443 被封）填写，形如 https://主机:端口，不带路径。</p>
      {override === null && <p role="alert" className="error">Hub 连接地址须为 http:// 或 https:// 加主机，可带端口，不带路径、查询或用户信息</p>}
      <label className="inline"><input type="checkbox" checked={updateSource === "hub"} onChange={(e) => editUpdateSource(e.target.checked ? "hub" : "github")} />在线更新经 hub 中转（--update-source hub）</label>
      <p className="muted">默认不启用，节点在线更新直连 GitHub；启用后经 hub 中转取官方签名产物。</p>
      <label className="inline">GitHub 代理地址<input aria-label="GitHub 代理地址" type="url" placeholder="直连 GitHub" value={ghProxyText} onChange={(e) => editGhProxy(e.target.value)} /></label>
      <p className="muted">留空直连 GitHub；主机连不上 GitHub 时填写，安装脚本与安装包都经它下载，形如 https://代理主机[:端口][/路径前缀]。</p>
      {ghProxy === null && <p role="alert" className="error">GitHub 代理地址须为 http:// 或 https:// 加主机，可带端口与路径前缀（字母、数字与 . _ ~ -），不带查询、片段或用户信息</p>}
      {command !== null && <>
        <div className="install-command"><strong>curl</strong><CopyableText label="curl 安装命令" copyLabel="复制 curl 命令" value={command.curl} /></div>
        <div className="install-command"><strong>wget</strong><CopyableText label="wget 安装命令" copyLabel="复制 wget 命令" value={command.wget} /></div>
        {updateSource === "hub" && <p className="muted">命令带 --update-source hub：在线更新经 hub 中转取官方签名产物，不直连 GitHub。OpenRC 主机（如 Alpine）不支持在线更新，安装脚本会拒绝这个参数，请删掉它再执行。</p>}
        {ghProxy !== "" && <p className="muted">安装脚本与它内嵌的校验清单都经 GitHub 代理取得，代理能同时换掉两者，这次安装等于完全信任该代理；不用代理时，脚本内嵌的清单仍挡得住被换掉的安装包。</p>}
        {command.packagesDirect && <p className="muted">hub 或它绑定的 agent 不是正式版本（开发版 hub），无法确定脚本内嵌的版本，命令不带 --base-url：安装包仍直连 GitHub 下载。</p>}
        {port !== null && <div className="install-command">
          <strong>SSH 反代（可选）</strong>
          <p>主机连不上 GitHub 又不用 GitHub 代理时，可借用开着代理的电脑：以 root 登录目标主机，<code>ssh root@主机地址</code> 后面接上这段参数，再在登录后的 shell 里执行上面的安装命令。</p>
          <CopyableText label="SSH 反代参数" copyLabel="复制 SSH 反代参数" value={sshProxyArgs(port)} />
          <p className="muted">参数把本地反代端口反向转发到目标主机的同一端口，并在登录后的 shell 里设好 http_proxy、https_proxy 与 all_proxy；该端口要同时接受 HTTP 与 SOCKS5（如 Clash 的混合端口）。sudo 可能丢掉这些变量，请以 root 登录或用 sudo -E。代理只用于这次安装，装好后 agent 直接连 hub。</p>
        </div>}
      </>}
      {isRelease(hubVersion)
        ? <p className="muted">脚本取自 hub {hubVersion} 的 release，安装 hub 绑定的 agent {boundAgentVersion || "（未知）"}。</p>
        : <p className="muted">hub 不是正式版本（{hubVersion || "未知"}），脚本取自最新 release，将安装最新 release。</p>}
      {command?.insecure && <p className="muted">hub 地址是 http 且不是 loopback IP，命令带 --insecure-http：节点 token 与指标将明文传输。</p>}
      <p className="muted">安装命令的可信来源是 README 与 GitHub Release：这里的命令由 hub 提供，hub 失守时不可信。</p>
    </>
  );
}
