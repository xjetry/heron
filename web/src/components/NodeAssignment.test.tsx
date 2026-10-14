import { fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TransportProvider } from "@connectrpc/connect-query";
import { createRouterTransport } from "@connectrpc/connect";
import { useState } from "react";
import { expect, it, vi } from "vitest";
import { AdminService } from "../gen/heron/v1/admin_pb";
import { assignmentValid, NodeAssignment, type NodeSelection } from "./NodeAssignment";

const nodes = [
  { id: 1n, name: "a", tags: ["prod", "edge"] }, { id: 2n, name: "b", tags: ["prod"] }, { id: 3n, name: "c", tags: [] },
] as never[];
const transport = createRouterTransport(({ service }) => service(AdminService, { listTags: async () => ({ tags: [{ name: "prod", nodeCount: 2 }, { name: "edge", nodeCount: 1 }] }) }));

function Harness({ initial, onChange }: { initial: NodeSelection; onChange?: (v: NodeSelection) => void }) {
  const [value, setValue] = useState(initial);
  const [client] = useState(() => new QueryClient({ defaultOptions: { queries: { retry: false } } }));
  return <TransportProvider transport={transport}><QueryClientProvider client={client}>
    <NodeAssignment nodes={nodes} value={value} legend="分配到节点" onChange={(patch) => { const next = { ...value, ...patch }; setValue(next); onChange?.(next); }} />
  </QueryClientProvider></TransportProvider>;
}
const empty = (): NodeSelection => ({ allNodes: false, nodeIds: new Set(), selectorTags: [], dynamic: false });

it.each([
  ["全部节点", { allNodes: true, dynamic: true }, true],
  ["动态有标签", { dynamic: true, selectorTags: ["prod"] }, true],
  ["动态无标签", { dynamic: true }, false],
  ["指定节点", {}, true],
])("分配校验：%s", (_name, patch, valid) => {
  expect(assignmentValid({ ...empty(), ...patch })).toBe(valid);
});

it("三张单选卡：全部节点与动态标签隐藏节点列表，指定节点显示搜索、快选与已选计数", async () => {
  render(<Harness initial={{ ...empty(), allNodes: true }} />);
  const group = screen.getByRole("radiogroup", { name: "分配方式" });
  expect(within(group).getAllByRole("radio").map((r) => r.getAttribute("aria-label") ?? "")).toEqual(["全部节点", "动态标签选择器", "指定节点"]);
  expect(within(group).getByRole("radio", { name: "全部节点" })).toBeChecked();
  expect(screen.queryByRole("searchbox", { name: "搜索节点" })).not.toBeInTheDocument();
  fireEvent.click(within(group).getByRole("radio", { name: "指定节点" }));
  expect(screen.getByRole("searchbox", { name: "搜索节点" })).toBeInTheDocument();
  expect(screen.getByText("只保存本次选中的节点，之后标签变化不会改变分配")).toBeInTheDocument();
  expect(screen.getByText("已选择 0 个节点")).toBeInTheDocument();
  expect(await screen.findByRole("button", { name: "按标签快选 prod" })).toBeInTheDocument();
});

it("按标签快选并入带该标签的节点；搜索过滤勾选列表；清空已选", async () => {
  const onChange = vi.fn();
  render(<Harness initial={empty()} onChange={onChange} />);
  fireEvent.click(await screen.findByRole("button", { name: "按标签快选 prod" }));
  expect(onChange).toHaveBeenLastCalledWith(expect.objectContaining({ nodeIds: new Set([1n, 2n]) }));
  expect(screen.getByText("已选择 2 个节点")).toBeInTheDocument();
  fireEvent.change(screen.getByRole("searchbox", { name: "搜索节点" }), { target: { value: "c" } });
  expect(screen.getAllByRole("checkbox").map((c) => c.getAttribute("aria-label"))).toEqual(["c（#3）"]);
  fireEvent.click(screen.getByRole("checkbox", { name: "c（#3）" }));
  expect(onChange).toHaveBeenLastCalledWith(expect.objectContaining({ nodeIds: new Set([1n, 2n, 3n]) }));
  fireEvent.click(screen.getByRole("button", { name: "清空已选" }));
  expect(onChange).toHaveBeenLastCalledWith(expect.objectContaining({ nodeIds: new Set() }));
});

it("已选里不在当前列表的节点标为已不存在并可单独移除", () => {
  const onChange = vi.fn();
  render(<Harness initial={{ ...empty(), nodeIds: new Set([2n, 9n]) }} onChange={onChange} />);
  expect(screen.getByText("#9（已不存在）")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "移除 #9" }));
  expect(onChange).toHaveBeenLastCalledWith(expect.objectContaining({ nodeIds: new Set([2n]) }));
});

it("动态标签选择器：匹配标签多选，显示当前匹配数；没选标签时提示至少一个", async () => {
  const onChange = vi.fn();
  render(<Harness initial={{ ...empty(), dynamic: true }} onChange={onChange} />);
  expect(screen.getByText("至少选择一个标签")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: /^匹配标签/ }));
  fireEvent.click(await screen.findByRole("checkbox", { name: "prod" }));
  expect(onChange).toHaveBeenLastCalledWith(expect.objectContaining({ selectorTags: ["prod"] }));
  expect(screen.getByText("当前匹配 2 个节点")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("checkbox", { name: "edge" }));
  expect(screen.getByText("当前匹配 1 个节点")).toBeInTheDocument();
  expect(screen.queryByRole("searchbox", { name: "搜索节点" })).not.toBeInTheDocument();
});

it("切换单选不丢另一种形状的草稿", () => {
  const onChange = vi.fn();
  render(<Harness initial={{ ...empty(), nodeIds: new Set([1n]), selectorTags: ["prod"] }} onChange={onChange} />);
  fireEvent.click(screen.getByRole("radio", { name: "动态标签选择器" }));
  expect(onChange).toHaveBeenLastCalledWith(expect.objectContaining({ dynamic: true, allNodes: false, nodeIds: new Set([1n]), selectorTags: ["prod"] }));
  fireEvent.click(screen.getByRole("radio", { name: "全部节点" }));
  expect(onChange).toHaveBeenLastCalledWith(expect.objectContaining({ dynamic: false, allNodes: true, nodeIds: new Set([1n]), selectorTags: ["prod"] }));
  fireEvent.click(screen.getByRole("radio", { name: "指定节点" }));
  expect(screen.getByText("已选择 1 个节点")).toBeInTheDocument();
});

it("快选保留已有节点并去重，已勾选的节点可取消", async () => {
  const onChange = vi.fn();
  render(<Harness initial={{ ...empty(), nodeIds: new Set([1n, 3n]) }} onChange={onChange} />);
  fireEvent.click(await screen.findByRole("button", { name: "按标签快选 prod" }));
  expect(onChange).toHaveBeenLastCalledWith(expect.objectContaining({ nodeIds: new Set([1n, 2n, 3n]) }));
  const node = screen.getByRole("checkbox", { name: "a（#1）" });
  expect(node).toBeChecked();
  fireEvent.click(node);
  expect(onChange).toHaveBeenLastCalledWith(expect.objectContaining({ nodeIds: new Set([2n, 3n]) }));
});

it("按标签快选超过 10 个时先列前 10 个，其余收在「全部 N 个」后面", async () => {
  const many = Array.from({ length: 12 }, (_, i) => `t${String(i + 1).padStart(2, "0")}`);
  render(<Harness initial={{ ...empty(), selectorTags: many }} />);
  const toggle = await screen.findByRole("button", { name: "全部 14 个" });
  expect(toggle).toHaveAttribute("aria-expanded", "false");
  expect(screen.getAllByRole("button", { name: /^按标签快选 / })).toHaveLength(10);
  fireEvent.click(toggle);
  expect(screen.getAllByRole("button", { name: /^按标签快选 / })).toHaveLength(14);
  expect(screen.getByRole("button", { name: "收起" })).toHaveAttribute("aria-expanded", "true");
});
