import { fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { expect, it } from "vitest";
import { MultiSelect, type MultiSelectOption } from "./MultiSelect";

const regions: MultiSelectOption[] = [
  { value: "HK", label: "香港", count: 3 }, { value: "JP", label: "日本", count: 2 }, { value: "US", label: "美国", count: 1 }, { value: "", label: "未知", count: 1 },
];

function Harness({ searchable = false, foldAt }: { searchable?: boolean; foldAt?: number }) {
  const [selected, setSelected] = useState<string[]>([]);
  return <MultiSelect label="地区" options={regions} selected={selected} onChange={setSelected} searchable={searchable} foldAt={foldAt} />;
}
const group = () => within(screen.getByRole("group", { name: "地区" }));
const trigger = () => group().getByRole("button", { name: /^地区/ });

it("收起时只有触发按钮；展开后选项是带计数的复选框，勾选即生效并显示为胶囊，触发按钮带已选数", () => {
  render(<Harness />);
  expect(group().queryByRole("checkbox")).toBeNull();
  fireEvent.click(trigger());
  expect(trigger()).toHaveAttribute("aria-expanded", "true");
  expect(group().getAllByRole("checkbox").map((c) => c.getAttribute("aria-label"))).toEqual(["香港", "日本", "美国", "未知"]);
  expect(group().getByRole("checkbox", { name: "香港" }).closest("label")).toHaveTextContent("3");
  fireEvent.click(group().getByRole("checkbox", { name: "日本" }));
  fireEvent.click(group().getByRole("checkbox", { name: "未知" }));
  expect(group().getByRole("checkbox", { name: "日本" })).toBeChecked();
  expect(trigger()).toHaveTextContent("地区 2");
  expect(group().getByRole("button", { name: "移除 日本" })).toBeInTheDocument();
  fireEvent.click(group().getByRole("button", { name: "移除 日本" }));
  expect(group().getByRole("checkbox", { name: "日本" })).not.toBeChecked();
  fireEvent.click(group().getByRole("button", { name: "清除" }));
  expect(trigger()).toHaveTextContent(/^地区$/);
});

it("Escape 与点击外部收起；选择保留", () => {
  render(<Harness />);
  fireEvent.click(trigger());
  fireEvent.click(group().getByRole("checkbox", { name: "美国" }));
  fireEvent.keyDown(trigger(), { key: "Escape" });
  expect(trigger()).toHaveAttribute("aria-expanded", "false");
  fireEvent.click(trigger());
  fireEvent.pointerDown(document.body);
  expect(trigger()).toHaveAttribute("aria-expanded", "false");
  expect(trigger()).toHaveTextContent("地区 1");
});

it("可搜索：按折叠大小写的字面匹配过滤选项，正则特殊字符按字面处理，无匹配时说明", () => {
  render(<MultiSelect label="标签" options={[{ value: "DB", label: "DB" }, { value: "db-2", label: "db-2" }, { value: "a.b", label: "a.b" }, { value: "aXb", label: "aXb" }]} selected={[]} onChange={() => {}} searchable />);
  const g = within(screen.getByRole("group", { name: "标签" }));
  fireEvent.click(g.getByRole("button", { name: /^标签/ }));
  const search = g.getByRole("searchbox", { name: "搜索标签" });
  fireEvent.change(search, { target: { value: "db" } });
  expect(g.getAllByRole("checkbox").map((c) => c.getAttribute("aria-label"))).toEqual(["DB", "db-2"]);
  fireEvent.change(search, { target: { value: "a.b" } });
  expect(g.getAllByRole("checkbox").map((c) => c.getAttribute("aria-label"))).toEqual(["a.b"]);
  fireEvent.change(search, { target: { value: "zzz" } });
  expect(g.queryByRole("checkbox")).toBeNull();
  expect(g.getByText("没有匹配的选项")).toBeInTheDocument();
});

it("已选超过 foldAt 个时只显示前几个胶囊，其余折成 +N", () => {
  render(<Harness foldAt={2} />);
  fireEvent.click(trigger());
  for (const name of ["香港", "日本", "美国"]) fireEvent.click(group().getByRole("checkbox", { name }));
  expect(group().getAllByRole("button", { name: /^移除/ })).toHaveLength(2);
  expect(group().getByText("+1")).toBeInTheDocument();
});

// 调用方按快照规范化 selected：不在 options 里的值不画胶囊，也不计入折叠数。
it("selected 里不在选项中的值被忽略", () => {
  render(<MultiSelect label="地区" options={regions} selected={["XX", "HK"]} onChange={() => {}} />);
  expect(group().getAllByRole("button", { name: /^移除/ })).toHaveLength(1);
});
