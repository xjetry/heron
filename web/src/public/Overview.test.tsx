import { create } from "@bufbuild/protobuf";
import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { PublicService, PublicSnapshotSchema } from "../gen/heron/v1/public_pb";
import { POLL_MS } from "../lib/poll";
import { renderWithService } from "../test/harness";
import { PublicOverview } from "./Overview";
import { FACET_SEARCH_AT } from "../components/Facet";
import { PUBLIC_FACET_MODE_KEYS, PUBLIC_TAG_MATCH_KEY, PUBLIC_VIEW_KEY, PUBLIC_WALL_GROUP_KEY } from "./prefs";
import { chooseOption, querySelectTrigger, selectTrigger } from "../test/select";

const snapshot = create(PublicSnapshotSchema, {
  now: 1_000n,
  reportIntervalMs: 4000,
  tags: ["db", "prod", "web"],
  nodes: [
    { id: 1n, name: "web-1", online: true, lastSeenAt: 998n, sortOrder: 0, country: "JP", tags: ["web", "prod"], metrics: { cpuPct: 42 } },
    { id: 2n, name: "db-1", online: false, lastSeenAt: 900n, sortOrder: 1, country: "JP", tags: ["db", "prod"], publicRemark: "联通 4837" },
    { id: 3n, name: "lab-1", online: true, lastSeenAt: 998n, sortOrder: 2, country: "HK", tags: ["DB"] },
    { id: 4n, name: "bare-1", online: false, sortOrder: 3, country: "" },
  ],
});
afterEach(() => { vi.useRealTimers(); localStorage.clear(); });

// 状态墙把在线、离线与从未上报都画成方块，筛选用例在墙上看结果；卡片视图把离线折起来。
const wall = () => fireEvent.click(within(screen.getByRole("group", { name: "视图" })).getByRole("button", { name: "状态墙" }));
const shown = () => screen.queryAllByRole("link").filter((a) => a.closest(".tile")).map((a) => a.getAttribute("aria-label"));
// 地区与标签是筛选行上的入口按钮（名称「地区 全部」这样带摘要），点开后面板是同名的 group，选项是带计数的胶囊按钮。
const trigger = (label: string) => screen.getByRole("button", { name: new RegExp(`^${label} `) });
const open = (label: string) => fireEvent.click(trigger(label));
const panel = (label: string) => within(screen.getByRole("group", { name: label }));
const chips = (label: string) => within(panel(label).getByRole("list")).getAllByRole("button").map((b) => b.textContent);
const pick = (label: string, name: string) => fireEvent.click(within(panel(label).getByRole("list")).getByRole("button", { name: new RegExp(`^${name}( \\d+)?$`) }));
const mode = (label: string, name: "单选" | "多选") => fireEvent.click(within(panel(label).getByRole("group", { name: "选择方式" })).getByRole("button", { name }));
function render(getSnapshot: () => Promise<typeof snapshot> = async () => snapshot) {
  renderWithService(PublicService, { getSnapshot }, [{ path: "/", Component: PublicOverview }], "/");
}

it("汇总、筛选行与节点一起出现；地区与标签入口点开后平铺带计数的选项，同一时间只展开一项", async () => {
  render();
  expect(await screen.findByText("2 / 4 在线")).toBeInTheDocument();
  wall();
  expect(shown()).toEqual(["web-1", "db-1", "lab-1", "bare-1"]);
  expect(trigger("地区")).toHaveAttribute("aria-expanded", "false");
  expect(screen.queryByRole("group", { name: "地区" })).toBeNull();
  open("地区");
  expect(trigger("地区")).toHaveAttribute("aria-expanded", "true");
  expect(trigger("地区")).toHaveAttribute("aria-controls", screen.getByRole("group", { name: "地区" }).id);
  expect(chips("地区")).toEqual(["全部", "香港 1", "日本 2", "未知 1"]);
  // 面板在 DOM 里紧跟自己的入口，Tab 从入口直接进面板（视觉上排到筛选行最后由 CSS 负责，e2e 核对）。
  expect(trigger("地区").nextElementSibling).toBe(screen.getByRole("group", { name: "地区" }));
  open("标签");
  expect(trigger("标签").nextElementSibling).toBe(screen.getByRole("group", { name: "标签" }));
  expect(screen.queryByRole("group", { name: "地区" })).toBeNull();
  expect(trigger("地区")).toHaveAttribute("aria-expanded", "false");
  // 标签的集合与顺序取 hub 下发的并集（按折叠键排序），页面不自己汇总、排序；计数按折叠比较，DB 的节点算进 db。
  expect(chips("标签")).toEqual(["全部", "db 2", "prod 2", "web 1"]);
  expect(panel("标签").queryByRole("searchbox")).toBeNull();
  open("标签");
  expect(screen.queryByRole("group", { name: "标签" })).toBeNull();
});

it("默认单选：点一个只看它，点另一个换成它，再点当前那一个回到全部；入口显示所选的名字", async () => {
  render();
  await screen.findByText("2 / 4 在线");
  wall();
  open("地区");
  expect(within(panel("地区").getByRole("group", { name: "选择方式" })).getByRole("button", { name: "单选" })).toHaveAttribute("aria-pressed", "true");
  expect(panel("地区").getByRole("button", { name: "全部" })).toHaveAttribute("aria-pressed", "true");
  pick("地区", "日本");
  expect(shown()).toEqual(["web-1", "db-1"]);
  expect(trigger("地区")).toHaveAccessibleName("地区 日本");
  expect(panel("地区").getByRole("button", { name: "全部" })).toHaveAttribute("aria-pressed", "false");
  pick("地区", "香港");
  expect(shown()).toEqual(["lab-1"]);
  expect(trigger("地区")).toHaveAccessibleName("地区 香港");
  pick("地区", "香港");
  expect(shown()).toEqual(["web-1", "db-1", "lab-1", "bare-1"]);
  expect(trigger("地区")).toHaveAccessibleName("地区 全部");
  pick("地区", "未知");
  pick("地区", "全部");
  expect(shown()).toEqual(["web-1", "db-1", "lab-1", "bare-1"]);
  expect(panel("地区").queryByText("显示属于任一所选地区的节点。")).toBeNull();
});

it("多选：地区并集、标签交集，与搜索、只看在线叠加；计数按筛选后的节点算；滤空时说明", async () => {
  render();
  await screen.findByText("2 / 4 在线");
  wall();
  open("地区");
  mode("地区", "多选");
  expect(panel("地区").getByText("显示属于任一所选地区的节点。")).toBeInTheDocument();
  expect(panel("地区").queryByRole("group", { name: "匹配方式" })).toBeNull();
  pick("地区", "日本");
  pick("地区", "未知");
  expect(shown()).toEqual(["web-1", "db-1", "bare-1"]);
  expect(trigger("地区")).toHaveAccessibleName("地区 已选 2 个");
  expect(screen.getByText("1 / 3 在线")).toBeInTheDocument();
  open("标签");
  mode("标签", "多选");
  expect(panel("标签").getByText("只显示同时带有全部所选标签的节点。")).toBeInTheDocument();
  pick("标签", "prod");
  pick("标签", "web");
  expect(shown()).toEqual(["web-1"]);
  pick("标签", "web");
  expect(shown()).toEqual(["web-1", "db-1"]);
  fireEvent.click(screen.getByRole("button", { name: "只看在线" }));
  expect(shown()).toEqual(["web-1"]);
  fireEvent.change(screen.getByRole("searchbox", { name: "搜索节点" }), { target: { value: "4837" } });
  expect(shown()).toEqual([]);
  expect(screen.getByText("没有符合筛选条件的节点。")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "只看在线" }));
  expect(shown()).toEqual(["db-1"]);
});

it("标签折叠比较：选 db 时 DB 的节点也命中", async () => {
  render();
  await screen.findByText("2 / 4 在线");
  wall();
  open("标签");
  pick("标签", "db");
  expect(shown()).toEqual(["lab-1", "db-1"]);
});

const match = (name: "满足任一" | "同时满足") => within(panel("标签").getByRole("group", { name: "匹配方式" })).getByRole("button", { name });

it("标签多选可切换满足任一 / 同时满足，默认同时满足；单选时不出这个切换；选择记到 localStorage", async () => {
  render();
  await screen.findByText("2 / 4 在线");
  wall();
  open("标签");
  expect(panel("标签").queryByRole("group", { name: "匹配方式" })).toBeNull();
  mode("标签", "多选");
  expect(match("同时满足")).toHaveAttribute("aria-pressed", "true");
  pick("标签", "db");
  pick("标签", "web");
  expect(shown()).toEqual([]);
  fireEvent.click(match("满足任一"));
  expect(match("满足任一")).toHaveAttribute("aria-pressed", "true");
  expect(panel("标签").getByText("显示带有任一所选标签的节点。")).toBeInTheDocument();
  expect(shown()).toEqual(["web-1", "db-1", "lab-1"]);
  expect(screen.getByText("2 / 3 在线")).toBeInTheDocument();
  expect(localStorage.getItem(PUBLIC_TAG_MATCH_KEY)).toBe("any");
  cleanup();
  render();
  await screen.findByText("2 / 4 在线");
  wall();
  open("标签");
  expect(match("满足任一")).toHaveAttribute("aria-pressed", "true");
  pick("标签", "prod");
  pick("标签", "web");
  expect(shown()).toEqual(["web-1", "db-1"]);
  fireEvent.click(match("同时满足"));
  expect(shown()).toEqual(["web-1"]);
  expect(localStorage.getItem(PUBLIC_TAG_MATCH_KEY)).toBe("all");
});

it("从多选切回单选只留按选项顺序最前的已选项，与勾选先后无关", async () => {
  render();
  await screen.findByText("2 / 4 在线");
  wall();
  open("地区");
  mode("地区", "多选");
  pick("地区", "未知");
  pick("地区", "香港");
  expect(shown()).toEqual(["lab-1", "bare-1"]);
  mode("地区", "单选");
  expect(shown()).toEqual(["lab-1"]);
  expect(trigger("地区")).toHaveAccessibleName("地区 香港");
  pick("地区", "日本");
  expect(shown()).toEqual(["web-1", "db-1"]);
});

it("单选 / 多选按入口各自记到 localStorage，下次打开沿用；默认单选", async () => {
  render();
  await screen.findByText("2 / 4 在线");
  open("地区");
  mode("地区", "多选");
  expect(localStorage.getItem(PUBLIC_FACET_MODE_KEYS.region)).toBe("multi");
  expect(localStorage.getItem(PUBLIC_FACET_MODE_KEYS.tag)).toBeNull();
  cleanup();
  render();
  await screen.findByText("2 / 4 在线");
  open("地区");
  expect(within(panel("地区").getByRole("group", { name: "选择方式" })).getByRole("button", { name: "多选" })).toHaveAttribute("aria-pressed", "true");
  open("标签");
  expect(within(panel("标签").getByRole("group", { name: "选择方式" })).getByRole("button", { name: "单选" })).toHaveAttribute("aria-pressed", "true");
  mode("标签", "多选");
  mode("标签", "单选");
  expect(localStorage.getItem(PUBLIC_FACET_MODE_KEYS.tag)).toBe("single");
});

it("Esc 收起面板，焦点回到入口按钮；在入口上按 Esc 同样收起", async () => {
  render();
  await screen.findByText("2 / 4 在线");
  open("地区");
  const chip = within(panel("地区").getByRole("list")).getByRole("button", { name: /^日本/ });
  chip.focus();
  fireEvent.keyDown(chip, { key: "Escape" });
  expect(screen.queryByRole("group", { name: "地区" })).toBeNull();
  expect(trigger("地区")).toHaveFocus();
  expect(trigger("地区")).toHaveAttribute("aria-expanded", "false");
  open("标签");
  fireEvent.keyDown(trigger("标签"), { key: "Escape" });
  expect(screen.queryByRole("group", { name: "标签" })).toBeNull();
});

const manyTags = Array.from({ length: FACET_SEARCH_AT + 1 }, (_, i) => `t${String(i + 1).padStart(2, "0")}`);
const withTags = (tags: string[]) => ({ ...snapshot, tags, nodes: snapshot.nodes.map((n, i) => ({ ...n, tags: i === 0 ? tags : [] })) });

it("选项超过 FACET_SEARCH_AT 个时出现搜索框，按折叠字面匹配收窄；没有匹配时说明", async () => {
  render(async () => withTags([...manyTags, "a.b", "axb"]));
  await screen.findByText("2 / 4 在线");
  open("标签");
  const box = panel("标签").getByRole("searchbox", { name: "搜索标签" });
  fireEvent.change(box, { target: { value: "T1" } });
  expect(chips("标签")).toEqual(["全部", "t10 1", "t11 1", "t12 1", "t13 1"]);
  fireEvent.change(box, { target: { value: "a.b" } });
  expect(chips("标签")).toEqual(["全部", "a.b 1"]);
  fireEvent.change(box, { target: { value: "zz" } });
  expect(chips("标签")).toEqual(["全部"]);
  expect(panel("标签").getByText("没有匹配的选项")).toBeInTheDocument();
});

it("选项恰好 FACET_SEARCH_AT 个时不出搜索框", async () => {
  render(async () => withTags(manyTags.slice(0, FACET_SEARCH_AT)));
  await screen.findByText("2 / 4 在线");
  open("标签");
  expect(chips("标签")).toHaveLength(FACET_SEARCH_AT + 1);
  expect(panel("标签").queryByRole("searchbox")).toBeNull();
});

it("被选中的标签或地区从快照消失后从选择集里移除，不留下看不见的过滤", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  let current = snapshot;
  render(async () => current);
  await screen.findByText("2 / 4 在线");
  wall();
  open("标签");
  pick("标签", "web");
  open("地区");
  pick("地区", "未知");
  expect(shown()).toEqual([]);
  current = { ...snapshot, tags: ["db", "prod"], nodes: snapshot.nodes.map((n) => ({ ...n, tags: n.tags?.filter((t) => t !== "web"), country: n.country || "CA" })) };
  await act(async () => vi.advanceTimersByTimeAsync(POLL_MS + 100));
  await waitFor(() => expect(shown()).toEqual(["web-1", "db-1", "lab-1", "bare-1"]));
  expect(trigger("标签")).toHaveAccessibleName("标签 全部");
  expect(trigger("地区")).toHaveAccessibleName("地区 全部");
  expect(panel("地区").getByRole("button", { name: "全部" })).toHaveAttribute("aria-pressed", "true");
});

it("没有任何节点带标签时不画标签入口", async () => {
  render(async () => ({ ...snapshot, tags: [], nodes: snapshot.nodes.map((n) => ({ ...n, tags: [] })) }));
  await screen.findByText("2 / 4 在线");
  expect(screen.queryByRole("button", { name: /^标签 / })).toBeNull();
  expect(trigger("地区")).toBeInTheDocument();
});

it("展开着的标签面板在标签全部消失时收起，标签重新出现时不自己弹开", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  let current = snapshot;
  render(async () => current);
  await screen.findByText("2 / 4 在线");
  open("标签");
  current = { ...snapshot, tags: [], nodes: snapshot.nodes.map((n) => ({ ...n, tags: [] })) };
  await act(async () => vi.advanceTimersByTimeAsync(POLL_MS + 100));
  await waitFor(() => expect(screen.queryByRole("button", { name: /^标签 / })).toBeNull());
  current = snapshot;
  await act(async () => vi.advanceTimersByTimeAsync(POLL_MS + 100));
  await waitFor(() => expect(trigger("标签")).toHaveAttribute("aria-expanded", "false"));
  expect(screen.queryByRole("group", { name: "标签" })).toBeNull();
});

it("没有公开节点时说明，不画汇总与筛选行", async () => {
  render(async () => ({ ...snapshot, now: 1n, nodes: [], tags: [] }));
  expect(await screen.findByText("没有公开的节点。")).toBeInTheDocument();
  expect(screen.queryByRole("group", { name: "筛选" })).toBeNull();
});

it("视图切换：没选过时默认卡片带排序；状态墙带着色依据；列表带排序", async () => {
  render();
  await screen.findByText("2 / 4 在线");
  const views = within(screen.getByRole("group", { name: "视图" }));
  expect(views.getByRole("button", { name: "卡片" })).toHaveAttribute("aria-pressed", "true");
  expect(selectTrigger("排序")).toHaveAccessibleName("排序 默认");
  expect(querySelectTrigger("着色依据")).toBeNull();
  expect(screen.getAllByRole("article").map((a) => a.getAttribute("aria-label"))).toEqual(["web-1", "lab-1"]);
  wall();
  expect(views.getByRole("button", { name: "状态墙" })).toHaveAttribute("aria-pressed", "true");
  expect(selectTrigger("着色依据")).toHaveAccessibleName("着色依据 状态");
  expect(querySelectTrigger("排序")).toBeNull();
  fireEvent.click(views.getByRole("button", { name: "列表" }));
  expect(views.getByRole("button", { name: "列表" })).toHaveAttribute("aria-pressed", "true");
  expect(selectTrigger("排序")).toHaveAccessibleName("排序 默认");
  expect(querySelectTrigger("着色依据")).toBeNull();
  expect(querySelectTrigger("分组")).toBeNull();
  expect(within(screen.getByRole("region", { name: "节点列表" })).getAllByRole("row").slice(1).map((r) => r.getAttribute("aria-label"))).toEqual(["web-1", "db-1", "lab-1", "bare-1"]);
  expect(shown()).toEqual([]);
});

const groupHeads = () => Array.from(document.querySelectorAll(".wall-group > summary")).map((s) => s.textContent);

it("状态墙可按标签分组：节点进它的每个标签组，组名取 hub 的写法，无标签最后；分组下拉只在状态墙出现", async () => {
  render();
  await screen.findByText("2 / 4 在线");
  expect(querySelectTrigger("分组")).toBeNull();
  wall();
  expect(selectTrigger("分组")).toHaveAccessibleName("分组 地区");
  expect(groupHeads()).toEqual(["日本 · 1 / 2 在线", "香港 · 1 / 1 在线", "未知 · 0 / 1 在线"]);
  chooseOption("分组", "标签");
  expect(groupHeads()).toEqual(["db · 1 / 2 在线", "prod · 1 / 2 在线", "web · 1 / 1 在线", "无标签 · 0 / 1 在线"]);
  expect(shown()).toEqual(["db-1", "lab-1", "web-1", "db-1", "web-1", "bare-1"]);
});

it("没有任何标签时不出分组下拉，按地区分组", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  let current = snapshot;
  render(async () => current);
  await screen.findByText("2 / 4 在线");
  wall();
  chooseOption("分组", "标签");
  current = { ...snapshot, tags: [], nodes: snapshot.nodes.map((n) => ({ ...n, tags: [] })) };
  await act(async () => vi.advanceTimersByTimeAsync(POLL_MS + 100));
  await waitFor(() => expect(querySelectTrigger("分组")).toBeNull());
  expect(groupHeads()).toEqual(["日本 · 1 / 2 在线", "香港 · 1 / 1 在线", "未知 · 0 / 1 在线"]);
});

it("搜索框：/ 与 ⌘K 聚焦，空时显示「输入 / 搜索」提示，有内容时不显示；在别的输入框里打 / 不抢焦点", async () => {
  render();
  await screen.findByText("2 / 4 在线");
  const box = screen.getByRole("searchbox", { name: "搜索节点" });
  expect(box).toHaveAttribute("aria-keyshortcuts", "/ Meta+K Control+K");
  const hint = () => box.parentElement!.querySelector(".search-hint");
  expect(hint()).toHaveTextContent("输入 / 搜索名称、标签、备注");
  fireEvent.keyDown(document.body, { key: "/" });
  expect(box).toHaveFocus();
  fireEvent.change(box, { target: { value: "web" } });
  expect(hint()).toBeNull();
  box.blur();
  fireEvent.keyDown(window, { key: "k", metaKey: true });
  expect(box).toHaveFocus();
});

it("在标签面板的搜索框里打 / 不抢焦点", async () => {
  render(async () => withTags(manyTags));
  await screen.findByText("2 / 4 在线");
  open("标签");
  const tagSearch = panel("标签").getByRole("searchbox", { name: "搜索标签" });
  tagSearch.focus();
  fireEvent.keyDown(tagSearch, { key: "/" });
  expect(tagSearch).toHaveFocus();
});

it("状态墙分组选择记到 localStorage，下次打开沿用", async () => {
  localStorage.setItem(PUBLIC_VIEW_KEY, "wall");
  render();
  await screen.findByText("2 / 4 在线");
  chooseOption("分组", "标签");
  expect(localStorage.getItem(PUBLIC_WALL_GROUP_KEY)).toBe("tag");
  cleanup();
  render();
  await screen.findByText("2 / 4 在线");
  expect(selectTrigger("分组")).toHaveAccessibleName("分组 标签");
  expect(groupHeads()).toEqual(["db · 1 / 2 在线", "prod · 1 / 2 在线", "web · 1 / 1 在线", "无标签 · 0 / 1 在线"]);
});

it("访客选的视图记到 localStorage，下次打开沿用", async () => {
  render();
  await screen.findByText("2 / 4 在线");
  wall();
  expect(localStorage.getItem(PUBLIC_VIEW_KEY)).toBe("wall");
  cleanup();
  render();
  await screen.findByText("2 / 4 在线");
  expect(within(screen.getByRole("group", { name: "视图" })).getByRole("button", { name: "状态墙" })).toHaveAttribute("aria-pressed", "true");
  expect(shown()).toEqual(["web-1", "db-1", "lab-1", "bare-1"]);
  fireEvent.click(within(screen.getByRole("group", { name: "视图" })).getByRole("button", { name: "卡片" }));
  expect(localStorage.getItem(PUBLIC_VIEW_KEY)).toBe("cards");
});

it("按 POLL_MS 轮询快照", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const getSnapshot = vi.fn(async () => snapshot);
  render(getSnapshot);
  await screen.findByText("2 / 4 在线");
  await act(async () => vi.advanceTimersByTimeAsync(POLL_MS + 100));
  await waitFor(() => expect(getSnapshot.mock.calls.length).toBeGreaterThanOrEqual(2));
});
