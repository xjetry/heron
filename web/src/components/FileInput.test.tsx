import { fireEvent, render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { FileInput } from "./FileInput";

it("按钮与文件名是自绘文字，选择落到原生输入上，文件名随调用方给出", () => {
  const onChange = vi.fn();
  const { rerender } = render(<FileInput label="主题包" caption="主题包（zip）" accept=".zip" onChange={onChange} />);
  const input = screen.getByLabelText("主题包");
  expect(input).toHaveAttribute("type", "file");
  expect(input).toHaveAttribute("accept", ".zip");
  expect(screen.getByText("选择文件")).toHaveAttribute("aria-hidden", "true");
  expect(screen.getByText("未选择文件")).toHaveClass("muted");
  expect(screen.getByText("主题包（zip）")).toBeInTheDocument();
  fireEvent.change(input, { target: { files: [new File(["x"], "theme.zip")] } });
  expect(onChange).toHaveBeenCalledTimes(1);
  rerender(<FileInput label="主题包" accept=".zip" onChange={onChange} fileName="theme.zip" disabled />);
  expect(screen.getByText("theme.zip")).not.toHaveClass("muted");
  expect(screen.getByLabelText("主题包")).toBeDisabled();
});
