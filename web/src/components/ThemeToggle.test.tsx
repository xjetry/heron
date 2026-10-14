import { fireEvent, render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { ThemeToggle } from "./ThemeToggle";

it("一个按钮循环三态，title 写明当前与下一档", () => {
  const onChange = vi.fn();
  render(<ThemeToggle choice="light" onChange={onChange} />);
  const button = screen.getByRole("button", { name: "明暗切换" });
  expect(button).toHaveAttribute("title", "当前：浅色，点击切到深色");
  fireEvent.click(button);
  expect(onChange).toHaveBeenCalledWith("dark");
});

// 系统是深色时「深色」与「跟随系统」页面看不出差别，图标是唯一能告诉访客这一步切到了哪一档的东西：三档图标各不相同。
it.each([["auto", "system"], ["light", "sun"], ["dark", "moon"]] as const)("%s 档用 %s 图标", (choice, icon) => {
  render(<ThemeToggle choice={choice} onChange={() => {}} />);
  expect(screen.getByRole("button", { name: "明暗切换" }).querySelector("svg")).toHaveAttribute("data-icon", icon);
});
