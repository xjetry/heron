import { loadGhProxy } from "./installGhProxy";
import { loadInstallHub } from "./installHub";
import { REPO_URL } from "./repo";
import { readChoice, writeChoice } from "./storedChoice";
import { needsInsecureHTTP } from "./transport";
import { isRelease } from "./version";

// 面板里每一条安装命令都由 installCommand 拼出：节点凭据抽屉、注册窗口里显示的命令与节点列表"复制安装命令"写进剪贴板的
// 命令同一个函数，四个安装参数（Hub 连接地址、在线更新来源、GitHub 代理；本地反代端口只进 SSH 参数，见 lib/installProxy.ts）
// 在这里只有一种拼法。油猴脚本（assets/heron-quick-node.user.js）独立分发、不能引用这里，自带一份，由它的测试对照。

// 在线更新来源对应 install.sh 的 --update-source：github 是安装脚本的缺省，命令里不写；hub 时带 --update-source hub。
// 按浏览器记住，hub 不记录：节点实际从哪取更新由更新器上报，再存一份会出现两个可能不一致的来源。
export type UpdateSource = "github" | "hub";
const updateSourceKey = "heron-install-update-source";
export const loadUpdateSource = (): UpdateSource => readChoice<UpdateSource>(updateSourceKey, ["github", "hub"], "github");
export const saveUpdateSource = (source: UpdateSource) => writeChoice(updateSourceKey, source);

// hub 是 agent 访问 hub 的生效地址（已校验的覆盖地址或当前站点 origin），ghProxy 是 parseGhProxy 规范化后的代理地址，
// 空串表示直连 GitHub。两者都由各自的 parse 保证只含能未加引号进 shell 命令的字符，这里不再校验。
export type InstallOptions = { hub: string; updateSource: UpdateSource; ghProxy: string };

// 这台浏览器记住的安装参数；记住的值不合法时各自的 load 已退回缺省（覆盖地址为空即用 origin）。
export function savedInstallOptions(origin: string): InstallOptions {
  return { hub: loadInstallHub() || origin, updateSource: loadUpdateSource(), ghProxy: loadGhProxy() };
}

const releaseURL = (ghProxy: string, path: string) => `${ghProxy === "" ? "" : `${ghProxy}/`}${REPO_URL}/releases/${path}`;

export type InstallCommand = {
  curl: string;
  wget: string;
  // hub 地址是 http 且不是 loopback IP：命令带 --insecure-http（判定见 lib/transport.ts）。
  insecure: boolean;
  // 设了 GitHub 代理但命令没有 --base-url：安装脚本经代理取，包仍从脚本缺省的 GitHub 地址直连下载。
  packagesDirect: boolean;
};

// 安装脚本只装自己所属的版本，版本由取哪个 URL 的脚本决定；每个 release 里的 agent 脚本装的就是这个 hub 版本
// 绑定的 agent——只发 hub 的 release 把绑定版本的脚本原样带进来，完整 release 本来就是同一版本。hub 为正式
// 版本（带 v 前缀的合法 semver，与节点落后判定同一个解析）时取同版本 release 的脚本；否则只能取最新 release 的脚本，装最新
// hub 绑定的 agent。
//
// 设了 GitHub 代理时脚本与包都经代理：脚本地址加代理前缀；包下载目录用 --base-url 指到代理后的绑定版本 release。--base-url
// 必须与脚本内嵌的 RELEASE_VERSION 同一版本，否则脚本按内嵌清单校验时拒绝那些包。只有 hub 与绑定版本都是正式版本时两者才
// 确定一致（正式 hub 的 release 里的脚本就是绑定版本的脚本）；hub 不是正式版本时脚本来自 latest，内嵌版本未必是绑定版本，
// 不给 --base-url，包直连 GitHub。
export function installCommand({ hubVersion, boundAgentVersion, registerKey, reRegister, options }: {
  hubVersion: string; boundAgentVersion: string; registerKey: string; reRegister: boolean; options: InstallOptions;
}): InstallCommand {
  const { hub, updateSource, ghProxy } = options;
  const script = releaseURL(ghProxy, isRelease(hubVersion) ? `download/${hubVersion}/install.sh` : "latest/download/install.sh");
  const baseURL = ghProxy !== "" && isRelease(hubVersion) && isRelease(boundAgentVersion) ? releaseURL(ghProxy, `download/${boundAgentVersion}`) : "";
  const insecure = needsInsecureHTTP(hub);
  const args = `--hub ${hub} --key ${registerKey}${insecure ? " --insecure-http" : ""}${reRegister ? " --re-register" : ""}`
    + `${updateSource === "hub" ? " --update-source hub" : ""}${baseURL === "" ? "" : ` --base-url ${baseURL}`}`;
  return {
    curl: `curl -fsSL ${script} | sh -s -- ${args}`,
    wget: `wget -qO- ${script} | sh -s -- ${args}`,
    insecure,
    packagesDirect: ghProxy !== "" && baseURL === "",
  };
}
