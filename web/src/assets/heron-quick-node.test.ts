import { afterEach, describe, expect, it, vi } from "vitest";
import { buildUserscript } from "../components/UserscriptButton";
import { defaultProxyPort, sshProxyArgs } from "../lib/installProxy";

// 在 jsdom 里真实运行油猴脚本：GM 接口与 hub 应答用桩，走一遍"添加节点 → 结果面板"，在结果面板上操作。
type Request = { url: string; data: string; onload: (res: { status: number; responseText: string }) => void };

// snapshot 是 GetSnapshot 的应答；缺省是完整 release 的 hub（绑定的 agent 版本与 hub 同号）。
type Snapshot = { hubVersion: string; boundAgentVersion: string };
const fullRelease: Snapshot = { hubVersion: "v1.2.3", boundAgentVersion: "v1.2.3" };

function hub(snapshot: Snapshot) {
  return (request: Request) => {
    const method = request.url.split("/").pop();
    const body = JSON.parse(request.data) as { preview?: boolean };
    const reply = method === "GetSnapshot" ? snapshot
      : body.preview ? { expectedVersion: "7" }
      : { result: { token: "node-tok", node: { id: "5", name: "vps" } } };
    setTimeout(() => request.onload({ status: 200, responseText: JSON.stringify(reply) }), 0);
  };
}

async function addNode(store: Map<string, string>, snapshot: Snapshot = fullRelease) {
  vi.stubGlobal("GM_getValue", (key: string, fallback: string) => store.get(key) ?? fallback);
  vi.stubGlobal("GM_setValue", (key: string, value: string) => { store.set(key, value); });
  vi.stubGlobal("GM_registerMenuCommand", () => {});
  vi.stubGlobal("GM_xmlhttpRequest", hub(snapshot));
  document.title = "vps";
  // 被测对象就是分发出去的脚本源码本身：像脚本管理器一样把它当一段代码执行，不是把数据当代码。
  // eslint-disable-next-line @typescript-eslint/no-implied-eval
  const run = new Function(buildUserscript("https://hub.example:28080", "api-token")) as () => void;
  run();
  const root = (document.documentElement.lastElementChild as HTMLElement).shadowRoot!;
  await new Promise((resolve) => setTimeout(resolve, 0));
  (root.querySelector(".fab") as HTMLButtonElement).click();
  await vi.waitFor(() => expect(root.querySelector(".panel .primary")).not.toBeNull());
  (root.querySelector(".panel .primary") as HTMLButtonElement).click();
  await vi.waitFor(() => expect(root.querySelector('[aria-label="安装命令"]')).not.toBeNull());
  return root;
}

const field = (root: ShadowRoot, label: string) => root.querySelector<HTMLInputElement>(`[aria-label="${label}"]`);
const setPort = (root: ShadowRoot, value: string) => {
  const input = field(root, "本机代理端口")!;
  input.value = value;
  input.dispatchEvent(new Event("input"));
};
const script = "https://github.com/xjetry/heron/releases/download/v1.2.3/install.sh";

afterEach(() => {
  document.documentElement.querySelectorAll(":scope > div").forEach((el) => el.remove());
  vi.unstubAllGlobals();
});

describe("油猴脚本 国内主机", () => {
  it("默认关闭；打开后安装命令带 --update-source hub，SSH 反代参数与面板同一拼法，复制拿到的是新命令", async () => {
    const root = await addNode(new Map());
    const command = `curl -fsSL ${script} | sh -s -- --hub https://hub.example:28080 --key node-tok`;
    expect(field(root, "国内主机")!.checked).toBe(false);
    expect(field(root, "安装命令")!.value).toBe(command);
    expect(field(root, "SSH 反代参数")).toBeNull();

    field(root, "国内主机")!.click();
    expect(field(root, "安装命令")!.value).toBe(`${command} --update-source hub`);
    expect(root.querySelector("pre")!.textContent).toBe(`${command} --update-source hub`);
    expect(field(root, "SSH 反代参数")!.value).toBe(sshProxyArgs(defaultProxyPort));

    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    (field(root, "安装命令")!.nextElementSibling as HTMLButtonElement).click();
    expect(writeText).toHaveBeenCalledWith(`${command} --update-source hub`);
  });

  // 油猴脚本自带一份默认端口：没记住过端口时输入框里就是面板的默认值，两边不各自漂移。
  it("没记住端口时默认端口与面板相同", async () => {
    const root = await addNode(new Map());
    expect(field(root, "本机代理端口")!.value).toBe(String(defaultProxyPort));
  });

  it("改端口后 SSH 参数跟着改并记住端口；不合法的端口不出参数、不覆盖记住的值", async () => {
    const store = new Map<string, string>();
    const root = await addNode(store);
    field(root, "国内主机")!.click();
    setPort(root, "7897");
    expect(field(root, "SSH 反代参数")!.value).toBe(sshProxyArgs(7897));
    expect(store.get("proxy_port")).toBe("7897");
    for (const value of ["0", "65536", "07890", "78a", "", "7890; rm -rf /"]) {
      setPort(root, value);
      expect(field(root, "SSH 反代参数"), value).toBeNull();
      expect(root.querySelector(".proxy-error")!.textContent).toContain("1–65535");
      expect(store.get("proxy_port")).toBe("7897");
    }
  });

  it("下次添加沿用记住的端口", async () => {
    const root = await addNode(new Map([["proxy_port", "7897"]]));
    field(root, "国内主机")!.click();
    expect(field(root, "本机代理端口")!.value).toBe("7897");
    expect(field(root, "SSH 反代参数")!.value).toBe(sshProxyArgs(7897));
  });
});

describe("油猴脚本 安装说明", () => {
  // 只发 hub 的 release：hub v1.2.6 绑定 agent v1.2.4。脚本仍取 hub 版本的 release（它原样带着 v1.2.4 的安装脚本），
  // 说明必须写明装上的是绑定的 agent，不能说"与 hub 同版本"。
  it("正式 hub 的命令取 hub 版本的脚本，说明写明装的是 hub 绑定的 agent 版本", async () => {
    const root = await addNode(new Map(), { hubVersion: "v1.2.6", boundAgentVersion: "v1.2.4" });
    expect(field(root, "安装命令")!.value).toBe("curl -fsSL https://github.com/xjetry/heron/releases/download/v1.2.6/install.sh | sh -s -- --hub https://hub.example:28080 --key node-tok");
    const note = [...root.querySelectorAll(".panel p.note")].map((p) => p.textContent);
    expect(note).toContain("在新节点上运行（脚本取自 hub v1.2.6 的 release，安装 hub 绑定的 agent v1.2.4）：");
    expect(root.textContent).not.toContain("与 hub 同版本");
  });
});
