import { fireEvent, screen, within } from "@testing-library/react";

// 分段日期 / 时刻控件（components/DateInput.tsx）的测试入口：控件是一个以 label 为名的 group，里面每段一个文本框。
type Scope = Pick<typeof screen, "getByRole">;
const segmentsOf = (label: string, scope: Scope) => within(scope.getByRole("group", { name: label })).getAllByRole<HTMLInputElement>("textbox");
const SEPARATORS: Record<number, string[]> = { 2: ["", ":"], 3: ["", "-", "-"], 5: ["", "-", "-", "T", ":"] };

// 按控件的标准写法（YYYY-MM-DD、HH:MM、YYYY-MM-DDTHH:MM）逐段填入；"" 清空全部分段。
export function fillSegments(label: string, value: string, scope: Scope = screen) {
  const inputs = segmentsOf(label, scope);
  const parts = value === "" ? [] : value.split(/[-T:]/);
  inputs.forEach((input, i) => fireEvent.change(input, { target: { value: parts[i] ?? "" } }));
}

// 把各段显示的内容按标准写法拼回；全空时是 ""。
export function segmentsValue(label: string, scope: Scope = screen): string {
  const parts = segmentsOf(label, scope).map((input) => input.value);
  if (parts.every((p) => p === "")) return "";
  return parts.map((p, i) => SEPARATORS[parts.length][i] + p).join("");
}
