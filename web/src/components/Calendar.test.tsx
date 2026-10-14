import { fireEvent, render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { Calendar } from "./Calendar";

// 点外关闭的监听只在挂载时注册一次；父组件换了 onClose 之后，关闭走的必须是新的那个，不是挂载那一刻的。
it("点到弹层之外时调用最近一次渲染给的 onClose，焦点不还给控件", () => {
  const first = vi.fn<(returnFocus: boolean) => void>();
  const latest = vi.fn<(returnFocus: boolean) => void>();
  const { rerender } = render(<Calendar label="到期日" value="2026-10-09" onPick={vi.fn()} onClose={first} />);
  rerender(<Calendar label="到期日" value="2026-10-09" onPick={vi.fn()} onClose={latest} />);
  fireEvent.pointerDown(document.body);
  expect(latest).toHaveBeenCalledExactlyOnceWith(false);
  expect(first).not.toHaveBeenCalled();
});

it("弹层之内的点击不关闭", () => {
  const onClose = vi.fn<(returnFocus: boolean) => void>();
  render(<Calendar label="到期日" value="2026-10-09" onPick={vi.fn()} onClose={onClose} />);
  fireEvent.pointerDown(screen.getByRole("button", { name: "下个月" }));
  expect(onClose).not.toHaveBeenCalled();
});
