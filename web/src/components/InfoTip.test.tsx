import { fireEvent, render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { InfoTip } from "./InfoTip";

it("说明文字始终在 DOM 里并由按钮 aria-describedby 指向；点击、聚焦展开，Escape 收起", () => {
  render(<InfoTip>级别 5m，每点 300 秒</InfoTip>);
  const button = screen.getByRole("button", { name: "说明" });
  const tip = screen.getByRole("tooltip");
  expect(tip).toHaveTextContent("级别 5m，每点 300 秒");
  expect(button).toHaveAttribute("aria-describedby", tip.id);
  expect(button).toHaveAttribute("aria-expanded", "false");
  fireEvent.click(button);
  expect(button).toHaveAttribute("aria-expanded", "true");
  fireEvent.keyDown(button, { key: "Escape" });
  expect(button).toHaveAttribute("aria-expanded", "false");
  fireEvent.focus(button);
  expect(button).toHaveAttribute("aria-expanded", "true");
  fireEvent.blur(button);
  expect(button).toHaveAttribute("aria-expanded", "false");
});

it("聚焦后首次点击保持说明展开", () => {
  render(<InfoTip>说明正文</InfoTip>);
  const button = screen.getByRole("button", { name: "说明" });
  fireEvent.focus(button);
  fireEvent.click(button);
  expect(button).toHaveAttribute("aria-expanded", "true");
  expect(button.parentElement).toHaveAttribute("data-open", "true");
  fireEvent.blur(button);
  fireEvent.click(button);
  expect(button).toHaveAttribute("aria-expanded", "false");
  expect(button.parentElement).not.toHaveAttribute("data-open");
});

it("悬停暂时展开，点击固定后离开仍展开，Escape 同时清除两种状态", () => {
  render(<InfoTip>说明正文</InfoTip>);
  const button = screen.getByRole("button", { name: "说明" });
  const wrapper = button.parentElement!;
  fireEvent.mouseEnter(wrapper);
  expect(button).toHaveAttribute("aria-expanded", "true");
  fireEvent.mouseLeave(wrapper);
  expect(button).toHaveAttribute("aria-expanded", "false");
  fireEvent.mouseEnter(wrapper);
  fireEvent.click(button);
  fireEvent.mouseLeave(wrapper);
  expect(button).toHaveAttribute("aria-expanded", "true");
  fireEvent.focus(button);
  fireEvent.keyDown(button, { key: "Escape" });
  expect(button).toHaveAttribute("aria-expanded", "false");
});

it("再次点击取消固定时，聚焦仍保持暂时展开，Escape 才收起", () => {
  render(<InfoTip label="口径">说明正文</InfoTip>);
  const button = screen.getByRole("button", { name: "口径" });
  fireEvent.focus(button);
  fireEvent.click(button);
  fireEvent.click(button);
  expect(button).toHaveAttribute("aria-expanded", "true");
  fireEvent.keyDown(button, { key: "Escape" });
  expect(button).toHaveAttribute("aria-expanded", "false");
});
