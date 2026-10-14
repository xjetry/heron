import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { PageHeader } from "./PageHeader";

it("页头在直接子容器中渲染一级标题", () => {
  const { container } = render(<PageHeader title="节点" />);
  expect(container.querySelector("header.page-header > div > h1")).toHaveTextContent("节点");
});

it("提供说明时在标题容器中显示弱化段落", () => {
  const { container } = render(<PageHeader title="节点" description="管理节点资料" />);
  expect(container.querySelector("header.page-header > div > p.muted")).toHaveTextContent("管理节点资料");
});

it("省略说明时不渲染段落", () => {
  const { container } = render(<PageHeader title="节点" />);
  expect(container.querySelector("p")).toBeNull();
});

it("提供操作时在页头操作容器中显示按钮", () => {
  const { container } = render(<PageHeader title="节点" actions={<button type="button">添加节点</button>} />);
  expect(container.querySelector("header.page-header > div.page-header-actions")).toContainElement(screen.getByRole("button", { name: "添加节点" }));
});

it("省略操作时不渲染操作容器", () => {
  const { container } = render(<PageHeader title="节点" />);
  expect(container.querySelector(".page-header-actions")).toBeNull();
});
