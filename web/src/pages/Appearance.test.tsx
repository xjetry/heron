import { isFieldSet } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { act, fireEvent, screen, waitFor, within, type BoundFunctions, type queries } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { GeoBackend, SettingsSchema, type UpdateSettingsRequest } from "../gen/heron/v1/admin_pb";
import { MAX_LOGO_BYTES } from "../lib/appearance";
import { BUILT_IN_ACCENT } from "../lib/palette";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { statefulHub } from "../test/settingsHub";
import { Appearance } from "./Appearance";

const current = { title: "机房", theme: "dark", accentColor: "#123abc", logo: "", customCss: "body { margin: 0 }" };
const routes = [{ path: "/appearance", Component: Appearance }];
const render = (impl: AdminImpl) => renderWithAdmin({ getSettings: async () => ({ settings: current }), listNotifyChannels: async () => ({ channels: [] }), ...impl }, routes, "/appearance");

it("保存外观不回传登录通知配置", async () => {
  const sent: UpdateSettingsRequest[] = [];
  render({ getSettings: async () => ({ settings: { ...current, loginNotify: { channelIds: [7n] } } }), updateSettings: async (r) => { sent.push(r); return { settings: r.settings }; } });
  const f = await form();
  fireEvent.click(f.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(sent.map((r) => ({ title: r.settings?.title, login: r.settings?.loginNotify }))).toEqual([{ title: "机房", login: undefined }]));
});

async function form() {
  return within(await screen.findByRole("form", { name: "公开页外观" }));
}

it("公开页总闸显示当前值并显式提交 false 与 true", async () => {
  const hub = statefulHub({ ...current, publicEnabled: true });
  const sent = hub.sent;
  render(hub.impl);
  const f = await form();
  const toggle = f.getByRole("checkbox", { name: "启用公开页" });
  expect(toggle).toBeChecked();
  expect(f.getByText(/节点的公开标记保留/)).toBeInTheDocument();
  fireEvent.click(toggle);
  hub.holdReads();
  fireEvent.click(f.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(sent.map((r) => r.settings?.publicEnabled)).toEqual([false]));
  expect(await f.findByRole("status")).toHaveTextContent("已保存");
  // 保存后的重新拉取还挂着，开关此时显示的只能是写进缓存的回显。
  await waitFor(() => expect(toggle).not.toBeChecked());
  hub.releaseReads();
  await waitFor(() => expect(toggle).toBeEnabled());
  fireEvent.click(toggle);
  fireEvent.click(f.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(sent.map((r) => r.settings?.publicEnabled)).toEqual([false, true]));
});

// 草稿是开始编辑时的快照。之后别处关了公开页，只改标题的保存不得带总闸——带上快照里的"开"就会把它重新打开。
it("开关没动过时保存不带总闸，别处关掉的公开页不被重新打开", async () => {
  const hub = statefulHub({ ...current, publicEnabled: true });
  const { queryClient } = render(hub.impl);
  const f = await form();
  const toggle = f.getByRole("checkbox", { name: "启用公开页" });
  expect(toggle).toBeChecked();
  fireEvent.change(f.getByLabelText("标题"), { target: { value: "新标题" } });
  hub.set({ publicEnabled: false });
  await act(() => queryClient.refetchQueries());
  // 查询通知经 setTimeout 调度，重新拉取落地后还要等一次渲染。
  await waitFor(() => expect(toggle).not.toBeChecked());
  expect(f.getByLabelText("标题")).toHaveValue("新标题");
  fireEvent.click(f.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(hub.sent).toHaveLength(1));
  expect(hub.sent[0].settings?.title).toBe("新标题");
  expect(isFieldSet(hub.sent[0].settings!, SettingsSchema.field.publicEnabled)).toBe(false);
  expect(await f.findByRole("status")).toHaveTextContent("已保存");
  expect(hub.state().publicEnabled).toBe(false);
  expect(toggle).not.toBeChecked();
  // 动过开关才带：这次显式打开。
  fireEvent.click(toggle);
  fireEvent.click(f.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(hub.sent).toHaveLength(2));
  expect(isFieldSet(hub.sent[1].settings!, SettingsSchema.field.publicEnabled)).toBe(true);
  expect(hub.sent[1].settings?.publicEnabled).toBe(true);
});

it("表单显示当前设置，标题留空时提示内置标题", async () => {
  render({ getSettings: async () => ({ settings: { ...current, title: "" } }) });
  const f = await form();
  expect(f.getByLabelText("标题")).toHaveValue("");
  expect(f.getByLabelText("标题")).toHaveAttribute("placeholder", "服务器状态");
  expect(f.getByLabelText("明暗")).toHaveValue("dark");
  expect(f.getByLabelText("主色")).toHaveValue("#123abc");
  expect(f.getByLabelText("自定义 CSS")).toHaveValue("body { margin: 0 }");
});

it("保存提交全部外观字段，表单改显 hub 实际保存的值", async () => {
  const sent: UpdateSettingsRequest[] = [];
  render({ updateSettings: async (req) => { sent.push(req); return { settings: { ...req.settings!, title: "新标题", accentColor: "#abcdef" } }; } });
  const f = await form();
  fireEvent.change(f.getByLabelText("标题"), { target: { value: " 新标题 " } });
  fireEvent.change(f.getByLabelText("明暗"), { target: { value: "auto" } });
  fireEvent.change(f.getByLabelText("主色"), { target: { value: "#ABCDEF" } });
  fireEvent.click(f.getByRole("button", { name: "保存" }));
  expect(await f.findByRole("status")).toHaveTextContent("已保存");
  expect(sent).toHaveLength(1);
  expect(sent[0].settings).toMatchObject({ title: " 新标题 ", theme: "auto", accentColor: "#ABCDEF", logo: "", customCss: "body { margin: 0 }" });
  expect(f.getByLabelText("标题")).toHaveValue("新标题");
  expect(f.getByLabelText("主色")).toHaveValue("#abcdef");
});

it("hub 拒绝时显示错误原文并保留草稿", async () => {
  render({ updateSettings: async () => { throw new ConnectError('settings.custom_css must not contain "</" (it could end the page\'s <style> element); found at byte 0', Code.InvalidArgument); } });
  const f = await form();
  fireEvent.change(f.getByLabelText("自定义 CSS"), { target: { value: "</style>" } });
  fireEvent.click(f.getByRole("button", { name: "保存" }));
  expect(await f.findByRole("alert")).toHaveTextContent('settings.custom_css must not contain "</"');
  expect(f.getByLabelText("自定义 CSS")).toHaveValue("</style>");
});

it("选中的 logo 以 data: URL 提交，可移除", async () => {
  const sent: UpdateSettingsRequest[] = [];
  render({ updateSettings: async (req) => { sent.push(req); return { settings: req.settings }; } });
  const f = await form();
  expect(f.getByLabelText("logo")).toHaveAttribute("accept", "image/png,image/jpeg,image/webp,image/svg+xml");
  fireEvent.change(f.getByLabelText("logo"), { target: { files: [new File([new Uint8Array([137, 80, 78, 71])], "logo.png", { type: "image/png" })] } });
  expect(await f.findByRole("img", { name: "logo 预览" })).toHaveAttribute("src", "data:image/png;base64,iVBORw==");
  fireEvent.click(f.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(sent.map((r) => r.settings?.logo)).toEqual(["data:image/png;base64,iVBORw=="]));
  fireEvent.click(f.getByRole("button", { name: "移除 logo" }));
  expect(f.queryByRole("img", { name: "logo 预览" })).toBeNull();
});

it("超出大小上限的 logo 在提交前报出，不发请求", async () => {
  const sent: UpdateSettingsRequest[] = [];
  render({ updateSettings: async (req) => { sent.push(req); return { settings: req.settings }; } });
  const f = await form();
  // 98304 字节编码成 131072 个 base64 字符，恰等于上限；加上 data:image/png;base64, 前缀就超出。
  const size = (MAX_LOGO_BYTES / 4) * 3;
  fireEvent.change(f.getByLabelText("logo"), { target: { files: [new File([new Uint8Array(size)], "big.png", { type: "image/png" })] } });
  expect(await f.findByRole("alert")).toHaveTextContent(`上限 ${MAX_LOGO_BYTES} 字节`);
  expect(f.getByRole("button", { name: "保存" })).toBeDisabled();
  // 按钮禁用只是提示，不发请求由 submit 里的检查承载：直接触发表单的 submit 事件验证它。
  // 随后换成合规的 logo 再保存；超限的那次若也发出了，会排在前面。
  fireEvent.submit(f.getByRole("button", { name: "保存" }).closest("form")!);
  fireEvent.change(f.getByLabelText("logo"), { target: { files: [new File([new Uint8Array([137, 80, 78, 71])], "logo.png", { type: "image/png" })] } });
  await f.findByRole("img", { name: "logo 预览" });
  await waitFor(() => expect(f.getByRole("button", { name: "保存" })).toBeEnabled());
  fireEvent.click(f.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(sent.length).toBeGreaterThan(0));
  expect(sent[0].settings?.logo).toBe("data:image/png;base64,iVBORw==");
});

// 保存进行中整个表单禁用：飞行中的保存返回时，回显覆盖的只能是这次提交自己送出的内容。
it("保存进行中表单禁用，回显落地后恢复可编辑", async () => {
  let resolve: (v: { settings: typeof current }) => void = () => {};
  let calls = 0;
  render({ updateSettings: () => { calls++; return new Promise((r) => { resolve = r; }); } });
  const f = await form();
  fireEvent.change(f.getByLabelText("标题"), { target: { value: "第一次" } });
  fireEvent.click(f.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(f.getByLabelText("标题")).toBeDisabled());
  for (const label of ["明暗", "主色", "取色", "logo", "自定义 CSS"]) expect(f.getByLabelText(label)).toBeDisabled();
  expect(f.getByRole("button", { name: "保存" })).toBeDisabled();
  // 按钮禁用之外，submit 自己也不在进行中再发：直接触发 submit 事件验证。
  fireEvent.submit(f.getByRole("button", { name: "保存" }).closest("form")!);
  resolve({ settings: { ...current, title: "第一次" } });
  expect(await f.findByRole("status")).toHaveTextContent("已保存");
  expect(f.getByLabelText("标题")).toBeEnabled();
  expect(f.getByLabelText("标题")).toHaveValue("第一次");
  expect(calls).toBe(1);
});

// 读 logo 文件是异步的：读取期间不能保存（读完的回调会改草稿），读取期间改的其他字段在读完后仍在。
it("读 logo 期间不能保存，期间的编辑不丢", async () => {
  const sent: UpdateSettingsRequest[] = [];
  render({ updateSettings: async (req) => { sent.push(req); return { settings: req.settings }; } });
  const f = await form();
  fireEvent.change(f.getByLabelText("logo"), { target: { files: [new File([new Uint8Array([137, 80, 78, 71])], "logo.png", { type: "image/png" })] } });
  expect(f.getByRole("button", { name: "保存" })).toBeDisabled();
  fireEvent.submit(f.getByRole("button", { name: "保存" }).closest("form")!);
  fireEvent.change(f.getByLabelText("标题"), { target: { value: "读取期间改的" } });
  await f.findByRole("img", { name: "logo 预览" });
  expect(f.getByLabelText("标题")).toHaveValue("读取期间改的");
  await waitFor(() => expect(f.getByRole("button", { name: "保存" })).toBeEnabled());
  fireEvent.click(f.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0].settings).toMatchObject({ title: "读取期间改的", logo: "data:image/png;base64,iVBORw==" });
});

// 主色为空时取色器显示内置主色；只有选了别的颜色才算设置，原样的兜底色（浏览器关上取色器时可能再报一次）不提交。
it("取色器的兜底色不当成设置提交", async () => {
  const sent: UpdateSettingsRequest[] = [];
  render({
    getSettings: async () => ({ settings: { ...current, accentColor: "" } }),
    updateSettings: async (req) => { sent.push(req); return { settings: req.settings }; },
  });
  const f = await form();
  expect(f.getByLabelText("取色")).toHaveValue(BUILT_IN_ACCENT);
  fireEvent.change(f.getByLabelText("取色"), { target: { value: BUILT_IN_ACCENT } });
  fireEvent.click(f.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0].settings?.accentColor).toBe("");
  fireEvent.change(f.getByLabelText("取色"), { target: { value: "#00ff00" } });
  fireEvent.click(f.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(sent).toHaveLength(2));
  expect(sent[1].settings?.accentColor).toBe("#00ff00");
});

// 可控的 FileReader：读取在测试调用 finish 时才结束，读取进行中的状态可以逐步观察。
class FakeReader {
  static all: FakeReader[] = [];
  result: string | null = null;
  error: unknown = null;
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  readAsDataURL() { FakeReader.all.push(this); }
  finish(dataUrl: string) { act(() => { this.result = dataUrl; this.onload?.(); }); }
}

function useFakeReader() {
  FakeReader.all = [];
  vi.stubGlobal("FileReader", FakeReader);
}

afterEach(() => vi.unstubAllGlobals());

const pick = (f: BoundFunctions<typeof queries>, name: string) =>
  fireEvent.change(f.getByLabelText("logo"), { target: { files: [new File([new Uint8Array([1])], name, { type: "image/png" })] } });

// 至多一个读者在飞：读取进行中文件输入禁用，reading 才恰好等于"有读者在飞"，保存也随之被拒。
// 文件输入若仍可用，第二次选文件会让先读完的那个把 reading 置回 false，保存在另一份还在读时发出。
it("读 logo 进行中文件输入与保存都禁用，读完恢复", async () => {
  useFakeReader();
  render({ updateSettings: async (req) => ({ settings: req.settings }) });
  const f = await form();
  pick(f, "a.png");
  expect(FakeReader.all).toHaveLength(1);
  expect(f.getByLabelText("logo")).toBeDisabled();
  expect(f.getByRole("button", { name: "保存" })).toBeDisabled();
  FakeReader.all[0].finish("data:image/png;base64,QQ==");
  expect(f.getByLabelText("logo")).toBeEnabled();
  expect(f.getByRole("button", { name: "保存" })).toBeEnabled();
  expect(f.getByRole("img", { name: "logo 预览" })).toHaveAttribute("src", "data:image/png;base64,QQ==");
});

it("读完 a 再选 b，最后选的生效", async () => {
  useFakeReader();
  const sent: UpdateSettingsRequest[] = [];
  render({ updateSettings: async (req) => { sent.push(req); return { settings: req.settings }; } });
  const f = await form();
  pick(f, "a.png");
  FakeReader.all[0].finish("data:image/png;base64,QQ==");
  pick(f, "b.png");
  FakeReader.all[1].finish("data:image/png;base64,Qg==");
  expect(f.getByRole("img", { name: "logo 预览" })).toHaveAttribute("src", "data:image/png;base64,Qg==");
  fireEvent.click(f.getByRole("button", { name: "保存" }));
  expect(await f.findByRole("status")).toHaveTextContent("已保存");
  expect(sent.map((r) => r.settings?.logo)).toEqual(["data:image/png;base64,Qg=="]);
});

// logo 字段的第二个写者"移除 logo"在读取中禁用：否则读取中点了移除，读完的回调又把 logo 写回来。
it("读 logo 进行中移除按钮禁用，读完恢复且移除生效", async () => {
  useFakeReader();
  render({ updateSettings: async (req) => ({ settings: req.settings }) });
  const f = await form();
  pick(f, "a.png");
  FakeReader.all[0].finish("data:image/png;base64,QQ==");
  expect(f.getByRole("button", { name: "移除 logo" })).toBeEnabled();
  pick(f, "b.png");
  expect(f.getByRole("button", { name: "移除 logo" })).toBeDisabled();
  FakeReader.all[1].finish("data:image/png;base64,Qg==");
  expect(f.getByRole("button", { name: "移除 logo" })).toBeEnabled();
  fireEvent.click(f.getByRole("button", { name: "移除 logo" }));
  expect(f.queryByRole("img", { name: "logo 预览" })).toBeNull();
});

// 换 logo 与改其他字段一样：上一次保存的错误不再描述当前草稿，读完即清掉。
it("上次保存失败后换 logo，旧的错误清掉", async () => {
  useFakeReader();
  render({ updateSettings: async () => { throw new ConnectError("settings.logo must be empty or data:<type>;base64,<data>", Code.InvalidArgument); } });
  const f = await form();
  fireEvent.click(f.getByRole("button", { name: "保存" }));
  expect(await f.findByRole("alert")).toHaveTextContent("settings.logo must be empty");
  pick(f, "a.png");
  FakeReader.all[0].finish("data:image/png;base64,QQ==");
  expect(f.queryByRole("alert")).toBeNull();
});

describe("国家 / 地区查询", () => {
  const geoForm = async () => within(await screen.findByRole("form", { name: "国家 / 地区查询" }));
  // hub 的 GetSettings 总带总闸、查询两项与 backup，并回显启动时选定的国家查询后端。
  const withGeo = {
    ...current, publicEnabled: true, geoEnabled: false, geoUrl: "https://ipinfo.io/{ip}/country", geoBackend: GeoBackend.HTTP, geoMmdbPath: "",
    backup: { region: "auto", configIntervalS: 300, metricsIntervalS: 86400, configKeep: 48, metricsKeep: 14, notify: { channelIds: [] }, hasSecret: false },
  };

  it("本地后端写明路径、不出网与服务地址不生效，不显示 HTTP 开启告知", async () => {
    render({ getSettings: async () => ({ settings: { ...withGeo, geoBackend: GeoBackend.MMDB, geoMmdbPath: "/data/country.mmdb" } }) });
    const f = await geoForm();
    expect(f.getByText("当前后端：本地文件 /data/country.mmdb，不出网；服务地址不生效。")).toBeInTheDocument();
    expect(f.queryByText(/开启即由 hub 把每个节点的来源地址发给/)).not.toBeInTheDocument();
    expect(f.getByLabelText("服务地址")).toHaveAccessibleDescription(/节点停在同一地址时查得一次即止；hub 记住每个节点最近 4 个地址的答案，在这些地址之间切换不再重查，\s*超过 4 个地址轮换或 hub 重启后会再查。/);
  });

  // 后端两项只回显：请求不带它们，hub 忽略请求里的值；保存后的描述取自回显写进
  // 缓存的那一份，本地库不会被说成 HTTP 服务。
  it("本地后端下保存查询设置：请求不带后端两项，保存后仍写明本地文件", async () => {
    const hub = statefulHub({ ...withGeo, geoBackend: GeoBackend.MMDB, geoMmdbPath: "/data/country.mmdb" });
    const sent = hub.sent;
    render(hub.impl);
    const f = await geoForm();
    fireEvent.click(f.getByRole("checkbox", { name: "按来源地址查询节点的国家 / 地区" }));
    hub.holdReads();
    fireEvent.click(f.getByRole("button", { name: "保存" }));
    expect(await f.findByRole("status")).toHaveTextContent("已保存");
    expect(sent).toHaveLength(1);
    expect(sent[0].settings).toMatchObject({ geoEnabled: true, geoBackend: GeoBackend.UNSPECIFIED, geoMmdbPath: "" });
    // 保存后的重新拉取还挂着，描述此时只能来自写进缓存的回显。
    expect(f.getByText("当前后端：本地文件 /data/country.mmdb，不出网；服务地址不生效。")).toBeInTheDocument();
    hub.releaseReads();
  });

  it("HTTP 后端显示当前已保存的服务地址", async () => {
    render({ getSettings: async () => ({ settings: { ...withGeo, geoBackend: GeoBackend.HTTP } }) });
    const f = await geoForm();
    expect(f.getByText("当前后端：HTTP 服务 https://ipinfo.io/{ip}/country")).toBeInTheDocument();
  });

  it("开关文案写明开启即把节点地址发给哪个服务，随输入的服务地址更新", async () => {
    render({ getSettings: async () => ({ settings: withGeo }) });
    const f = await geoForm();
    const toggle = f.getByRole("checkbox", { name: "按来源地址查询节点的国家 / 地区" });
    expect(toggle).not.toBeChecked();
    expect(f.getByLabelText("服务地址")).toHaveValue("https://ipinfo.io/{ip}/country");
    expect(f.getByLabelText("服务地址")).toHaveAccessibleDescription(/^开启即由 hub 把每个节点的来源地址发给 https:\/\/ipinfo\.io\/\{ip\}\/country（\{ip\} 处换成地址）/);
    fireEvent.change(f.getByLabelText("服务地址"), { target: { value: "https://geo.example/{ip}" } });
    expect(f.getByLabelText("服务地址")).toHaveAccessibleDescription(/^开启即由 hub 把每个节点的来源地址发给 https:\/\/geo\.example\/\{ip\}（/);
  });

  // 文案写出"不再外呼"的上界，不许诺无条件的"每地址一次"：hub 只记住每个节点最近 4 个地址的答案。
  it("开关说明写出答案表的上界", async () => {
    render({ getSettings: async () => ({ settings: withGeo }) });
    const f = await geoForm();
    const description = f.getByLabelText("服务地址");
    expect(description).toHaveAccessibleDescription(/节点停在同一地址时查得一次即止；hub 记住每个节点最近 4 个地址的答案，在这些地址之间切换不再外呼，\s*超过 4 个地址轮换或 hub 重启后会再查。/);
  });

  // 查询表单只提交自己这一组：外观五项全空（hub 按"这一组没给"对待外观，原样保留）、不带总闸与 backup。外观表单里未
  // 保存的标题与总闸既不随它提交，也仍留在外观表单的草稿里，不被查询表单保存后写进缓存的回显（总闸仍开）盖掉。
  it("保存只提交开关与服务地址，不带外观、总闸与备份", async () => {
    const hub = statefulHub(withGeo);
    const sent = hub.sent;
    render(hub.impl);
    const appearance = await form();
    fireEvent.change(appearance.getByLabelText("标题"), { target: { value: "未保存的标题" } });
    fireEvent.click(appearance.getByRole("checkbox", { name: "启用公开页" }));
    const f = await geoForm();
    fireEvent.click(f.getByRole("checkbox", { name: "按来源地址查询节点的国家 / 地区" }));
    fireEvent.click(f.getByRole("button", { name: "保存" }));
    expect(await f.findByRole("status")).toHaveTextContent("已保存");
    expect(sent).toHaveLength(1);
    expect(sent[0].settings).toMatchObject({ title: "", theme: "", accentColor: "", logo: "", customCss: "", geoEnabled: true, geoUrl: "https://ipinfo.io/{ip}/country" });
    expect(isFieldSet(sent[0].settings!, SettingsSchema.field.publicEnabled)).toBe(false);
    expect(isFieldSet(sent[0].settings!, SettingsSchema.field.backup)).toBe(false);
    expect(hub.state()).toMatchObject({ ...current, publicEnabled: true, geoEnabled: true });
    expect(appearance.getByLabelText("标题")).toHaveValue("未保存的标题");
    expect(appearance.getByRole("checkbox", { name: "启用公开页" })).not.toBeChecked();
  });

  // hub 对"一组都没给出"的请求报错。三个表单不改任何值直接保存也各自带着自己的组：外观表单带全部外观（明暗总有值），
  // 查询表单显式带开关与服务地址，备份表单带 backup，所以都不会撞上这条拒绝。
  it("三个表单不改值直接保存，各自带着自己的组", async () => {
    const hub = statefulHub(withGeo);
    render(hub.impl);
    for (const name of ["公开页外观", "国家 / 地区查询", "备份到 S3"]) {
      const f = within(await screen.findByRole("form", { name }));
      await waitFor(() => expect(f.getByRole("button", { name: "保存" })).toBeEnabled());
      fireEvent.click(f.getByRole("button", { name: "保存" }));
      expect(await f.findByRole("status")).toHaveTextContent("已保存");
    }
    expect(hub.sent).toHaveLength(3);
    expect(hub.sent[0].settings?.theme).toBe("dark");
    expect(isFieldSet(hub.sent[1].settings!, SettingsSchema.field.geoEnabled)).toBe(true);
    expect(isFieldSet(hub.sent[1].settings!, SettingsSchema.field.geoUrl)).toBe(true);
    expect(isFieldSet(hub.sent[2].settings!, SettingsSchema.field.backup)).toBe(true);
  });

  it("外观保存后刷新失败，查询表单保存不会把外观改回去", async () => {
    const hub = statefulHub(withGeo);
    render(hub.impl);
    const appearance = await form();
    hub.failReads();
    fireEvent.change(appearance.getByLabelText("标题"), { target: { value: " 新标题 " } });
    fireEvent.click(appearance.getByRole("button", { name: "保存" }));
    expect(await appearance.findByRole("status")).toHaveTextContent("已保存");
    expect(await screen.findByText("hub restarting")).toBeInTheDocument();
    const f = await geoForm();
    fireEvent.click(f.getByRole("checkbox", { name: "按来源地址查询节点的国家 / 地区" }));
    fireEvent.click(f.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(hub.sent).toHaveLength(2));
    expect(hub.sent[1].settings).toMatchObject({ title: "", theme: "", accentColor: "", logo: "", customCss: "", geoEnabled: true });
    expect(hub.state()).toMatchObject({ ...current, title: "新标题", geoEnabled: true });
  });

  it("查询表单保存后刷新失败，重新进入页面时显示刚保存的开关", async () => {
    const hub = statefulHub(withGeo);
    const { router } = renderWithAdmin(hub.impl, [...routes, { path: "/elsewhere", Component: () => null }], "/appearance");
    const f = await geoForm();
    hub.failReads();
    fireEvent.click(f.getByRole("checkbox", { name: "按来源地址查询节点的国家 / 地区" }));
    fireEvent.click(f.getByRole("button", { name: "保存" }));
    expect(await f.findByRole("status")).toHaveTextContent("已保存");
    expect(await screen.findByText("hub restarting")).toBeInTheDocument();
    await act(() => router.navigate("/elsewhere"));
    await act(() => router.navigate("/appearance"));
    expect((await geoForm()).getByRole("checkbox", { name: "按来源地址查询节点的国家 / 地区" })).toBeChecked();
  });

  it("外观表单不提交查询设置：hub 对缺席的两项不改", async () => {
    const sent: UpdateSettingsRequest[] = [];
    render({ getSettings: async () => ({ settings: { ...withGeo, geoEnabled: true } }), updateSettings: async (req) => { sent.push(req); return { settings: { ...req.settings!, geoEnabled: true, geoUrl: withGeo.geoUrl } }; } });
    const f = await form();
    fireEvent.click(f.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0].settings?.geoEnabled).toBeUndefined();
    expect(sent[0].settings?.geoUrl).toBeUndefined();
  });
});

// 外观页的三个设置表单共用 SAVE_SETTINGS（见 api/saveSettings.ts；登录通知表单在通知页，它与外观表单的互斥见
// SettingsForms.test.tsx）：任一个的保存在途时，另外两个的保存按钮禁用，直接触发 submit
// 也不发请求；在途的保存连同之后的刷新结束，其余表单恢复。逐个把每个表单当作在途的一方：某个表单的保存漏带这把键时，
// 以它为在途方的一组红；某个表单不按 useSettingsSaving 禁用自己时，以其余表单为在途方的两组红。
describe("设置表单的保存互斥", () => {
  const forms = ["公开页外观", "国家 / 地区查询", "备份到 S3"];
  for (const busy of forms) {
    it(`${busy}的保存在途时其余两个表单不能提交`, async () => {
      const hub = statefulHub({ ...current, publicEnabled: true, geoEnabled: false, geoUrl: "https://ipinfo.io/{ip}/country" });
      render(hub.impl);
      const others = forms.filter((name) => name !== busy);
      const saveButton = async (name: string) => within(await screen.findByRole("form", { name })).getByRole("button", { name: "保存" });
      for (const name of others) await screen.findByRole("form", { name });
      hub.holdSaves();
      fireEvent.click(await saveButton(busy));
      await waitFor(() => expect(hub.sent).toHaveLength(1));
      for (const name of others) {
        await waitFor(async () => expect(await saveButton(name)).toBeDisabled());
        fireEvent.submit(screen.getByRole("form", { name }));
      }
      hub.releaseSaves();
      expect(await within(screen.getByRole("form", { name: busy })).findByRole("status")).toHaveTextContent("已保存");
      for (const name of others) await waitFor(async () => expect(await saveButton(name)).toBeEnabled());
      expect(hub.sent).toHaveLength(1);
    });
  }

  // 互斥的理由而不是它的机制：两个保存同时在途、响应逆序到达时，后写进缓存的是先提交的那份回显，缺了另一次保存的改动；
  // 刷新失败时缓存一直停在那里，重新进入页面的表单从它初始化，再保存即把旧值写回。断言写这些后果，换成别的机制也照样约束。
  it("两个表单的保存响应逆序到达，重新进入的表单也不会把旧值写回", async () => {
    const hub = statefulHub({ ...current, publicEnabled: true, geoEnabled: true, geoUrl: "https://ipinfo.io/{ip}/country" });
    const { router, queryClient } = renderWithAdmin(hub.impl, [...routes, { path: "/elsewhere", Component: () => null }], "/appearance");
    const appearance = await form();
    const geo = async () => within(await screen.findByRole("form", { name: "国家 / 地区查询" }));
    const toggle = async () => (await geo()).getByRole("checkbox", { name: "按来源地址查询节点的国家 / 地区" });
    expect(await toggle()).toBeChecked();
    hub.failReads();
    hub.holdResponses();
    fireEvent.change(appearance.getByLabelText("标题"), { target: { value: "新标题" } });
    fireEvent.click(appearance.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(hub.sent).toHaveLength(1));
    fireEvent.click(await toggle());
    fireEvent.submit(screen.getByRole("form", { name: "国家 / 地区查询" }));
    // 放行前等在途的保存都到达 hub（互斥时查询表单的提交不发出，在途的只有外观那一个），放行后等它们连同刷新都结束。
    await waitFor(() => expect(hub.sent).toHaveLength(queryClient.isMutating()));
    hub.deliverNewestFirst();
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    expect(appearance.getByRole("status")).toHaveTextContent("已保存");
    if (!(await geo()).queryByRole("status")) {
      const save = (await geo()).getByRole("button", { name: "保存" });
      await waitFor(() => expect(save).toBeEnabled());
      fireEvent.click(save);
      await waitFor(() => expect(hub.sent).toHaveLength(2));
      hub.deliverNewestFirst();
    }
    expect(await (await geo()).findByRole("status")).toHaveTextContent("已保存");
    expect(hub.state()).toMatchObject({ title: "新标题", geoEnabled: false });
    await act(() => router.navigate("/elsewhere"));
    await act(() => router.navigate("/appearance"));
    expect(await toggle()).not.toBeChecked();
    fireEvent.change((await geo()).getByLabelText("服务地址"), { target: { value: "https://geo.example/{ip}" } });
    fireEvent.click((await geo()).getByRole("button", { name: "保存" }));
    await waitFor(() => expect(hub.sent).toHaveLength(3));
    hub.deliverNewestFirst();
    expect(await (await geo()).findByRole("status")).toHaveTextContent("已保存");
    expect(hub.state().geoEnabled).toBe(false);
  });
});

// 各分区的标题与说明在左栏、表单与状态在右栏（components/SettingsSection.tsx）；说明不再压在表单上方。
it("设置分区：标题与说明在左栏，表单在右栏", async () => {
  render({ getBackupStatus: async () => ({ enabled: false }), getHeartbeatStatus: async () => ({ enabled: false }) });
  const pairs = [["公开页", "公开页外观"], ["国家 / 地区查询", "国家 / 地区查询"], ["备份到 S3", "备份到 S3"], ["心跳外推", "心跳外推"]] as const;
  for (const [title, form] of pairs) {
    const heading = await screen.findByRole("heading", { level: 2, name: title });
    const section = heading.closest(".settings-section") as HTMLElement;
    expect(heading.closest(".settings-aside")).not.toBeNull();
    expect(within(section).getByRole("form", { name: form }).closest(".settings-main")).not.toBeNull();
  }
  const backup = (await screen.findByRole("heading", { level: 2, name: "备份状态" })).closest(".settings-section") as HTMLElement;
  expect(within(backup).getByText(/停用备份即结束两层的故障跟踪/).closest(".settings-aside")).not.toBeNull();
  expect(within(backup).getByRole("region", { name: "备份状态" }).closest(".settings-main")).not.toBeNull();
});
