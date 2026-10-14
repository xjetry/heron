import { Code, ConnectError } from "@connectrpc/connect";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { Nodes } from "./Nodes";

const nodes = Array.from({ length: 5 }, (_, i) => ({ id: BigInt(i + 1), name: `n${i + 1}`, tags: i < 3 ? ["家宽", "DB"] : ["DB"] }));
function renderNodes(impl: AdminImpl = {}) {
  return renderWithAdmin({ listNodes: async () => ({ nodes }), listTags: async () => ({ tags: [{ name: "家宽" }, { name: "DB" }, { name: "备用" }] }), getSnapshot: async () => ({}), ...impl }, [{ path: "/nodes", Component: Nodes }], "/nodes");
}
async function openBatch() {
  fireEvent.click(await screen.findByRole("checkbox", { name: "选择当前结果全部节点" }));
  fireEvent.click(screen.getByRole("button", { name: "批量编辑标签" }));
  return within(screen.getByRole("dialog", { name: "批量编辑标签" }));
}

it("五台中三台有标签时半选，保留未操作标签，只提交批量增删意图", async () => {
  const batchUpdateNodeTags = vi.fn(async () => ({}));
  renderNodes({ batchUpdateNodeTags });
  const dialog = await openBatch();
  expect(screen.getByRole("dialog", { name: "批量编辑标签" })).toHaveClass("drawer");
  expect(dialog.getByRole("checkbox", { name: "家宽" })).toBePartiallyChecked();
  expect(dialog.getByText("3/5 个节点")).toBeInTheDocument();
  expect(dialog.getByRole("checkbox", { name: "DB" })).toBeChecked();
  expect(dialog.getByRole("checkbox", { name: "备用" })).not.toBeChecked();
  expect(dialog.getByRole("button", { name: "保存" })).toBeDisabled();
  fireEvent.click(dialog.getByRole("checkbox", { name: "家宽" }));
  expect(dialog.getByRole("checkbox", { name: "家宽" })).toBeChecked();
  fireEvent.click(dialog.getByRole("checkbox", { name: "DB" }));
  fireEvent.change(dialog.getByRole("textbox", { name: "新标签" }), { target: { value: " 客户A " } });
  fireEvent.click(dialog.getByRole("button", { name: "添加标签" }));
  fireEvent.click(dialog.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(batchUpdateNodeTags).toHaveBeenCalledWith(expect.objectContaining({ nodeIds: [1n, 2n, 3n, 4n, 5n], addTags: ["家宽", "客户A"], removeTags: ["DB"] }), expect.anything()));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

it("半选可切换到全部移除或恢复原样，新增同名标签沿用简单折叠口径", async () => {
  const batchUpdateNodeTags = vi.fn(async () => ({}));
  renderNodes({ batchUpdateNodeTags });
  const dialog = await openBatch();
  const tag = dialog.getByRole("checkbox", { name: "家宽" });
  fireEvent.click(tag);
  fireEvent.click(tag);
  expect(tag).not.toBeChecked();
  expect(tag).not.toBePartiallyChecked();
  fireEvent.click(tag);
  expect(tag).toBePartiallyChecked();
  expect(dialog.getByRole("button", { name: "保存" })).toBeDisabled();
  fireEvent.change(dialog.getByRole("textbox", { name: "新标签" }), { target: { value: " db " } });
  fireEvent.click(dialog.getByRole("button", { name: "添加标签" }));
  expect(dialog.getAllByRole("checkbox")).toHaveLength(3);
  fireEvent.click(tag);
  fireEvent.click(tag);
  fireEvent.click(dialog.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(batchUpdateNodeTags).toHaveBeenCalledWith(expect.objectContaining({ removeTags: ["家宽"], addTags: [] }), expect.anything()));
});

it("筛选切换清除选择，不会修改隐藏节点；失败保留草稿，取消不发送", async () => {
  const batchUpdateNodeTags = vi.fn(async () => { throw new ConnectError("tag limit", Code.InvalidArgument); });
  renderNodes({ batchUpdateNodeTags });
  fireEvent.click(await screen.findByRole("checkbox", { name: "选择 n1（#1）" }));
  expect(screen.getByRole("checkbox", { name: "选择当前结果全部节点" })).toBePartiallyChecked();
  fireEvent.change(screen.getByRole("searchbox"), { target: { value: "n5" } });
  expect(screen.queryByRole("toolbar", { name: "批量操作" })).toBeNull();
  const dialog = await openBatch();
  fireEvent.click(dialog.getByRole("checkbox", { name: "家宽" }));
  fireEvent.click(dialog.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(dialog.getByRole("alert")).toHaveTextContent("tag limit"));
  expect(dialog.getByRole("checkbox", { name: "家宽" })).toBeChecked();
  expect(batchUpdateNodeTags).toHaveBeenCalledWith(expect.objectContaining({ nodeIds: [5n] }), expect.anything());
  fireEvent.click(dialog.getByRole("button", { name: "取消" }));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(batchUpdateNodeTags).toHaveBeenCalledTimes(1);
});

it("写入成功但回读失败时丢弃旧基线草稿，列表报错期间不可重新批量编辑", async () => {
  let saved = false;
  const batchUpdateNodeTags = vi.fn(async () => { saved = true; return {}; });
  renderNodes({ batchUpdateNodeTags, listNodes: async () => {
    if (saved) throw new ConnectError("readback failed", Code.Unavailable);
    return { nodes };
  } });
  const dialog = await openBatch();
  fireEvent.click(dialog.getByRole("checkbox", { name: "家宽" }));
  fireEvent.click(dialog.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(batchUpdateNodeTags).toHaveBeenCalledTimes(1));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(await screen.findByText(/已保存，但回读失败/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("checkbox", { name: "选择当前结果全部节点" }));
  expect(screen.getByRole("button", { name: "批量编辑标签" })).toBeDisabled();
});
