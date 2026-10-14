import { fireEvent, render, screen, within } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { expect, it, vi } from "vitest";
import { RowMenu } from "./RowMenu";

function mount(items: Parameters<typeof RowMenu>[0]["items"]) {
  const router = createMemoryRouter([{ path: "/", element: <RowMenu label="web-01（#1）" items={items} /> }, { path: "/nodes/1", element: <p>详情页</p> }], { initialEntries: ["/"] });
  render(<RouterProvider router={router} />);
  return router;
}

it("触发器与菜单的可访问名沿用「动词 节点名（#id）」，链接项导航，普通项回调并关闭", () => {
  const onEdit = vi.fn();
  const router = mount([{ label: "编辑", onSelect: onEdit }, { label: "查看详情", to: "/nodes/1" }]);
  const trigger = screen.getByRole("button", { name: "更多操作 web-01（#1）" });
  expect(trigger).toHaveAttribute("aria-haspopup", "menu");
  expect(trigger).toHaveAttribute("aria-expanded", "false");
  expect(screen.queryByRole("menu")).toBeNull();
  fireEvent.click(trigger);
  expect(trigger).toHaveAttribute("aria-expanded", "true");
  const menu = screen.getByRole("menu", { name: "web-01（#1） 的操作" });
  expect(within(menu).getAllByRole("menuitem").map((el) => el.getAttribute("aria-label"))).toEqual(["编辑 web-01（#1）", "查看详情 web-01（#1）"]);
  fireEvent.click(within(menu).getByRole("menuitem", { name: "编辑 web-01（#1）" }));
  expect(onEdit).toHaveBeenCalledWith(trigger);
  expect(screen.queryByRole("menu")).toBeNull();
  fireEvent.click(trigger);
  fireEvent.click(screen.getByRole("menuitem", { name: "查看详情 web-01（#1）" }));
  expect(router.state.location.pathname).toBe("/nodes/1");
});

it("危险项两段式：首击只武装，再击才执行；Escape 关闭并撤销武装、焦点回到触发器", () => {
  const onDelete = vi.fn();
  mount([{ label: "删除", danger: true, confirm: "确认删除 web-01（#1）", onSelect: onDelete }]);
  const trigger = screen.getByRole("button", { name: "更多操作 web-01（#1）" });
  fireEvent.click(trigger);
  fireEvent.click(screen.getByRole("menuitem", { name: "删除 web-01（#1）" }));
  expect(onDelete).not.toHaveBeenCalled();
  expect(screen.getByRole("menuitem", { name: "确认删除 web-01（#1）" })).toBeInTheDocument();
  expect(screen.getByRole("menuitem", { name: "取消" })).toBeInTheDocument();
  fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
  expect(screen.queryByRole("menu")).toBeNull();
  expect(trigger).toHaveFocus();
  fireEvent.click(trigger);
  expect(screen.getByRole("menuitem", { name: "删除 web-01（#1）" })).toBeInTheDocument();
  fireEvent.click(screen.getByRole("menuitem", { name: "删除 web-01（#1）" }));
  fireEvent.click(screen.getByRole("menuitem", { name: "确认删除 web-01（#1）" }));
  expect(onDelete).toHaveBeenCalledTimes(1);
  expect(screen.queryByRole("menu")).toBeNull();
});

it("方向键在项间循环移动焦点，禁用项仍可到达但不可选", () => {
  const onRotate = vi.fn();
  mount([{ label: "编辑", onSelect: () => {} }, { label: "换 token", onSelect: onRotate, disabled: true }, { label: "删除", danger: true, confirm: "确认删除 web-01（#1）", onSelect: () => {} }]);
  fireEvent.click(screen.getByRole("button", { name: "更多操作 web-01（#1）" }));
  const menu = screen.getByRole("menu");
  const items = within(menu).getAllByRole("menuitem");
  expect(items[0]).toHaveFocus();
  expect(items[1]).toHaveAttribute("aria-disabled", "true");
  fireEvent.keyDown(menu, { key: "ArrowDown" });
  expect(items[1]).toHaveFocus();
  fireEvent.click(items[1]);
  expect(onRotate).not.toHaveBeenCalled();
  fireEvent.keyDown(menu, { key: "ArrowUp" });
  fireEvent.keyDown(menu, { key: "ArrowUp" });
  expect(items[2]).toHaveFocus();
  fireEvent.keyDown(menu, { key: "Home" });
  expect(items[0]).toHaveFocus();
  fireEvent.keyDown(menu, { key: "End" });
  expect(items[2]).toHaveFocus();
  fireEvent.keyDown(menu, { key: "ArrowDown" });
  expect(items[0]).toHaveFocus();
});

it("鼠标武装危险项后焦点留在确认项，后续 Escape 能关闭菜单", () => {
  mount([{ label: "编辑" }, { label: "删除", confirm: "确认删除 web-01（#1）" }]);
  fireEvent.click(screen.getByRole("button", { name: "更多操作 web-01（#1）" }));
  fireEvent.click(screen.getByRole("menuitem", { name: "删除 web-01（#1）" }));
  expect(screen.getByRole("menuitem", { name: "确认删除 web-01（#1）" })).toHaveFocus();
});

it("点菜单外关闭并撤销武装", () => {
  mount([{ label: "删除", confirm: "确认删除 web-01（#1）", onSelect: () => {} }]);
  const trigger = screen.getByRole("button", { name: "更多操作 web-01（#1）" });
  fireEvent.click(trigger);
  fireEvent.click(screen.getByRole("menuitem", { name: "删除 web-01（#1）" }));
  fireEvent.pointerDown(screen.getByRole("menu"));
  expect(screen.getByRole("menu")).toBeInTheDocument();
  fireEvent.pointerDown(document.body);
  expect(screen.queryByRole("menu")).toBeNull();
  fireEvent.click(trigger);
  expect(screen.getByRole("menuitem", { name: "删除 web-01（#1）" })).toBeInTheDocument();
});

it("武装后的危险项下方显示附注，取消后附注消失", () => {
  mount([{ label: "删除", danger: true, confirm: "确认删除 web-01（#1）", note: "它是登录通知唯一的接收渠道，删除后登录通知关闭。", onSelect: () => {} }]);
  fireEvent.click(screen.getByRole("button", { name: "更多操作 web-01（#1）" }));
  expect(screen.queryByText(/唯一的接收渠道/)).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("menuitem", { name: "删除 web-01（#1）" }));
  expect(screen.getByText("它是登录通知唯一的接收渠道，删除后登录通知关闭。")).toHaveClass("row-menu-note");
  fireEvent.click(screen.getByRole("menuitem", { name: "取消" }));
  expect(screen.queryByText(/唯一的接收渠道/)).not.toBeInTheDocument();
});

it("列表刷新替换操作数组时保持菜单、武装与焦点，确认使用最新回调", () => {
  const oldSelect = vi.fn();
  const newSelect = vi.fn();
  const items = (onSelect: () => void) => [{ label: "编辑", onSelect: () => {} }, { label: "删除", confirm: "确认删除 web-01（#1）", onSelect }];
  const { rerender } = render(<RowMenu label="web-01（#1）" items={items(oldSelect)} />);
  fireEvent.click(screen.getByRole("button", { name: "更多操作 web-01（#1）" }));
  fireEvent.keyDown(screen.getByRole("menu"), { key: "ArrowDown" });
  fireEvent.click(screen.getByRole("menuitem", { name: "删除 web-01（#1）" }));
  const confirmation = screen.getByRole("menuitem", { name: "确认删除 web-01（#1）" });
  rerender(<RowMenu label="web-01（#1）" items={items(newSelect)} />);
  expect(screen.getByRole("menuitem", { name: "确认删除 web-01（#1）" })).toBe(confirmation);
  expect(confirmation).toHaveFocus();
  fireEvent.click(confirmation);
  expect(oldSelect).not.toHaveBeenCalled();
  expect(newSelect).toHaveBeenCalledTimes(1);
});

it("数据刷新改写某项的 label 时该项不重挂，焦点留在原按钮上", () => {
  const items = (label: string) => [{ label: "编辑", onSelect: () => {} }, { label, onSelect: () => {} }];
  const { rerender } = render(<RowMenu label="web-01（#1）" items={items("停用")} />);
  fireEvent.click(screen.getByRole("button", { name: "更多操作 web-01（#1）" }));
  fireEvent.keyDown(screen.getByRole("menu"), { key: "ArrowDown" });
  const toggle = screen.getByRole("menuitem", { name: "停用 web-01（#1）" });
  expect(toggle).toHaveFocus();
  rerender(<RowMenu label="web-01（#1）" items={items("启用")} />);
  expect(screen.getByRole("menuitem", { name: "启用 web-01（#1）" })).toBe(toggle);
  expect(toggle).toHaveFocus();
});
