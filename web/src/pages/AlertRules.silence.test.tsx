import { expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { AlertKind, SilenceKind, type SaveSilenceRequest } from "../gen/heron/v1/admin_pb";
import { renderWithAdmin } from "../test/harness";
import { AlertRules } from "./AlertRules";

const now = 1700000037n;
const routes = [{ path: "/alerts", Component: AlertRules }];

it.each([
  ["两个长名", "规".repeat(64), "节".repeat(64), "规".repeat(64)],
  ["多字节码点", "🌏".repeat(64), "🛰️节点", "🌏".repeat(64)],
  ["先去首尾空白", " ".repeat(64) + "规则", "东京", "规则 · 东京"],
  ["截断后去尾空白", "规".repeat(63), "东京", "规".repeat(63)],
  ["全空白规则名", " \t\n ", "东京", "东京"],
])("一键静默预填直接提交合法名称：%s", async (_case, ruleName, nodeName, expectedName) => {
  vi.useFakeTimers();
  vi.setSystemTime(Number(now + 3600n) * 1000);
  try {
    const saved: SaveSilenceRequest[] = [];
    renderWithAdmin({
      listNodes: async () => ({ nodes: [{ id: 1n, name: nodeName }] }),
      listNotifyChannels: async () => ({}), listProbeTasks: async () => ({}), listTags: async () => ({}),
      listSilences: async () => ({ now }),
      listAlertRules: async () => ({ rules: [{ id: 7n, name: ruleName, kind: AlertKind.OFFLINE, enabled: true }], states: [{ ruleId: 7n, nodeId: 1n, state: "firing" }] }),
      saveSilence: async (req) => { saved.push(req); return {}; },
    }, routes, "/alerts");
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    fireEvent.click(screen.getByRole("button", { name: `静默 ${nodeName}` }));
    const form = within(screen.getByRole("form", { name: "新建维护静默" }));
    expect(form.getByLabelText("名称")).toHaveValue(expectedName);
    expect(form.getByLabelText("规则")).toHaveValue("selected");
    expect(form.getByRole("group", { name: "指定规则" }).querySelector("input")).toBeChecked();
    expect(form.getByRole("radio", { name: "指定节点" })).toBeChecked();
    expect(form.getByRole("checkbox", { name: `${nodeName}（#1）` })).toBeChecked();
    expect(form.getByLabelText("类型")).toHaveValue(String(SilenceKind.ONCE));
    fireEvent.click(form.getByRole("button", { name: "创建" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    expect(saved).toHaveLength(1);
    const value = saved[0].silence!;
    expect(value).toMatchObject({ name: expectedName, allRules: false, ruleIds: [7n], allNodes: false, nodeIds: [1n], selectorTags: [], kind: SilenceKind.ONCE, fromAt: now, untilAt: now + 86400n, startHhmm: "", endHhmm: "" });
    expect(Array.from(value.name).length).toBeGreaterThanOrEqual(1);
    expect(Array.from(value.name).length).toBeLessThanOrEqual(64);
    expect(value.name).toBe(value.name.trim());
  } finally { vi.useRealTimers(); }
});

it("日历类 firing 和普通 pending 不显示静默按钮，普通 firing 显示", async () => {
  renderWithAdmin({
    listNodes: async () => ({ nodes: [{ id: 1n, name: "东京" }] }), listNotifyChannels: async () => ({}), listProbeTasks: async () => ({}),
    listSilences: async () => ({ now }),
    listAlertRules: async () => ({
      rules: [{ id: 7n, name: "离线", kind: AlertKind.OFFLINE, enabled: true }, { id: 8n, name: "到期", kind: AlertKind.EXPIRY, enabled: true }, { id: 9n, name: "证书", kind: AlertKind.CERT_EXPIRY, enabled: true }, { id: 10n, name: "待定", kind: AlertKind.OFFLINE, enabled: true }],
      states: [7n, 8n, 9n, 10n].map((id) => ({ ruleId: id, nodeId: 1n, state: id === 10n ? "pending" : "firing" })),
    }),
  }, routes, "/alerts");
  await waitFor(() => expect(screen.getByRole("row", { name: "离线" })).toBeInTheDocument());
  for (const name of ["到期", "证书", "待定"]) expect(within(screen.getByRole("row", { name })).queryByRole("button", { name: "静默 东京" })).toBeNull();
  expect(within(screen.getByRole("row", { name: "离线" })).getByRole("button", { name: "静默 东京" })).toBeEnabled();
});
