import { expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { Picks } from "./Picks";

it("分组展示候选项并按勾选回传新集合", () => {
  const spy = vi.fn();
  render(<Picks legend="作用域节点" items={[{ id: 1n, name: "东京" }, { id: 2n, name: "法兰克福" }]} selected={new Set([1n])} onChange={spy} />);
  expect(screen.getByRole("group", { name: "作用域节点" })).toBeInTheDocument();
  expect(screen.getByRole("checkbox", { name: "东京（#1）" })).toBeChecked();
  expect(screen.getByRole("checkbox", { name: "法兰克福（#2）" })).not.toBeChecked();
  fireEvent.click(screen.getByRole("checkbox", { name: "法兰克福（#2）" }));
  expect(spy).toHaveBeenCalledTimes(1);
  expect(spy.mock.calls[0][0]).toEqual(new Set([1n, 2n]));
});

it("候选为空时显示提示而不是空框", () => {
  render(<Picks legend="作用域节点" items={[]} selected={new Set()} onChange={() => {}} />);
  expect(screen.getByRole("group", { name: "作用域节点" })).toHaveTextContent("没有可选项。");
  expect(screen.queryAllByRole("checkbox")).toEqual([]);
});

it("同名候选项按 id 勾选", () => {
  const spy = vi.fn();
  render(<Picks legend="作用域节点" items={[{ id: 1n, name: "东京" }, { id: 2n, name: "东京" }]} selected={new Set()} onChange={spy} />);
  // 带 id 时只命中第二项；名称不含 id 时两项同名，getBy 必须报多个。
  fireEvent.click(screen.getByRole("checkbox", { name: (n) => n === "东京（#2）" || n === "东京" }));
  expect(spy).toHaveBeenCalledTimes(1);
  expect(spy.mock.calls[0][0]).toEqual(new Set([2n]));
});
