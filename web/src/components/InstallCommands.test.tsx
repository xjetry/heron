import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { InstallCommands } from "./InstallCommands";

afterEach(() => localStorage.clear());

const release = "https://github.com/xjetry/heron/releases/download/v1.2.3";
const script = `${release}/install.sh`;
const curl = () => screen.getByLabelText("curl 安装命令").textContent;
const wget = () => screen.getByLabelText("wget 安装命令").textContent;
const sshArgs = () => screen.queryByLabelText("SSH 反代参数")?.textContent;
const portInput = () => screen.getByRole("textbox", { name: "本地反代端口" });
const hubInput = () => screen.getByRole("textbox", { name: "Hub 连接地址" });
const proxyInput = () => screen.getByRole("textbox", { name: "GitHub 代理地址" });
const viaHub = () => screen.getByRole("checkbox", { name: /在线更新经 hub 中转/ });
const editHub = (value: string) => fireEvent.change(hubInput(), { target: { value } });
const editProxy = (value: string) => fireEvent.change(proxyInput(), { target: { value } });
const renderCommands = (props: Partial<{ hubVersion: string; boundAgentVersion: string; origin: string; reRegister: boolean }> = {}) =>
  render(<InstallCommands hubVersion="v1.2.3" boundAgentVersion="v1.2.3" origin="https://hub.example:28080" registerKey="k1" {...props} />);
// 两条命令只差下载器，参数部分相同。
const commands = (rest: string, scriptURL = script) => [`curl -fsSL ${scriptURL} | sh -s -- ${rest}`, `wget -qO- ${scriptURL} | sh -s -- ${rest}`];
const ssh = (port: number) => `-t -R 127.0.0.1:${port}:127.0.0.1:${port} 'export http_proxy=http://127.0.0.1:${port}; export https_proxy=http://127.0.0.1:${port}; export all_proxy=socks5h://127.0.0.1:${port}; exec $SHELL -l'`;

describe("InstallCommands 缺省", () => {
  it("四项都没填：命令只有 --hub 与 --key、脚本直连 GitHub，SSH 反代参数按默认端口 7890 给出，没有国内主机开关", () => {
    renderCommands();
    expect([curl(), wget()]).toEqual(commands("--hub https://hub.example:28080 --key k1"));
    expect(portInput()).toHaveValue("7890");
    expect(sshArgs()).toBe(ssh(7890));
    expect(viaHub()).not.toBeChecked();
    expect(proxyInput()).toHaveValue("");
    expect(hubInput()).toHaveValue("");
    expect(screen.queryByText(/国内主机/)).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("InstallCommands 本地反代端口", () => {
  it("改端口只改 SSH 反代参数，不进安装命令；端口记在浏览器里，重新打开沿用", () => {
    const { unmount } = renderCommands();
    fireEvent.change(portInput(), { target: { value: "7897" } });
    expect(sshArgs()).toBe(ssh(7897));
    expect([curl(), wget()]).toEqual(commands("--hub https://hub.example:28080 --key k1"));
    expect(localStorage.getItem("heron-install-proxy-port")).toBe("7897");
    unmount();
    renderCommands();
    expect(portInput()).toHaveValue("7897");
    expect(sshArgs()).toBe(ssh(7897));
  });

  // 端口拼进可复制的 shell 命令：不合法的输入一律不出 SSH 参数，也不覆盖已记住的端口；安装命令不受影响。
  it.each(["0", "65536", "07890", "78a", "", "7890; rm -rf /", " 7890"])("端口 %j 不合法时不给 SSH 参数", (value) => {
    localStorage.setItem("heron-install-proxy-port", "7897");
    renderCommands();
    fireEvent.change(portInput(), { target: { value } });
    expect(sshArgs()).toBeUndefined();
    expect(screen.getByRole("alert")).toHaveTextContent("1–65535");
    expect(localStorage.getItem("heron-install-proxy-port")).toBe("7897");
    expect([curl(), wget()]).toEqual(commands("--hub https://hub.example:28080 --key k1"));
  });

  it("记住的端口不合法时回到默认 7890", () => {
    localStorage.setItem("heron-install-proxy-port", "not-a-port");
    renderCommands();
    expect(portInput()).toHaveValue("7890");
    expect(sshArgs()).toBe(ssh(7890));
  });
});

describe("InstallCommands Hub 连接地址", () => {
  it("默认留空：占位是当前域名，命令用传入的 origin", () => {
    renderCommands();
    expect(hubInput()).toHaveAttribute("placeholder", "https://hub.example:28080");
    expect([curl(), wget()]).toEqual(commands("--hub https://hub.example:28080 --key k1"));
  });

  it("填了覆盖地址后两条命令的 --hub 都用它的规范写法，并立即记在浏览器里；重新打开沿用", () => {
    const { unmount } = renderCommands();
    editHub("HTTPS://Heron-Panel.O1.pw:28080/");
    expect([curl(), wget()]).toEqual(commands("--hub https://heron-panel.o1.pw:28080 --key k1"));
    expect(localStorage.getItem("heron-install-hub")).toBe("https://heron-panel.o1.pw:28080");
    unmount();
    renderCommands();
    expect(hubInput()).toHaveValue("https://heron-panel.o1.pw:28080");
    expect([curl(), wget()]).toEqual(commands("--hub https://heron-panel.o1.pw:28080 --key k1"));
  });

  it("清空即删除记住的地址，命令回到当前域名", () => {
    localStorage.setItem("heron-install-hub", "https://heron-panel.o1.pw:28080");
    renderCommands();
    expect([curl(), wget()]).toEqual(commands("--hub https://heron-panel.o1.pw:28080 --key k1"));
    editHub("  ");
    expect(localStorage.getItem("heron-install-hub")).toBeNull();
    expect([curl(), wget()]).toEqual(commands("--hub https://hub.example:28080 --key k1"));
  });

  // 覆盖地址未加引号地进 shell 命令：不合法时一律不出命令（含 SSH 反代参数），也不覆盖已记住的地址。
  it.each(["https://hub.example/admin", "hub.example:28080", "ftp://hub.example", "https://a$(reboot).example", "https://user@hub.example"])("覆盖地址 %j 不合法时不出任何命令", (value) => {
    localStorage.setItem("heron-install-hub", "https://heron-panel.o1.pw:28080");
    renderCommands();
    editHub(value);
    expect(screen.getByRole("alert")).toHaveTextContent("Hub 连接地址须为");
    expect(screen.queryByLabelText("curl 安装命令")).toBeNull();
    expect(screen.queryByLabelText("wget 安装命令")).toBeNull();
    expect(sshArgs()).toBeUndefined();
    expect(document.body.textContent).not.toContain("sh -s --");
    expect(localStorage.getItem("heron-install-hub")).toBe("https://heron-panel.o1.pw:28080");
  });

  it("记住的值不合法时按空处理", () => {
    localStorage.setItem("heron-install-hub", "https://hub.example/admin");
    renderCommands();
    expect(hubInput()).toHaveValue("");
    expect([curl(), wget()]).toEqual(commands("--hub https://hub.example:28080 --key k1"));
  });

  // --insecure-http 与明文提示跟 --hub 读同一个生效地址，两个方向都钉住。
  it("http 覆盖地址带 --insecure-http 与明文提示，即使当前域名是 https", () => {
    renderCommands();
    editHub("http://heron-panel.o1.pw:28080");
    expect([curl(), wget()]).toEqual(commands("--hub http://heron-panel.o1.pw:28080 --key k1 --insecure-http"));
    expect(screen.getByText(/token 与指标将明文传输/)).toBeInTheDocument();
  });

  it("https 覆盖地址不带 --insecure-http，即使当前域名是 http", () => {
    renderCommands({ origin: "http://hub.example:8080" });
    expect(curl()).toContain("--insecure-http");
    editHub("https://heron-panel.o1.pw:28080");
    expect([curl(), wget()]).toEqual(commands("--hub https://heron-panel.o1.pw:28080 --key k1"));
    expect(screen.queryByText(/token 与指标将明文传输/)).toBeNull();
  });
});

describe("InstallCommands 在线更新经 hub 中转", () => {
  it("默认不启用；启用后两条命令都带 --update-source hub 并给出 OpenRC 提示，选择记在浏览器里，关掉后回到不带", () => {
    const { unmount } = renderCommands();
    expect(screen.queryByText(/OpenRC/)).toBeNull();
    fireEvent.click(viaHub());
    expect([curl(), wget()]).toEqual(commands("--hub https://hub.example:28080 --key k1 --update-source hub"));
    expect(screen.getByText(/OpenRC 主机（如 Alpine）不支持在线更新/)).toBeInTheDocument();
    expect(localStorage.getItem("heron-install-update-source")).toBe("hub");
    unmount();
    renderCommands();
    expect(viaHub()).toBeChecked();
    expect(curl()).toContain("--update-source hub");
    fireEvent.click(viaHub());
    expect([curl(), wget()]).toEqual(commands("--hub https://hub.example:28080 --key k1"));
    expect(localStorage.getItem("heron-install-update-source")).toBe("github");
  });

  it("记住的值不认识时按不启用处理", () => {
    localStorage.setItem("heron-install-update-source", "hub; reboot");
    renderCommands();
    expect(viaHub()).not.toBeChecked();
    expect(curl()).not.toContain("--update-source");
  });
});

describe("InstallCommands GitHub 代理地址", () => {
  const proxied = (proxy: string, version = "v1.2.3") => `${proxy}/https://github.com/xjetry/heron/releases/download/${version}`;

  it("填了代理后脚本地址与 --base-url 都经代理，代理去掉结尾斜杠，记在浏览器里，并写明信任边界", () => {
    const { unmount } = renderCommands();
    expect(screen.queryByText(/完全信任该代理/)).toBeNull();
    editProxy("https://ghfast.top/");
    expect([curl(), wget()]).toEqual(commands(`--hub https://hub.example:28080 --key k1 --base-url ${proxied("https://ghfast.top")}`, `${proxied("https://ghfast.top")}/install.sh`));
    expect(localStorage.getItem("heron-install-gh-proxy")).toBe("https://ghfast.top");
    expect(screen.getByText(/代理能同时换掉两者，这次安装等于完全信任该代理/)).toBeInTheDocument();
    unmount();
    renderCommands();
    expect(proxyInput()).toHaveValue("https://ghfast.top");
    expect(curl()).toBe(commands(`--hub https://hub.example:28080 --key k1 --base-url ${proxied("https://ghfast.top")}`, `${proxied("https://ghfast.top")}/install.sh`)[0]);
  });

  it("代理可带端口与路径前缀，规范化后原样拼在原始地址前", () => {
    renderCommands();
    editProxy("HTTPS://Proxy.Example:8443/gh/raw/");
    expect(curl()).toBe(commands(`--hub https://hub.example:28080 --key k1 --base-url ${proxied("https://proxy.example:8443/gh/raw")}`, `${proxied("https://proxy.example:8443/gh/raw")}/install.sh`)[0]);
  });

  // 只发 hub 的 release：hub v1.2.6 绑定 agent v1.2.4。脚本取 hub 版本的 release（它内嵌的就是 v1.2.4），包取绑定版本的 release。
  it("脚本按 hub 版本取，--base-url 按绑定的 agent 版本取", () => {
    renderCommands({ hubVersion: "v1.2.6", boundAgentVersion: "v1.2.4" });
    editProxy("https://ghfast.top");
    expect(curl()).toBe(`curl -fsSL ${proxied("https://ghfast.top", "v1.2.6")}/install.sh | sh -s -- --hub https://hub.example:28080 --key k1 --base-url ${proxied("https://ghfast.top", "v1.2.4")}`);
  });

  // 开发版 hub 的脚本来自 latest，内嵌版本未必是绑定版本：脚本照样经代理，但不给 --base-url，并说明包直连 GitHub。
  it.each([
    ["dev", "v1.2.3"],
    ["v1.2.3", ""],
    ["v1.2.3", "dev"],
  ])("hub %j、绑定版本 %j 时不出 --base-url，说明安装包直连 GitHub", (hubVersion, boundAgentVersion) => {
    renderCommands({ hubVersion, boundAgentVersion });
    expect(screen.queryByText(/命令不带 --base-url/)).toBeNull();
    editProxy("https://ghfast.top");
    expect(curl()).not.toContain("--base-url");
    expect(curl()).toMatch(/^curl -fsSL https:\/\/ghfast\.top\/https:\/\/github\.com\/xjetry\/heron\/releases\/(latest\/download|download\/v1\.2\.3)\/install\.sh \| sh -s -- --hub https:\/\/hub\.example:28080 --key k1$/);
    expect(screen.getByText(/命令不带 --base-url：安装包仍直连 GitHub 下载/)).toBeInTheDocument();
  });

  it("开发版 hub 的脚本地址经代理指向 latest", () => {
    renderCommands({ hubVersion: "dev" });
    editProxy("https://ghfast.top");
    expect(curl()).toBe("curl -fsSL https://ghfast.top/https://github.com/xjetry/heron/releases/latest/download/install.sh | sh -s -- --hub https://hub.example:28080 --key k1");
  });

  it("清空即删除记住的代理，命令回到直连", () => {
    localStorage.setItem("heron-install-gh-proxy", "https://ghfast.top");
    renderCommands();
    expect(curl()).toContain("--base-url");
    editProxy(" ");
    expect(localStorage.getItem("heron-install-gh-proxy")).toBeNull();
    expect([curl(), wget()]).toEqual(commands("--hub https://hub.example:28080 --key k1"));
    expect(screen.queryByText(/完全信任该代理/)).toBeNull();
  });

  // 代理地址未加引号地进 shell 命令：不合法时一律不出命令（含 SSH 反代参数），也不覆盖已记住的代理。
  it.each(["ghfast.top", "ftp://ghfast.top", "https://ghfast.top?x=1", "https://ghfast.top/a#b", "https://user@ghfast.top", "https://ghfast.top/$(reboot)", "https://ghfast.top/a;b", "https://ghfast.top/../x", "https://ghfast.top//x"])("代理 %j 不合法时不出任何命令", (value) => {
    localStorage.setItem("heron-install-gh-proxy", "https://ghfast.top");
    renderCommands();
    editProxy(value);
    expect(screen.getByRole("alert")).toHaveTextContent("GitHub 代理地址须为");
    expect(screen.queryByLabelText("curl 安装命令")).toBeNull();
    expect(screen.queryByLabelText("wget 安装命令")).toBeNull();
    expect(sshArgs()).toBeUndefined();
    expect(document.body.textContent).not.toContain("sh -s --");
    expect(localStorage.getItem("heron-install-gh-proxy")).toBe("https://ghfast.top");
  });

  it("记住的值不合法时按直连处理", () => {
    localStorage.setItem("heron-install-gh-proxy", "https://ghfast.top/?x");
    renderCommands();
    expect(proxyInput()).toHaveValue("");
    expect([curl(), wget()]).toEqual(commands("--hub https://hub.example:28080 --key k1"));
  });
});

describe("InstallCommands 四项同时生效", () => {
  it("换 token 的重新注册命令：参数顺序固定，四项各就各位", () => {
    renderCommands({ reRegister: true });
    editHub("http://heron-panel.o1.pw:28080");
    fireEvent.click(viaHub());
    editProxy("https://ghfast.top");
    fireEvent.change(portInput(), { target: { value: "7897" } });
    expect([curl(), wget()]).toEqual(commands(
      `--hub http://heron-panel.o1.pw:28080 --key k1 --insecure-http --re-register --update-source hub --base-url ${release.replace("https://", "https://ghfast.top/https://")}`,
      `https://ghfast.top/${script}`,
    ));
    expect(sshArgs()).toBe(ssh(7897));
  });
});
