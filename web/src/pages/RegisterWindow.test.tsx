import { Code, ConnectError } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { renderWithAdmin } from "../test/harness";
import { InstallCommands } from "../components/InstallCommands";
import { RegisterWindow } from "./RegisterWindow";
import { AdminService, GetSnapshotResponseSchema, ListNodesResponseSchema } from "../gen/heron/v1/admin_pb";

afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); window.getSelection()?.removeAllRanges(); });

const snapshotOf = (hubVersion: string, boundAgentVersion = "") => async () => ({ now: 1n, reportIntervalMs: 4000, nodes: [], hubVersion, boundAgentVersion });
const renderOpen = (hubVersion: string, boundAgentVersion = "") => {
  let opened = false;
  return renderWithAdmin({
    getRegisterWindow: async () => ({ open: opened, expiresAt: 4_000_000_000n, remaining: 3 }),
    openRegisterWindow: async () => { opened = true; return { key: "k1", expiresAt: 4_000_000_000n, maxNodes: 3 }; },
    getSnapshot: snapshotOf(hubVersion, boundAgentVersion),
  }, [{ path: "/register", Component: RegisterWindow }], "/register");
};

async function openDrawer() {
  fireEvent.click(await screen.findByRole("button", { name: "开启接入窗口" }));
  return screen.getByRole("dialog", { name: "开启接入窗口" });
}

async function openWindow() {
  fireEvent.click(within(await openDrawer()).getByRole("button", { name: "开启" }));
}

describe("RegisterWindow", () => {
  it.each(["curl", "wget"])("一键复制完整的 %s 安装命令并独立反馈", async (tool) => {
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    render(<InstallCommands hubVersion="v1.2.3" boundAgentVersion="v1.2.3" origin="http://hub.example:8080" registerKey="copy-test-key" />);
    fireEvent.click(screen.getByRole("button", { name: `复制 ${tool} 命令` }));
    const prefix = tool === "curl" ? "curl -fsSL" : "wget -qO-";
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(`${prefix} https://github.com/xjetry/heron/releases/download/v1.2.3/install.sh | sh -s -- --hub http://hub.example:8080 --key copy-test-key --insecure-http`));
    await waitFor(() => expect(screen.getByRole("button", { name: `复制 ${tool} 命令` })).toHaveTextContent("已复制"));
    expect(screen.getByRole("button", { name: `复制 ${tool === "curl" ? "wget" : "curl"} 命令` })).toHaveTextContent("复制");
  });

  it.each(["missing", "rejected"])("剪贴板 %s 时提示并选中完整命令", async (failure) => {
    vi.stubGlobal("navigator", failure === "missing" ? {} : { clipboard: { writeText: async () => { throw new Error("denied"); } } });
    render(<InstallCommands hubVersion="v1.2.3" boundAgentVersion="v1.2.3" origin="https://hub.example" registerKey="copy-test-key" />);
    fireEvent.click(screen.getByRole("button", { name: "复制 wget 命令" }));
    expect(await screen.findByText("复制失败，请手动选择")).toBeInTheDocument();
    expect(window.getSelection()?.toString()).toBe("wget -qO- https://github.com/xjetry/heron/releases/download/v1.2.3/install.sh | sh -s -- --hub https://hub.example --key copy-test-key");
  });

  it.each(["copied", "failed"])("命令变化后忽略旧复制请求迟到的 %s 结果", async (result) => {
    let finish!: () => void;
    const pending = new Promise<void>((resolve, reject) => { finish = () => result === "copied" ? resolve() : reject(new Error("denied")); });
    vi.stubGlobal("navigator", { clipboard: { writeText: () => pending } });
    const { rerender } = render(<InstallCommands hubVersion="v1.2.3" boundAgentVersion="v1.2.3" origin="https://hub.example" registerKey="old-key" />);
    fireEvent.click(screen.getByRole("button", { name: "复制 curl 命令" }));
    rerender(<InstallCommands hubVersion="v1.2.4" boundAgentVersion="v1.2.4" origin="https://hub.example" registerKey="new-key" />);
    await act(async () => finish());
    expect(screen.getByRole("button", { name: "复制 curl 命令" })).toHaveTextContent(/^复制$/);
    expect(screen.queryByText("复制失败，请手动选择")).not.toBeInTheDocument();
    expect(window.getSelection()?.toString()).toBe("");
  });
  it.each(["open", "close"])("%s 只失效注册窗口，不标脏其他查询", async (operation) => {
    let opened = operation === "close";
    const getRegisterWindow = vi.fn(async () => ({ open: opened, expiresAt: 4_000_000_000n, remaining: 3 }));
    const { queryClient } = renderWithAdmin({
      getRegisterWindow,
      openRegisterWindow: async () => { opened = true; return { key: "key", expiresAt: 4_000_000_000n, maxNodes: 3 }; },
      closeRegisterWindow: async () => { opened = false; return {}; },
    }, [{ path: "/register", Component: RegisterWindow }], "/register");
    const snapshotKey = createConnectQueryKey({ schema: AdminService.method.getSnapshot, cardinality: "finite" });
    const nodesKey = createConnectQueryKey({ schema: AdminService.method.listNodes, cardinality: "finite" });
    queryClient.setQueryData(snapshotKey, create(GetSnapshotResponseSchema));
    queryClient.setQueryData(nodesKey, create(ListNodesResponseSchema));
    await waitFor(() => expect(getRegisterWindow).toHaveBeenCalledTimes(1));
    if (operation === "open") await openWindow();
    else fireEvent.click(await screen.findByRole("button", { name: "关闭接入窗口" }));
    await waitFor(() => expect(getRegisterWindow).toHaveBeenCalledTimes(2));
    expect([snapshotKey, nodesKey].map((key) => queryClient.getQueryState(key)?.isInvalidated)).toEqual([false, false]);
  });

  it("清空名额保留空白而不是零值", async () => {
    renderWithAdmin({ getRegisterWindow: async () => ({ open: false }) }, [{ path: "/register", Component: RegisterWindow }], "/register");
    const input = within(await openDrawer()).getByLabelText<HTMLInputElement>("最多接入台数");
    fireEvent.change(input, { target: { value: "" } });
    expect({ value: input.value, nan: Number.isNaN(input.valueAsNumber) }).toEqual({ value: "", nan: true });
  });

  it("状态挂起时显示加载中，不渲染页头主按钮", async () => {
    renderWithAdmin({ getRegisterWindow: () => new Promise(() => {}) }, [{ path: "/register", Component: RegisterWindow }], "/register");
    expect(await screen.findByText("加载中…")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "开启接入窗口" })).toBeNull();
    expect(screen.queryByText("当前没有开启的接入窗口。")).toBeNull();
  });

  it("状态首次失败只显示错误", async () => {
    renderWithAdmin({ getRegisterWindow: async () => { throw new ConnectError("status unavailable", Code.Unavailable); } }, [{ path: "/register", Component: RegisterWindow }], "/register");
    expect(await screen.findByRole("alert")).toHaveTextContent("status unavailable");
    expect(screen.queryByText("当前没有开启的接入窗口。")).toBeNull();
    expect(screen.queryByRole("button", { name: "开启接入窗口" })).toBeNull();
  });

  it.each(["", "0", "-1"])("名额为 '%s' 时禁用开窗", async (value) => {
    renderWithAdmin({ getRegisterWindow: async () => ({ open: false }) }, [{ path: "/register", Component: RegisterWindow }], "/register");
    await screen.findByText("当前没有开启的接入窗口。");
    const drawer = within(await openDrawer());
    fireEvent.change(drawer.getByLabelText("最多接入台数"), { target: { value } });
    expect(drawer.getByRole("button", { name: "开启" })).toBeDisabled();
  });

  it("轮询发现窗口失效时撤下 key 与命令", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let opened = false;
    renderWithAdmin({
      getRegisterWindow: async () => ({ open: opened, expiresAt: 4_000_000_000n, remaining: 3 }),
      openRegisterWindow: async () => { opened = true; return { key: "expires", expiresAt: 4_000_000_000n, maxNodes: 3 }; },
      getSnapshot: snapshotOf("v1.2.3"),
    }, [{ path: "/register", Component: RegisterWindow }], "/register");
    await screen.findByText("当前没有开启的接入窗口。");
    await openWindow();
    await screen.findByRole("button", { name: "关闭接入窗口" });
    await screen.findByLabelText("注册 key");
    expect(await screen.findAllByText(/install\.sh \| sh -s --/)).toHaveLength(2);
    opened = false;
    await act(async () => vi.advanceTimersByTimeAsync(10_000));
    await screen.findByText("当前没有开启的接入窗口。");
    expect(screen.queryByLabelText("注册 key")).not.toBeInTheDocument();
    expect(screen.queryByText(/install\.sh/)).not.toBeInTheDocument();
  });
  it.each(["status", "open"])("%s 失败时展示错误正文", async (source) => {
    const fail = async () => { throw new ConnectError("request failed", Code.Unavailable); };
    renderWithAdmin({
      getRegisterWindow: source === "status" ? fail : async () => ({ open: false, expiresAt: 0n, remaining: 0 }),
      openRegisterWindow: fail,
      getSnapshot: snapshotOf("v1.2.3"),
    }, [{ path: "/register", Component: RegisterWindow }], "/register");
    if (source === "open") await openWindow();
    expect(await screen.findByRole("alert")).toHaveTextContent(/^request failed$/);
  });
  it("开窗后展示 key 与安装命令", async () => {
    let opened = false;
    const openRegisterWindow = vi.fn(async () => { opened = true; return { key: "cafe", expiresAt: 4_000_000_000n, maxNodes: 5 }; });
    const { router } = renderWithAdmin(
      { getRegisterWindow: async () => ({ open: opened, expiresAt: 4_000_000_000n, remaining: 5 }), openRegisterWindow, getSnapshot: snapshotOf("v1.2.3") },
      [{ path: "/register", Component: RegisterWindow }, { path: "/away", element: <h1>away</h1> }], "/register",
    );
    await screen.findByText("当前没有开启的接入窗口。");
    await openWindow();
    expect(await screen.findByLabelText("注册 key")).toHaveTextContent("cafe");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    for (const p of await screen.findAllByText(/install\.sh \| sh -s --/)) expect(p.textContent).toMatch(/--hub \S+ --key cafe/);
    expect(openRegisterWindow).toHaveBeenCalledWith(expect.objectContaining({ ttlS: 3600, maxNodes: 5 }), expect.anything());
    await act(() => router.navigate("/away"));
    await act(() => router.navigate("/register"));
    await screen.findByRole("button", { name: "关闭接入窗口" });
    expect(screen.queryByLabelText("注册 key")).not.toBeInTheDocument();
  });

  it("开启中的窗口显示剩余名额并可关闭", async () => {
    const closeRegisterWindow = vi.fn(async () => ({}));
    renderWithAdmin(
      { getRegisterWindow: async () => ({ open: true, expiresAt: 4_000_000_000n, remaining: 3 }), closeRegisterWindow },
      [{ path: "/register", Component: RegisterWindow }], "/register",
    );
    expect(await screen.findByText(/还能接入 3 台/)).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "窗口状态" })).toHaveClass("card");
    expect(screen.getByRole("button", { name: "开启接入窗口" })).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "关闭接入窗口" }));
    await waitFor(() => expect(closeRegisterWindow).toHaveBeenCalled());
  });

  it("关窗清除刚展示的 key 与命令", async () => {
    let opened = false;
    renderWithAdmin({
      getRegisterWindow: async () => ({ open: opened, expiresAt: 4_000_000_000n, remaining: 3 }),
      openRegisterWindow: async () => { opened = true; return { key: "ephemeral", expiresAt: 4_000_000_000n, maxNodes: 3 }; },
      closeRegisterWindow: async () => { opened = false; return {}; },
      getSnapshot: snapshotOf("v1.2.3"),
    }, [{ path: "/register", Component: RegisterWindow }], "/register");
    await screen.findByText("当前没有开启的接入窗口。");
    await openWindow();
    await screen.findByLabelText("注册 key");
    expect(await screen.findAllByText(/install\.sh \| sh -s --/)).toHaveLength(2);
    fireEvent.click(await screen.findByRole("button", { name: "关闭接入窗口" }));
    await screen.findByText("当前没有开启的接入窗口。");
    expect(screen.queryByLabelText("注册 key")).not.toBeInTheDocument();
    expect(screen.queryByText(/install\.sh/)).not.toBeInTheDocument();
  });
  it("正式版本的 hub 取同版本 release 的脚本，命令不带 --version", async () => {
    renderOpen("v1.2.3");
    await openWindow();
    const pres = await screen.findAllByText(/install\.sh \| sh -s --/);
    expect(pres).toHaveLength(2);
    expect(pres[0].textContent).toContain("curl -fsSL");
    expect(pres[1].textContent).toContain("wget -qO-");
    for (const p of pres) {
      expect(p.textContent).toContain("https://github.com/xjetry/heron/releases/download/v1.2.3/install.sh");
      expect(p.textContent).not.toContain("--version");
      expect(p.textContent).toContain("--key k1");
      expect(p.textContent).not.toContain("--re-register");
    }
    expect(screen.getByText(/以 root 执行（agent 若经其他地址访问 hub/)).toBeInTheDocument();
    expect(screen.queryByText(/将安装最新 release/)).toBeNull();
    expect(screen.getByText(/安装命令的可信来源是 README 与 GitHub Release/)).toBeInTheDocument();
  });

  // jsdom 的 origin 是 http://localhost:3000：localhost 是名字不是 loopback IP 字面量，命令要带 --insecure-http。
  it("面板的 origin 是非 loopback 的 http 时命令带 --insecure-http 并提示明文传输", async () => {
    expect(window.location.origin).toMatch(/^http:\/\/localhost(:\d+)?$/);
    renderOpen("v1.2.3");
    await openWindow();
    for (const p of await screen.findAllByText(/install\.sh \| sh -s --/)) {
      expect(p.textContent).toMatch(new RegExp(`--hub ${window.location.origin} --key k1 --insecure-http$`));
    }
    expect(screen.getByText(/token 与指标将明文传输/)).toBeInTheDocument();
  });

  it.each([
    ["https://hub.example", false],
    ["http://127.0.0.1:8080", false],
    ["http://[::1]:8080", false],
    ["http://10.0.0.1", true],
  ])("origin %s：带 --insecure-http 为 %s", (origin, insecure) => {
    render(<InstallCommands hubVersion="v1.2.3" boundAgentVersion="v1.2.3" origin={origin} registerKey="k1" />);
    const pres = screen.getAllByText(/install\.sh \| sh -s --/);
    expect(pres).toHaveLength(2);
    for (const p of pres) {
      expect(p.textContent).toContain(`--hub ${origin} --key k1`);
      expect(p.textContent?.includes("--insecure-http")).toBe(insecure);
    }
    expect(screen.queryByText(/token 与指标将明文传输/) !== null).toBe(insecure);
    expect(screen.getByText(/安装命令的可信来源是 README 与 GitHub Release/)).toBeInTheDocument();
  });

  it("hub v0.5.6 绑定 agent v0.5.4 时命令取 v0.5.6 的脚本并写明装的是 v0.5.4", async () => {
    renderOpen("v0.5.6", "v0.5.4");
    await openWindow();
    for (const p of await screen.findAllByText(/install\.sh \| sh -s --/)) {
      expect(p.textContent).toContain("https://github.com/xjetry/heron/releases/download/v0.5.6/install.sh");
    }
    expect(screen.getByText(/安装 hub 绑定的 agent v0\.5\.4/)).toBeInTheDocument();
  });

  it("v1.0 不是合法 semver，走 latest、不带 --version", async () => {
    renderOpen("v1.0");
    await openWindow();
    for (const p of await screen.findAllByText(/install\.sh \| sh -s --/)) {
      expect(p.textContent).toContain("https://github.com/xjetry/heron/releases/latest/download/install.sh");
      expect(p.textContent).not.toContain("--version");
      expect(p.textContent).not.toContain("/download/v1.0/");
    }
    expect(await screen.findByText(/将安装最新 release/)).toBeInTheDocument();
  });

  it("dev hub uses latest/download and shows the latest-release hint", async () => {
    renderOpen("dev");
    await openWindow();
    for (const p of await screen.findAllByText(/install\.sh \| sh -s --/)) {
      expect(p.textContent).toContain("https://github.com/xjetry/heron/releases/latest/download/install.sh");
      expect(p.textContent).not.toContain("--version");
      expect(p.textContent).not.toContain("/download/dev/");
    }
    expect(await screen.findByText(/将安装最新 release/)).toBeInTheDocument();
  });

  it("空的 hub_version 按非正式版本给出 latest 命令，不当作未就绪", async () => {
    renderOpen("");
    await openWindow();
    const pres = await screen.findAllByText(/install\.sh \| sh -s --/);
    expect(pres).toHaveLength(2);
    for (const p of pres) {
      expect(p.textContent).toContain("https://github.com/xjetry/heron/releases/latest/download/install.sh");
      expect(p.textContent).not.toContain("--version");
    }
    expect(screen.getByText(/hub 不是正式版本（未知）.*将安装最新 release/)).toBeInTheDocument();
    expect(screen.queryByText("加载中…")).toBeNull();
  });

  it("does not render install commands while the snapshot is pending", async () => {
    let opened = false;
    renderWithAdmin({
      getRegisterWindow: async () => ({ open: opened, expiresAt: 4_000_000_000n, remaining: 3 }),
      openRegisterWindow: async () => { opened = true; return { key: "k1", expiresAt: 4_000_000_000n, maxNodes: 3 }; },
      getSnapshot: () => new Promise(() => {}),
    }, [{ path: "/register", Component: RegisterWindow }], "/register");
    await openWindow();
    expect(await screen.findByText("加载中…")).toBeInTheDocument();
    expect(screen.queryByText(/install\.sh/)).toBeNull();
  });

  it("shows only the snapshot error when it fails before commands exist", async () => {
    let opened = false;
    renderWithAdmin({
      getRegisterWindow: async () => ({ open: opened, expiresAt: 4_000_000_000n, remaining: 3 }),
      openRegisterWindow: async () => { opened = true; return { key: "k1", expiresAt: 4_000_000_000n, maxNodes: 3 }; },
      getSnapshot: async () => { throw new ConnectError("snapshot unavailable", Code.Unavailable); },
    }, [{ path: "/register", Component: RegisterWindow }], "/register");
    await openWindow();
    expect(await screen.findByRole("alert")).toHaveTextContent("snapshot unavailable");
    expect(screen.queryByText(/install\.sh/)).toBeNull();
  });

  it("快照就绪后刷新失败保留命令并显示错误横幅", async () => {
    let fail = false;
    let opened = false;
    const { queryClient } = renderWithAdmin({
      getRegisterWindow: async () => ({ open: opened, expiresAt: 4_000_000_000n, remaining: 3 }),
      openRegisterWindow: async () => { opened = true; return { key: "k1", expiresAt: 4_000_000_000n, maxNodes: 3 }; },
      getSnapshot: async () => {
        if (fail) throw new ConnectError("snapshot refresh failed", Code.Unavailable);
        return { now: 1n, reportIntervalMs: 4000, nodes: [], hubVersion: "v1.2.3" };
      },
    }, [{ path: "/register", Component: RegisterWindow }], "/register");
    await openWindow();
    expect(await screen.findAllByText(/install\.sh \| sh -s --/)).toHaveLength(2);
    fail = true;
    const snapshotKey = createConnectQueryKey({ schema: AdminService.method.getSnapshot, cardinality: "finite" });
    await act(async () => { await queryClient.refetchQueries({ queryKey: snapshotKey }); });
    expect(await screen.findByRole("alert")).toHaveTextContent("snapshot refresh failed");
    expect(screen.getAllByText(/releases\/download\/v1\.2\.3\/install\.sh/)).toHaveLength(2);
  });
});

it("页面说明是批量添加服务器节点：限时限量的接入窗口、每台执行一次安装命令、名称默认取主机名，并指向逐台添加的入口", async () => {
  renderOpen("v0.9.4");
  const guide = within(await screen.findByRole("region", { name: "使用说明" }));
  expect(screen.getByRole("heading", { name: "批量添加节点" })).toBeInTheDocument();
  expect(screen.getByText("一次接入多台服务器，不用逐台在面板里建节点。")).toBeInTheDocument();
  expect(guide.getByText(/在每台要监控的服务器上以 root 执行一次，它就自动注册为一个新节点，名称默认取主机名/)).toBeInTheDocument();
  expect(guide.getByText("--name 名称")).toBeInTheDocument();
  expect(guide.getByRole("link", { name: "节点" })).toHaveAttribute("href", "/nodes");
});

it("有效期是自绘下拉，默认 1 小时，选中的有效期随开窗请求提交", async () => {
  const openRegisterWindow = vi.fn(async () => ({ key: "k", expiresAt: 4_000_000_000n, maxNodes: 5 }));
  renderWithAdmin({ getRegisterWindow: async () => ({ open: false, expiresAt: 0n, remaining: 0 }), openRegisterWindow, getSnapshot: snapshotOf("v1.2.3") },
    [{ path: "/register", Component: RegisterWindow }], "/register");
  const drawer = within(await openDrawer());
  const ttl = drawer.getByRole("button", { name: /^有效期 / });
  expect(ttl).toHaveAccessibleName("有效期 1 小时");
  fireEvent.click(ttl);
  fireEvent.click(drawer.getByRole("option", { name: "24 小时" }));
  expect(ttl).toHaveAccessibleName("有效期 24 小时");
  fireEvent.click(drawer.getByRole("button", { name: "开启" }));
  await waitFor(() => expect(openRegisterWindow).toHaveBeenCalledWith(expect.objectContaining({ ttlS: 86400, maxNodes: 5 }), expect.anything()));
});
