import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { StatusBadge } from "./StatusBadge";
import { Bar } from "./Bar";

it("徽章是色点加文字：文字是可访问名，色点对读屏隐藏，状态写在 data-status 上", () => {
  render(<StatusBadge status="offline" detail="3 天前" />);
  const badge = screen.getByText("离线 · 3 天前");
  expect(badge).toHaveAttribute("data-status", "offline");
  expect(badge.querySelector(".status-dot")).toHaveAttribute("aria-hidden", "true");
  expect(screen.queryByRole("img")).toBeNull();
});

it("没有补充文字时只有状态名", () => {
  render(<StatusBadge status="maintenance" />);
  expect(screen.getByText("维护中")).toHaveAttribute("data-status", "maintenance");
});

it.each([[42, "neutral"], [75, "attention"], [95, "critical"]] as const)("进度条 %d 的档位是 %s", (value, level) => {
  render(<Bar value={value} label={`${value}%`} thin />);
  const meter = screen.getByRole("meter", { name: `${value}%` });
  expect(meter).toHaveAttribute("data-level", level);
  expect(meter).toHaveClass("thin");
});
