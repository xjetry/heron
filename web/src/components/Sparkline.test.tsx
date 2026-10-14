import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { Sparkline } from "./Sparkline";

it.each([null, undefined, NaN, Infinity])("有限值连成折线，null 处断开成新的一段；y 按最大值归一到高度（缺读数=%s）", (gap) => {
  render(<Sparkline values={[0, 4, gap, 2, 2]} label="下行速率" width={40} height={12} />);
  const svg = screen.getByRole("img", { name: "下行速率" });
  const d = svg.querySelector("path")?.getAttribute("d");
  // 五个样本槽位等分 40px；四个有效点 x = 0, 10, 30, 40；最大值 4 贴顶（y=1），0 贴底（y=11）。
  expect(d).toBe("M0.0 11.0 L10.0 1.0 M30.0 6.0 L40.0 6.0");
});

// 空态是文字不是图：放在 preserveAspectRatio="none" 的 SVG 里会随容器拉宽，SVG <text> 的默认 fill 又是黑色、不随主题。
it.each([[], [null, undefined], [NaN, Infinity]].map((values) => ({ values })))("没有有限值时不画 SVG，用普通文本写「无读数」并进可访问名称（$values）", ({ values }) => {
  const { container } = render(<Sparkline values={values} label="上行速率" />);
  const empty = screen.getByRole("img", { name: "上行速率：无读数" });
  expect(empty.tagName).toBe("SPAN");
  expect(empty).toHaveTextContent("无读数");
  expect(container.querySelector("svg")).toBeNull();
});

it("恒为 0 画一条贴底的线，不当成无读数", () => {
  render(<Sparkline values={[0, 0]} label="x" width={10} height={12} />);
  expect(screen.getByRole("img", { name: "x" }).querySelector("path")?.getAttribute("d")).toBe("M0.0 11.0 L10.0 11.0");
});
