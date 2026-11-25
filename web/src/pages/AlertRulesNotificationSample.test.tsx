import { expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { create } from "@bufbuild/protobuf";
import { ConnectError, Code } from "@connectrpc/connect";
import { AlertKind, ChannelKind, DeliveryFailure, ListAlertRulesResponseSchema, ListNodesResponseSchema, ListNotifyChannelsResponseSchema, type PreviewAlertNotificationRequest, type SaveAlertRuleRequest, type TestAlertNotificationRequest } from "../gen/heron/v1/admin_pb";
import { renderWithAdmin, type AdminImpl } from "../test/harness";
import { AlertRules } from "./AlertRules";

const nodes = create(ListNodesResponseSchema, { nodes: [{ id: 1n, name: "东京" }, { id: 2n, name: "法兰克福" }] });
const channels = create(ListNotifyChannelsResponseSchema, { channels: [
  { id: 5n, name: "hook", kind: ChannelKind.WEBHOOK },
  { id: 6n, name: "群聊", kind: ChannelKind.TELEGRAM },
] });
const rules = create(ListAlertRulesResponseSchema, { rules: [{ id: 7n, name: "离线", kind: AlertKind.OFFLINE, enabled: true, allNodes: true, channelIds: [5n, 6n] }] });
const base: AdminImpl = { listNodes: async () => nodes, listNotifyChannels: async () => channels, listProbeTasks: async () => ({}), listAlertRules: async () => rules, listSilences: async () => ({ now: 1700000037n }) };
const render = (impl: AdminImpl) => renderWithAdmin({ ...base, ...impl }, [{ path: "/alerts", Component: AlertRules }], "/alerts");

async function openEdit() {
  fireEvent.click(await screen.findByRole("button", { name: "更多操作 离线（#7）" }));
  fireEvent.click(screen.getByRole("menuitem", { name: "编辑 离线（#7）" }));
  return within(screen.getByRole("form", { name: "编辑 离线（#7）" }));
}

it("按未保存的草稿、所选样例节点与阶段预览，逐渠道显示请求或错误，不保存规则", async () => {
  const previewed: PreviewAlertNotificationRequest[] = [];
  const saved: SaveAlertRuleRequest[] = [];
  render({
    saveAlertRule: async (r) => { saved.push(r); return {}; },
    previewAlertNotification: async (r) => {
      previewed.push(r);
      return { previews: [
        { channelId: 5n, method: "PUT", urlHost: "https://hook.example", body: "{\"s\":\"节点 法兰克福 已恢复上报\"}" },
        { channelId: 6n, error: "template: body:1: can't evaluate field Missing" },
      ] };
    },
  });
  const form = await openEdit();
  fireEvent.change(form.getByLabelText("名称"), { target: { value: "离线（未保存）" } });
  fireEvent.change(form.getByLabelText("样例节点"), { target: { value: "2" } });
  fireEvent.change(form.getByLabelText("阶段"), { target: { value: "recovered" } });
  fireEvent.click(form.getByRole("button", { name: "预览" }));
  const list = await form.findByRole("list", { name: "预览结果" });
  expect(previewed).toHaveLength(1);
  expect(previewed[0]).toMatchObject({ nodeId: 2n, transition: "recovered", rule: { id: 7n, name: "离线（未保存）", kind: AlertKind.OFFLINE, channelIds: [5n, 6n] } });
  const [hook, chat] = within(list).getAllByRole("listitem");
  expect(hook).toHaveTextContent("hook");
  expect(hook).toHaveTextContent("PUT https://hook.example");
  expect(within(hook).getByText("{\"s\":\"节点 法兰克福 已恢复上报\"}").tagName).toBe("PRE");
  expect(chat).toHaveTextContent("群聊");
  expect(within(chat).getByText(/can't evaluate field Missing/)).toHaveClass("error");
  expect(saved).toHaveLength(0);
});

it("发送测试默认取第一个节点与触发阶段，逐渠道显示送达或失败原因", async () => {
  const sent: TestAlertNotificationRequest[] = [];
  render({
    testAlertNotification: async (r) => {
      sent.push(r);
      return { results: [
        { channelId: 5n, ok: true },
        { channelId: 6n, failure: DeliveryFailure.HTTP_STATUS, httpStatus: 401, error: "HTTP 401 Unauthorized: bad token" },
      ] };
    },
  });
  const form = await openEdit();
  fireEvent.click(form.getByRole("button", { name: "发送测试" }));
  const list = await form.findByRole("list", { name: "发送测试结果" });
  expect(sent[0]).toMatchObject({ nodeId: 1n, transition: "firing", rule: { id: 7n } });
  const [hook, chat] = within(list).getAllByRole("listitem");
  expect(hook).toHaveTextContent("hook：已送达");
  expect(chat).toHaveTextContent("群聊：HTTP 401 Unauthorized: bad token");
});

it("hub 拒绝草稿时在抽屉里显示错误原文", async () => {
  render({ previewAlertNotification: async () => { throw new ConnectError("rule.name must contain between 1 and 64 characters", Code.InvalidArgument); } });
  const form = await openEdit();
  fireEvent.click(form.getByRole("button", { name: "预览" }));
  expect(await form.findByRole("alert")).toHaveTextContent("rule.name must contain between 1 and 64 characters");
});

it("没有选通知渠道时预览与发送测试不可用并说明原因", async () => {
  render({});
  fireEvent.click(await screen.findByRole("button", { name: "新建告警规则" }));
  const form = within(screen.getByRole("form", { name: "新建告警规则" }));
  expect(form.getByRole("button", { name: "预览" })).toBeDisabled();
  expect(form.getByRole("button", { name: "发送测试" })).toBeDisabled();
  expect(form.getByText("选择通知渠道后可预览与发送测试。")).toBeInTheDocument();
  fireEvent.click(form.getByRole("checkbox", { name: /hook/ }));
  await waitFor(() => expect(form.getByRole("button", { name: "预览" })).toBeEnabled());
});
