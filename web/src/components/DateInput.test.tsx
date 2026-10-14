import { useState } from "react";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { DateInput, DateTimeInput, TimeInput } from "./DateInput";

function Fixture({ initial = "", changed = (_value: string) => {} }) {
  const [value, setValue] = useState(initial);
  return <form aria-label="计费"><DateInput label="到期日" caption="到期日" value={value} onChange={value => { setValue(value); changed(value); }} /></form>;
}
const segment = (name: string) => screen.getByRole<HTMLInputElement>("textbox", { name: `到期日 ${name}` });
const shown = () => ["年", "月", "日"].map(name => segment(name).value);

it("四位年份进入月份，两位月份进入日期，输出标准日期", () => {
  const changed = vi.fn();
  render(<Fixture initial="2026-09-29" changed={changed} />);
  fireEvent.change(segment("年"), { target: { value: "203" } });
  expect(segment("月")).not.toHaveFocus();
  expect(screen.getByRole("form")).toBeInvalid();
  fireEvent.change(segment("年"), { target: { value: "2031" } });
  expect(segment("月")).toHaveFocus();
  fireEvent.change(segment("月"), { target: { value: "12" } });
  expect(segment("日")).toHaveFocus();
  fireEvent.change(segment("日"), { target: { value: "25" } });
  expect(changed).toHaveBeenLastCalledWith("2031-12-25");
  expect(screen.getByRole("form")).toBeValid();
});

it("月日不足两位在齐全时补零，未齐全时原样输出", () => {
  const changed = vi.fn();
  render(<Fixture changed={changed} />);
  fireEvent.change(segment("年"), { target: { value: "2031" } });
  expect(changed).toHaveBeenLastCalledWith("2031--");
  fireEvent.change(segment("月"), { target: { value: "2" } });
  fireEvent.change(segment("日"), { target: { value: "3" } });
  expect(changed).toHaveBeenLastCalledWith("2031-02-03");
});

it("年份不能超过四位，月日不能超过两位，非数字被丢弃", () => {
  render(<Fixture />);
  for (const [name, input, result, limit] of [["年", "203012", "2030", "4"], ["月", "1a23", "12", "2"], ["日", "251", "25", "2"]]) {
    fireEvent.change(segment(name), { target: { value: input } });
    expect(segment(name)).toHaveValue(result);
    expect(segment(name)).toHaveAttribute("maxlength", limit);
  }
});

it.each(["2031-02-29", "2032-04-31", "0000-01-01", "2032-13-01", "2032-01-00", "203-01-01", "2032--01"])("不允许保存无效日期 %s", initial => {
  render(<Fixture initial={initial} />);
  expect(screen.getByRole("form")).toBeInvalid();
  expect(screen.getByText("请输入四位年份和有效的完整日期")).toHaveClass("error");
});

it.each(["2032-02-29", "0001-01-01", "9999-12-31", ""])("允许保存日期 %s", initial => {
  render(<Fixture initial={initial} />);
  expect(screen.getByRole("form")).toBeValid();
});

it("输入分隔符跳到下一段，空段退格回到上一段", () => {
  render(<Fixture />);
  fireEvent.change(segment("年"), { target: { value: "2031" } });
  fireEvent.change(segment("月"), { target: { value: "3" } });
  fireEvent.keyDown(segment("月"), { key: "-" });
  expect(segment("日")).toHaveFocus();
  fireEvent.keyDown(segment("日"), { key: "Backspace" });
  expect(segment("月")).toHaveFocus();
});

it("清空全部分段输出空值，而不是残缺日期；清除按钮同样复位并回到年份", () => {
  const changed = vi.fn();
  render(<Fixture initial="2032-02-29" changed={changed} />);
  for (const name of ["年", "月", "日"]) fireEvent.change(segment(name), { target: { value: "" } });
  expect(changed).toHaveBeenLastCalledWith("");
  expect(screen.getByRole("form")).toBeValid();
  fireEvent.change(segment("年"), { target: { value: "2030" } });
  fireEvent.click(screen.getByRole("button", { name: "清除到期日" }));
  expect(shown()).toEqual(["", "", ""]);
  expect(changed).toHaveBeenLastCalledWith("");
  expect(segment("年")).toHaveFocus();
});

it("月历从当前值所在的月份打开，点选写入全部分段并把焦点还给按钮", () => {
  const changed = vi.fn();
  render(<Fixture initial="2032-02-10" changed={changed} />);
  const trigger = screen.getByRole("button", { name: "选择到期日" });
  fireEvent.click(trigger);
  const calendar = screen.getByRole("dialog", { name: "选择到期日" });
  expect(within(calendar).getByText("2032 年 2 月")).toBeInTheDocument();
  expect(within(calendar).getByRole("button", { name: "2032-02-10" })).toHaveAttribute("aria-pressed", "true");
  expect(within(calendar).getByRole("button", { name: "2032-02-10" })).toHaveFocus();
  // 周一开头：2032-02-01 是星期日，首格是 1 月 26 日（星期一），淡显但可选。
  expect(within(calendar).getAllByRole("button", { name: /^\d{4}-/ })[0]).toHaveAccessibleName("2032-01-26");
  fireEvent.click(within(calendar).getByRole("button", { name: "2032-02-29" }));
  expect(changed).toHaveBeenLastCalledWith("2032-02-29");
  expect(shown()).toEqual(["2032", "02", "29"]);
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(trigger).toHaveFocus();
});

it("月历键盘：方向键按日与周移动，PageDown 换月时夹到月末，Esc 关闭并还焦点", () => {
  render(<Fixture initial="2032-01-31" />);
  fireEvent.click(screen.getByRole("button", { name: "选择到期日" }));
  const calendar = screen.getByRole("dialog", { name: "选择到期日" });
  const focused = () => (document.activeElement as HTMLElement).getAttribute("aria-label");
  fireEvent.keyDown(document.activeElement!, { key: "PageDown" });
  expect(focused()).toBe("2032-02-29");
  expect(within(calendar).getByText("2032 年 2 月")).toBeInTheDocument();
  fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" });
  expect(focused()).toBe("2032-03-01");
  fireEvent.keyDown(document.activeElement!, { key: "ArrowUp" });
  expect(focused()).toBe("2032-02-23");
  fireEvent.keyDown(document.activeElement!, { key: "Home" });
  expect(focused()).toBe("2032-02-23");
  fireEvent.keyDown(document.activeElement!, { key: "End" });
  expect(focused()).toBe("2032-02-29");
  fireEvent.keyDown(document.activeElement!, { key: "Escape" });
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(screen.getByRole("button", { name: "选择到期日" })).toHaveFocus();
});

it("点到控件之外关闭月历，焦点不被拉回控件", () => {
  render(<><Fixture /><button type="button">别处</button></>);
  fireEvent.click(screen.getByRole("button", { name: "选择到期日" }));
  expect(screen.getByRole("dialog")).toBeInTheDocument();
  const elsewhere = screen.getByRole("button", { name: "别处" });
  elsewhere.focus();
  fireEvent.pointerDown(elsewhere);
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(elsewhere).toHaveFocus();
});

// 日期筛选只接受完整日期：调用方丢弃未完成的值时，prop 不变，各段不能被旧值冲掉；外部改值仍同步进来。
it("调用方不接受未完成的值时各段保持输入，外部改值时同步", () => {
  function Strict() {
    const [value, setValue] = useState("");
    return <>
      <DateInput label="起始日期" value={value} onChange={(v) => { if (v === "" || /^\d{4}-\d{2}-\d{2}$/.test(v)) setValue(v); }} />
      <button type="button" onClick={() => setValue("2030-05-06")}>外部设值</button>
      <output>{value}</output>
    </>;
  }
  render(<Strict />);
  const year = screen.getByRole("textbox", { name: "起始日期 年" });
  fireEvent.change(year, { target: { value: "2031" } });
  expect(year).toHaveValue("2031");
  expect(screen.getByRole("status")).toHaveTextContent("");
  fireEvent.change(screen.getByRole("textbox", { name: "起始日期 月" }), { target: { value: "07" } });
  fireEvent.change(screen.getByRole("textbox", { name: "起始日期 日" }), { target: { value: "08" } });
  expect(screen.getByRole("status")).toHaveTextContent("2031-07-08");
  fireEvent.click(screen.getByRole("button", { name: "外部设值" }));
  expect(year).toHaveValue("2030");
  expect(screen.getByRole("textbox", { name: "起始日期 日" })).toHaveValue("06");
});

it("时刻按 24 小时制补零输出，越界时刻与必填为空都拦下表单", () => {
  const changed = vi.fn();
  function Time({ initial = "" }) {
    const [value, setValue] = useState(initial);
    return <form aria-label="静默"><TimeInput label="每日开始" caption="开始" required value={value} onChange={(v) => { setValue(v); changed(v); }} /></form>;
  }
  render(<Time />);
  const hour = screen.getByRole<HTMLInputElement>("textbox", { name: "每日开始 时" });
  const minute = screen.getByRole("textbox", { name: "每日开始 分" });
  expect(screen.getByRole("form")).toBeInvalid();
  expect(hour.validationMessage).toBe("请填写开始");
  fireEvent.change(hour, { target: { value: "7" } });
  fireEvent.change(minute, { target: { value: "5" } });
  expect(changed).toHaveBeenLastCalledWith("07:05");
  expect(screen.getByRole("form")).toBeValid();
  fireEvent.change(hour, { target: { value: "24" } });
  expect(screen.getByRole("form")).toBeInvalid();
  expect(screen.getByText("请输入 00:00 到 23:59 之间的时刻")).toHaveClass("error");
  expect(screen.queryByRole("button", { name: /选择/ })).toBeNull();
});

it("日期加时刻输出 datetime-local 同格式的值，月历只改日期段", () => {
  const changed = vi.fn();
  function Both() {
    const [value, setValue] = useState("2026-10-04T22:00");
    return <form aria-label="静默"><DateTimeInput label="开始" required value={value} onChange={(v) => { setValue(v); changed(v); }} /></form>;
  }
  render(<Both />);
  expect(within(screen.getByRole("group", { name: "开始" })).getAllByRole("textbox").map((i) => (i as HTMLInputElement).value)).toEqual(["2026", "10", "04", "22", "00"]);
  fireEvent.click(screen.getByRole("button", { name: "选择开始" }));
  fireEvent.click(screen.getByRole("button", { name: "2026-10-09" }));
  expect(changed).toHaveBeenLastCalledWith("2026-10-09T22:00");
  fireEvent.change(screen.getByRole("textbox", { name: "开始 分" }), { target: { value: "" } });
  expect(changed).toHaveBeenLastCalledWith("2026-10-09T22:");
  expect(screen.getByRole("form")).toBeInvalid();
});
