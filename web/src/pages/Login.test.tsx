import { Code, ConnectError } from "@connectrpc/connect";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { renderWithAdmin } from "../test/harness";
import { Login } from "./Login";
import { passkeyCredential } from "../lib/passkey";

vi.mock("../lib/passkey", () => ({ passkeyCredential: vi.fn() }));

function renderLogin(login: (req: { password: string }) => Promise<Record<string, never>>) {
  return renderWithAdmin(
    { login },
    [
      { path: "/login", Component: Login },
      { path: "/", element: <h1>home</h1> },
    ],
    "/login",
  );
}

describe("Login", () => {
  it("登录按钮是主按钮，Passkey 入口是次级按钮，两者都在表单内", () => {
    renderWithAdmin({}, [{ path: "/login", Component: Login }], "/login");
    const form = screen.getByRole("button", { name: "登录" }).closest("form")!;
    expect(form).toHaveClass("login-card");
    expect(screen.getByRole("button", { name: "登录" })).toHaveClass("primary-button");
    expect(within(form).getByRole("button", { name: "使用 Passkey 登录" })).toHaveClass("passkey-button");
  });

  it("登录页显示 Heron 品牌与主机监控定位", () => {
    renderLogin(vi.fn(async () => ({})));
    expect(screen.getByRole("heading", { name: "Heron" })).toBeInTheDocument();
    expect(screen.getByText("轻量自托管主机监控")).toBeInTheDocument();
  });

  it("Passkey 无密码登录依次完成挑战、浏览器证明与会话签发", async () => {
    const order: string[] = [];
    const beginPasskeyLogin = vi.fn(async () => { order.push("begin"); return { challengeId: "challenge", optionsJson: "options" }; });
    vi.mocked(passkeyCredential).mockImplementationOnce(async (options) => { expect(options).toBe("options"); order.push("credential"); return "assertion"; });
    const finishPasskeyLogin = vi.fn(async () => { order.push("finish"); return {}; });
    const { router } = renderWithAdmin({ beginPasskeyLogin, finishPasskeyLogin }, [{ path: "/login", Component: Login }, { path: "/", element: <h1>home</h1> }], "/login");
    fireEvent.click(screen.getByRole("button", { name: "使用 Passkey 登录" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/"));
    expect(order).toEqual(["begin", "credential", "finish"]);
    expect(finishPasskeyLogin).toHaveBeenCalledWith(expect.objectContaining({ challengeId: "challenge", credentialJson: "assertion" }), expect.anything());
  });
  it("提交密码，成功后回到首页", async () => {
    const login = vi.fn(async () => ({}));
    const { router, queryClient } = renderLogin(login);
    queryClient.setQueryData(["previous-session"], "old data");
    await queryClient.getMutationCache().build(queryClient, { mutationFn: async () => "old token" }).execute(undefined);
    let cacheAtHome: number[] = [];
    router.subscribe((state) => {
      if (state.location.pathname === "/") cacheAtHome = [queryClient.getQueryCache().getAll().length, queryClient.getMutationCache().getAll().length];
    });
    fireEvent.change(screen.getByLabelText("管理员密码"), { target: { value: "correct horse battery" } });
    fireEvent.click(screen.getByRole("button", { name: "登录" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/"));
    expect(login).toHaveBeenCalledWith(expect.objectContaining({ password: "correct horse battery" }), expect.anything());
    expect(cacheAtHome).toEqual([0, 0]);
  });

  it("失败时原样显示 hub 的信息并留在登录页", async () => {
    const { router } = renderLogin(async () => {
      throw new ConnectError("wrong password", Code.Unauthenticated);
    });
    fireEvent.change(screen.getByLabelText("管理员密码"), { target: { value: "nope nope nope" } });
    fireEvent.click(screen.getByRole("button", { name: "登录" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/^wrong password$/);
    expect(router.state.location.pathname).toBe("/login");
  });

  it("空密码不能提交", () => {
    renderLogin(vi.fn(async () => ({})));
    expect(screen.getByRole("button", { name: "登录" })).toBeDisabled();
  });

  it("密码校验繁忙时提示稍后重试而不是密码错误", async () => {
    renderLogin(async () => {
      throw new ConnectError("password verification is busy; please try again later", Code.ResourceExhausted);
    });
    fireEvent.change(screen.getByLabelText("管理员密码"), { target: { value: "correct horse battery" } });
    fireEvent.click(screen.getByRole("button", { name: "登录" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/^登录繁忙，请稍后再试。$/);
  });
});
