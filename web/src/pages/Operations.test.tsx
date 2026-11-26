import { expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { OperationPrincipal, type ListOperationsRequest } from "../gen/heron/v1/admin_pb";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { Operations } from "./Operations";

const routes = [{ path: "/operations", Component: Operations }];
const render = (impl: AdminImpl, path = "/operations") => renderWithAdmin(impl, routes, path);

const sessionID = "ab".repeat(32);
const operations = [
  { id: "op-session", requestId: "op:1", action: "update_node", resourceId: 7n, committedAt: 1_700_000_000n, principal: OperationPrincipal.SESSION,
    sessionId: sessionID, source: "203.0.113.9", method: "UpdateNode",
    beforeJson: JSON.stringify({ node: [{ id: 7, name: "old" }] }), afterJson: JSON.stringify({ node: [{ id: 7, name: "new" }] }) },
  { id: "op-token", requestId: "agent-1", action: "update_node", resourceId: 7n, ownerId: 4n, committedAt: 1_699_999_000n,
    principal: OperationPrincipal.API_TOKEN, source: "198.51.100.2", method: "ExecuteChange", beforeJson: "", afterJson: "" },
  { id: "op-system", requestId: "op:2", action: "auto_renew_node", resourceId: 7n, committedAt: 1_699_998_000n, principal: OperationPrincipal.SYSTEM,
    beforeJson: "{}", afterJson: "{}" },
  { id: "op-theme", requestId: "op:3", action: "enable_theme", resourceKey: "aurora", committedAt: 1_699_997_000n, principal: OperationPrincipal.SESSION,
    sessionId: sessionID, beforeJson: "{}", afterJson: "{}" },
];

it("列出主体、来源、方法、对象，展开看到按字段的差异", async () => {
  render({ listOperations: async () => ({ operations }) });
  const region = await screen.findByRole("region", { name: "操作记录" });
  const items = region.querySelectorAll("details");
  expect(items).toHaveLength(4);
  const [session, token, system, theme] = [...items];
  expect(session.querySelector("summary")).toHaveTextContent(/面板会话 abababab · update_node #7$/);
  expect(within(session).getByText("abababab")).toHaveAttribute("title", sessionID);
  expect(within(session).getByText("来源：203.0.113.9")).toBeInTheDocument();
  expect(within(session).getByText("UpdateNode")).toBeInTheDocument();
  const changes = within(session).getByRole("region", { name: "字段变化 op-session" });
  expect(within(changes).getAllByRole("row").map((r) => r.textContent)).toEqual(["字段修改前修改后", 'node[0].name"old""new"']);
  expect(token.querySelector("summary")).toHaveTextContent(/API token #4 · update_node #7$/);
  expect(within(token).getAllByText("详细记录已过保留期")).toHaveLength(2);
  expect(within(token).queryByRole("region")).toBeNull();
  expect(system.querySelector("summary")).toHaveTextContent(/hub 自身 · auto_renew_node #7$/);
  expect(within(system).getByText("快照没有字段变化。")).toBeInTheDocument();
  expect(theme.querySelector("summary")).toHaveTextContent(/面板会话 abababab · enable_theme aurora$/);
});

it("按主体与动作筛选，条件写进地址并随请求发出", async () => {
  const requests: ListOperationsRequest[] = [];
  const { router } = render({ listOperations: async (req) => { requests.push(req); return { operations: [] }; } });
  expect(await screen.findByText("暂无操作记录。")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("主体"), { target: { value: "system" } });
  await waitFor(() => expect(requests.at(-1)?.principal).toBe(OperationPrincipal.SYSTEM));
  fireEvent.change(screen.getByLabelText("动作"), { target: { value: " auto_renew_node " } });
  fireEvent.click(screen.getByRole("button", { name: "筛选" }));
  await waitFor(() => expect(requests.at(-1)).toMatchObject({ principal: OperationPrincipal.SYSTEM, action: "auto_renew_node", limit: 200 }));
  await waitFor(() => expect(router.state.location.search).toBe("?principal=system&action=auto_renew_node"));
  fireEvent.click(screen.getByRole("button", { name: "清除筛选" }));
  await waitFor(() => expect(requests.at(-1)).toMatchObject({ principal: OperationPrincipal.UNSPECIFIED, action: "" }));
  expect(screen.getByLabelText("动作")).toHaveValue("");
});

it("未知的主体参数报错且不发请求", async () => {
  let calls = 0;
  render({ listOperations: async () => { calls++; return { operations: [] }; } }, "/operations?principal=admin");
  expect(await screen.findByRole("alert")).toHaveTextContent("主体参数 admin 无效");
  expect(calls).toBe(0);
});
