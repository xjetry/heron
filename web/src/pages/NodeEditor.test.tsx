import { create } from "@bufbuild/protobuf";
import { fireEvent, render, screen, cleanup } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { NodeSchema, TagSchema } from "../gen/heron/v1/admin_pb";
import { TrafficQuotaMode } from "../gen/heron/v1/types_pb";
import { NodeEditor } from "./NodeEditor";

afterEach(cleanup);

function editor(bytes = 1073741825n) {
  const onSave = vi.fn<Parameters<typeof NodeEditor>[0]["onSave"]>();
  const node = create(NodeSchema, { id: 1n, name: "quota", trafficResetDay: 1, trafficQuotaBytes: bytes, trafficQuotaMode: TrafficQuotaMode.TX });
  render(<NodeEditor node={node} knownTags={[]} saving={false} error={null} listError={null} onClose={() => {}} onSave={onSave} opener={document.createElement("button")} />);
  return onSave;
}

it("preserves exact untouched bytes instead of round-tripping the displayed value", () => {
  const save = editor();
  fireEvent.click(screen.getByRole("button", { name: "保存" }));
  expect(save.mock.calls[0][0]).toMatchObject({ trafficQuotaBytes: 1073741825n, trafficQuotaMode: TrafficQuotaMode.TX });
});

it("converts edited decimal input and changed unit", () => {
  const save = editor();
  fireEvent.change(screen.getByLabelText(/流量配额 quota/), { target: { value: "0.001" } });
  fireEvent.click(screen.getByRole("button", { name: "保存" }));
  expect(save.mock.calls[0][0].trafficQuotaBytes).toBe(1073742n);
  fireEvent.click(screen.getByRole("button", { name: /^配额单位 quota/ }));
  fireEvent.click(screen.getByRole("option", { name: "GB" }));
  fireEvent.click(screen.getByRole("button", { name: "保存" }));
  expect(save.mock.calls[1][0].trafficQuotaBytes).toBe(1000000n);
});

it("blocks tiny positive values and out-of-range quotas", () => {
  const save = editor();
  for (const value of ["0.00000000001", "4294967296"]) {
    fireEvent.change(screen.getByLabelText(/流量配额 quota/), { target: { value } });
    expect(screen.getByRole<HTMLButtonElement>("button", { name: "保存" }).disabled).toBe(true);
    expect(screen.getByRole("alert").textContent).toContain("配额无效");
  }
  expect(save).not.toHaveBeenCalled();
});

function taggedEditor() {
  const onSave = vi.fn<Parameters<typeof NodeEditor>[0]["onSave"]>();
  const node = create(NodeSchema, { id: 1n, name: "quota", trafficResetDay: 1, tags: ["DB"], trafficQuotaMode: TrafficQuotaMode.SUM });
  const knownTags = ["db", "web", "web-edge", "客户A"].map((name) => create(TagSchema, { name, nodeCount: 1 }));
  render(<NodeEditor node={node} knownTags={knownTags} saving={false} error={null} listError={null} onClose={() => {}} onSave={onSave} opener={document.createElement("button")} />);
  return onSave;
}
const tagInput = () => screen.getByRole("combobox", { name: /^新标签 quota/ });
const suggested = () => screen.queryAllByRole("option").map((o) => o.textContent);

it("标签候选：已有标签里节点还没带的（按折叠比较），按输入折叠字面匹配；不再用原生 datalist", () => {
  taggedEditor();
  expect(document.querySelector("datalist")).toBeNull();
  fireEvent.focus(tagInput());
  expect(suggested()).toEqual(["web", "web-edge", "客户A"]);
  fireEvent.change(tagInput(), { target: { value: "WEB" } });
  expect(suggested()).toEqual(["web", "web-edge"]);
  fireEvent.change(tagInput(), { target: { value: "b-e" } });
  expect(suggested()).toEqual(["web-edge"]);
  fireEvent.change(tagInput(), { target: { value: "w.b" } });
  expect(suggested()).toEqual([]);
  expect(tagInput()).toHaveAttribute("aria-expanded", "false");
});

it("标签候选：方向键移动、Enter 选中当前候选；没有当前候选时 Enter 按输入的文字添加；点候选直接添加", () => {
  const save = taggedEditor();
  const input = tagInput();
  fireEvent.focus(input);
  fireEvent.keyDown(input, { key: "ArrowDown" });
  fireEvent.keyDown(input, { key: "ArrowDown" });
  expect(input).toHaveAttribute("aria-activedescendant", screen.getByRole("option", { name: "web-edge" }).id);
  fireEvent.keyDown(input, { key: "ArrowUp" });
  fireEvent.keyDown(input, { key: "Enter" });
  expect(input).toHaveValue("");
  fireEvent.change(input, { target: { value: "新的" } });
  fireEvent.keyDown(input, { key: "Enter" });
  fireEvent.click(screen.getByRole("option", { name: "客户A" }));
  expect(suggested()).toEqual(["web-edge"]);
  fireEvent.click(screen.getByRole("button", { name: "保存" }));
  expect(save.mock.calls[0][0].tags).toEqual(["DB", "web", "新的", "客户A"]);
});

it("标签候选：Esc 只收起候选，不冒泡到外层（抽屉不随之关闭）", () => {
  taggedEditor();
  const outer = vi.fn();
  document.addEventListener("keydown", outer);
  try {
    fireEvent.focus(tagInput());
    fireEvent.keyDown(tagInput(), { key: "Escape" });
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(outer).not.toHaveBeenCalled();
    fireEvent.keyDown(tagInput(), { key: "Escape" });
    expect(outer).toHaveBeenCalledTimes(1);
  } finally {
    document.removeEventListener("keydown", outer);
  }
});

it("流量口径是自绘下拉，选中的口径随保存提交", () => {
  const save = taggedEditor();
  expect(screen.getByRole("button", { name: /^流量口径 quota/ })).toHaveTextContent("收+发");
  fireEvent.click(screen.getByRole("button", { name: /^流量口径 quota/ }));
  fireEvent.click(screen.getByRole("option", { name: "只发" }));
  fireEvent.click(screen.getByRole("button", { name: "保存" }));
  expect(save.mock.calls[0][0].trafficQuotaMode).toBe(TrafficQuotaMode.TX);
});

function flagsEditor(flags: { rented?: boolean; maintenance?: boolean }) {
  const onSave = vi.fn<Parameters<typeof NodeEditor>[0]["onSave"]>();
  const node = create(NodeSchema, { id: 1n, name: "quota", trafficResetDay: 1, ...flags });
  render(<NodeEditor node={node} knownTags={[]} saving={false} error={null} listError={null} onClose={() => {}} onSave={onSave} opener={document.createElement("button")} />);
  return onSave;
}

it("已出租随整体替换提交，为真时维护开关禁用", () => {
  const save = flagsEditor({ rented: true });
  fireEvent.click(screen.getByRole("button", { name: "保存" }));
  expect(save.mock.calls[0][0]).toMatchObject({ rented: true, maintenance: false });
  const rented = screen.getByLabelText<HTMLInputElement>("已出租 quota（#1）");
  const maint = screen.getByLabelText<HTMLInputElement>("维护 quota（#1）");
  expect(rented.checked).toBe(true);
  expect(maint).toBeDisabled();
  expect(screen.getByText(/与维护中互斥/)).toBeInTheDocument();
  expect(screen.getByText(/与已出租互斥/)).toBeInTheDocument();
});

it("维护中为真时已出租开关禁用", () => {
  flagsEditor({ maintenance: true });
  expect(screen.getByLabelText<HTMLInputElement>("维护 quota（#1）").checked).toBe(true);
  expect(screen.getByLabelText("已出租 quota（#1）")).toBeDisabled();
});
