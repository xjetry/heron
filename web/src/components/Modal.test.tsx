import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { Drawer, Modal } from "./Modal";

const triggers: HTMLElement[] = [];
afterEach(() => triggers.splice(0).forEach((trigger) => trigger.remove()));

function opener() {
  const button = document.createElement("button");
  document.body.appendChild(button);
  button.focus();
  triggers.push(button);
  return button;
}

it("抽屉是原生 dialog：有标题名，Escape 关闭，关闭后焦点回到触发元素", () => {
  const trigger = opener();
  const onClose = vi.fn();
  const { unmount } = render(<Drawer title="编辑节点" opener={trigger} onClose={onClose}><div className="modal-body"><input data-autofocus aria-label="名称" /></div></Drawer>);
  const dialog = screen.getByRole("dialog", { name: "编辑节点" });
  expect(dialog).toHaveClass("drawer");
  expect(dialog).toHaveAttribute("open");
  expect(screen.getByLabelText("名称")).toHaveFocus();
  fireEvent(dialog, new Event("cancel", { cancelable: true }));
  expect(onClose).toHaveBeenCalledTimes(1);
  expect(screen.getByRole("button", { name: "关闭抽屉" })).toBeInTheDocument();
  unmount();
  expect(trigger).toHaveFocus();
});

it("busy 时 Escape 与关闭按钮都不关", () => {
  const onClose = vi.fn();
  render(<Drawer title="保存中" busy opener={opener()} onClose={onClose}><p /></Drawer>);
  fireEvent(screen.getByRole("dialog"), new Event("cancel", { cancelable: true }));
  const close = screen.getByRole("button", { name: "关闭抽屉" });
  expect(close).toBeDisabled();
  fireEvent.click(close);
  expect(onClose).not.toHaveBeenCalled();
});

it("Modal 默认仍是居中弹窗，关闭按钮名不变", () => {
  const onClose = vi.fn();
  render(<Modal title="移动节点" opener={opener()} onClose={onClose}><p /></Modal>);
  expect(screen.getByRole("dialog", { name: "移动节点" })).toHaveClass("admin-modal");
  fireEvent.click(screen.getByRole("button", { name: "关闭弹窗" }));
  expect(onClose).toHaveBeenCalledTimes(1);
});
