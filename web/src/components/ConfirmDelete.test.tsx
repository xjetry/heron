import { expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { ConfirmDelete } from "./ConfirmDelete";

it("首击只进入确认态，确认才执行", () => {
  const onDelete = vi.fn();
  render(<ConfirmDelete label="删除 东京" confirm="确认删除 东京" pending={false} onDelete={onDelete} />);
  fireEvent.click(screen.getByRole("button", { name: "删除 东京" }));
  expect(onDelete).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "确认删除 东京" }));
  expect(onDelete).toHaveBeenCalledTimes(1);
});

it("执行挂起时确认按钮禁用", () => {
  render(<ConfirmDelete label="删除 东京" confirm="确认删除 东京" pending={true} onDelete={() => {}} />);
  fireEvent.click(screen.getByRole("button", { name: "删除 东京" }));
  expect(screen.getByRole("button", { name: "确认删除 东京" })).toBeDisabled();
});

it("取消回到首击按钮，全程不执行", () => {
  const onDelete = vi.fn();
  render(<ConfirmDelete label="删除 东京" confirm="确认删除 东京" pending={false} onDelete={onDelete} />);
  fireEvent.click(screen.getByRole("button", { name: "删除 东京" }));
  fireEvent.click(screen.getByRole("button", { name: "取消删除 东京" }));
  expect(screen.getByRole("button", { name: "删除 东京" })).toBeInTheDocument();
  expect(onDelete).not.toHaveBeenCalled();
});

it("两行同时武装时取消按钮可区分", () => {
  render(
    <>
      <ConfirmDelete label="删除 东京" confirm="确认删除 东京" pending={false} onDelete={() => {}} />
      <ConfirmDelete label="删除 大阪" confirm="确认删除 大阪" pending={false} onDelete={() => {}} />
    </>,
  );
  fireEvent.click(screen.getByRole("button", { name: "删除 东京" }));
  fireEvent.click(screen.getByRole("button", { name: "删除 大阪" }));
  expect(screen.getByRole("button", { name: "取消删除 东京" })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "取消删除 大阪" })).toBeInTheDocument();
});

it("确认态显示可选说明文字", () => {
  render(<ConfirmDelete label="删除 东京" confirm="确认删除 东京" note="历史保留至到期清理" pending={false} onDelete={() => {}} />);
  fireEvent.click(screen.getByRole("button", { name: "删除 东京" }));
  expect(screen.getByText("历史保留至到期清理")).toBeInTheDocument();
});

it("自定义动词同时出现在可见文字与可访问名", () => {
  render(<ConfirmDelete label="吊销 ci（#2）" confirm="确认吊销 ci（#2）" verb="吊销" pending={false} onDelete={() => {}} />);
  expect(screen.getByRole("button", { name: "吊销 ci（#2）" })).toHaveTextContent("吊销");
});
