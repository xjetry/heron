import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { CopyableText } from "./CopyableText";

afterEach(() => { vi.unstubAllGlobals(); window.getSelection()?.removeAllRanges(); });

// 反馈只属于产生它的那段文本：换成另一段文本时回到"复制"，换回来也不复用旧反馈。
it.each([
  ["copied", async () => {}, "已复制"],
  ["failed", async () => { throw new Error("denied"); }, "复制失败，请手动选择"],
] as const)("%s 的反馈随文本切换重置", async (_name, writeText, shown) => {
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  const { rerender } = render(<CopyableText label="注册 key" value="first" />);
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: /复制/ })); });
  expect(screen.getByText(shown)).toBeInTheDocument();
  rerender(<CopyableText label="注册 key" value="second" />);
  expect(screen.getByRole("button")).toHaveTextContent(/^复制$/);
  expect(screen.getByRole("status")).toHaveTextContent("");
  rerender(<CopyableText label="注册 key" value="first" />);
  expect(screen.getByRole("button")).toHaveTextContent(/^复制$/);
  expect(screen.getByRole("status")).toHaveTextContent("");
});

it("文本不变时重渲染保留反馈", async () => {
  vi.stubGlobal("navigator", { clipboard: { writeText: async () => {} } });
  const { rerender } = render(<CopyableText label="注册 key" value="same" />);
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: /复制/ })); });
  rerender(<CopyableText label="注册 key" value="same" copyLabel="复制 key" />);
  expect(screen.getByRole("button", { name: "复制 key" })).toHaveTextContent("已复制");
});
