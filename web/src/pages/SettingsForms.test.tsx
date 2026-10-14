import { expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { create } from "@bufbuild/protobuf";
import { ChannelKind, ListNotifyChannelsResponseSchema, SettingsSchema, type UpdateSettingsRequest } from "../gen/heron/v1/admin_pb";
import { renderWithAdmin } from "../test/harness";
import { Appearance } from "./Appearance";
import { Channels } from "./Channels";

// 外观表单与登录通知表单共用 SAVE_SETTINGS：一个在途时另一个不能编辑、不能提交，直到在途那个的设置刷新读完。
// 两个表单在不同页面上，互斥靠同一个 QueryClient 里的 mutation 缓存，所以用例在途中切换页面。

const channels = create(ListNotifyChannelsResponseSchema, { channels: [
  { id: 1n, name: "tg", kind: ChannelKind.TELEGRAM, telegram: { chatId: "42", hasBotToken: true }, createdAt: 1_700_000_000n, ratePerMinute: 20 },
] });
const current = create(SettingsSchema, { title: "站点", theme: "dark", loginNotify: { channelIds: [] } });
const routes = [{ path: "/channels", Component: Channels }, { path: "/appearance", Component: Appearance }];

// 保存与读设置都由用例放行：finish 让在途的保存成功；hold 为真时读设置挂起（held 置真），直到 release。
function setup(path: string) {
  const sent: UpdateSettingsRequest[] = [];
  const ctl = { hold: false, held: false, finish: () => {}, release: () => {} };
  const rendered = renderWithAdmin({
    listNotifyChannels: async () => channels,
    getSettings: async () => {
      if (ctl.hold) await new Promise<void>((resolve) => { ctl.held = true; ctl.release = resolve; });
      return { settings: current };
    },
    updateSettings: async (r) => {
      sent.push(r);
      await new Promise<void>((resolve) => { ctl.finish = resolve; });
      return { settings: r.settings };
    },
  }, routes, path);
  return { sent, ctl, router: rendered.router };
}

it("登录通知保存在途时外观表单不能提交，设置刷新读完后才恢复", async () => {
  const { sent, ctl, router } = setup("/channels");
  const login = within(await screen.findByRole("form", { name: "登录通知" }));
  fireEvent.click(login.getByLabelText("tg（#1）"));
  fireEvent.click(login.getByRole("button", { name: "保存登录通知" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  await router.navigate("/appearance");
  const appearance = await screen.findByRole("form", { name: "公开页外观" });
  const save = within(appearance).getByRole("button", { name: "保存" });
  expect(save).toBeDisabled();
  fireEvent.submit(appearance);
  ctl.hold = true;
  ctl.finish();
  // 保存已成功，设置的刷新还挂着：在途持续到刷新读完，外观表单仍不能提交。
  await waitFor(() => expect(ctl.held).toBe(true));
  expect(save).toBeDisabled();
  ctl.release();
  await waitFor(() => expect(save).toBeEnabled());
  expect(sent).toHaveLength(1);
});

it("外观保存在途时登录通知表单不能编辑与提交", async () => {
  const { sent, ctl, router } = setup("/appearance");
  const appearance = within(await screen.findByRole("form", { name: "公开页外观" }));
  fireEvent.change(appearance.getByLabelText("标题"), { target: { value: "新标题" } });
  fireEvent.click(appearance.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  await router.navigate("/channels");
  const form = await screen.findByRole("form", { name: "登录通知" });
  const login = within(form);
  expect(login.getByLabelText("tg（#1）")).toBeDisabled();
  expect(login.getByRole("button", { name: "保存登录通知" })).toBeDisabled();
  fireEvent.submit(form);
  expect(sent).toHaveLength(1);
  ctl.finish();
  await waitFor(() => expect(login.getByRole("button", { name: "保存登录通知" })).toBeEnabled());
});
