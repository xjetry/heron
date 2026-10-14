// @vitest-environment-options {"url":"https://panel.example/"}
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { Code, ConnectError } from "@connectrpc/connect";
import { AdminService, SecurityActionKind } from "../gen/heron/v1/admin_pb";
import { renderWithAdmin } from "../test/harness";
import { Security } from "./Security";
import { passkeyCredential } from "../lib/passkey";

vi.mock("../lib/passkey", () => ({ passkeyCredential: vi.fn() }));
beforeEach(() => {
  vi.stubGlobal("isSecureContext", true);
  vi.stubGlobal("PublicKeyCredential", class {});
  vi.mocked(passkeyCredential).mockResolvedValue("credential-json");
});
afterEach(() => { vi.unstubAllGlobals(); vi.clearAllMocks(); });

const securityRoutes = [{ path: "/security/credentials", Component: Security }, { path: "/login", element: <h1>login</h1> }];

it("账户安全使用统一页头与三张认证卡片", async () => {
  renderWithAdmin({ getSecurity: async () => ({}) }, securityRoutes, "/security/credentials");
  await waitFor(() => expect(screen.getByRole("button", { name: "设置 TOTP" })).toBeEnabled());
  expect.soft(screen.getByRole("heading", { name: "账户安全" }).closest("header")).toHaveClass("page-header");
  for (const name of ["重新证明身份", "TOTP 与恢复码", "Passkey"]) {
    expect.soft(screen.getByRole("group", { name })).toHaveClass("card");
  }
});

describe("Security", () => {
  it("TOTP 启用后保留一次性恢复码，不再查询已撤销的会话", async () => {
    const getSecurity = vi.fn(async () => ({ totpEnabled: false, passkeyAvailable: true, passkeys: [], recoveryCodesRemaining: 0 }));
    const securityAction = vi.fn(async (request: { action: SecurityActionKind }) => {
      if (request.action === SecurityActionKind.TOTP_BEGIN) return { challengeId: "setup", totpSecret: "SECRET", totpUri: "otpauth://totp/probe" };
      if (request.action === SecurityActionKind.TOTP_ENABLE) return { recoveryCodes: ["RECOVERY-ONE", "RECOVERY-TWO"] };
      throw new Error("unexpected action");
    });
    const { queryClient, router } = renderWithAdmin({ getSecurity, securityAction }, [{ path: "/security/credentials", Component: Security }, { path: "/login", element: <h1>login</h1> }], "/security/credentials");
    await waitFor(() => expect(screen.getByRole("button", { name: "设置 TOTP" })).toBeEnabled());
    fireEvent.change(screen.getByLabelText("管理员密码"), { target: { value: "correct horse battery" } });
    fireEvent.click(screen.getByRole("button", { name: "设置 TOTP" }));
    expect(await screen.findByText("SECRET")).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("新认证器验证码"), { target: { value: "123456" } });
    fireEvent.click(screen.getByRole("button", { name: "验证并启用" }));
    expect(await screen.findByText(/RECOVERY-ONE/)).toHaveTextContent("RECOVERY-TWO");
    expect(securityAction).toHaveBeenLastCalledWith(expect.objectContaining({ action: SecurityActionKind.TOTP_ENABLE, challengeId: "setup", otp: "123456" }), expect.anything());
    // useQuery 在 effect 里才把 enabled: !changed 交给 observer，恢复码进 DOM 时查询可能仍是启用的。要钉的是停用之后
    // 不再查询，所以先等到停用，再用失效触发一次本会发起的重取。
    const securityKey = createConnectQueryKey({ schema: AdminService.method.getSecurity, cardinality: "finite" });
    await waitFor(() => expect(queryClient.getQueryCache().findAll({ queryKey: securityKey }).map((q) => q.isDisabled())).toEqual([true]));
    const calls = getSecurity.mock.calls.length;
    await queryClient.invalidateQueries();
    expect(getSecurity).toHaveBeenCalledTimes(calls);
    expect(router.state.location.pathname).toBe("/security/credentials");
    expect(screen.getByRole("link", { name: "重新登录" })).toBeInTheDocument();
  });
});

it("首次 Passkey 注册自动使用可信当前来源，不要求填写 admin-origin", async () => {
  const securityAction = vi.fn(async (request: { action: SecurityActionKind }) => request.action === SecurityActionKind.PASSKEY_BEGIN ? { challengeId: "register", optionsJson: "options" } : {});
  renderWithAdmin({ getSecurity: async () => ({ currentOrigin: "https://panel.example", passkeyAvailable: true }), securityAction }, securityRoutes, "/security/credentials");
  expect(await screen.findByText(/首次注册成功后绑定当前访问域名/, {}, { timeout: 1000 })).toBeVisible();
  expect(screen.queryByText(/admin-origin/)).toBeNull();
  fireEvent.change(screen.getByLabelText("管理员密码"), { target: { value: "secret" } });
  fireEvent.change(screen.getByLabelText("认证器名称"), { target: { value: "我的密钥" } });
  fireEvent.click(screen.getByRole("button", { name: "添加 Passkey" }));
  expect(await screen.findByRole("heading", { name: "认证方式已更新" })).toBeVisible();
  expect(securityAction).toHaveBeenNthCalledWith(1, expect.objectContaining({ action: SecurityActionKind.PASSKEY_BEGIN, password: "secret" }), expect.anything());
  expect(passkeyCredential).toHaveBeenCalledWith("options", true);
  expect(securityAction).toHaveBeenLastCalledWith(expect.objectContaining({ action: SecurityActionKind.PASSKEY_REGISTER, challengeId: "register", credentialJson: "credential-json", name: "我的密钥" }), expect.anything());
});

it.each([
  ["https_required", "当前请求不是可信 HTTPS"],
  ["untrusted_proxy", "反向代理未受信任"],
  ["invalid_forwarded_proto", "转发协议头无效或冲突"],
  ["request_origin_mismatch", "请求来源与访问地址不一致"],
])("服务端拒绝原因 %s 明确显示且不能注册或重新绑定", async (reason, message) => {
  renderWithAdmin({ getSecurity: async () => ({ origin: "https://old.example", currentOrigin: "https://panel.example", unavailableReason: reason, passkeys: [{ id: "old", name: "旧密钥" }] }) }, securityRoutes, "/security/credentials");
  expect(await screen.findByText(message)).toBeVisible();
  fireEvent.change(screen.getByLabelText("认证器名称"), { target: { value: "新密钥" } });
  fireEvent.change(screen.getByLabelText("管理员密码"), { target: { value: "secret" } });
  expect(screen.getByRole("button", { name: "添加 Passkey" })).toBeDisabled();
  expect(screen.queryByRole("button", { name: "重新绑定并添加 Passkey" })).toBeNull();
  expect(screen.getByRole("button", { name: "使用 Passkey 重新认证" })).toBeDisabled();
});

it.each(["不安全上下文", "不支持 WebAuthn"])("浏览器%s时不开放注册", async (condition) => {
  if (condition === "不安全上下文") vi.stubGlobal("isSecureContext", false);
  else vi.stubGlobal("PublicKeyCredential", undefined);
  renderWithAdmin({ getSecurity: async () => ({ currentOrigin: "https://panel.example", passkeyAvailable: true }) }, securityRoutes, "/security/credentials");
  await screen.findByText(/当前访问来源/);
  fireEvent.change(screen.getByLabelText("认证器名称"), { target: { value: "新密钥" } });
  expect(screen.getByRole("button", { name: "添加 Passkey" })).toBeDisabled();
  expect(screen.getByText(condition === "不安全上下文" ? "浏览器未处于安全上下文，不能使用 Passkey。" : "当前浏览器不支持 WebAuthn，不能使用 Passkey。")).toBeVisible();
});

it("换域重绑定需要密码和现有第二因素，成功前保留旧认证器", async () => {
  const securityAction = vi.fn(async (request: { action: SecurityActionKind }) => {
    if (request.action === SecurityActionKind.PASSKEY_REBIND_BEGIN) return { challengeId: "rebind", optionsJson: "new-options" };
    throw new ConnectError("new registration failed", Code.InvalidArgument);
  });
  renderWithAdmin({ getSecurity: async () => ({ origin: "https://old.example", currentOrigin: "https://panel.example", unavailableReason: "origin_mismatch", totpEnabled: true, passkeys: [{ id: "old", name: "旧密钥" }] }), securityAction }, securityRoutes, "/security/credentials");
  const rebind = await screen.findByRole("button", { name: "重新绑定并添加 Passkey" });
  expect(screen.getByText(/注册成功后替换旧域名的 Passkey/)).toBeVisible();
  expect(rebind).toBeDisabled();
  fireEvent.change(screen.getByLabelText("认证器名称"), { target: { value: "新密钥" } });
  fireEvent.change(screen.getByLabelText("管理员密码"), { target: { value: "secret" } });
  expect(rebind).toBeDisabled();
  fireEvent.change(screen.getByLabelText("当前动态验证码"), { target: { value: "123456" } });
  expect(rebind).toBeEnabled();
  fireEvent.click(rebind);
  expect(await screen.findByRole("alert")).toHaveTextContent("new registration failed");
  expect(securityAction).toHaveBeenNthCalledWith(1, expect.objectContaining({ action: SecurityActionKind.PASSKEY_REBIND_BEGIN, password: "secret", otp: "123456", proofToken: "" }), expect.anything());
  expect(securityAction).toHaveBeenLastCalledWith(expect.objectContaining({ action: SecurityActionKind.PASSKEY_REGISTER, challengeId: "rebind", credentialJson: "credential-json" }), expect.anything());
  expect(screen.getByText("旧密钥")).toBeVisible();
  expect(screen.getByText(/已绑定来源/)).toHaveTextContent("https://old.example");
  expect(screen.queryByRole("heading", { name: "认证方式已更新" })).toBeNull();
});

it.each(["binding_missing", "binding_invalid"])("来源绑定 %s 时允许用恢复码重新绑定，而不自动换域", async (unavailableReason) => {
  const securityAction = vi.fn(async (request: { action: SecurityActionKind }) => request.action === SecurityActionKind.PASSKEY_REBIND_BEGIN ? { challengeId: "rebind", optionsJson: "options" } : {});
  renderWithAdmin({ getSecurity: async () => ({ currentOrigin: "https://panel.example", unavailableReason, totpEnabled: true, passkeys: [{ id: "old", name: "旧密钥" }] }), securityAction }, securityRoutes, "/security/credentials");
  const rebind = await screen.findByRole("button", { name: "重新绑定并添加 Passkey" });
  fireEvent.change(screen.getByLabelText("认证器名称"), { target: { value: "新密钥" } });
  fireEvent.change(screen.getByLabelText("管理员密码"), { target: { value: "secret" } });
  fireEvent.change(screen.getByLabelText("或一次性恢复码"), { target: { value: "RECOVERY" } });
  fireEvent.click(rebind);
  expect(await screen.findByRole("heading", { name: "认证方式已更新" })).toBeVisible();
  expect(securityAction).toHaveBeenNthCalledWith(1, expect.objectContaining({ action: SecurityActionKind.PASSKEY_REBIND_BEGIN, password: "secret", recoveryCode: "RECOVERY", proofToken: "" }), expect.anything());
});
