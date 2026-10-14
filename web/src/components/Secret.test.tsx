import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { Secret } from "./Secret";

afterEach(() => { vi.unstubAllGlobals(); window.getSelection()?.removeAllRanges(); });

it.each(["copied", "failed"])("凭据变化清除 %s 反馈", async (result) => {
  vi.stubGlobal("navigator", { clipboard: { writeText: async () => { if (result === "failed") throw new Error("denied"); } } });
  const { rerender } = render(<Secret label="token" value="A" />);
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "复制" })); });
  rerender(<Secret label="token" value="B" />);
  expect({ button: screen.getByRole("button").textContent, feedback: screen.queryByRole("status")?.textContent }).toEqual({ button: "复制", feedback: "" });
});

it.each(["copied", "failed"])("旧凭据迟到的 %s 结果不影响新凭据", async (result) => {
  let finish!: () => void;
  const pending = new Promise<void>((resolve, reject) => { finish = () => result === "copied" ? resolve() : reject(new Error("denied")); });
  vi.stubGlobal("navigator", { clipboard: { writeText: () => pending } });
  const { rerender } = render(<Secret label="token" value="A" />);
  fireEvent.click(screen.getByRole("button", { name: "复制" }));
  rerender(<Secret label="token" value="B" />);
  await act(async () => { finish(); });
  expect({ button: screen.getByRole("button").textContent, feedback: screen.queryByRole("status")?.textContent, selection: window.getSelection()?.toString() }).toEqual({ button: "复制", feedback: "", selection: "" });
});

it.each(["missing", "rejected"])("剪贴板 %s 时反馈失败并选中明文", async (reason) => {
  vi.stubGlobal("navigator", reason === "missing" ? {} : { clipboard: { writeText: async () => { throw new Error("denied"); } } });
  render(<Secret label="token" value="select-this-secret" />);
  fireEvent.click(screen.getByRole("button", { name: "复制" }));
  // 提示区常驻，等区域出现不等于等到反馈。
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("复制失败，请手动选择"));
  expect(window.getSelection()?.toString()).toBe("select-this-secret");
});
