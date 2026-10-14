import { Code, ConnectError } from "@connectrpc/connect";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import { PublicService } from "../gen/heron/v1/public_pb";
import { renderWithService } from "../test/harness";
import { PublicLayout } from "./Layout";
import { DEFAULT_TITLE } from "./site";
import heron from "../assets/heron.svg";
import { POLL_MS } from "../lib/poll";
import { PUBLIC_SCHEME_KEY } from "../lib/scheme";

afterEach(() => { document.title = ""; localStorage.clear(); delete document.documentElement.dataset.theme; });

// 顶栏装配边界见 PublicLayout 注释；测试同时核对允许的入口与不应出现的导航、铃铛、刷新、页脚。
it("顶栏只有标题链接、实时说明、GitHub 仓库、明暗切换与登录；没有导航与页脚", async () => {
  renderWithService(PublicService, { getSite: async () => ({ title: "机房", adminPath: "/admin/" }) }, [{ path: "/", Component: PublicLayout }], "/");
  const header = (await screen.findByRole("link", { name: "机房" })).closest("header")!;
  expect(within(header).getAllByRole("link").map((a) => a.getAttribute("aria-label") ?? a.textContent)).toEqual(["机房", "GitHub 仓库", "登录"]);
  const github = within(header).getByRole("link", { name: "GitHub 仓库" });
  expect(github).toHaveAttribute("href", "https://github.com/xjetry/heron");
  expect(github).toHaveAttribute("target", "_blank");
  expect(github).toHaveAttribute("rel", "noreferrer");
  expect(within(header).getByText(`实时 · 每 ${POLL_MS / 1000} 秒`)).toBeInTheDocument();
  expect(within(header).getByRole("button", { name: "明暗切换" })).toBeInTheDocument();
  expect(screen.queryByRole("navigation")).toBeNull();
  expect(screen.queryByRole("contentinfo")).toBeNull();
});

it("访客点明暗切换：立即写 data-theme 并记到 localStorage，压过站点设置", async () => {
  renderWithService(PublicService, { getSite: async () => ({ theme: "dark" }) }, [{ path: "/", Component: PublicLayout }], "/");
  await waitFor(() => expect(document.documentElement.dataset.theme).toBe("dark"));
  fireEvent.click(screen.getByRole("button", { name: "明暗切换" }));   // auto → light
  await waitFor(() => expect(document.documentElement.dataset.theme).toBe("light"));
  expect(localStorage.getItem(PUBLIC_SCHEME_KEY)).toBe("light");
});

// 取不到站点设置（这里是被限流）时按全部为空处理：标签页标题与页头同为内置标题。
it("站点设置取不到时标签页标题与页头一致", async () => {
  renderWithService(PublicService, { getSite: async () => { throw new ConnectError("rate limit exceeded", Code.ResourceExhausted); } },
    [{ path: "/", Component: PublicLayout }], "/");
  expect(await screen.findByRole("alert")).toHaveTextContent("rate limit exceeded");
  expect(screen.getByRole("link", { name: DEFAULT_TITLE })).toBeInTheDocument();
  expect(screen.getByRole("link", { name: DEFAULT_TITLE }).querySelector("img")).toHaveAttribute("src", heron);
  expect(screen.queryByRole("link", { name: "登录" })).not.toBeInTheDocument();
  await waitFor(() => expect(document.title).toBe(DEFAULT_TITLE));
});

it("自定义站点标题与 logo 不被 Heron 默认品牌覆盖", async () => {
  renderWithService(PublicService, { getSite: async () => ({ title: "我的机房", logo: "https://example.com/custom.svg" }) },
    [{ path: "/", Component: PublicLayout }], "/");
  const brand = await screen.findByRole("link", { name: "我的机房" });
  expect(brand.querySelectorAll("img")).toHaveLength(1);
  expect(brand.querySelector("img")).toHaveAttribute("src", "https://example.com/custom.svg");
  await waitFor(() => expect(document.title).toBe("我的机房"));
});

// 登录入口跟着 hub 下发的 admin_path 走：有值即整页跳转，空串与取不到站点设置时都不出现。
it("hub 下发面板路径时页头有登录入口", async () => {
  renderWithService(PublicService, { getSite: async () => ({ adminPath: "/admin/" }) },
    [{ path: "/", Component: PublicLayout }], "/");
  expect(await screen.findByRole("link", { name: "登录" })).toHaveAttribute("href", "/admin/");
});

it("面板路径为空时页头没有登录入口", async () => {
  renderWithService(PublicService, { getSite: async () => ({ title: "我的机房", adminPath: "" }) },
    [{ path: "/", Component: PublicLayout }], "/");
  await screen.findByRole("link", { name: "我的机房" });
  expect(screen.queryByRole("link", { name: "登录" })).not.toBeInTheDocument();
});
