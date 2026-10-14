import { expect, it } from "vitest";
import { render } from "@testing-library/react";
import { queryGateAll } from "./queryGate";

it("null 是合法的就绪数据，类型与运行时都不排除", () => {
  const gate = queryGateAll({ data: null as { value: number } | null, error: null });
  if (!gate.ready) throw new Error("not ready");
  // @ts-expect-error 就绪只排除 undefined，null 不能赋给非空类型
  const declaredNonNull: { value: number } = gate.data[0];
  expect(declaredNonNull).toBeNull();
});

it("元组位置与输入查询一一对应", () => {
  const gate = queryGateAll({ data: "s", error: null }, { data: 1, error: null });
  if (!gate.ready) throw new Error("not ready");
  const text: string = gate.data[0];
  const count: number = gate.data[1];
  expect([text, count]).toEqual(["s", 1]);
});

it("同文错误去重为一条，异文全部保留", () => {
  const gate = queryGateAll(
    { data: 1, error: new Error("same") },
    { data: 2, error: new Error("same") },
    { data: 3, error: new Error("other") },
  );
  if (!gate.ready) throw new Error("not ready");
  const { queryAllByRole } = render(<>{gate.banner}</>);
  expect(queryAllByRole("alert").map((a) => a.textContent)).toEqual(["Error: same", "Error: other"]);
});

it("未就绪时全部已失败查询的错误都给出，有失败时没有加载占位", () => {
  const gate = queryGateAll(
    { data: undefined, error: new Error("first failed") },
    { data: 1, error: new Error("second failed") },
    { data: undefined, error: null },
  );
  if (gate.ready) throw new Error("ready");
  expect(gate.errors.map((e) => String(e))).toEqual(["Error: first failed", "Error: second failed"]);
  expect(gate.loading).toBeNull();
});

it("只有加载中时给出加载占位", () => {
  const gate = queryGateAll({ data: undefined, error: null }, { data: 1, error: null });
  if (gate.ready) throw new Error("ready");
  expect(gate.errors).toEqual([]);
  const { getByText } = render(<>{gate.loading}</>);
  expect(getByText("加载中…")).toBeInTheDocument();
});
