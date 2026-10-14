import { expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { create } from "@bufbuild/protobuf";
import { ConnectError, Code } from "@connectrpc/connect";
import { ChannelKind, ListNotifyChannelsResponseSchema, type SaveNotifyChannelRequest, type UpdateSettingsRequest } from "../gen/heron/v1/admin_pb";
import { MAX_NOTIFY_CHANNELS } from "../lib/alerts";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { statefulHub } from "../test/settingsHub";
import { Channels } from "./Channels";
import { expectEmptyState } from "../test/empty";

const channels = create(ListNotifyChannelsResponseSchema, { channels: [
  { id: 1n, name: "tg", kind: ChannelKind.TELEGRAM, telegram: { chatId: "42", hasBotToken: true }, createdAt: 1_700_000_000n, ratePerMinute: 20 },
  { id: 2n, name: "hook", kind: ChannelKind.WEBHOOK, webhook: { method: "POST", hasUrl: true, urlHost: "https://hooks.example", headerNames: ["Authorization"], bodyTemplate: "{{.Summary}}" }, createdAt: 1_700_000_000n, ratePerMinute: 0 },
] });
const routes = [{ path: "/channels", Component: Channels }];
const render = (impl: AdminImpl) => renderWithAdmin({ listNotifyChannels: async () => channels, getSettings: async () => ({ settings: { theme: "dark", title: "站点", loginNotify: { channelIds: [2n] } } }), ...impl }, routes, "/channels");

async function openCreate() {
  fireEvent.click(await screen.findByRole("button", { name: "新建通知渠道" }));
  return within(screen.getByRole("dialog")).getByRole("form", { name: "新建通知渠道" });
}

async function openRowAction(label: string, action: string) {
  fireEvent.click(await screen.findByRole("button", { name: `更多操作 ${label}` }));
  fireEvent.click(screen.getByRole("menuitem", { name: `${action} ${label}` }));
}

it("登录通知读取选择且只提交通知字段，空集合可关闭", async () => {
  const sent: UpdateSettingsRequest[] = [];
  render({ updateSettings: async (r) => { sent.push(r); return { settings: r.settings }; } });
  const f = within(await screen.findByRole("form", { name: "登录通知" }));
  expect([f.getByLabelText("tg（#1）"), f.getByLabelText("hook（#2）")].map((c) => (c as HTMLInputElement).checked)).toEqual([false, true]);
  fireEvent.click(f.getByLabelText("tg（#1）"));
  fireEvent.click(f.getByRole("button", { name: "保存登录通知" }));
  await waitFor(() => expect(sent.map((r) => ({ title: r.settings?.title, theme: r.settings?.theme, ids: r.settings?.loginNotify?.channelIds }))).toEqual([{ title: "", theme: "", ids: [1n, 2n] }]));
  await waitFor(() => expect(f.getByRole("button", { name: "保存登录通知" })).toBeEnabled());
  fireEvent.click(f.getByLabelText("tg（#1）"));
  fireEvent.click(f.getByLabelText("hook（#2）"));
  fireEvent.click(f.getByRole("button", { name: "保存登录通知" }));
  await waitFor(() => expect(sent[1]?.settings?.loginNotify?.channelIds).toEqual([]));
});

// 登录通知表单与其余设置表单一样经 useAdoptSavedSettings：保存后的刷新失败时，缓存里已是 hub 的回显，重新进入页面
// 显示刚保存的选择；请求只带 login_notify 这一组，hub 照常保存，外观不变。
it("登录通知保存后刷新失败，重新进入页面时显示刚保存的渠道", async () => {
  const hub = statefulHub({ title: "站点", theme: "dark", loginNotify: { channelIds: [2n] } });
  const { router } = renderWithAdmin({ ...hub.impl, listNotifyChannels: async () => channels }, [...routes, { path: "/elsewhere", Component: () => null }], "/channels");
  const f = within(await screen.findByRole("form", { name: "登录通知" }));
  hub.failReads();
  fireEvent.click(f.getByLabelText("tg（#1）"));
  fireEvent.click(f.getByRole("button", { name: "保存登录通知" }));
  expect(await f.findByRole("status")).toHaveTextContent("登录通知已保存");
  expect(hub.state()).toMatchObject({ title: "站点", theme: "dark", loginNotify: { channelIds: [1n, 2n] } });
  await act(() => router.navigate("/elsewhere"));
  await act(() => router.navigate("/channels"));
  const again = within(await screen.findByRole("form", { name: "登录通知" }));
  expect([again.getByLabelText("tg（#1）"), again.getByLabelText("hook（#2）")].map((c) => (c as HTMLInputElement).checked)).toEqual([true, true]);
});

// 点开删除确认，取出确认旁的提示后取消。
async function deleteNoteOf(name: string) {
  await openRowAction(name, "删除");
  const menu = screen.getByRole("menu");
  const note = menu.querySelector(".row-menu-note")?.textContent ?? null;
  fireEvent.click(within(menu).getByRole("menuitem", { name: "取消" }));
  fireEvent.keyDown(menu, { key: "Escape" });
  return note;
}

it("删除登录通知唯一的接收渠道时，确认写明登录通知会关闭；不在列表里的渠道不提示", async () => {
  render({});
  expect([await deleteNoteOf("hook（#2）"), await deleteNoteOf("tg（#1）")]).toEqual(["它是登录通知唯一的接收渠道，删除后登录通知关闭。", null]);
});

// 删渠道成功后要重读设置：hub 在同一事务里把它从登录通知列表摘除，面板若沿用删之前的设置，剩下那个渠道的确认会
// 少算"唯一接收渠道"这一条后果。重读完成的信号是删除菜单项恢复可用：删除在途时它带 aria-disabled，
// remove 的 onSuccess 等列表与设置都重新拉取完才返回，在途一直持续到那时。删除之后的设置读取先挂起，钉住"重读期间
// 仍在途"：在途若不覆盖重读，按钮在放行前就可用，用户这时点确认读到的是旧提示。放行后等按钮可用，读一次提示即可，
// 不在 waitFor 里反复点开取消——那样失败时要等满超时才红。
it("删掉一个登录通知渠道后，剩下那个的删除确认按重读的设置写明是唯一接收渠道", async () => {
  let listed = channels.channels;
  let loginIds = [1n, 2n];
  let reread: Promise<void> = Promise.resolve();
  let release = () => {};
  render({
    listNotifyChannels: async () => ({ channels: listed }),
    getSettings: async () => {
      await reread;
      return { settings: { theme: "dark", loginNotify: { channelIds: loginIds } } };
    },
    deleteNotifyChannel: async (r) => {
      listed = listed.filter((c) => c.id !== r.id);
      loginIds = loginIds.filter((id) => id !== r.id);
      reread = new Promise((done) => { release = done; });
      return {};
    },
  });
  await openRowAction("tg（#1）", "删除");
  fireEvent.click(screen.getByRole("menuitem", { name: "确认删除 tg（#1）" }));
  await waitFor(() => expect(screen.queryByRole("button", { name: "更多操作 tg（#1）" })).toBeNull());
  fireEvent.click(screen.getByRole("button", { name: "更多操作 hook（#2）" }));
  const remove = screen.getByRole("menuitem", { name: "删除 hook（#2）" });
  expect(remove).toHaveAttribute("aria-disabled", "true");
  release();
  await waitFor(() => expect(remove).not.toHaveAttribute("aria-disabled"));
  fireEvent.click(remove);
  expect(screen.getByText("它是登录通知唯一的接收渠道，删除后登录通知关闭。")).toBeInTheDocument();
});

// 备份失败通知与登录通知同为设置里的渠道列表，删除确认按同一份逻辑逐个列表写明影响。
it("删除确认逐个列表写明影响：备份失败通知唯一的渠道、两个列表都含的渠道", async () => {
  render({ getSettings: async () => ({ settings: { theme: "dark", loginNotify: { channelIds: [1n, 2n] }, backup: { notify: { channelIds: [1n] } } } }) });
  expect([await deleteNoteOf("tg（#1）"), await deleteNoteOf("hook（#2）")]).toEqual([
    "删除后登录通知不再发到这个渠道。它是备份失败通知唯一的接收渠道，删除后备份失败通知关闭。",
    "删除后登录通知不再发到这个渠道。",
  ]);
});

it("删除登录通知的接收渠道之一时，确认写明不再发到它", async () => {
  render({ getSettings: async () => ({ settings: { theme: "dark", loginNotify: { channelIds: [1n, 2n] } } }) });
  expect(await deleteNoteOf("tg（#1）")).toBe("删除后登录通知不再发到这个渠道。");
});

it("删除流量报告的接收渠道时，确认按它是不是唯一的接收渠道写明影响", async () => {
  render({ getSettings: async () => ({ settings: { theme: "dark", trafficReport: { enabled: true, daily: true, channelIds: [1n] }, loginNotify: { channelIds: [1n, 2n] } } }) });
  expect(await deleteNoteOf("tg（#1）")).toBe("删除后登录通知不再发到这个渠道。它是流量报告唯一的接收渠道，删除后流量报告关闭。");
});

it("设置没读到时删除确认照最坏的情况提醒", async () => {
  render({ getSettings: async () => { throw new ConnectError("settings unavailable", Code.Unavailable); } });
  // 登录通知与流量报告两个表单各报一次读设置失败。
  expect(await screen.findAllByText(/settings unavailable/)).toHaveLength(2);
  expect(await deleteNoteOf("tg（#1）")).toBe("通知设置未读到：它若是登录通知或备份失败通知或流量报告的接收渠道，删除后不再发到它。");
});

it("登录通知选满上限后未选的渠道不可再选", async () => {
  const many = create(ListNotifyChannelsResponseSchema, { channels: Array.from({ length: MAX_NOTIFY_CHANNELS + 1 }, (_, i) => (
    { id: BigInt(i + 1), name: `c${i + 1}`, kind: ChannelKind.TELEGRAM, telegram: { chatId: "42", hasBotToken: true }, createdAt: 1_700_000_000n, ratePerMinute: 20 }
  )) });
  const chosen = Array.from({ length: MAX_NOTIFY_CHANNELS }, (_, i) => BigInt(i + 1));
  render({ listNotifyChannels: async () => many, getSettings: async () => ({ settings: { theme: "dark", loginNotify: { channelIds: chosen } } }) });
  const f = within(await screen.findByRole("form", { name: "登录通知" }));
  const last = `c${MAX_NOTIFY_CHANNELS + 1}（#${MAX_NOTIFY_CHANNELS + 1}）`;
  expect(f.getByLabelText(last)).toBeDisabled();
  expect(f.getByLabelText("c1（#1）")).toBeEnabled();
  expect(f.getByText(`最多选 ${MAX_NOTIFY_CHANNELS} 个渠道`)).toBeInTheDocument();
  fireEvent.click(f.getByLabelText("c1（#1）"));
  expect(f.getByLabelText(last)).toBeEnabled();
});

it("登录通知保存中禁用选择，失败显示错误并保留草稿", async () => {
  let reject!: (err: Error) => void;
  render({ updateSettings: () => new Promise((_resolve, r) => { reject = r; }) });
  const f = within(await screen.findByRole("form", { name: "登录通知" }));
  fireEvent.click(f.getByLabelText("tg（#1）"));
  fireEvent.click(f.getByRole("button", { name: "保存登录通知" }));
  await waitFor(() => expect(f.getByLabelText("tg（#1）")).toBeDisabled());
  await act(async () => { reject(new ConnectError("channel 1 does not exist", Code.InvalidArgument)); });
  await f.findByRole("alert");
  expect({ error: f.getByRole("alert").textContent, checked: f.getByLabelText<HTMLInputElement>("tg（#1）").checked }).toEqual({ error: "channel 1 does not exist", checked: true });
});

it("渠道刷新失败保留同一编辑表单与草稿", async () => {
  let fail = false;
  const { queryClient } = render({ listNotifyChannels: async () => {
    if (fail) throw new ConnectError("channels refresh failed", Code.Unavailable);
    return channels;
  } });
  await openRowAction("tg（#1）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 tg（#1）" });
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "尚未保存" } });
  fail = true;
  await act(async () => { await queryClient.refetchQueries(); });
  expect(await screen.findByRole("alert")).toHaveTextContent("channels refresh failed");
  expect(screen.getByRole("form", { name: "编辑 tg（#1）" })).toBe(form);
  expect(within(form).getByLabelText("名称")).toHaveValue("尚未保存");
});

it("列表挂起时显示加载中而不是空提示", async () => {
  render({ listNotifyChannels: () => new Promise(() => {}) });
  expect(await screen.findByText("加载中…")).toBeInTheDocument();
  expect(screen.queryByText("还没有通知渠道。")).toBeNull();
});

it("列表只显示非凭据字段", async () => {
  render({});
  expect(await screen.findByRole("cell", { name: "会话 42" })).toBeInTheDocument();
  expect(screen.getByRole("cell", { name: "POST https://hooks.example，头 Authorization" })).toBeInTheDocument();
  expect(screen.getByRole("cell", { name: "每分钟 20 条" })).toBeInTheDocument();
  expect(screen.getByRole("cell", { name: "不限" })).toBeInTheDocument();
});

it("新建 Telegram 渠道只发 telegram 配置，成功后表单复位", async () => {
  const saved: SaveNotifyChannelRequest[] = [];
  render({ saveNotifyChannel: async (req) => { saved.push(req); return {}; } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "值班群" } });
  fireEvent.change(within(form).getByLabelText("Bot token"), { target: { value: "123:abc" } });
  fireEvent.change(within(form).getByLabelText("Chat ID"), { target: { value: "-100" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  const c = saved[0].channel!;
  // 节奏上限留空时不发这个字段，由 hub 按种类取默认值。
  expect({ id: c.id, name: c.name, kind: c.kind, token: c.telegram?.botToken, chat: c.telegram?.chatId, webhook: c.webhook, rate: c.ratePerMinute }).toEqual(
    { id: 0n, name: "值班群", kind: ChannelKind.TELEGRAM, token: "123:abc", chat: "-100", webhook: undefined, rate: undefined });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(within(await openCreate()).getByLabelText("名称")).toHaveValue("");
});

it("编辑同种类时凭据留空即保留", async () => {
  const saved: SaveNotifyChannelRequest[] = [];
  render({ saveNotifyChannel: async (req) => { saved.push(req); return {}; } });
  await openRowAction("hook（#2）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 hook（#2）" });
  const url = within(form).getByLabelText("URL");
  expect(url).toHaveAttribute("placeholder", "已保存 https://hooks.example，留空保持不变");
  expect(url).not.toBeRequired();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "hook2" } });
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  const w = saved[0].channel!.webhook!;
  expect({ url: w.url, headers: w.headers, removeHeaders: w.removeHeaders, method: w.method, bodyTemplate: w.bodyTemplate }).toEqual(
    { url: "", headers: {}, removeHeaders: [], method: "POST", bodyTemplate: "{{.Summary}}" });
});

it("删除已保存的头", async () => {
  const saved: SaveNotifyChannelRequest[] = [];
  render({ saveNotifyChannel: async (req) => { saved.push(req); return {}; } });
  await openRowAction("hook（#2）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 hook（#2）" });
  fireEvent.click(within(form).getByLabelText("删除 Authorization"));
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].channel!.webhook!.removeHeaders).toEqual(["Authorization"]);
});

it("新增头与覆盖", async () => {
  const saved: SaveNotifyChannelRequest[] = [];
  render({ saveNotifyChannel: async (req) => { saved.push(req); return {}; } });
  await openRowAction("hook（#2）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 hook（#2）" });
  fireEvent.click(within(form).getByRole("button", { name: "添加请求头" }));
  fireEvent.change(within(form).getByLabelText("请求头名"), { target: { value: "X-Tag" } });
  const value = within(form).getByLabelText("请求头值");
  expect(value).toHaveAttribute("autocomplete", "new-password");
  fireEvent.change(value, { target: { value: "v1" } });
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].channel!.webhook!.headers).toEqual({ "X-Tag": "v1" });
});

it("同名头拒绝提交", async () => {
  const saved: SaveNotifyChannelRequest[] = [];
  render({ saveNotifyChannel: async (req) => { saved.push(req); return {}; } });
  const form = await openCreate();
  fireEvent.click(within(form).getByRole("radio", { name: "Webhook" }));
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "hook3" } });
  fireEvent.change(within(form).getByLabelText("URL"), { target: { value: "https://hook3.example" } });
  fireEvent.click(within(form).getByRole("button", { name: "添加请求头" }));
  fireEvent.click(within(form).getByRole("button", { name: "添加请求头" }));
  const names = within(form).getAllByLabelText("请求头名");
  fireEvent.change(names[0], { target: { value: "X-A" } });
  fireEvent.change(names[1], { target: { value: "x-a" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  expect(screen.getByRole("alert")).toHaveTextContent("请求头 x-a 填写了两次");
  // 修正重名后同一表单可提交；若同名请求漏出，它经由同一路径排在这个请求之前。
  fireEvent.change(names[1], { target: { value: "X-B" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(saved.at(-1)?.channel?.webhook?.headers).toEqual({ "X-A": "", "X-B": "" }));
  expect(saved).toHaveLength(1);
});

it("编辑时 Webhook 换成 Telegram 要求新 token", async () => {
  render({});
  await openRowAction("hook（#2）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 hook（#2）" });
  fireEvent.click(within(form).getByRole("radio", { name: "Telegram" }));
  expect(within(form).getByLabelText("Bot token")).toBeRequired();
});

it("编辑时切换种类要求新凭据", async () => {
  render({});
  await openRowAction("tg（#1）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 tg（#1）" });
  fireEvent.click(within(form).getByRole("radio", { name: "Webhook" }));
  expect(within(form).getByLabelText("URL")).toBeRequired();
  expect(within(form).queryByText("已保存的请求头（值不回显）")).toBeNull();
});

it("Telegram 编辑 token 可留空", async () => {
  render({});
  await openRowAction("tg（#1）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 tg（#1）" });
  const token = within(form).getByLabelText("Bot token");
  expect(token).toHaveValue("");
  expect(token).not.toBeRequired();
  expect(token).toHaveAttribute("placeholder", "已保存，留空保持不变");
  expect(token).toHaveAttribute("autocomplete", "new-password");
});

it("发送测试成功与失败", async () => {
  const tested: bigint[] = [];
  let fail = false;
  render({ testNotifyChannel: async (req) => {
    tested.push(req.id);
    if (fail) throw new ConnectError("telegram: 401 Unauthorized", Code.FailedPrecondition);
    return {};
  } });
  expect((await screen.findByRole("status")).textContent).toBe("");
  await openRowAction("tg（#1）", "发送测试");
  await waitFor(() => expect(screen.getByRole("status").textContent).toBe("已向 tg 发送测试消息"));
  fail = true;
  await openRowAction("tg（#1）", "发送测试");
  expect(await screen.findByRole("alert")).toHaveTextContent("telegram: 401 Unauthorized");
  expect(screen.getByRole("status").textContent).toBe("");
  expect(tested).toEqual([1n, 1n]);
});

it("在途测试被新操作打断后不出现成功提示", async () => {
  let releaseTest!: () => void;
  const testGate = new Promise<void>((r) => { releaseTest = r; });
  render({
    testNotifyChannel: async () => { await testGate; return {}; },
    deleteNotifyChannel: async () => { throw new ConnectError("ref", Code.FailedPrecondition); },
  });
  await openRowAction("tg（#1）", "发送测试");
  await openRowAction("tg（#1）", "删除");
  fireEvent.click(screen.getByRole("menuitem", { name: "确认删除 tg（#1）" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("ref");
  await act(async () => { releaseTest(); });
  fireEvent.click(screen.getByRole("button", { name: "更多操作 tg（#1）" }));
  await waitFor(() => expect(screen.getByRole("menuitem", { name: "发送测试 tg（#1）" })).not.toHaveAttribute("aria-disabled"));
  expect(screen.getByRole("status").textContent).toBe("");
  expect(screen.getByRole("alert")).toHaveTextContent("ref");
});

it("删除被引用渠道显示服务端原文", async () => {
  render({ deleteNotifyChannel: async () => { throw new ConnectError("notify channel 1 is referenced by alert rules: 离线 (id 4)", Code.FailedPrecondition); } });
  await openRowAction("tg（#1）", "删除");
  fireEvent.click(screen.getByRole("menuitem", { name: "确认删除 tg（#1）" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("notify channel 1 is referenced by alert rules: 离线 (id 4)");
  expect(screen.getByRole("cell", { name: "tg" })).toBeInTheDocument();
});

it("编辑往返撤销已武装的删除确认", async () => {
  render({});
  await openRowAction("tg（#1）", "删除");
  expect(screen.getByRole("menuitem", { name: "确认删除 tg（#1）" })).toBeInTheDocument();
  fireEvent.click(screen.getByRole("menuitem", { name: "编辑 tg（#1）" }));
  fireEvent.click(screen.getByRole("button", { name: "取消" }));
  fireEvent.click(screen.getByRole("button", { name: "更多操作 tg（#1）" }));
  expect(screen.getByRole("menuitem", { name: "删除 tg（#1）" })).toBeInTheDocument();
  expect(screen.queryByRole("menuitem", { name: "确认删除 tg（#1）" })).toBeNull();
});

it("mutation 配置不携带展示层方法", async () => {
  const { queryClient } = render({ testNotifyChannel: async () => ({}) });
  await openRowAction("tg（#1）", "发送测试");
  await waitFor(() => expect(screen.getByRole("status").textContent).toBe("已向 tg 发送测试消息"));
  const mutations = queryClient.getMutationCache().getAll();
  expect(mutations.length).toBeGreaterThan(0);
  for (const m of mutations) expect(m.options).not.toHaveProperty("isLatest");
});

it("表格可聚焦滚动", async () => {
  render({});
  const region = await screen.findByRole("region", { name: "通知渠道管理" });
  expect(region).toHaveAttribute("tabindex", "0");
  expect(within(region).getByRole("columnheader", { name: "操作" })).toBeInTheDocument();
});

const mk = (name: string) => create(ListNotifyChannelsResponseSchema, { channels: [
  { id: 1n, name: "tg", kind: ChannelKind.TELEGRAM, telegram: { chatId: "42", hasBotToken: true }, createdAt: 1_700_000_000n, ratePerMinute: 20 },
  { id: 2n, name, kind: ChannelKind.WEBHOOK, webhook: { method: "POST", hasUrl: true, urlHost: "https://hooks.example", headerNames: ["Authorization"], bodyTemplate: "{{.Summary}}" }, createdAt: 1_700_000_000n, ratePerMinute: 0 },
] });

it("删除首击不发请求", async () => {
  const removed: bigint[] = [];
  render({ deleteNotifyChannel: async (req) => { removed.push(req.id); throw new ConnectError("ref", Code.FailedPrecondition); } });
  await openRowAction("tg（#1）", "删除");
  fireEvent.click(screen.getByRole("menuitem", { name: "取消" }));
  fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
  await openRowAction("hook（#2）", "删除");
  fireEvent.click(screen.getByRole("menuitem", { name: "确认删除 hook（#2）" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("ref");
  expect(removed).toEqual([2n]);
});

it("新操作开始时清除测试提示", async () => {
  render({
    testNotifyChannel: async () => ({}),
    deleteNotifyChannel: async () => { throw new ConnectError("ref", Code.FailedPrecondition); },
  });
  await openRowAction("tg（#1）", "发送测试");
  await waitFor(() => expect(screen.getByRole("status").textContent).toBe("已向 tg 发送测试消息"));
  await openRowAction("tg（#1）", "删除");
  fireEvent.click(screen.getByRole("menuitem", { name: "确认删除 tg（#1）" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("ref");
  expect(screen.getByRole("status").textContent).toBe("");
});

it("编辑态在刷新完成后才关闭", async () => {
  let releaseList!: () => void;
  const listGate = new Promise<void>((r) => { releaseList = r; });
  let listCalls = 0;
  let current = mk("hook");
  render({ listNotifyChannels: async () => { listCalls++; if (listCalls > 1) await listGate; return current; },
    saveNotifyChannel: async () => { current = mk("hook2"); return {}; } });
  await openRowAction("hook（#2）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 hook（#2）" });
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "hook2" } });
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  try {
    await waitFor(() => expect(listCalls).toBe(2));
    vi.useFakeTimers();
    await act(async () => { await vi.runAllTimersAsync(); });
    expect(form).toBeInTheDocument();
  } finally { vi.useRealTimers(); await act(async () => { releaseList(); }); }
  await waitFor(() => expect(screen.queryByRole("form", { name: "编辑 hook（#2）" })).toBeNull());
  expect(screen.getByRole("cell", { name: "hook2" })).toBeInTheDocument();
});

it("单抽屉保存挂起时不能关闭或取消，刷新完成才退出", async () => {
  let releaseSave!: () => void;
  const saveGate = new Promise<void>((r) => { releaseSave = r; });
  render({ saveNotifyChannel: async () => { await saveGate; return {}; } });
  await openRowAction("hook（#2）", "编辑");
  const hookForm = screen.getByRole("form", { name: "编辑 hook（#2）" });
  fireEvent.click(within(hookForm).getByRole("button", { name: "保存" }));
  try {
    await waitFor(() => expect(screen.getByRole("button", { name: "关闭抽屉" })).toBeDisabled());
    expect(screen.getByRole("button", { name: "取消" })).toBeDisabled();
  } finally { await act(async () => { releaseSave(); }); }
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

it("创建失败保留草稿", async () => {
  render({ saveNotifyChannel: async () => { throw new ConnectError("channel.name: must not be empty", Code.InvalidArgument); } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "值班群" } });
  fireEvent.change(within(form).getByLabelText("Bot token"), { target: { value: "123:abc" } });
  fireEvent.change(within(form).getByLabelText("Chat ID"), { target: { value: "-100" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("channel.name: must not be empty");
  expect(within(screen.getByRole("form", { name: "新建通知渠道" })).getByLabelText("名称")).toHaveValue("值班群");
});

it("请求头行带序号分组与序号化移除名", async () => {
  render({});
  const form = await openCreate();
  fireEvent.click(within(form).getByRole("radio", { name: "Webhook" }));
  fireEvent.click(within(form).getByRole("button", { name: "添加请求头" }));
  fireEvent.click(within(form).getByRole("button", { name: "添加请求头" }));
  expect(within(form).getByRole("group", { name: "请求头 1" })).toBeInTheDocument();
  expect(within(form).getByRole("group", { name: "请求头 2" })).toBeInTheDocument();
  expect(within(form).getByRole("button", { name: "移除请求头 2" })).toBeInTheDocument();
  fireEvent.click(within(form).getByRole("button", { name: "移除请求头 1" }));
  expect(within(form).getByRole("group", { name: "请求头 1" })).toBeInTheDocument();
  expect(within(form).queryByRole("group", { name: "请求头 2" })).toBeNull();
});

it("移除中间请求头行不搬动其余行的输入节点", async () => {
  render({});
  const form = await openCreate();
  fireEvent.click(within(form).getByRole("radio", { name: "Webhook" }));
  const add = () => fireEvent.click(within(form).getByRole("button", { name: "添加请求头" }));
  add();
  add();
  add();
  const values = within(form).getAllByLabelText("请求头值");
  fireEvent.change(values[0], { target: { value: "v1" } });
  fireEvent.change(values[2], { target: { value: "v3" } });
  const third = within(form).getAllByLabelText("请求头值")[2];
  fireEvent.click(within(form).getByRole("button", { name: "移除请求头 2" }));
  const rest = within(form).getAllByLabelText("请求头值");
  expect(rest.map((el) => (el as HTMLInputElement).value)).toEqual(["v1", "v3"]);
  expect(rest[1]).toBe(third);
});

it("编辑时带出已存的节奏上限，改写后原样提交，清空即交给 hub 取默认值", async () => {
  const saved: SaveNotifyChannelRequest[] = [];
  render({ saveNotifyChannel: async (req) => { saved.push(req); return {}; } });
  await openRowAction("tg（#1）", "编辑");
  let form = screen.getByRole("form", { name: "编辑 tg（#1）" });
  const rate = within(form).getByLabelText("每分钟上限");
  expect(rate).toHaveValue(20);
  fireEvent.change(rate, { target: { value: "0" } });
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].channel!.ratePerMinute).toBe(0);
  await openRowAction("tg（#1）", "编辑");
  form = screen.getByRole("form", { name: "编辑 tg（#1）" });
  fireEvent.change(within(form).getByLabelText("每分钟上限"), { target: { value: "" } });
  expect(within(form).getByLabelText("每分钟上限")).toHaveAttribute("placeholder", "留空取默认 20");
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(2));
  expect(saved[1].channel!.ratePerMinute).toBeUndefined();
});

// 负数由表单校验拦在提交之前：不能靠协议编码 uint32 时报错兜底，那条路径会把编码错误当作保存失败显示出来。
it("节奏上限不接受负数", async () => {
  const saved: SaveNotifyChannelRequest[] = [];
  render({ saveNotifyChannel: async (req) => { saved.push(req); return {}; } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "值班群" } });
  fireEvent.change(within(form).getByLabelText("Bot token"), { target: { value: "123:abc" } });
  fireEvent.change(within(form).getByLabelText("Chat ID"), { target: { value: "-100" } });
  fireEvent.change(within(form).getByLabelText("每分钟上限"), { target: { value: "-1" } });
  expect(within(form).getByLabelText("每分钟上限")).toBeInvalid();
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await act(async () => {});
  expect(screen.queryByRole("alert")).toBeNull();
  fireEvent.change(within(form).getByLabelText("每分钟上限"), { target: { value: "3" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].channel!.ratePerMinute).toBe(3);
});

// 协议里是 uint32：更大的数由表单校验拦下，而不是在编码时报出一个看不出原因的错误。
it("节奏上限不超过 uint32", async () => {
  const saved: SaveNotifyChannelRequest[] = [];
  render({ saveNotifyChannel: async (req) => { saved.push(req); return {}; } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "值班群" } });
  fireEvent.change(within(form).getByLabelText("Bot token"), { target: { value: "123:abc" } });
  fireEvent.change(within(form).getByLabelText("Chat ID"), { target: { value: "-100" } });
  fireEvent.change(within(form).getByLabelText("每分钟上限"), { target: { value: "4294967296" } });
  expect(within(form).getByLabelText("每分钟上限")).toBeInvalid();
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await act(async () => {});
  expect(screen.queryByRole("alert")).toBeNull();
  fireEvent.change(within(form).getByLabelText("每分钟上限"), { target: { value: "4294967295" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].channel!.ratePerMinute).toBe(4294967295);
});

it("页头、六列表格与渠道类型分段使用统一契约", async () => {
  render({});
  await screen.findByRole("heading", { name: "通知渠道", level: 1 });
  const headers = screen.getAllByRole("columnheader").map((cell) => cell.textContent);
  const form = await openCreate();
  const group = within(form).getByRole("radiogroup", { name: "类型" });
  expect({ headers, kinds: within(group).getAllByRole("radio").map((radio) => radio.getAttribute("aria-label")) }).toEqual({
    headers: ["名称", "类型", "目标", "节奏上限", "创建于", "操作"], kinds: ["Telegram", "Webhook"],
  });
});

it("列表移除正在编辑的渠道后保留草稿，保存失败原文留在抽屉", async () => {
  let removed = false;
  const { queryClient } = render({
    listNotifyChannels: async () => removed ? { channels: [] } : channels,
    saveNotifyChannel: async () => { throw new ConnectError("channel 1 not found", Code.NotFound); },
  });
  await openRowAction("tg（#1）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 tg（#1）" });
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "未保存的渠道" } });
  removed = true;
  await act(async () => { await queryClient.refetchQueries(); });
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  const dialog = screen.getByRole("dialog");
  const error = await within(dialog).findByRole("alert");
  expect({ form: within(dialog).getByRole("form"), name: within(form).getByLabelText<HTMLInputElement>("名称").value, error: error.textContent }).toEqual({ form, name: "未保存的渠道", error: "channel 1 not found" });
});

it("同名渠道经第二条菜单打开后只保存第二条 id", async () => {
  const saved: SaveNotifyChannelRequest[] = [];
  render({
    listNotifyChannels: async () => ({ channels: channels.channels.map((channel) => ({ ...channel, name: "同名" })) }),
    saveNotifyChannel: async (req) => { saved.push(req); return {}; },
  });
  await openRowAction("同名（#2）", "编辑");
  fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved.map((req) => req.channel?.id)).toEqual([2n]));
});

it("没有通知渠道时只有空态卡，不画只有表头的表", async () => {
  render({ listNotifyChannels: async () => ({ channels: [] }) });
  await expectEmptyState("还没有通知渠道。", { region: "通知渠道管理" });
});
