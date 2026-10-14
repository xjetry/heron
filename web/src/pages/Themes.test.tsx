import { afterEach, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { AdminService, ListThemesResponseSchema } from "../gen/heron/v1/admin_pb";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { Themes } from "./Themes";
import { expectEmptyState } from "../test/empty";

const current = "a".repeat(64), previous = "b".repeat(64), available = "c".repeat(64), legacy = "d".repeat(64);
const label = (name: string, id: string, version: string, digest: string) => `${name}（${id}）${version} [${digest.slice(0, 12)}]`;
const dark = label("Dark", "dark", "1.2", current), old = label("Dark", "dark", "1.1", previous), plain = label("Plain", "plain", "0.1", available);
const listed = (publicDir = false) => create(ListThemesResponseSchema, {
  publicDir,
  themes: [
    { id: "dark", digest: current, sdk: 1, name: "Dark", version: "1.2", uploadedAt: 1_700_000_000n, enabled: true, published: true, hasPreview: true, repository: "owner/theme", release: "v1.2", asset: "theme.zip" },
    { id: "dark", digest: previous, sdk: 1, name: "Dark", version: "1.1", uploadedAt: 1_700_000_000n, previous: true, published: true },
    { id: "plain", digest: available, sdk: 1, name: "Plain", version: "0.1", uploadedAt: 1_700_000_000n },
    { id: "legacy", digest: legacy, sdk: 0, name: "Legacy", version: "0.0", uploadedAt: 1_700_000_000n },
  ],
});
const routes = [{ path: "/themes", Component: Themes }];
const render = (impl: AdminImpl = {}) => renderWithAdmin({ getBackupStatus: async () => ({}), listThemes: async () => listed(), getThemePreview: async () => ({ content: new Uint8Array([1]), contentType: "image/png" }), ...impl }, routes, "/themes");
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });
const pick = (bytes: number[]) => {
  const form = screen.getByRole("form", { name: "上传主题" });
  fireEvent.change(within(form).getByLabelText(/主题包/), { target: { files: [new File([new Uint8Array(bytes)], "theme.zip", { type: "application/zip" })] } });
  return form;
};

it("当前域名首页、来源和保留版本可见，不再要求独立 origin", async () => {
  render();
  expect(await screen.findByRole("link", { name: "打开公开首页" })).toHaveAttribute("href", "/");
  expect(screen.getByText(/当前启用 Dark/)).toBeVisible();
  expect(screen.getByText("可回滚")).toBeVisible();
  expect(screen.getByText("owner/theme · v1.2 · theme.zip")).toBeVisible();
  expect(screen.queryByText(/theme-origin/)).toBeNull();
  expect(screen.getByRole("button", { name: `回滚 ${old}` })).toBeEnabled();
});

it("未启用主题时说明使用内置页", async () => {
  render({ listThemes: async () => ({ themes: [] }) });
  expect(await screen.findByText(/当前使用内置公开页/)).toBeVisible();
  expect(screen.getByText("还没有主题。")).toBeVisible();
});

it("主题读取失败不伪装成配置缺失", async () => {
  render({ listThemes: async () => { throw new ConnectError("cannot load versions", Code.FailedPrecondition); } });
  expect(await screen.findByRole("alert")).toHaveTextContent("cannot load versions");
  expect(screen.queryByText(/theme-origin/)).toBeNull();
});

it("public-dir 接管阻止启用和回滚但仍可安装", async () => {
  render({ listThemes: async () => listed(true) });
  expect(await screen.findByRole("note", { name: "公开页由目录接管" })).toHaveTextContent("不能启用托管主题");
  expect(screen.getByRole("button", { name: `启用 ${plain}` })).toBeDisabled();
  expect(screen.getByRole("button", { name: `回滚 ${old}` })).toBeDisabled();
  expect(screen.getByRole("form", { name: "上传主题" })).toBeVisible();
});

it("旧 SDK 包只归档，不能预览或启用", async () => {
  render();
  const row = (await screen.findByRole("cell", { name: "Legacy" })).closest("tr")!;
  expect(within(row).getByRole("button", { name: /^预览 / })).toBeDisabled();
  expect(within(row).getByRole("button", { name: /^启用 / })).toBeDisabled();
  expect(within(row).getByText(/不支持 SDK/)).toBeVisible();
});

it("当前和上一版本不能删除，其他版本确认后按摘要删除", async () => {
  const remove = vi.fn(async () => ({}));
  render({ deleteThemeVersion: remove });
  await screen.findByRole("cell", { name: "Plain" });
  expect(screen.getByRole("button", { name: `删除 ${dark}` })).toBeDisabled();
  expect(screen.getByRole("button", { name: `删除 ${old}` })).toBeDisabled();
  fireEvent.click(screen.getByRole("button", { name: `删除 ${plain}` }));
  expect(remove).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: `确认删除 ${plain}` }));
  await waitFor(() => expect(remove).toHaveBeenCalledWith(expect.objectContaining({ id: "plain", digest: available }), expect.anything()));
});

it("启用和回滚发送精确版本，切回内置页发送空引用", async () => {
  const enable = vi.fn(async () => ({}));
  render({ enableTheme: enable });
  for (const [name, id, digest] of [[`启用 ${plain}`, "plain", available], [`回滚 ${old}`, "dark", previous], ["切回内置主题", "", ""]]) {
    await waitFor(() => expect(screen.getByRole("button", { name })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name }));
    await waitFor(() => expect(enable).toHaveBeenLastCalledWith(expect.objectContaining({ id, digest }), expect.anything()));
  }
});

it("每个主题仅有一个整包卸载入口，确认后清理全部版本并回落内置", async () => {
  let removed = false;
  const uninstall = vi.fn(async () => { removed = true; return {}; });
  render({ listThemes: async () => {
    const response = listed();
    if (removed) response.themes = response.themes.filter((theme) => theme.id !== "dark");
    return response;
  }, deleteTheme: uninstall });
  const button = await screen.findByRole("button", { name: "卸载全部版本 Dark（dark）" });
  expect(screen.getAllByRole("button", { name: "卸载全部版本 Dark（dark）" })).toHaveLength(1);
  fireEvent.click(button);
  expect(uninstall).not.toHaveBeenCalled();
  expect(screen.getByText(/将删除该主题的全部版本；若正在启用则回落到内置页/)).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "确认卸载全部版本 Dark（dark）" }));
  await waitFor(() => expect(uninstall).toHaveBeenCalledWith(expect.objectContaining({ id: "dark" }), expect.anything()));
  await waitFor(() => expect(screen.queryByRole("button", { name: "卸载全部版本 Dark（dark）" })).toBeNull());
  expect(screen.getByRole("cell", { name: "Plain" })).toBeVisible();
  expect(screen.getByText(/当前使用内置公开页/)).toBeVisible();
});

it("沙箱预览固定版本并给出新标签链接，不改变启用状态", async () => {
  const preview = vi.fn(async () => ({ url: "/_theme/preview/secret/" })), enable = vi.fn(async () => ({}));
  render({ previewTheme: preview, enableTheme: enable });
  fireEvent.click(await screen.findByRole("button", { name: `预览 ${plain}` }));
  const link = await screen.findByRole("link", { name: "打开沙箱预览" });
  expect(link).toHaveAttribute("href", "/_theme/preview/secret/");
  expect(link).toHaveAttribute("target", "_blank");
  expect(preview).toHaveBeenCalledWith(expect.objectContaining({ id: "plain", digest: available }), expect.anything());
  expect(enable).not.toHaveBeenCalled();
});

it.each(["启用", "删除", "上传", "GitHub 安装"])("%s成功后移除已撤销的预览链接", async (operation) => {
  render({
    previewTheme: async () => ({ url: "/_heron/preview/token/" }),
    enableTheme: async () => ({}), deleteThemeVersion: async () => ({}),
    uploadTheme: async () => ({}), installThemeRelease: async () => ({}),
    listThemeReleases: async () => ({ releases: [{ tag: "v1", assets: [{ id: 1n, name: "theme.zip", size: 1n }] }] }),
  });
  fireEvent.click(await screen.findByRole("button", { name: `预览 ${plain}` }));
  await screen.findByRole("link", { name: "打开沙箱预览" });
  if (operation === "启用") fireEvent.click(screen.getByRole("button", { name: `启用 ${plain}` }));
  if (operation === "删除") {
    fireEvent.click(screen.getByRole("button", { name: `删除 ${plain}` }));
    fireEvent.click(screen.getByRole("button", { name: `确认删除 ${plain}` }));
  }
  if (operation === "上传") fireEvent.click(within(pick([1])).getByRole("button", { name: "上传" }));
  if (operation === "GitHub 安装") {
    fireEvent.change(screen.getByLabelText("GitHub 仓库或 Release 链接"), { target: { value: "owner/theme" } });
    fireEvent.click(screen.getByRole("button", { name: "查询版本" }));
    fireEvent.change(await screen.findByLabelText("Release 版本"), { target: { value: "v1" } });
    fireEvent.change(screen.getByLabelText("ZIP 资产"), { target: { value: "1" } });
    fireEvent.click(screen.getByRole("button", { name: "安装所选资产" }));
  }
  await waitFor(() => expect(screen.queryByRole("link", { name: "打开沙箱预览" })).toBeNull());
});

it("预览图按摘要查询，不混用版本", async () => {
  const asked: string[] = [];
  render({ getThemePreview: async (req) => { asked.push(`${req.id}/${req.digest}`); return { content: new Uint8Array([1, 2, 3]), contentType: "image/webp" }; } });
  expect(await screen.findByRole("img", { name: "Dark 预览图" })).toHaveAttribute("src", "data:image/webp;base64,AQID");
  expect(asked).toEqual([`dark/${current}`]);
});

it("旧 SDK 原包按摘要下载为 ZIP，并释放 Blob URL", async () => {
  const archive = vi.fn(async () => ({ package: new Uint8Array([80, 75, 3, 4]) }));
  const createObjectURL = vi.fn((_blob: Blob) => "blob:theme-package"), revokeObjectURL = vi.fn();
  vi.stubGlobal("URL", class extends URL { static createObjectURL = createObjectURL; static revokeObjectURL = revokeObjectURL; });
  const clicked: { href: string; download: string }[] = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) { clicked.push({ href: this.href, download: this.download }); });
  render({ getThemePackage: archive });
  const row = (await screen.findByRole("cell", { name: "Legacy" })).closest("tr")!;
  fireEvent.click(within(row).getByRole("button", { name: /^下载原包 / }));
  await waitFor(() => expect(archive).toHaveBeenCalledWith(expect.objectContaining({ id: "legacy", digest: legacy }), expect.anything()));
  await waitFor(() => expect(clicked).toEqual([{ href: "blob:theme-package", download: `legacy-${legacy}.zip` }]));
  const blob = createObjectURL.mock.calls[0][0];
  expect(blob.type).toBe("application/zip");
  expect([...new Uint8Array(await blob.arrayBuffer())]).toEqual([80, 75, 3, 4]);
  expect(revokeObjectURL).toHaveBeenCalledWith("blob:theme-package");
});

it("缺少产物摘要时不能下载原包", async () => {
  render({ listThemes: async () => ({ themes: [{ id: "old", name: "Old" }] }) });
  const row = (await screen.findByRole("cell", { name: "Old" })).closest("tr")!;
  expect(within(row).getByRole("button", { name: /^下载原包 / })).toBeDisabled();
});

it("上传发送原包和更新目标，刷新列表与备份但不自动启用", async () => {
  const upload = vi.fn(async () => ({ theme: { id: "dark", name: "Dark", version: "1.3" } })), enable = vi.fn(async () => ({}));
  const { queryClient } = render({ uploadTheme: upload, enableTheme: enable });
  await screen.findByRole("img", { name: "Dark 预览图" });
  const invalidate = vi.spyOn(queryClient, "invalidateQueries"), form = pick([80, 75, 3, 4]);
  fireEvent.change(within(form).getByLabelText("用途"), { target: { value: "dark" } });
  expect(within(form).getAllByRole("option")).toHaveLength(4);
  fireEvent.click(within(form).getByRole("button", { name: "上传" }));
  expect(await screen.findByRole("status")).toHaveTextContent("已安装 Dark（dark）版本 1.3，尚未启用");
  expect(upload).toHaveBeenCalledWith(expect.objectContaining({ package: new Uint8Array([80, 75, 3, 4]), expectId: "dark" }), expect.anything());
  for (const queryKey of [
    createConnectQueryKey({ schema: AdminService.method.listThemes, cardinality: "finite" }),
    createConnectQueryKey({ schema: AdminService.method.getThemePreview, cardinality: "finite" }),
    createConnectQueryKey({ schema: AdminService.method.getBackupStatus, cardinality: "finite" }),
  ]) expect(invalidate).toHaveBeenCalledWith({ queryKey });
  expect(enable).not.toHaveBeenCalled();
  expect(within(form).getByRole("button", { name: "上传" })).toBeDisabled();
});

it("上传失败保留当前主题并展示错误", async () => {
  render({ uploadTheme: async () => { throw new ConnectError("SDK protocol is required", Code.InvalidArgument); } });
  await screen.findByRole("cell", { name: "Plain" });
  fireEvent.click(within(pick([1])).getByRole("button", { name: "上传" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("SDK protocol is required");
  expect(screen.getByText(/当前启用 Dark/)).toBeVisible();
});

it("GitHub 必须显式选择版本和资产，切换版本或仓库清除旧选择", async () => {
  const install = vi.fn(async () => ({ theme: { id: "plain", name: "Plain", version: "0.2" } }));
  const lookup = vi.fn(async () => ({ releases: [{ name: "Beta", tag: "v2", prerelease: true, assets: [{ id: 7n, name: "theme.zip", size: 42n }, { id: 8n, name: "extra.zip", size: 48n }] }, { name: "Stable", tag: "v1", assets: [{ id: 9n, name: "stable.zip", size: 32n }] }] }));
  render({ listThemeReleases: lookup, installThemeRelease: install });
  fireEvent.change(await screen.findByLabelText("GitHub 仓库或 Release 链接"), { target: { value: "owner/theme" } });
  fireEvent.click(screen.getByRole("button", { name: "查询版本" }));
  const select = await screen.findByLabelText("Release 版本");
  expect(screen.getByRole("button", { name: "安装所选资产" })).toBeDisabled();
  fireEvent.change(select, { target: { value: "v2" } });
  expect(screen.getByRole("option", { name: /Beta.*预发布/ })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "安装所选资产" })).toBeDisabled();
  fireEvent.change(screen.getByLabelText("ZIP 资产"), { target: { value: "8" } });
  fireEvent.change(select, { target: { value: "v1" } });
  expect(screen.getByLabelText("ZIP 资产")).toHaveValue("");
  expect(screen.getByRole("button", { name: "安装所选资产" })).toBeDisabled();
  fireEvent.change(screen.getByLabelText("ZIP 资产"), { target: { value: "9" } });
  fireEvent.change(screen.getByLabelText("GitHub 安装用途"), { target: { value: "plain" } });
  fireEvent.click(screen.getByRole("button", { name: "安装所选资产" }));
  expect(await screen.findByRole("status")).toHaveTextContent("已安装 Plain（plain）版本 0.2，尚未启用");
  expect(lookup).toHaveBeenCalledWith(expect.objectContaining({ repository: "owner/theme" }), expect.anything());
  expect(install).toHaveBeenCalledWith(expect.objectContaining({ repository: "owner/theme", tag: "v1", assetId: 9n, expectId: "plain" }), expect.anything());
  fireEvent.change(screen.getByLabelText("GitHub 仓库或 Release 链接"), { target: { value: "other/theme" } });
  expect(screen.queryByLabelText("Release 版本")).toBeNull();
});

it("GitHub 空版本、无资产和超限资产均明确提示", async () => {
  let empty = true;
  render({ listThemeReleases: async () => ({ releases: empty ? [] : [{ tag: "v1", assets: [] }, { tag: "v2", assets: [{ id: 8n, name: "large.zip", size: 8_388_609n }] }] }) });
  fireEvent.change(await screen.findByLabelText("GitHub 仓库或 Release 链接"), { target: { value: "owner/theme" } });
  fireEvent.click(screen.getByRole("button", { name: "查询版本" }));
  expect(await screen.findByText("没有公开的 Release。可上传作者提供的已构建主题包。")).toBeVisible();
  empty = false;
  fireEvent.click(screen.getByRole("button", { name: "查询版本" }));
  fireEvent.change(await screen.findByLabelText("Release 版本"), { target: { value: "v1" } });
  expect(screen.getByText("这个版本没有 ZIP 资产；GitHub 自动生成的源码归档不能安装。")).toBeVisible();
  fireEvent.change(screen.getByLabelText("Release 版本"), { target: { value: "v2" } });
  expect(screen.getByRole("option", { name: /large.zip.*超过 8 MiB/ })).toBeDisabled();
});

it("GitHub 限流错误不会隐藏当前主题", async () => {
  render({ listThemeReleases: async () => { throw new ConnectError("GitHub rate limit", Code.Unavailable); } });
  fireEvent.change(await screen.findByLabelText("GitHub 仓库或 Release 链接"), { target: { value: "owner/theme" } });
  fireEvent.click(screen.getByRole("button", { name: "查询版本" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("GitHub rate limit");
  expect(screen.getByText(/当前启用 Dark/)).toBeVisible();
});

it("显示原包备份缺口和上传时限", async () => {
  render({ getBackupStatus: async () => ({ themesWithoutPackage: ["plain"] }) });
  expect(await screen.findByText("主题 plain 未备份：请重新上传原包")).toBeVisible();
  expect(screen.getByText(/30 秒内传完/)).toBeVisible();
});

it("没有主题时只有空态卡，不画只有表头的表", async () => {
  render({ listThemes: async () => ({ themes: [] }) });
  await expectEmptyState("还没有主题。", { region: "主题管理" });
});

it("下载主题开发指南：skill 格式，文中的 hub 地址换成当前站点，不留占位符", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const blobs: Blob[] = [];
  vi.spyOn(URL, "createObjectURL").mockImplementation((b) => { blobs.push(b as Blob); return "blob:theme"; });
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  const clicked: HTMLAnchorElement[] = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) { clicked.push(this); });
  render();
  const card = within(await screen.findByRole("region", { name: "开发主题" }));
  expect(card.getByText(/~\/\.claude\/skills\/heron-theme\/SKILL\.md/)).toBeInTheDocument();
  fireEvent.click(card.getByRole("button", { name: "下载主题开发指南" }));
  expect(clicked).toHaveLength(1);
  expect(clicked[0].download).toBe("heron-theme-SKILL.md");
  const text = await blobs[0].text();
  expect(text).toMatch(/^---\nname: heron-theme\ndescription: .+\n---\n/);
  expect(text).not.toContain("__HERON_");
  expect(text).toContain(`HUB=${window.location.origin}`);
  expect(text).toContain(`${window.location.origin}/admin/`);
  expect(text).toContain("from '/_heron/theme-sdk.js'");
  vi.useRealTimers();
});
