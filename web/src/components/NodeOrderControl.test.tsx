import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { NodeOrderControl } from "./NodeOrderControl";

describe("节点排序手柄", () => {
  it("方向键、Home、End 走同一移动入口；序号显示传入的全序名次", () => {
    const move = vi.fn();
    render(<NodeOrderControl label="a" count={3} position={2} reorderDisabled={false} onMove={move} onDragStart={vi.fn()} onDragEnd={vi.fn()} />);
    const handle = screen.getByRole("button", { name: "调整顺序 a" });
    expect(handle).toHaveTextContent("2");
    expect(handle).toHaveAttribute("draggable", "true");
    for (const key of ["ArrowUp", "ArrowDown", "Home", "End", "Enter"]) fireEvent.keyDown(handle, { key });
    expect(move.mock.calls).toEqual([[-1], [1], ["first"], ["last"]]);
    expect(screen.queryByRole("combobox")).toBeNull();
  });

  it.each([["排序禁用", { reorderDisabled: true, count: 3 }], ["只有一个节点", { reorderDisabled: false, count: 1 }]])("%s时手柄禁用、不可拖动、按键无效", (_, props) => {
    const move = vi.fn();
    render(<NodeOrderControl label="a" position={1} onMove={move} onDragStart={vi.fn()} onDragEnd={vi.fn()} {...props} />);
    const handle = screen.getByRole("button", { name: "调整顺序 a" });
    expect(handle).toBeDisabled();
    expect(handle).toHaveAttribute("draggable", "false");
    fireEvent.keyDown(handle, { key: "Home" });
    expect(move).not.toHaveBeenCalled();
  });
});
