import { expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { create } from "@bufbuild/protobuf";
import { ChannelKind, ListNotifyChannelsResponseSchema, type UpdateSettingsRequest } from "../gen/heron/v1/admin_pb";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { statefulHub } from "../test/settingsHub";
import { Channels } from "./Channels";

const channels = create(ListNotifyChannelsResponseSchema, { channels: [
  { id: 1n, name: "tg", kind: ChannelKind.TELEGRAM, telegram: { chatId: "42", hasBotToken: true }, createdAt: 1_700_000_000n, ratePerMinute: 20 },
  { id: 2n, name: "hook", kind: ChannelKind.WEBHOOK, webhook: { method: "POST", hasUrl: true, urlHost: "hooks.example" }, createdAt: 1_700_000_000n, ratePerMinute: 0 },
] });
const routes = [{ path: "/channels", Component: Channels }];
const render = (impl: AdminImpl) => renderWithAdmin({ listNotifyChannels: async () => channels, ...impl }, routes, "/channels");
const checked = (el: HTMLElement) => (el as HTMLInputElement).checked;

it("流量报告读取当前设置，只提交 traffic_report 这一组并整组替换", async () => {
  const sent: UpdateSettingsRequest[] = [];
  render({
    getSettings: async () => ({ settings: { theme: "dark", title: "站点", trafficReport: { enabled: true, weekly: true, hour: 9, channelIds: [2n] } } }),
    updateSettings: async (r) => { sent.push(r); return { settings: r.settings }; },
  });
  const f = within(await screen.findByRole("form", { name: "流量报告" }));
  expect(checked(f.getByLabelText("启用流量报告"))).toBe(true);
  expect([f.getByLabelText("日报（每天）"), f.getByLabelText("周报（每周一）"), f.getByLabelText("月报（每月 1 日）")].map(checked)).toEqual([false, true, false]);
  expect(f.getByLabelText<HTMLSelectElement>("投递时刻").value).toBe("9");
  expect([f.getByLabelText("tg（#1）"), f.getByLabelText("hook（#2）")].map(checked)).toEqual([false, true]);
  fireEvent.click(f.getByLabelText("日报（每天）"));
  fireEvent.change(f.getByLabelText("投递时刻"), { target: { value: "23" } });
  fireEvent.click(f.getByLabelText("tg（#1）"));
  fireEvent.click(f.getByRole("button", { name: "保存流量报告" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  const s = sent[0].settings!;
  expect({ title: s.title, theme: s.theme, login: s.loginNotify, backup: s.backup, heartbeat: s.heartbeat }).toEqual({ title: "", theme: "", login: undefined, backup: undefined, heartbeat: undefined });
  expect(s.trafficReport).toMatchObject({ enabled: true, daily: true, weekly: true, monthly: false, hour: 23 });
  expect([...(s.trafficReport?.channelIds ?? [])].sort()).toEqual([1n, 2n]);
  expect(await f.findByRole("status")).toHaveTextContent("流量报告已保存");
});

it("启用而一种周期都没选时在提交前拦下，不发请求", async () => {
  const sent: UpdateSettingsRequest[] = [];
  render({
    getSettings: async () => ({ settings: { theme: "dark", trafficReport: { daily: true } } }),
    updateSettings: async (r) => { sent.push(r); return { settings: r.settings }; },
  });
  const f = within(await screen.findByRole("form", { name: "流量报告" }));
  fireEvent.click(f.getByLabelText("启用流量报告"));
  fireEvent.click(f.getByLabelText("日报（每天）"));
  fireEvent.click(f.getByRole("button", { name: "保存流量报告" }));
  expect(await f.findByRole("alert")).toHaveTextContent("启用时至少选择一种周期");
  expect(sent).toHaveLength(0);
  fireEvent.click(f.getByLabelText("月报（每月 1 日）"));
  expect(f.queryByRole("alert")).toBeNull();
  fireEvent.click(f.getByRole("button", { name: "保存流量报告" }));
  await waitFor(() => expect(sent.map((r) => r.settings?.trafficReport?.monthly)).toEqual([true]));
});

// 只改流量报告不动其它组；hub 的回显经 useAdoptSavedSettings 进缓存，表单显示保存后的值。
it("保存后 hub 的其它设置不变，表单显示回显", async () => {
  const hub = statefulHub({ title: "站点", theme: "dark", loginNotify: { channelIds: [2n] } });
  render(hub.impl);
  const f = within(await screen.findByRole("form", { name: "流量报告" }));
  fireEvent.click(f.getByLabelText("启用流量报告"));
  fireEvent.click(f.getByLabelText("周报（每周一）"));
  fireEvent.click(f.getByLabelText("hook（#2）"));
  fireEvent.click(f.getByLabelText("tg（#1）"));
  fireEvent.click(f.getByRole("button", { name: "保存流量报告" }));
  expect(await f.findByRole("status")).toHaveTextContent("流量报告已保存");
  expect(hub.state()).toMatchObject({ title: "站点", theme: "dark", loginNotify: { channelIds: [2n] }, trafficReport: { enabled: true, weekly: true, hour: 0, channelIds: [1n, 2n] } });
  expect(checked(f.getByLabelText("周报（每周一）"))).toBe(true);
});
