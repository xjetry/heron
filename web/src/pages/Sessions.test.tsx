import { Code, ConnectError } from "@connectrpc/connect";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { expect, it } from "vitest";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { Sessions } from "./Sessions";
import { dateTime } from "../lib/format";
import { expectEmptyState } from "../test/empty";

const currentID = "a".repeat(64);
const otherID = "b".repeat(64);
const sessions = [
  { id: currentID, createdAt: 1700000000n, lastUsedAt: 1700000600n, current: true, source: "203.0.113.10" },
  { id: otherID, createdAt: 1699000000n, lastUsedAt: 1699001200n, current: false, source: "" },
];
const routes = [{ path: "/security", Component: Sessions }, { path: "/login", element: <h1>登录页</h1> }];
const render = (impl: AdminImpl = {}) => renderWithAdmin({ listSessions: async () => ({ sessions, requestSource: "198.51.100.9" }), getSecurity: async () => ({}), ...impl }, routes, "/security");
async function openRowAction(label: string, action: string) {
  fireEvent.click(await screen.findByRole("button", { name: `更多操作 ${label}` }));
  fireEvent.click(screen.getByRole("menuitem", { name: `${action} ${label}` }));
}

it("会话列表显示创建与最近使用时刻并仅标记当前会话", async () => {
  render();
  for (const s of sessions) {
    const row = (await screen.findByRole("button", { name: `更多操作 ${s.id.slice(0, 12)}` })).closest("tr")!;
    expect(within(row).getByText(dateTime(s.createdAt))).toBeInTheDocument();
    expect(within(row).getByText(dateTime(s.lastUsedAt))).toBeInTheDocument();
    expect(within(row).queryByText("当前") != null).toBe(s.current);
  }
});

it("来源列显示签发来源、空串显示未记录；表上方标注本次请求来源", async () => {
  render();
  const currentRow = (await screen.findByRole("button", { name: `更多操作 ${currentID.slice(0, 12)}` })).closest("tr")!;
  expect(within(currentRow).getByText("203.0.113.10")).toBeInTheDocument();
  const otherRow = (await screen.findByRole("button", { name: `更多操作 ${otherID.slice(0, 12)}` })).closest("tr")!;
  expect(within(otherRow).getByText("未记录")).toHaveAttribute("title", "迁移前已存在的会话，或没有请求的签发路径");
  expect(await screen.findByText(/本次请求的来源：198\.51\.100\.9/)).toBeInTheDocument();
});

it("撤销其它会话先确认再按 hash 调用并刷新列表", async () => {
  const revoked: string[] = [];
  const { router } = render({
    listSessions: async () => ({ sessions: sessions.filter((s) => !revoked.includes(s.id)) }),
    revokeSession: async (req) => { revoked.push(req.id); return {}; },
  });
  await openRowAction(otherID.slice(0, 12), "撤销会话");
  expect(revoked).toEqual([]);
  fireEvent.click(screen.getByRole("menuitem", { name: `确认撤销会话 ${otherID.slice(0, 12)}` }));
  await waitFor(() => expect(revoked).toEqual([otherID]));
  await waitFor(() => expect(screen.queryByRole("row", { name: new RegExp(otherID.slice(0, 12)) })).not.toBeInTheDocument());
  expect(router.state.location.pathname).toBe("/security");
  expect(screen.getByRole("button", { name: `更多操作 ${currentID.slice(0, 12)}` })).toBeInTheDocument();
});

it("撤销当前会话清空缓存后跳转登录页", async () => {
  const revoked: string[] = [];
  const { queryClient, router } = render({ revokeSession: async (req) => { revoked.push(req.id); return {}; } });
  queryClient.setQueryData(["private-data"], "private");
  let cacheAtLogin: number[] = [];
  router.subscribe((state) => {
    if (state.location.pathname === "/login") cacheAtLogin = [queryClient.getQueryCache().getAll().length, queryClient.getMutationCache().getAll().length];
  });
  await openRowAction(currentID.slice(0, 12), "撤销会话");
  fireEvent.click(screen.getByRole("menuitem", { name: `确认撤销会话 ${currentID.slice(0, 12)}` }));
  await screen.findByRole("heading", { name: "登录页" });
  expect(revoked).toEqual([currentID]);
  expect(cacheAtLogin).toEqual([0, 0]);
});

it("撤销失败保留页面并显示服务器错误", async () => {
  const { router } = render({ revokeSession: async () => { throw new ConnectError("revoking session failed", Code.Internal); } });
  await openRowAction(currentID.slice(0, 12), "撤销会话");
  fireEvent.click(screen.getByRole("menuitem", { name: `确认撤销会话 ${currentID.slice(0, 12)}` }));
  expect(await screen.findByRole("alert")).toHaveTextContent("revoking session failed");
  expect(router.state.location.pathname).toBe("/security");
});

it("三张卡展示密码说明、TOTP 恢复码与 Passkey 来源，并提供管理入口", async () => {
  render({ getSecurity: async () => ({ totpEnabled: true, recoveryCodesRemaining: 7, passkeys: [{ id: "k1", name: "key" }], origin: "https://h.example" }) });
  await screen.findByText("已启用，剩余 7 个恢复码");
  expect.soft(screen.getByRole("article", { name: "密码" })).toHaveTextContent("密码由 hub 的启动配置提供；认证设备全部丢失时由运维者在 hub 主机运行安全重置命令。");
  expect.soft(screen.getByRole("article", { name: "TOTP" })).toHaveTextContent("已启用，剩余 7 个恢复码");
  expect.soft(screen.getByRole("article", { name: "Passkey" })).toHaveTextContent("已注册 1 个");
  expect.soft(screen.getByRole("article", { name: "Passkey" })).toHaveTextContent("已绑定来源 https://h.example");
  for (const name of ["TOTP", "Passkey"]) {
    expect.soft(within(screen.getByRole("article", { name })).getByRole("link", { name: "管理 TOTP、恢复码与 Passkey" })).toHaveAttribute("href", "/security/credentials");
  }
});

it("getSecurity 读取失败只影响卡片，会话表照常", async () => {
  render({ getSecurity: async () => { throw new ConnectError("security unavailable", Code.Internal); } });
  await screen.findAllByText(/读取失败/);
  expect.soft(screen.queryByRole("row", { name: new RegExp(currentID.slice(0, 12)) })).toBeInTheDocument();
  expect.soft(screen.queryByRole("article", { name: "TOTP" })).toHaveTextContent("读取失败");
  expect.soft(screen.queryByRole("article", { name: "Passkey" })).toHaveTextContent("读取失败");
});

it("没有会话时只有空态卡，不画只有表头的表", async () => {
  render({ listSessions: async () => ({ sessions: [] }) });
  await expectEmptyState("没有有效会话。", { region: "登录会话" });
});
