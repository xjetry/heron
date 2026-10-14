import { createConnectQueryKey } from "@connectrpc/connect-query";
import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { AdminService } from "../gen/heron/v1/admin_pb";
import { renderWithAdmin } from "../test/harness";
import { Nodes } from "./Nodes";

// 行菜单入口携带 id，同名节点仍能定位到各自的操作。
const rowAction = (label: string, action: string) => {
  const trigger = screen.getByRole("button", { name: `更多操作 ${label}` });
  if (trigger.getAttribute("aria-expanded") !== "true") fireEvent.click(trigger);
  return screen.getByRole("menuitem", { name: `${action} ${label}` });
};
const openRowAction = (label: string, action: string) => fireEvent.click(rowAction(label, action));

const nodes = Array.from({ length: 100 }, (_, i) => ({
  id: BigInt(i + 1), name: `节点-${i + 1}-${"长名称".repeat(12)}`, note: `客户备注-${"不能把单元格撑出页面".repeat(20)}`,
  public: true, trafficResetDay: 1, tags: [`region-${i % 5}`, `服务-${"长标签".repeat(5)}`],
}));
const label = (i: number) => `${nodes[i].name}（#${nodes[i].id}）`;
const shown = () => screen.getAllByRole("link").map((link) => link.textContent);
const defer = () => {
  let resolve!: () => void;
  const promise = new Promise<void>((yes) => { resolve = yes; });
  return { promise, resolve };
};

describe("100 节点管理交互", () => {
  it("长文本完整展示，连续排序不丢步，后台刷新保留编辑草稿", async () => {
    let current = [...nodes];
    const gate = defer();
    const reorderNodes = vi.fn(async (request: { ids: bigint[] }) => {
      if (reorderNodes.mock.calls.length === 1) await gate.promise;
      current = request.ids.map((id) => current.find((node) => node.id === id)!);
      return {};
    });
    const { queryClient } = renderWithAdmin({
      listNodes: async () => ({ nodes: current }), listTags: async () => ({ tags: [] }),
      getSnapshot: async () => ({ nodes: [] }), reorderNodes,
    }, [{ path: "/nodes", Component: Nodes }], "/nodes");
    await screen.findByRole("link", { name: label(99) });
    expect(shown()).toEqual(nodes.map((node) => node.name));
    expect(screen.getAllByText(nodes[0].note)).toHaveLength(100);
    openRowAction(label(0), "下移一位");
    openRowAction(label(0), "下移一位");
    openRowAction(label(50), "编辑");
    const draft = screen.getByRole("textbox", { name: `备注 ${label(50)}` });
    fireEvent.change(draft, { target: { value: "尚未保存的规模验收备注" } });
    expect(shown().slice(0, 3)).toEqual([nodes[1].name, nodes[2].name, nodes[0].name]);
    await waitFor(() => expect(reorderNodes).toHaveBeenCalledTimes(1));
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listNodes, cardinality: "finite" }) });
    });
    expect(shown().slice(0, 3)).toEqual([nodes[1].name, nodes[2].name, nodes[0].name]);
    expect(draft).toHaveValue("尚未保存的规模验收备注");
    await act(async () => { gate.resolve(); });
    await waitFor(() => expect(screen.queryByRole("status")).toBeNull());
    expect(reorderNodes).toHaveBeenCalledTimes(2);
    expect(current.slice(0, 3).map((node) => node.id)).toEqual([2n, 3n, 1n]);
    expect(draft).toHaveValue("尚未保存的规模验收备注");
  });

  it("保存已生效但回读失败时明确提示且禁止重排，重新读取后恢复权威顺序", async () => {
    let current = [...nodes];
    let unavailable = false;
    const reorderNodes = vi.fn(async (request: { ids: bigint[] }) => {
      current = request.ids.map((id) => current.find((node) => node.id === id)!);
      unavailable = true;
      return {};
    });
    renderWithAdmin({
      listNodes: async () => { if (unavailable) throw new Error("readback unavailable"); return { nodes: current }; },
      listTags: async () => ({ tags: [] }), getSnapshot: async () => ({ nodes: [] }), reorderNodes,
    }, [{ path: "/nodes", Component: Nodes }], "/nodes");
    await screen.findByRole("link", { name: label(99) });
    openRowAction(label(0), "下移一位");
    const recover = await screen.findByRole("button", { name: "重新读取排序" });
    await waitFor(() => expect(recover).toBeEnabled());
    expect(screen.getAllByRole("alert").some((alert) => alert.textContent?.includes("无法确认服务端排序"))).toBe(true);
    for (const button of screen.getAllByRole("button", { name: /^调整顺序/ })) expect(button).toBeDisabled();
    unavailable = false;
    fireEvent.click(recover);
    await waitFor(() => expect(screen.queryByRole("button", { name: "重新读取排序" })).toBeNull());
    expect(shown().slice(0, 3)).toEqual([nodes[1].name, nodes[0].name, nodes[2].name]);
    expect(screen.getByRole("alert")).toHaveTextContent("排序未完成");
    expect(reorderNodes).toHaveBeenCalledTimes(1);
  });
});
