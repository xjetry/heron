import { expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { create } from "@bufbuild/protobuf";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { ConnectError, Code } from "@connectrpc/connect";
import { AdminService, AlertKind, BaselineMode, ChannelKind, ListAlertRulesResponseSchema, ListNodesResponseSchema, ListNotifyChannelsResponseSchema, ListProbeTasksResponseSchema, ProbeMetric, RttMode, type SaveAlertRuleRequest } from "../gen/heron/v1/admin_pb";
import { ProbeKind } from "../gen/heron/v1/types_pb";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { AlertRules } from "./AlertRules";
import { expectEmptyState } from "../test/empty";

const nodes = create(ListNodesResponseSchema, { nodes: [{ id: 1n, name: "东京" }, { id: 2n, name: "法兰克福" }] });
const channels = create(ListNotifyChannelsResponseSchema, { channels: [{ id: 5n, name: "hook", kind: ChannelKind.WEBHOOK }] });
const tasks = create(ListProbeTasksResponseSchema, { tasks: [{ task: { id: 3n, kind: ProbeKind.TCP, target: "1.1.1.1:443" }, nodeIds: [1n, 2n] }] });
const rules = create(ListAlertRulesResponseSchema, {
  rules: [
    { id: 7n, name: "离线", kind: AlertKind.OFFLINE, enabled: true, allNodes: true, channelIds: [5n] },
    { id: 8n, name: "丢包", kind: AlertKind.PROBE, enabled: true, nodeIds: [1n, 9n], taskId: 3n, metric: ProbeMetric.LOSS_PCT, threshold: 50, forMinutes: 3 },
    { id: 9n, name: "停用", kind: AlertKind.OFFLINE, enabled: false, allNodes: true },
  ],
  states: [
    { ruleId: 7n, nodeId: 1n, state: "firing" },
    { ruleId: 7n, nodeId: 2n, state: "pending" },
  ],
});
const routes = [{ path: "/alerts", Component: AlertRules }];
const base: AdminImpl = { listNodes: async () => nodes, listNotifyChannels: async () => channels, listProbeTasks: async () => tasks, listAlertRules: async () => rules };
function openRowAction(label: string, action: string) {
  fireEvent.click(screen.getByRole("button", { name: `更多操作 ${label}` }));
  fireEvent.click(screen.getByRole("menuitem", { name: `${action} ${label}` }));
}
async function openCreate() {
  fireEvent.click(await screen.findByRole("button", { name: "新建告警规则" }));
  return screen.getByRole("form", { name: "新建告警规则" });
}
const render = (impl: AdminImpl) => renderWithAdmin({ ...base, ...impl }, routes, "/alerts");

it("流量规则只提交百分比阈值，编辑与摘要保留阈值", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ listAlertRules: async () => create(ListAlertRulesResponseSchema, { rules: [{ id: 21n, name: "流量提醒", kind: AlertKind.TRAFFIC, enabled: true, allNodes: true, threshold: 80 }] }), saveAlertRule: async (r) => { saved.push(r); return {}; } });
  expect(await screen.findByText("周期流量用量 ≥ 配额的 80%")).toBeInTheDocument();
  openRowAction("流量提醒（#21）", "编辑");
  const form = within(screen.getByRole("form", { name: "编辑 流量提醒（#21）" }));
  expect(form.getByLabelText(/流量阈值/)).toHaveValue(80);
  expect(form.queryByLabelText("探测任务")).toBeNull();
  expect(form.queryByLabelText(/持续/)).toBeNull();
  fireEvent.change(form.getByLabelText(/流量阈值/), { target: { value: "100" } });
  fireEvent.click(form.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].rule).toMatchObject({ kind: AlertKind.TRAFFIC, threshold: 100, taskId: 0n, forMinutes: 0, daysBefore: 0, recoveryThreshold: 0 });
});

it("轮询成功更新状态，随后刷新失败仍保留同一编辑表单与草稿", async () => {
  let calls = 0;
  vi.useFakeTimers();
  try {
    render({ listAlertRules: async () => {
      calls++;
      if (calls > 2) throw new ConnectError("rules refresh failed", Code.Unavailable);
      return calls === 1 ? rules : create(ListAlertRulesResponseSchema, {
        rules: rules.rules, states: [{ ruleId: 7n, nodeId: 2n, state: "firing" }],
      });
    } });
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    openRowAction("丢包（#8）", "编辑");
    const form = screen.getByRole("form", { name: "编辑 丢包（#8）" });
    fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "尚未保存" } });
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(calls).toBe(2);
    expect(screen.getByRole("cell", { name: "触发：法兰克福" })).toBeInTheDocument();
    expect(screen.getByRole("form", { name: "编辑 丢包（#8）" })).toBe(form);
    expect(within(form).getByLabelText("名称")).toHaveValue("尚未保存");
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(calls).toBe(3);
    expect(screen.getByRole("alert")).toHaveTextContent("rules refresh failed");
    expect(screen.getByRole("form", { name: "编辑 丢包（#8）" })).toBe(form);
    expect(within(form).getByLabelText("名称")).toHaveValue("尚未保存");
  } finally { vi.useRealTimers(); }
});

it("规则首次失败无数据时只显示错误而无表单与表格", async () => {
  render({ listAlertRules: async () => { throw new ConnectError("rules unavailable", Code.Unavailable); } });
  expect(await screen.findByRole("alert")).toHaveTextContent("rules unavailable");
  expect(screen.queryByRole("form")).toBeNull();
  expect(screen.queryByRole("table")).toBeNull();
});

it("依赖列表刷新失败显示横幅且编辑中的表单与草稿仍在", async () => {
  let fail = false;
  const { queryClient } = render({ listNodes: async () => {
    if (fail) throw new ConnectError("nodes refresh failed", Code.Unavailable);
    return nodes;
  } });
  await screen.findByRole("button", { name: "更多操作 丢包（#8）" });
  openRowAction("丢包（#8）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 丢包（#8）" });
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "尚未保存" } });
  fail = true;
  await act(async () => { await queryClient.refetchQueries(); });
  expect(await screen.findByRole("alert")).toHaveTextContent("nodes refresh failed");
  expect(within(screen.getByRole("form", { name: "编辑 丢包（#8）" })).getByLabelText("名称")).toHaveValue("尚未保存");
});

it("多个依赖同时刷新失败时错误全部可见，恢复其一即只移除其错误", async () => {
  const failing = new Set<string>();
  const { queryClient } = render({
    listNodes: async () => {
      if (failing.has("listNodes")) throw new ConnectError("nodes refresh failed", Code.Unavailable);
      return nodes;
    },
    listNotifyChannels: async () => {
      if (failing.has("listNotifyChannels")) throw new ConnectError("channels refresh failed", Code.Unavailable);
      return channels;
    },
    listProbeTasks: async () => {
      if (failing.has("listProbeTasks")) throw new ConnectError("tasks refresh failed", Code.Unavailable);
      return tasks;
    },
  });
  await screen.findByRole("button", { name: "更多操作 丢包（#8）" });
  openRowAction("丢包（#8）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 丢包（#8）" });
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "尚未保存" } });
  failing.add("listNodes");
  failing.add("listNotifyChannels");
  failing.add("listProbeTasks");
  await act(async () => { await queryClient.refetchQueries(); });
  expect((await screen.findAllByRole("alert")).map((a) => a.textContent)).toEqual(["nodes refresh failed", "channels refresh failed", "tasks refresh failed"]);
  expect(within(screen.getByRole("form", { name: "编辑 丢包（#8）" })).getByLabelText("名称")).toHaveValue("尚未保存");
  failing.delete("listNodes");
  await act(async () => { await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listNodes, cardinality: "finite" }) }); });
  await waitFor(() => expect(screen.getAllByRole("alert").map((a) => a.textContent)).toEqual(["channels refresh failed", "tasks refresh failed"]));
});

it("显式空作用域不是全部节点，编辑保存仍由 hub 拒绝而不放宽", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  const message = "rule.node_ids must not be empty unless all_nodes is true";
  render({
    listAlertRules: async () => create(ListAlertRulesResponseSchema, { rules: [
      { id: 1n, name: "显式空", kind: AlertKind.OFFLINE, enabled: true, allNodes: false },
      { id: 2n, name: "全部", kind: AlertKind.OFFLINE, enabled: true, allNodes: true },
    ] }),
    saveAlertRule: async (req) => { saved.push(req); throw new ConnectError(message, Code.InvalidArgument); },
  });
  const empty = within((await screen.findByRole("cell", { name: "显式空" })).closest("tr")!);
  const all = within(screen.getByRole("cell", { name: "全部" }).closest("tr")!);
  expect(empty.getByRole("cell", { name: "无节点" })).toBeInTheDocument();
  expect(empty.queryByRole("cell", { name: "全部节点" })).toBeNull();
  expect(all.getByRole("cell", { name: "全部节点" })).toBeInTheDocument();
  openRowAction("显式空（#1）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 显式空（#1）" });
  expect(within(form).getByRole("radio", { name: "全部节点" })).not.toBeChecked();
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  expect(await screen.findByRole("alert")).toHaveTextContent(message);
  expect(saved).toHaveLength(1);
  expect({ allNodes: saved[0].rule!.allNodes, nodeIds: saved[0].rule!.nodeIds }).toEqual({ allNodes: false, nodeIds: [] });
  expect(screen.getByRole("form", { name: "编辑 显式空（#1）" })).toBe(form);
});

const rttRules = create(ListAlertRulesResponseSchema, {
  rules: [{ id: 10n, name: "延迟", kind: AlertKind.PROBE, enabled: true, allNodes: false, nodeIds: [1n],
    channelIds: [5n, 9n], taskId: 7n, metric: ProbeMetric.RTT_MS, rttMode: RttMode.THRESHOLD, threshold: 75, forMinutes: 4 }],
  states: [{ ruleId: 10n, nodeId: 1n, state: "firing" }],
});
const rttTasks = create(ListProbeTasksResponseSchema, { tasks: [{ task: { id: 7n, kind: ProbeKind.ICMP, target: "127.0.0.1" }, nodeIds: [1n] }] });

it("RTT 规则只改名称保留完整载荷，已有渠道保留且删除的渠道掉出", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ listAlertRules: async () => rttRules, listProbeTasks: async () => rttTasks,
    saveAlertRule: async (req) => { saved.push(req); return {}; },
  });
  await screen.findByRole("button", { name: "更多操作 延迟（#10）" });
  openRowAction("延迟（#10）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 延迟（#10）" });
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "延迟新名" } });
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].rule).toEqual({ ...rttRules.rules[0], name: "延迟新名", channelIds: [5n] });
});

it.each([true, false])("从 enabled=%s 编辑开关，保存与刷新后状态一致", async (enabled) => {
  let current = create(ListAlertRulesResponseSchema, { rules: [{ ...rules.rules[0], enabled }], states: enabled ? rules.states : [] });
  const saved: SaveAlertRuleRequest[] = [];
  render({ listAlertRules: async () => current, saveAlertRule: async (req) => {
    saved.push(req);
    current = create(ListAlertRulesResponseSchema, { rules: [req.rule!] });
    return {};
  } });
  await screen.findByRole("button", { name: "更多操作 离线（#7）" });
  openRowAction("离线（#7）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 离线（#7）" });
  const input = within(form).getByLabelText("启用");
  expect((input as HTMLInputElement).checked).toBe(enabled);
  fireEvent.click(input);
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].rule!.enabled).toBe(!enabled);
  await screen.findByRole("button", { name: "更多操作 离线（#7）" });
  const row = within(screen.getByRole("button", { name: "更多操作 离线（#7）" }).closest("tr")!);
  expect(row.getByRole("cell", { name: enabled ? "—" : "正常" })).toBeInTheDocument();
  expect(row.queryByText(/触发：/)).toBeNull();
});

it.each(["指标", "离线", "探测"])("主动切换%s发送新身份，刷新后不沿用旧状态", async (change) => {
  const original = change === "探测" ? rules.rules[0] : rttRules.rules[0];
  let current = create(ListAlertRulesResponseSchema, { rules: [original], states: [{ ruleId: original.id, nodeId: 1n, state: "firing" }] });
  const saved: SaveAlertRuleRequest[] = [];
  render({ listAlertRules: async () => current, listProbeTasks: async () => rttTasks, saveAlertRule: async (req) => {
    saved.push(req);
    current = create(ListAlertRulesResponseSchema, { rules: [req.rule!] });
    return {};
  } });
  await screen.findByRole("button", { name: `更多操作 ${original.name}（#${original.id}）` });
  openRowAction(`${original.name}（#${original.id}）`, "编辑");
  const form = screen.getByRole("form", { name: `编辑 ${original.name}（#${original.id}）` });
  if (change === "指标") {
    fireEvent.change(within(form).getByLabelText("指标"), { target: { value: ProbeMetric.LOSS_PCT } });
  } else {
    fireEvent.change(within(form).getByLabelText("类型"), { target: { value: change === "离线" ? AlertKind.OFFLINE : AlertKind.PROBE } });
    if (change === "探测") {
      fireEvent.change(within(form).getByLabelText("探测任务"), { target: { value: "7" } });
      fireEvent.change(within(form).getByLabelText("阈值（%）"), { target: { value: "25" } });
    }
  }
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  // rtt 规则换成别的指标或种类后 rtt_mode 回到未指定：hub 拒绝非 rtt 规则带判定方式。
  const expected = change === "离线"
    ? { kind: AlertKind.OFFLINE, taskId: 0n, metric: ProbeMetric.UNSPECIFIED, rttMode: RttMode.UNSPECIFIED, threshold: 0, forMinutes: 0 }
    : { kind: AlertKind.PROBE, taskId: 7n, metric: ProbeMetric.LOSS_PCT, rttMode: RttMode.UNSPECIFIED, threshold: change === "探测" ? 25 : 75, forMinutes: change === "探测" ? 3 : 4 };
  expect(saved[0].rule).toEqual({ ...original, channelIds: [5n], ...expected });
  await screen.findByRole("button", { name: `更多操作 ${original.name}（#${original.id}）` });
  expect(screen.getByRole("cell", { name: "正常" })).toBeInTheDocument();
  expect(screen.queryByText(/触发：/)).toBeNull();
});
const withRule8Name = (name: string) => create(ListAlertRulesResponseSchema, {
  rules: [
    { id: 7n, name: "离线", kind: AlertKind.OFFLINE, enabled: true, allNodes: true, channelIds: [5n] },
    { id: 8n, name, kind: AlertKind.PROBE, enabled: true, nodeIds: [1n, 9n], taskId: 3n, metric: ProbeMetric.LOSS_PCT, threshold: 50, forMinutes: 3 },
    { id: 9n, name: "停用", kind: AlertKind.OFFLINE, enabled: false, allNodes: true },
  ],
  states: [
    { ruleId: 7n, nodeId: 1n, state: "firing" },
    { ruleId: 7n, nodeId: 2n, state: "pending" },
  ],
});

it("列表展示名称、条件、作用域、通知与当前状态", async () => {
  render({});
  const rowOf = (cell: HTMLElement) => within(cell.closest("tr")!);
  const offline = rowOf(await screen.findByRole("cell", { name: "hook" }));
  expect(offline.getByRole("cell", { name: "全部节点" })).toBeInTheDocument();
  expect(offline.getByRole("cell", { name: "超过宽限期未上报" })).toBeInTheDocument();
  const state = offline.getByRole("cell", { name: /触发：东京/ });
  expect(state).toHaveTextContent("触发：东京");
  expect(state).toHaveTextContent("待定：法兰克福");
  const probe = rowOf(screen.getByRole("cell", { name: "TCP 1.1.1.1:443 丢包率 ≥ 50%，连续 3 分钟" }));
  expect(probe.getByText("2 个指定节点")).toHaveAttribute("title", "东京、节点 #9");
  expect(probe.getByRole("cell", { name: "只记事件" })).toBeInTheDocument();
  expect(probe.getByRole("cell", { name: "正常" })).toBeInTheDocument();
  expect(screen.getByRole("switch", { name: "启用 停用（#9）" })).not.toBeChecked();
  expect(within(screen.getByRole("row", { name: "停用" })).getByRole("cell", { name: "—" })).toBeInTheDocument();
});

// 离线规则的探测字段与提前天数、到期规则的探测字段都必须是零值：hub 拒绝带着别的种类字段的规则（alert.CheckRule）。
// 新建草稿里连续分钟默认 3、提前天数默认 7，所以这里这两项的断言不是空转；任务与阈值在新建草稿里是空串，
// 由"主动切换离线"那例从探测规则出发钉住。
it("新建离线规则覆盖全部节点时不带节点列表，别的种类的字段都是零值", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ saveAlertRule: async (req) => { saved.push(req); return {}; } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "全网离线" } });
  fireEvent.click(within(form).getByLabelText("hook（#5）"));
  fireEvent.click(within(form).getByRole("radio", { name: "指定节点" }));
  fireEvent.click(within(form).getByLabelText("东京（#1）"));
  fireEvent.click(within(form).getByRole("radio", { name: "全部节点" }));
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  const r = saved[0].rule!;
  expect({ id: r.id, name: r.name, kind: r.kind, enabled: r.enabled, allNodes: r.allNodes, nodeIds: r.nodeIds, channelIds: r.channelIds,
    taskId: r.taskId, metric: r.metric, threshold: r.threshold, forMinutes: r.forMinutes, daysBefore: r.daysBefore }).toEqual(
    { id: 0n, name: "全网离线", kind: AlertKind.OFFLINE, enabled: true, allNodes: true, nodeIds: [], channelIds: [5n],
      taskId: 0n, metric: ProbeMetric.UNSPECIFIED, threshold: 0, forMinutes: 0, daysBefore: 0 });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(within(await openCreate()).getByLabelText("名称")).toHaveValue("");
});

it("显式作用域按升序发出节点列表", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ saveAlertRule: async (req) => { saved.push(req); return {}; } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "部分节点" } });
  fireEvent.click(within(form).getByRole("radio", { name: "指定节点" }));
  const scope = within(form).getByRole("group", { name: "作用域节点" });
  fireEvent.click(within(scope).getByLabelText("法兰克福（#2）"));
  fireEvent.click(within(scope).getByLabelText("东京（#1）"));
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].rule!.allNodes).toBe(false);
  expect(saved[0].rule!.nodeIds).toEqual([1n, 2n]);
});

it("同类型同目标的探测任务按 id 保存第二个", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  const duplicate = create(ListProbeTasksResponseSchema, { tasks: [
    { task: { id: 3n, kind: ProbeKind.ICMP, target: "1.1.1.1" }, nodeIds: [1n] },
    { task: { id: 11n, kind: ProbeKind.ICMP, target: "1.1.1.1" }, nodeIds: [2n] },
  ] });
  render({ listProbeTasks: async () => duplicate, saveAlertRule: async (req) => { saved.push(req); return {}; } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "同名任务" } });
  fireEvent.change(within(form).getByLabelText("类型"), { target: { value: String(AlertKind.PROBE) } });
  const select = within(form).getByLabelText("探测任务");
  // 带 id 时只命中第二项；名称不含 id 时两项同名，getBy 必须报多个。
  const option = within(select).getByRole("option", { name: (n) => n === "ICMP 1.1.1.1 #11" || n === "ICMP 1.1.1.1" });
  fireEvent.change(select, { target: { value: (option as HTMLOptionElement).value } });
  fireEvent.change(within(form).getByLabelText("阈值（%）"), { target: { value: "10" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].rule!.taskId).toBe(11n);
});

it("探测规则字段随指标切换单位", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ saveAlertRule: async (req) => { saved.push(req); return {}; } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "高延迟" } });
  fireEvent.change(within(form).getByLabelText("类型"), { target: { value: String(AlertKind.PROBE) } });
  fireEvent.change(within(form).getByLabelText("探测任务"), { target: { value: "3" } });
  fireEvent.change(within(form).getByLabelText("指标"), { target: { value: String(ProbeMetric.RTT_MS) } });
  fireEvent.change(within(form).getByLabelText("阈值（ms）"), { target: { value: "150" } });
  fireEvent.change(within(form).getByLabelText("连续分钟"), { target: { value: "5" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  const r = saved[0].rule!;
  expect({ kind: r.kind, taskId: r.taskId, metric: r.metric, rttMode: r.rttMode, threshold: r.threshold, forMinutes: r.forMinutes }).toEqual(
    { kind: AlertKind.PROBE, taskId: 3n, metric: ProbeMetric.RTT_MS, rttMode: RttMode.THRESHOLD, threshold: 150, forMinutes: 5 });
});

async function openRttCreate() {
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "相对延迟" } });
  fireEvent.change(within(form).getByLabelText("类型"), { target: { value: String(AlertKind.PROBE) } });
  fireEvent.change(within(form).getByLabelText("探测任务"), { target: { value: "3" } });
  fireEvent.change(within(form).getByLabelText("指标"), { target: { value: String(ProbeMetric.RTT_MS) } });
  fireEvent.change(within(form).getByLabelText("判定方式"), { target: { value: String(RttMode.RELATIVE) } });
  return form;
}

it("相对基线（自适应）只发自适应字段，分钟换算成秒，阈值为 0", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ saveAlertRule: async (req) => { saved.push(req); return {}; } });
  const form = await openRttCreate();
  expect(within(form).queryByLabelText("阈值（ms）")).toBeNull();
  expect(within(form).queryByLabelText("固定基线（ms）")).toBeNull();
  fireEvent.change(within(form).getByLabelText("基线窗口（分钟）"), { target: { value: "120" } });
  fireEvent.change(within(form).getByLabelText("最少桶数"), { target: { value: "6" } });
  fireEvent.change(within(form).getByLabelText("上偏差（%）"), { target: { value: "150" } });
  fireEvent.change(within(form).getByLabelText("下偏差（%）"), { target: { value: "40" } });
  fireEvent.change(within(form).getByLabelText("冷却（分钟）"), { target: { value: "15" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  const r = saved[0].rule!;
  expect({ rttMode: r.rttMode, baselineMode: r.baselineMode, threshold: r.threshold, baselineWindowS: r.baselineWindowS, baselineMinSamples: r.baselineMinSamples,
    upperDeviationPct: r.upperDeviationPct, lowerDeviationPct: r.lowerDeviationPct, cooldownS: r.cooldownS, fixedBaselineMs: r.fixedBaselineMs, forMinutes: r.forMinutes }).toEqual(
    { rttMode: RttMode.RELATIVE, baselineMode: BaselineMode.ADAPTIVE, threshold: 0, baselineWindowS: 7200, baselineMinSamples: 6,
      upperDeviationPct: 150, lowerDeviationPct: 40, cooldownS: 900, fixedBaselineMs: 0, forMinutes: 3 });
});

it("相对基线（固定）只发固定基线，窗口与桶数为 0", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ saveAlertRule: async (req) => { saved.push(req); return {}; } });
  const form = await openRttCreate();
  fireEvent.change(within(form).getByLabelText("基线来源"), { target: { value: String(BaselineMode.FIXED) } });
  expect(within(form).queryByLabelText("基线窗口（分钟）")).toBeNull();
  fireEvent.change(within(form).getByLabelText("固定基线（ms）"), { target: { value: "42.5" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  const r = saved[0].rule!;
  expect({ baselineMode: r.baselineMode, fixedBaselineMs: r.fixedBaselineMs, baselineWindowS: r.baselineWindowS, baselineMinSamples: r.baselineMinSamples, threshold: r.threshold, cooldownS: r.cooldownS }).toEqual(
    { baselineMode: BaselineMode.FIXED, fixedBaselineMs: 42.5, baselineWindowS: 0, baselineMinSamples: 0, threshold: 0, cooldownS: 1800 });
});

it("基线窗口短于连续分钟时表单不提交", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ saveAlertRule: async (req) => { saved.push(req); return {}; } });
  const form = await openRttCreate();
  fireEvent.change(within(form).getByLabelText("连续分钟"), { target: { value: "10" } });
  fireEvent.change(within(form).getByLabelText("基线窗口（分钟）"), { target: { value: "9" } });
  fireEvent.submit(form);
  fireEvent.change(within(form).getByLabelText("下偏差（%）"), { target: { value: "100" } });
  fireEvent.change(within(form).getByLabelText("基线窗口（分钟）"), { target: { value: "10" } });
  fireEvent.change(within(form).getByLabelText("冷却（分钟）"), { target: { value: "0.5" } });
  fireEvent.submit(form);
  await act(async () => {});
  expect(saved).toHaveLength(0);
});

const relativeRules = create(ListAlertRulesResponseSchema, {
  rules: [{ id: 30n, name: "相对", kind: AlertKind.PROBE, enabled: true, allNodes: true, taskId: 3n, metric: ProbeMetric.RTT_MS, forMinutes: 5,
    rttMode: RttMode.RELATIVE, baselineMode: BaselineMode.ADAPTIVE, baselineWindowS: 86400, baselineMinSamples: 12, upperDeviationPct: 100, lowerDeviationPct: 50, cooldownS: 1800 }],
});

it("相对基线规则的条件摘要写出基线来源、偏差与冷却，只改名称保留完整载荷", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ listAlertRules: async () => relativeRules, saveAlertRule: async (req) => { saved.push(req); return {}; } });
  expect(await screen.findByText("TCP 1.1.1.1:443 RTT 均值越出基线 +100% / −50%（近 24 小时的 5 分钟桶均值中位数（至少 12 个桶）），连续 5 分钟，冷却 30 分钟")).toBeInTheDocument();
  openRowAction("相对（#30）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 相对（#30）" });
  expect(within(form).getByLabelText("基线窗口（分钟）")).toHaveValue(1440);
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "相对新名" } });
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].rule).toEqual({ ...relativeRules.rules[0], name: "相对新名" });
});

it("编辑回填并去掉已删除节点", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ saveAlertRule: async (req) => { saved.push(req); return {}; } });
  await screen.findByRole("button", { name: "更多操作 丢包（#8）" });
  openRowAction("丢包（#8）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 丢包（#8）" });
  expect(within(form).getByLabelText("探测任务")).toHaveValue("3");
  expect(within(form).getByLabelText("阈值（%）")).toHaveValue(50);
  expect(within(form).getByRole("radio", { name: "全部节点" })).not.toBeChecked();
  expect(within(form).getByLabelText("东京（#1）")).toBeChecked();
  expect(within(form).getByLabelText("法兰克福（#2）")).not.toBeChecked();
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].rule!.id).toBe(8n);
  expect(saved[0].rule!.nodeIds).toEqual([1n]);
});

it("空显式作用域被 hub 拒绝时显示原文", async () => {
  render({ saveAlertRule: async () => { throw new ConnectError("rule.node_ids must not be empty unless all_nodes is true", Code.InvalidArgument); } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "空作用域" } });
  fireEvent.click(within(form).getByRole("radio", { name: "指定节点" }));
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("rule.node_ids must not be empty unless all_nodes is true");
});

it("删除两段式确认且事件记录保留", async () => {
  const removed: bigint[] = [];
  render({ deleteAlertRule: async (req) => { removed.push(req.id); return {}; } });
  await screen.findByRole("button", { name: "更多操作 离线（#7）" });
  openRowAction("离线（#7）", "删除");
  expect(removed).toEqual([]);
  expect(screen.getByRole("menuitem", { name: "确认删除 离线（#7）" })).toBeInTheDocument();
  expect(screen.getByText("事件记录保留")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("menuitem", { name: "确认删除 离线（#7）" }));
  await waitFor(() => expect(removed).toEqual([7n]));
});

it("没有渠道时提示只记事件", async () => {
  render({ listNotifyChannels: async () => create(ListNotifyChannelsResponseSchema, {}) });
  const form = await openCreate();
  expect(within(form).getByText("还没有通知渠道；规则只记录事件，不发送通知。")).toBeInTheDocument();
  expect(within(form).queryByRole("group", { name: "通知渠道" })).toBeNull();
});

it("依赖列表未到达时不渲染表单与规则表格", async () => {
  const { queryClient } = render({ listProbeTasks: () => new Promise(() => {}) });
  const key = createConnectQueryKey({ schema: AdminService.method.listAlertRules, cardinality: "finite" });
  await waitFor(() => expect(queryClient.getQueriesData({ queryKey: key })[0]?.[1]).toEqual(rules));
  expect({
    loading: screen.queryByText("加载中…") !== null,
    forms: screen.queryAllByRole("form").length,
    rows: screen.queryAllByRole("row").length,
  }).toEqual({ loading: true, forms: 0, rows: 0 });
});

it("删除首击不发请求", async () => {
  const removed: bigint[] = [];
  render({ deleteAlertRule: async (req) => { removed.push(req.id); throw new ConnectError("ref", Code.FailedPrecondition); } });
  await screen.findByRole("button", { name: "更多操作 离线（#7）" });
  openRowAction("离线（#7）", "删除");
  fireEvent.click(screen.getByRole("menuitem", { name: "取消" }));
  openRowAction("丢包（#8）", "删除");
  fireEvent.click(screen.getByRole("menuitem", { name: "确认删除 丢包（#8）" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("ref");
  expect(removed).toEqual([8n]);
});

it("编辑往返撤销已武装的删除确认", async () => {
  render({});
  await screen.findByRole("button", { name: "更多操作 离线（#7）" });
  openRowAction("离线（#7）", "删除");
  expect(screen.getByRole("menuitem", { name: "确认删除 离线（#7）" })).toBeInTheDocument();
  fireEvent.click(screen.getByRole("menuitem", { name: "编辑 离线（#7）" }));
  fireEvent.click(screen.getByRole("button", { name: "取消" }));
  fireEvent.click(screen.getByRole("button", { name: "更多操作 离线（#7）" }));
  expect(screen.getByRole("menuitem", { name: "删除 离线（#7）" })).toBeInTheDocument();
  expect(screen.queryByRole("menuitem", { name: "确认删除 离线（#7）" })).toBeNull();
});

it("保存挂起时抽屉不可关闭、取消与行上开关禁用", async () => {
  let releaseSave!: () => void;
  const saveGate = new Promise<void>((r) => { releaseSave = r; });
  render({ saveAlertRule: async () => { await saveGate; return {}; } });
  await screen.findByRole("button", { name: "更多操作 丢包（#8）" });
  openRowAction("丢包（#8）", "编辑");
  fireEvent.click(screen.getByRole("button", { name: "保存" }));
  try {
    await waitFor(() => expect(screen.getByRole("button", { name: "关闭抽屉" })).toBeDisabled());
    expect(screen.getByRole("button", { name: "取消" })).toBeDisabled();
    expect(screen.getByRole("switch", { name: "启用 离线（#7）" })).toBeDisabled();
  } finally { await act(async () => { releaseSave(); }); }
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

it("编辑态在刷新完成后才关闭", async () => {
  let releaseList!: () => void;
  const listGate = new Promise<void>((r) => { releaseList = r; });
  let listCalls = 0;
  let current = rules;
  render({ listAlertRules: async () => { listCalls++; if (listCalls > 1) await listGate; return current; },
    saveAlertRule: async () => { current = withRule8Name("丢包2"); return {}; } });
  await screen.findByRole("button", { name: "更多操作 丢包（#8）" });
  openRowAction("丢包（#8）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 丢包（#8）" });
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "丢包2" } });
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  try {
    await waitFor(() => expect(listCalls).toBe(2));
    vi.useFakeTimers();
    await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
    expect(form).toBeInTheDocument();
  } finally { vi.useRealTimers(); await act(async () => { releaseList(); }); }
  await waitFor(() => expect(screen.queryByRole("form", { name: "编辑 丢包（#8）" })).toBeNull());
  expect(screen.getByRole("cell", { name: "丢包2" })).toBeInTheDocument();
});

it("创建失败保留草稿", async () => {
  render({ saveAlertRule: async () => { throw new ConnectError("rule.name: must not be empty", Code.InvalidArgument); } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "全网离线" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("rule.name: must not be empty");
  expect(within(screen.getByRole("form", { name: "新建告警规则" })).getByLabelText("名称")).toHaveValue("全网离线");
});

const expiryRules = create(ListAlertRulesResponseSchema, {
  rules: [{ id: 12n, name: "续费", kind: AlertKind.EXPIRY, enabled: true, allNodes: true, channelIds: [5n], daysBefore: 14 }],
  states: [{ ruleId: 12n, nodeId: 2n, state: "firing" }],
});

it("新建到期规则只发提前天数，默认 7 天", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ saveAlertRule: async (req) => { saved.push(req); return {}; } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "续费提醒" } });
  fireEvent.change(within(form).getByLabelText("类型"), { target: { value: String(AlertKind.EXPIRY) } });
  expect(within(form).getByLabelText("提前天数")).toHaveValue(7);
  expect(within(form).queryByLabelText("探测任务")).toBeNull();
  fireEvent.change(within(form).getByLabelText("提前天数"), { target: { value: "30" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  const r = saved[0].rule!;
  expect({ kind: r.kind, daysBefore: r.daysBefore, taskId: r.taskId, metric: r.metric, threshold: r.threshold, forMinutes: r.forMinutes }).toEqual(
    { kind: AlertKind.EXPIRY, daysBefore: 30, taskId: 0n, metric: ProbeMetric.UNSPECIFIED, threshold: 0, forMinutes: 0 });
});

it("提前天数超出 1–365 时表单不提交", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ saveAlertRule: async (req) => { saved.push(req); return {}; } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "越界" } });
  fireEvent.change(within(form).getByLabelText("类型"), { target: { value: String(AlertKind.EXPIRY) } });
  for (const value of ["0", "366", ""]) {
    fireEvent.change(within(form).getByLabelText("提前天数"), { target: { value } });
    fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  }
  await act(async () => {});
  expect(saved).toHaveLength(0);
});

it("列表写出到期规则的条件与状态，编辑时带回提前天数", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ listAlertRules: async () => expiryRules, saveAlertRule: async (req) => { saved.push(req); return {}; } });
  const row = within((await screen.findByRole("cell", { name: "到期日距今不超过 14 天（含已过期）" })).closest("tr")!);
  expect(row.getByRole("cell", { name: "到期" })).toBeInTheDocument();
  expect(row.getByRole("cell", { name: "触发：法兰克福" })).toBeInTheDocument();
  openRowAction("续费（#12）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 续费（#12）" });
  expect(within(form).getByLabelText("提前天数")).toHaveValue(14);
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "续费提醒" } });
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].rule).toEqual({ ...expiryRules.rules[0], name: "续费提醒" });
});

it("到期规则改成离线时不再带提前天数", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ listAlertRules: async () => expiryRules, saveAlertRule: async (req) => { saved.push(req); return {}; } });
  await screen.findByRole("button", { name: "更多操作 续费（#12）" });
  openRowAction("续费（#12）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 续费（#12）" });
  fireEvent.change(within(form).getByLabelText("类型"), { target: { value: String(AlertKind.OFFLINE) } });
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect({ kind: saved[0].rule!.kind, daysBefore: saved[0].rule!.daysBefore }).toEqual({ kind: AlertKind.OFFLINE, daysBefore: 0 });
});

// 新建草稿的探测任务与阈值是空串，换算后本来就是零值；从带着任务与阈值的探测规则切过来，这两项的断言才不是空转。
it("探测规则改成到期时只带提前天数", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ listAlertRules: async () => rttRules, listProbeTasks: async () => rttTasks,
    saveAlertRule: async (req) => { saved.push(req); return {}; },
  });
  await screen.findByRole("button", { name: "更多操作 延迟（#10）" });
  openRowAction("延迟（#10）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 延迟（#10）" });
  fireEvent.change(within(form).getByLabelText("类型"), { target: { value: String(AlertKind.EXPIRY) } });
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].rule).toEqual({ ...rttRules.rules[0], channelIds: [5n], kind: AlertKind.EXPIRY,
    taskId: 0n, metric: ProbeMetric.UNSPECIFIED, rttMode: RttMode.UNSPECIFIED, threshold: 0, forMinutes: 0, daysBefore: 7 });
});

const httpsTasks = create(ListProbeTasksResponseSchema, { tasks: [
  { task: { id: 3n, kind: ProbeKind.TCP, target: "1.1.1.1:443" }, nodeIds: [1n] },
  { task: { id: 4n, kind: ProbeKind.HTTP, target: "http://example.com/" }, nodeIds: [1n] },
  { task: { id: 5n, kind: ProbeKind.HTTP, target: "https://example.com/" }, nodeIds: [1n] },
] });
const certRules = create(ListAlertRulesResponseSchema, {
  rules: [{ id: 13n, name: "证书", kind: AlertKind.CERT_EXPIRY, enabled: true, allNodes: true, taskId: 5n, daysBefore: 14 }],
  states: [{ ruleId: 13n, nodeId: 1n, state: "firing" }],
});

it("新建证书到期规则只列 HTTPS 任务，发任务与提前天数", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ listProbeTasks: async () => httpsTasks, saveAlertRule: async (req) => { saved.push(req); return {}; } });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "证书提醒" } });
  fireEvent.change(within(form).getByLabelText("类型"), { target: { value: String(AlertKind.CERT_EXPIRY) } });
  const select = within(form).getByLabelText("探测任务");
  expect(within(select).getAllByRole("option").map((o) => o.textContent)).toEqual(["选择任务", "HTTP https://example.com/"]);
  expect(within(form).getByLabelText("提前天数")).toHaveValue(7);
  expect(within(form).queryByLabelText("指标")).toBeNull();
  fireEvent.change(select, { target: { value: "5" } });
  fireEvent.change(within(form).getByLabelText("提前天数"), { target: { value: "30" } });
  fireEvent.click(within(form).getByRole("button", { name: "创建" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  const r = saved[0].rule!;
  expect({ kind: r.kind, taskId: r.taskId, daysBefore: r.daysBefore, metric: r.metric, threshold: r.threshold, forMinutes: r.forMinutes }).toEqual(
    { kind: AlertKind.CERT_EXPIRY, taskId: 5n, daysBefore: 30, metric: ProbeMetric.UNSPECIFIED, threshold: 0, forMinutes: 0 });
});

it("列表写出证书到期的条件与状态，编辑带回任务与提前天数", async () => {
  const saved: SaveAlertRuleRequest[] = [];
  render({ listProbeTasks: async () => httpsTasks, listAlertRules: async () => certRules,
    saveAlertRule: async (req) => { saved.push(req); return {}; } });
  const row = within((await screen.findByRole("cell", { name: "HTTP https://example.com/ 的证书到期日距今不超过 14 天（含已过期）" })).closest("tr")!);
  expect(row.getByRole("cell", { name: "证书到期" })).toBeInTheDocument();
  expect(row.getByRole("cell", { name: "触发：东京" })).toBeInTheDocument();
  openRowAction("证书（#13）", "编辑");
  const form = screen.getByRole("form", { name: "编辑 证书（#13）" });
  expect(within(form).getByLabelText("探测任务")).toHaveValue("5");
  expect(within(form).getByLabelText("提前天数")).toHaveValue(14);
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "证书提醒" } });
  fireEvent.click(within(form).getByRole("button", { name: "保存" }));
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].rule).toEqual({ ...certRules.rules[0], name: "证书提醒" });
});

it("待定节点按 hub 的 flapping 标出抖动中，其余待定不标", async () => {
  render({ listAlertRules: async () => create(ListAlertRulesResponseSchema, {
    rules: [{ id: 7n, name: "离线", kind: AlertKind.OFFLINE, enabled: true, allNodes: true }],
    states: [
      { ruleId: 7n, nodeId: 1n, state: "pending", flapping: true },
      { ruleId: 7n, nodeId: 2n, state: "pending" },
    ],
  }) });
  const state = await screen.findByRole("cell", { name: /待定：/ });
  expect(state).toHaveTextContent("待定：东京（抖动中）、法兰克福");
});

it("静默中的 firing 状态标注已静默，恢复配对不由面板推断", async () => {
  render({ listAlertRules: async () => create(ListAlertRulesResponseSchema, {
    rules: rules.rules,
    states: [
      { ruleId: 7n, nodeId: 1n, state: "firing", silenced: true },
      { ruleId: 7n, nodeId: 2n, state: "firing" },
    ],
  }) });
  const row = within((await screen.findByRole("button", { name: "更多操作 离线（#7）" })).closest("tr")!);
  expect(row.getByText(/触发：东京（已静默）、法兰克福/)).toBeInTheDocument();
});

it("?state=firing 只列触发中规则，非法值忽略，清除筛选回写 URL", async () => {
  const { router } = renderWithAdmin(base, routes, "/alerts?state=firing");
  await screen.findByRole("row", { name: "离线" });
  expect(screen.queryByRole("row", { name: "丢包" })).toBeNull();
  expect(screen.getByRole("checkbox", { name: "只看触发中" })).toBeChecked();
  fireEvent.click(screen.getByRole("button", { name: "清除筛选" }));
  expect(router.state.location.search).toBe("");
  expect(screen.getByRole("row", { name: "丢包" })).toBeInTheDocument();
  await act(async () => { await router.navigate("/alerts?state=bogus"); });
  expect(screen.getByRole("checkbox", { name: "只看触发中" })).not.toBeChecked();
  expect(screen.getByRole("row", { name: "丢包" })).toBeInTheDocument();
});

it("行上的启用开关提交整条规则，在途禁用，失败弹回，成功不打开抽屉", async () => {
  let resolve!: () => void;
  let reject = false;
  let current = rules;
  const saveAlertRule = vi.fn(async (req: SaveAlertRuleRequest) => {
    if (reject) throw new ConnectError("nope", Code.InvalidArgument);
    await new Promise<void>((r) => { resolve = r; });
    current = create(ListAlertRulesResponseSchema, { ...rules, rules: rules.rules.map((r) => r.id === req.rule!.id ? req.rule! : r) });
    return {};
  });
  render({ listAlertRules: async () => current, saveAlertRule });
  const toggle = await screen.findByRole("switch", { name: "启用 丢包（#8）" });
  expect(toggle).toBeChecked();
  fireEvent.click(toggle);
  await waitFor(() => expect(saveAlertRule).toHaveBeenCalledTimes(1));
  expect(saveAlertRule).toHaveBeenCalledWith(expect.objectContaining({
    rule: { ...rules.rules[1], enabled: false, nodeIds: [1n] },
  }), expect.anything());
  expect(toggle).toBeDisabled();
  expect(toggle).toHaveAttribute("aria-busy", "true");
  await act(async () => { resolve(); });
  await waitFor(() => expect(toggle).not.toBeDisabled());
  expect(toggle).not.toBeChecked();
  expect(screen.queryByRole("dialog")).toBeNull();
  reject = true;
  fireEvent.click(toggle);
  expect(await screen.findByRole("alert")).toHaveTextContent("nope");
  expect(toggle).not.toBeChecked();
});

it("空动态标签不发请求，选择标签后可以提交", async () => {
  const saveAlertRule = vi.fn(async () => ({}));
  render({ saveAlertRule, listTags: async () => ({ tags: [{ name: "db" }] }) });
  const form = await openCreate();
  fireEvent.change(within(form).getByLabelText("名称"), { target: { value: "动态" } });
  fireEvent.click(within(form).getByRole("radio", { name: "动态标签选择器" }));
  fireEvent.submit(form);
  await act(async () => {});
  expect(saveAlertRule).not.toHaveBeenCalled();
  fireEvent.click(await within(form).findByRole("button", { name: /匹配标签/ }));
  fireEvent.click(screen.getByRole("checkbox", { name: /db/ }));
  fireEvent.submit(form);
  await waitFor(() => expect(saveAlertRule).toHaveBeenCalledTimes(1));
});

it("没有规则时只有空态卡；只看触发中没有结果时空态播报", async () => {
  render({ listAlertRules: async () => ({ rules: [], states: [] }) });
  await expectEmptyState("还没有告警规则。", { region: "告警规则管理" });
  cleanup();
  render({ listAlertRules: async () => create(ListAlertRulesResponseSchema, { rules: rules.rules, states: [] }) });
  fireEvent.click(await screen.findByRole("checkbox", { name: "只看触发中" }));
  await expectEmptyState("没有触发中的规则。", { region: "告警规则管理", status: true });
});
