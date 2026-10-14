import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { NodeCreateDrawer } from "./NodeCreateDrawer";

const triggers: HTMLElement[] = [];
afterEach(() => triggers.splice(0).forEach((trigger) => trigger.remove()));

function opener() {
  const button = document.createElement("button");
  document.body.appendChild(button);
  triggers.push(button);
  return button;
}

// 未勾选不带 rented：缺席即 false，与「没填计费就不带 billing」同一条缺席语义。勾选才显式 rented: true。
it("勾选已出租时创建请求带 rented，未勾选不带", () => {
  const onCreate = vi.fn();
  render(<NodeCreateDrawer opener={opener()} pending={false} error={null} onClose={() => {}} onCreate={onCreate} />);
  fireEvent.change(screen.getByLabelText("新节点名称"), { target: { value: "leased" } });
  const box = screen.getByRole("checkbox", { name: "已出租（这台机器不装 agent）" });
  expect(box).not.toBeChecked();
  fireEvent.click(screen.getByRole("button", { name: "创建" }));
  expect(onCreate).toHaveBeenCalledWith({ name: "leased" });
  fireEvent.click(box);
  fireEvent.click(screen.getByRole("button", { name: "创建" }));
  expect(onCreate).toHaveBeenLastCalledWith({ name: "leased", rented: true });
});

const installHint = "创建后在这里直接给出安装命令；token 仅用于注册 agent，不能上报指标。计费可留空，稍后在编辑里补。";
const rentedHint = "已出租的机器不装 agent：创建后不给安装命令，之后取消已出租再从节点行菜单取。计费可留空，稍后在编辑里补。";

it("说明随已出租切换：未勾仍是安装命令，勾了就不给安装命令", () => {
  render(<NodeCreateDrawer opener={opener()} pending={false} error={null} onClose={() => {}} onCreate={() => {}} />);
  const dialog = screen.getByRole("dialog", { name: "添加节点" });
  expect(dialog).toHaveAccessibleDescription(installHint);
  fireEvent.click(screen.getByRole("checkbox", { name: "已出租（这台机器不装 agent）" }));
  expect(dialog).toHaveAccessibleDescription(rentedHint);
  fireEvent.click(screen.getByRole("checkbox", { name: "已出租（这台机器不装 agent）" }));
  expect(dialog).toHaveAccessibleDescription(installHint);
});
