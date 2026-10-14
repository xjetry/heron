import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { BillingCycle } from "../gen/heron/v1/types_pb";
import { Renewals } from "./Renewals";

// hub 的今天是 2026-01-01：每个节点的 daysLeft 都按它算（expiresOn − today）。
const node = (id: bigint, name: string, expiresOn: string, daysLeft: number | undefined, billingCycle = BillingCycle.MONTHLY, autoRenew = false) => ({
  id, name, public: false, note: "", sortOrder: Number(id), createdAt: 0n, trafficResetDay: 1, tags: [],
  billing: { price: "5", currency: "USD", billingCycle, expiresOn, autoRenew, daysLeft },
});
const fleet = [
  node(1n, "tokyo", "2026-01-02", 1),
  node(2n, "osaka", "2026-01-10", 9, BillingCycle.YEARLY, true),
  node(3n, "seoul", "2026-01-10", 9, BillingCycle.UNSPECIFIED),
  node(4n, "paris", "2025-12-20", -12),
];

const render = (impl: AdminImpl) => renderWithAdmin({ listTags: async () => ({ tags: [] }), ...impl }, [{ path: "/renewals", Component: Renewals }], "/renewals");
const day = (ymd: string) => screen.getByRole("button", { name: new RegExp(`^${ymd}，`) });

// 进程原来的时区（可能由 TZ=… 跑整组用例时给出）在每条用例之后还原，假时区不漏到别的用例。
const startTZ = process.env.TZ;
afterEach(() => {
  vi.useRealTimers();
  if (startTZ === undefined) delete process.env.TZ;
  else process.env.TZ = startTZ;
});

describe("Renewals", () => {
  // 浏览器所在的时区与 hub 的不同：hub 的今天（2026-01-01，由 daysLeft 反推）、浏览器本地的日历日、浏览器时钟的 UTC
  // 日历日三者互不相同，格子、今天与默认选中的日子仍是 hub 的。两个方向各一例：基里巴斯（UTC+14）本地已是 1 月 3 日、
  // UTC 是 1 月 2 日；帕果帕果（UTC−11）本地还是 12 月 30 日、UTC 是 12 月 31 日。按浏览器本地日期或按浏览器时钟的
  // UTC 日期取今天的实现，在两例里都会把今天标错一天以上。
  it.each([
    ["Pacific/Kiritimati", "2026-01-02T10:30:00Z", 3, 2],
    ["Pacific/Pago_Pago", "2025-12-31T10:30:00Z", 30, 31],
  ])("日期按 hub 时区（浏览器在 %s）", async (zone, instant, localDate, utcDate) => {
    process.env.TZ = zone;
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date(instant));
    // 假时区真的生效了：浏览器的两种日历日都不是 hub 的今天，否则这条用例证明不了什么。
    expect([new Date().getDate(), new Date().getUTCDate()]).toEqual([localDate, utcDate]);
    render({ listNodes: async () => ({ nodes: fleet }) });
    expect(await screen.findByText("2026 年 1 月")).toBeInTheDocument();
    const grid = screen.getByRole("table", { name: "2026 年 1 月到期日历" });
    const current = within(grid).getAllByRole("button").filter((b) => b.getAttribute("aria-current") === "date");
    expect(current.map((b) => b.dataset.ymd)).toEqual(["2026-01-01"]);
    expect(day("2026-01-01")).toHaveAttribute("aria-pressed", "true");
    expect(day("2026-01-02")).toHaveAccessibleName("2026-01-02，1 台到期");
    expect(day("2026-01-10")).toHaveAccessibleName("2026-01-10，2 台到期");
    expect(screen.getByRole("heading", { name: "2026-01-01 到期" })).toBeInTheDocument();
    expect(screen.getByText("2026-01-01 没有到期的节点。")).toBeInTheDocument();
  });

  it("点日期列出当日到期的节点；没有周期的节点没有「已续费」", async () => {
    render({ listNodes: async () => ({ nodes: fleet }) });
    fireEvent.click(await screen.findByRole("button", { name: "2026-01-10，2 台到期" }));
    const list = screen.getByRole("region", { name: "2026-01-10 到期的节点" });
    const rows = within(list).getAllByRole("row").slice(1);
    expect(rows.map((r) => within(r).getByRole("link").textContent)).toEqual(["osaka", "seoul"]);
    expect(within(rows[0]).getByRole("button", { name: "已续费 osaka（#2）" })).toBeInTheDocument();
    expect(within(rows[1]).queryByRole("button", { name: /^已续费/ })).toBeNull();
    expect(within(rows[1]).getByRole("button", { name: "编辑计费 seoul（#3）" })).toBeInTheDocument();
  });

  it("同一天计入已出租节点，格子颜色只取第一个未出租节点", async () => {
    // 第一个已出租、剩 3 天（若取它上色会是需关注）；第二个未出租、剩 40 天。两者同一到期日，台数含全部，颜色为中性。
    const leased = { ...node(1n, "leased", "2026-01-04", 3), rented: true };
    const kept = node(2n, "kept", "2026-01-04", 40);
    render({ listNodes: async () => ({ nodes: [leased, kept] }) });
    const cell = await screen.findByRole("button", { name: "2026-01-04，2 台到期" });
    expect(cell.querySelector(".renewal-count")).toHaveAttribute("data-level", "neutral");
    expect(cell.querySelector(".renewal-count")).toHaveTextContent("2 台");
  });

  it("已过期的节点留在它的到期日，翻到上个月可见并带严重级别", async () => {
    render({ listNodes: async () => ({ nodes: fleet }) });
    fireEvent.click(await screen.findByRole("button", { name: "上个月" }));
    const cell = day("2025-12-20");
    expect(cell).toHaveAccessibleName("2025-12-20，1 台到期");
    expect(cell.querySelector(".renewal-count")).toHaveAttribute("data-level", "critical");
  });

  it("「已续费」两段式确认后调用 RenewNodeBilling，推后的日期取 hub 的回答，不经 UpdateNode", async () => {
    let expires = "2026-01-02";
    const renewNodeBilling = vi.fn(async ({ nodeId }: { nodeId: bigint }) => {
      expires = "2026-02-02";
      return { node: { ...node(nodeId, "tokyo", expires, 32) } };
    });
    const updateNode = vi.fn(async () => ({}));
    render({ listNodes: async () => ({ nodes: [node(1n, "tokyo", expires, expires === "2026-01-02" ? 1 : 32), fleet[1]] }), renewNodeBilling, updateNode });
    fireEvent.click(await screen.findByRole("button", { name: "2026-01-02，1 台到期" }));
    fireEvent.click(screen.getByRole("button", { name: "已续费 tokyo（#1）" }));
    expect(renewNodeBilling).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "确认已续费" }));
    await waitFor(() => expect(renewNodeBilling).toHaveBeenCalledTimes(1));
    expect(renewNodeBilling.mock.calls[0][0]).toMatchObject({ nodeId: 1n });
    expect(updateNode).not.toHaveBeenCalled();
    expect(await screen.findByText("tokyo（#1） 已续费，到期日推后到 2026-02-02")).toHaveAttribute("role", "status");
    await waitFor(() => expect(screen.getByText("2026-01-02 没有到期的节点。")).toBeInTheDocument());
  });

  it("「编辑计费」打开节点编辑抽屉", async () => {
    render({ listNodes: async () => ({ nodes: fleet }) });
    fireEvent.click(await screen.findByRole("button", { name: "2026-01-02，1 台到期" }));
    fireEvent.click(screen.getByRole("button", { name: "编辑计费 tokyo（#1）" }));
    expect(await screen.findByRole("dialog", { name: "编辑节点 · tokyo（#1）" })).toBeInTheDocument();
  });

  // 续费页与节点页共用同一个编辑草稿：保存走 UpdateNode 整体替换，缺了 rented 会把已出租清掉。
  it("编辑计费保存的请求带上 rented", async () => {
    const updateNode = vi.fn(async () => ({}));
    const leased = { ...node(1n, "tokyo", "2026-01-02", 1), rented: true };
    render({ listNodes: async () => ({ nodes: [leased] }), updateNode });
    fireEvent.click(await screen.findByRole("button", { name: "2026-01-02，1 台到期" }));
    fireEvent.click(screen.getByRole("button", { name: "编辑计费 tokyo（#1）" }));
    fireEvent.click(await screen.findByRole("button", { name: "保存" }));
    await waitFor(() => expect(updateNode).toHaveBeenCalledWith(expect.objectContaining({ id: 1n, rented: true }), expect.anything()));
  });

  it("方向键在格子间移动焦点，PageDown 换月", async () => {
    render({ listNodes: async () => ({ nodes: fleet }) });
    const start = await screen.findByRole("button", { name: "2026-01-01，无到期" });
    expect(start).toHaveAttribute("tabindex", "0");
    fireEvent.keyDown(start, { key: "ArrowRight" });
    await waitFor(() => expect(day("2026-01-02")).toHaveFocus());
    fireEvent.keyDown(day("2026-01-02"), { key: "PageDown" });
    await waitFor(() => expect(day("2026-02-02")).toHaveFocus());
    expect(screen.getByText("2026 年 2 月")).toBeInTheDocument();
  });

  it("没有任何节点带到期日时给出空态，不按浏览器时钟画一个月", async () => {
    render({ listNodes: async () => ({ nodes: [node(1n, "bare", "", undefined), node(2n, "broken", "2026-02-30", undefined)] }) });
    expect(await screen.findByText("还没有设置到期日的节点。")).toBeInTheDocument();
    expect(screen.queryByRole("table")).toBeNull();
  });
});
