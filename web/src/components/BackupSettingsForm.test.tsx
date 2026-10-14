import { isFieldSet, toJson } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { expect, it } from "vitest";
import { SettingsSchema } from "../gen/heron/v1/admin_pb";
import { Appearance } from "../pages/Appearance";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { statefulHub } from "../test/settingsHub";

const backup = { endpoint: "https://s3.example", bucket: "private-backups", region: "auto", accessKey: "access", prefix: "hub", configIntervalS: 300, metricsIntervalS: 86400, configKeep: 48, metricsKeep: 14, notify: { channelIds: [7n] }, hasSecret: true };
// hub 的 GetSettings 总带总闸、国家查询两项与 backup（还有 login_notify，这里的表单不读它，略去）。
const saved = { title: "机房", theme: "dark", accentColor: "#123abc", logo: "", customCss: "body { margin: 0 }", publicEnabled: true, geoEnabled: false, geoUrl: "https://ipinfo.io/{ip}/country", backup };
const channels = { channels: [{ id: 7n, name: "运维" }, { id: 8n, name: "值班" }] };

const routes = [{ path: "/appearance", Component: Appearance }];
const render = (impl: AdminImpl) => renderWithAdmin({ getSettings: async () => ({ settings: saved }), listNotifyChannels: async () => channels, ...impl }, routes, "/appearance");
const backupForm = async () => within(await screen.findByRole("form", { name: "备份到 S3" }));
const appearanceForm = async () => within(await screen.findByRole("form", { name: "公开页外观" }));

it("通知渠道选满 16 后禁用未选项，取消后允许重选", async () => {
  render({ listNotifyChannels: async () => ({ channels: Array.from({ length: 17 }, (_, i) => ({ id: BigInt(i+1), name: `渠道${i+1}` })) }),
    getSettings: async () => ({ settings: { ...saved, backup: { ...backup, notify: { channelIds: [] } } } }) });
  const form = await backupForm();
  const boxes = form.getAllByRole("checkbox");
  for (const box of boxes.slice(0,16)) fireEvent.click(box);
  expect(form.getByText("最多选 16 个渠道")).toBeInTheDocument();
  expect(boxes[16]).toBeDisabled();
  expect(boxes[0]).toBeEnabled();
  fireEvent.click(boxes[0]);
  expect(boxes[16]).toBeEnabled();
});

it("备份表单保存：secret 只写且下次缺席，渠道以 notify 提交，关闭用空 endpoint", async () => {
  const hub = statefulHub(saved);
  const sent = hub.sent;
  render(hub.impl);
  const form = await backupForm();
  const secret = form.getByLabelText("Secret");
  expect(secret).toHaveValue("");
  expect(secret).toHaveAttribute("type", "password");
  expect(form.getByLabelText("配置周期（秒）")).toHaveValue(300);
  expect(form.getByLabelText("指标周期（秒）")).toHaveValue(86400);
  expect(form.getByLabelText("运维（#7）")).toBeChecked();
  fireEvent.change(secret, { target: { value: "new-secret" } });
  fireEvent.change(form.getByLabelText("配置保留份数"), { target: { value: "50" } });
  fireEvent.click(form.getByLabelText("值班（#8）"));
  fireEvent.click(form.getByRole("button", { name: "保存" }));
  await form.findByRole("status");
  expect(sent[0].settings?.backup).toMatchObject({ secret: "new-secret", configKeep: 50, notify: { channelIds: [7n, 8n] } });
  expect(sent[0].settings?.backup?.hasSecret).toBe(false);
  expect(secret).toHaveValue("");
  fireEvent.change(form.getByLabelText("Endpoint"), { target: { value: "" } });
  fireEvent.click(form.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(sent).toHaveLength(2));
  expect(sent[1].settings?.backup?.secret).toBeUndefined();
  expect(sent[1].settings?.backup?.endpoint).toBe("");
});

it("备份周期越界时不发送保存请求，恢复有效边界后可保存", async () => {
  const hub = statefulHub(saved);
  const sent = hub.sent;
  render(hub.impl);
  const form = await backupForm();
  const element = screen.getByRole("form", { name: "备份到 S3" });
  const field = form.getByLabelText("配置周期（秒）");
  fireEvent.change(field, { target: { value: "0" } });
  fireEvent.submit(element);
  fireEvent.change(field, { target: { value: "60" } });
  fireEvent.submit(element);
  await form.findByRole("status");
  expect(sent).toHaveLength(1);
  expect(sent[0].settings?.backup?.configIntervalS).toBe(60);
});

it("Secret 旁显示 hub 是否已保存 secret", async () => {
  render({ getSettings: async () => ({ settings: { ...saved, backup: { ...backup, hasSecret: false } } }) });
  expect((await backupForm()).getByLabelText("Secret")).toHaveAccessibleDescription("Secret 未设置");
});

it("Secret 已保存时如实显示", async () => {
  render({});
  expect((await backupForm()).getByLabelText("Secret")).toHaveAccessibleDescription("Secret 已保存");
});

// 两个表单各管各的：只改外观时请求里没有 backup（hub 对缺席的 backup 不改）。
it("外观表单的保存不带 backup", async () => {
  const hub = statefulHub(saved);
  const sent = hub.sent;
  render(hub.impl);
  const form = await appearanceForm();
  fireEvent.change(form.getByLabelText("标题"), { target: { value: "新标题" } });
  fireEvent.click(form.getByRole("button", { name: "保存" }));
  await form.findByRole("status");
  expect(sent).toHaveLength(1);
  expect(sent[0].settings?.title).toBe("新标题");
  expect(sent[0].settings?.backup).toBeUndefined();
  expect(hub.state().backup).toEqual(backup);
});

// 备份表单只提交 backup 这一组：外观五项全空（hub 按"这一组没给"对待外观，原样保留），总闸与国家查询两项都不带，hub
// 对缺席的这几组不改。外观表单里未保存的标题与总闸仍留在草稿里，不被备份保存后写进缓存的回显（总闸仍开）盖掉。
it("备份表单只提交 backup，不带外观、总闸与国家查询", async () => {
  const hub = statefulHub(saved);
  const sent = hub.sent;
  render(hub.impl);
  const appearance = await appearanceForm();
  fireEvent.change(appearance.getByLabelText("标题"), { target: { value: "未保存的标题" } });
  fireEvent.click(appearance.getByRole("checkbox", { name: "启用公开页" }));
  const form = await backupForm();
  fireEvent.change(form.getByLabelText("配置保留份数"), { target: { value: "40" } });
  fireEvent.click(form.getByRole("button", { name: "保存" }));
  await form.findByRole("status");
  expect(sent).toHaveLength(1);
  expect(Object.keys(toJson(SettingsSchema, sent[0].settings!)!)).toEqual(["backup"]);
  expect(sent[0].settings?.backup?.configKeep).toBe(40);
  for (const field of [SettingsSchema.field.publicEnabled, SettingsSchema.field.geoEnabled, SettingsSchema.field.geoUrl]) {
    expect(isFieldSet(sent[0].settings!, field)).toBe(false);
  }
  expect(hub.state()).toMatchObject({ title: "机房", theme: "dark", accentColor: "#123abc", customCss: "body { margin: 0 }", publicEnabled: true });
  expect(hub.state().backup?.configKeep).toBe(40);
  expect(appearance.getByLabelText("标题")).toHaveValue("未保存的标题");
  expect(appearance.getByRole("checkbox", { name: "启用公开页" })).not.toBeChecked();
});

// 备份表单同样经 useAdoptSavedSettings：保存后的刷新失败时，缓存里已是 hub 的回显，重新进入页面显示刚保存的值。
it("备份保存后刷新失败，重新进入页面时显示刚保存的值", async () => {
  const hub = statefulHub(saved);
  const { router } = renderWithAdmin({ ...hub.impl, listNotifyChannels: async () => channels }, [...routes, { path: "/elsewhere", Component: () => null }], "/appearance");
  const form = await backupForm();
  hub.failReads();
  fireEvent.change(form.getByLabelText("配置保留份数"), { target: { value: "40" } });
  fireEvent.click(form.getByRole("button", { name: "保存" }));
  expect(await form.findByRole("status")).toHaveTextContent("已保存");
  expect(await screen.findByText("hub restarting")).toBeInTheDocument();
  await act(() => router.navigate("/elsewhere"));
  await act(() => router.navigate("/appearance"));
  expect((await backupForm()).getByLabelText("配置保留份数")).toHaveValue(40);
});

// 外观保存在途时备份表单不能提交；外观保存完成之后备份表单照常保存，只带 backup，hub 里新外观与新备份都在。
it("两个表单的保存互斥，外观保存完成后备份照常保存", async () => {
  const hub = statefulHub(saved);
  const sent = hub.sent;
  render(hub.impl);
  const appearance = await appearanceForm();
  const form = await backupForm();
  fireEvent.change(appearance.getByLabelText("标题"), { target: { value: "新标题" } });
  hub.holdSaves();
  fireEvent.click(appearance.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(form.getByRole("button", { name: "保存" })).toBeDisabled());
  fireEvent.submit(screen.getByRole("form", { name: "备份到 S3" }));
  expect(sent).toHaveLength(1);
  hub.releaseSaves();
  await appearance.findByRole("status");
  await waitFor(() => expect(form.getByRole("button", { name: "保存" })).toBeEnabled());
  fireEvent.change(form.getByLabelText("配置保留份数"), { target: { value: "40" } });
  fireEvent.click(form.getByRole("button", { name: "保存" }));
  await form.findByRole("status");
  expect(sent).toHaveLength(2);
  expect(sent[1].settings?.title).toBe("");
  expect(hub.state().title).toBe("新标题");
  expect(hub.state().backup?.configKeep).toBe(40);
});

// 反方向：备份保存在途时外观表单不能提交；备份保存完成、设置重新读到之后，外观表单照常保存。
it("备份保存在途时外观表单不能提交", async () => {
  const hub = statefulHub(saved);
  const sent = hub.sent;
  render(hub.impl);
  const appearance = await appearanceForm();
  const form = await backupForm();
  fireEvent.change(form.getByLabelText("配置保留份数"), { target: { value: "40" } });
  hub.holdSaves();
  fireEvent.click(form.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(appearance.getByRole("button", { name: "保存" })).toBeDisabled());
  fireEvent.submit(screen.getByRole("form", { name: "公开页外观" }));
  expect(sent).toHaveLength(1);
  hub.releaseSaves();
  await form.findByRole("status");
  await waitFor(() => expect(appearance.getByRole("button", { name: "保存" })).toBeEnabled());
  fireEvent.change(appearance.getByLabelText("标题"), { target: { value: "新标题" } });
  fireEvent.click(appearance.getByRole("button", { name: "保存" }));
  await appearance.findByRole("status");
  // 备份表单只带 backup，标题为空串；外观表单的保存带的是新标题。
  expect(sent.map((r) => r.settings?.title)).toEqual(["", "新标题"]);
});

// 渠道列表读不到时不渲染备份表单：拿空列表求交会把已选渠道作为显式空集合提交，关掉备份失败通知。
it("渠道列表读取失败时不给出备份表单", async () => {
  render({ listNotifyChannels: async () => { throw new ConnectError("channels unavailable", Code.Unavailable); } });
  await appearanceForm();
  expect(await screen.findByText(/channels unavailable/)).toBeInTheDocument();
  expect(screen.queryByRole("form", { name: "备份到 S3" })).toBeNull();
});
