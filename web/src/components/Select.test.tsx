import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { expect, it, vi } from "vitest";
import { Select } from "./Select";

type Sort = "default" | "expiry" | "cpu";
const options: { value: Sort; label: string }[] = [{ value: "default", label: "默认" }, { value: "expiry", label: "到期" }, { value: "cpu", label: "CPU" }];

function Harness({ onChange }: { onChange?: (next: Sort) => void }) {
  const [value, setValue] = useState<Sort>("expiry");
  return (
    <>
      <Select label="排序" value={value} options={options} onChange={(next) => { setValue(next); onChange?.(next); }} />
      <button type="button">外面</button>
    </>
  );
}
const trigger = () => screen.getByRole("button", { name: /^排序 / });
const option = (name: string) => screen.getByRole("option", { name });

it("收起时只有写着当前值的触发按钮；点开后是自绘列表，当前项标为选中并获得焦点", () => {
  render(<Harness />);
  expect(trigger()).toHaveAccessibleName("排序 到期");
  expect(trigger()).toHaveAttribute("aria-haspopup", "listbox");
  expect(screen.queryByRole("listbox")).toBeNull();
  fireEvent.click(trigger());
  expect(trigger()).toHaveAttribute("aria-expanded", "true");
  expect(trigger()).toHaveAttribute("aria-controls", screen.getByRole("listbox", { name: "排序" }).id);
  expect(screen.getAllByRole("option").map((o) => [o.textContent, o.getAttribute("aria-selected")])).toEqual([["默认", "false"], ["到期", "true"], ["CPU", "false"]]);
  expect(option("到期")).toHaveFocus();
});

it("点选项即选中并收起，焦点回到触发按钮；点当前项不回调", () => {
  const onChange = vi.fn();
  render(<Harness onChange={onChange} />);
  fireEvent.click(trigger());
  fireEvent.click(option("CPU"));
  expect(onChange).toHaveBeenCalledWith("cpu");
  expect(screen.queryByRole("listbox")).toBeNull();
  expect(trigger()).toHaveAccessibleName("排序 CPU");
  expect(trigger()).toHaveFocus();
  fireEvent.click(trigger());
  fireEvent.click(option("CPU"));
  expect(onChange).toHaveBeenCalledTimes(1);
});

it("键盘：下键打开，上下键 / Home / End 移动且停在两端，Enter 与空格选中", () => {
  render(<Harness />);
  fireEvent.keyDown(trigger(), { key: "ArrowDown" });
  const list = screen.getByRole("listbox");
  expect(option("到期")).toHaveFocus();
  fireEvent.keyDown(list, { key: "ArrowDown" });
  expect(option("CPU")).toHaveFocus();
  fireEvent.keyDown(list, { key: "ArrowDown" });
  expect(option("CPU")).toHaveFocus();
  fireEvent.keyDown(list, { key: "Home" });
  expect(option("默认")).toHaveFocus();
  fireEvent.keyDown(list, { key: "ArrowUp" });
  expect(option("默认")).toHaveFocus();
  fireEvent.keyDown(list, { key: "End" });
  expect(option("CPU")).toHaveFocus();
  fireEvent.keyDown(list, { key: "Enter" });
  expect(trigger()).toHaveAccessibleName("排序 CPU");
  expect(trigger()).toHaveFocus();
  fireEvent.keyDown(trigger(), { key: "ArrowUp" });
  fireEvent.keyDown(screen.getByRole("listbox"), { key: "ArrowUp" });
  fireEvent.keyDown(screen.getByRole("listbox"), { key: " " });
  expect(trigger()).toHaveAccessibleName("排序 到期");
});

it("Tab 收起不改值，焦点先回到触发按钮、不拦默认动作（浏览器从触发按钮出发移动焦点，e2e 核对落点）", () => {
  render(<Harness />);
  fireEvent.click(trigger());
  const list = screen.getByRole("listbox");
  fireEvent.keyDown(list, { key: "ArrowDown" });
  expect(fireEvent.keyDown(list, { key: "Tab" })).toBe(true);
  expect(screen.queryByRole("listbox")).toBeNull();
  expect(trigger()).toHaveFocus();
  expect(trigger()).toHaveAccessibleName("排序 到期");
});

it("Esc 收起不改值并还焦点；点外面收起", () => {
  render(<Harness />);
  fireEvent.click(trigger());
  fireEvent.keyDown(screen.getByRole("listbox"), { key: "ArrowDown" });
  fireEvent.keyDown(screen.getByRole("listbox"), { key: "Escape" });
  expect(screen.queryByRole("listbox")).toBeNull();
  expect(trigger()).toHaveAccessibleName("排序 到期");
  expect(trigger()).toHaveFocus();
  fireEvent.click(trigger());
  fireEvent.pointerDown(screen.getByRole("button", { name: "外面" }));
  expect(screen.queryByRole("listbox")).toBeNull();
  expect(trigger()).toHaveAttribute("aria-expanded", "false");
});
