import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";
import { AlertDeliverySchema, AlertEventSchema, AlertKind, AlertKindSchema, AlertRuleSchema, AlertStateEntrySchema, ChannelKind, DeliveryFailure, DeliveryFailureSchema, ListProbeTasksResponseSchema, NotifyChannelSchema, ProbeMetric, ProbeTaskDetailSchema, ResourceMetric } from "../gen/heron/v1/admin_pb";
import { ProbeKind } from "../gen/heron/v1/types_pb";
import { ALERT_KINDS, CHANNEL_KINDS, channelTarget, deliveryText, eventValueText, ruleLabel, failureText, graceText, labelOf, rateLabel, ruleCondition, statesOf, taskLabel, taskLabels, transitionLabel } from "./alerts";
import { PROBE_KINDS } from "./probes";

describe("taskLabel", () => {
  const tasks = [create(ProbeTaskDetailSchema, { task: { id: 3n, kind: ProbeKind.TCP, target: "1.1.1.1:443", intervalS: 30, timeoutMs: 1000 }, nodeIds: [] })];
  it("有任务时用类型与目标", () => expect(taskLabel(3n, tasks)).toBe("TCP 1.1.1.1:443"));
  it("列表里没有该任务时退回编号", () => expect(taskLabel(7n, tasks)).toBe("任务 #7"));
  it("任务列表未到时也退回编号", () => expect(taskLabel(3n, undefined)).toBe("任务 #3"));
  it("类型标签与图例同一张表", () => {
    for (const { kind, label } of PROBE_KINDS) {
      expect(taskLabel(1n, [create(ProbeTaskDetailSchema, { task: { id: 1n, kind, target: "host" } })])).toBe(`${label} host`);
    }
  });
});

it.each([
  { ids: [7n, 3n, 9n], labels: ["ICMP host #7", "ICMP host #3", "任务 #9"] },
  { ids: [3n], labels: ["ICMP host"] },
])("仅在当前列表内消歧同名标签 $ids", ({ ids, labels }) => {
  const tasks = [3n, 7n].map((id) => create(ProbeTaskDetailSchema, { task: { id, kind: ProbeKind.ICMP, target: "host" } }));
  expect(taskLabels(ids, tasks)).toEqual(labels);
});

describe("labelOf", () => {
  it("表内值给标签，表外值显示原值而不抛错", () => {
    expect(labelOf(CHANNEL_KINDS, ChannelKind.WEBHOOK)).toBe("Webhook");
    expect(labelOf(CHANNEL_KINDS, 9 as ChannelKind)).toBe("未知（9）");
  });
});

describe("channelTarget", () => {
  it("Telegram 只显示会话", () => {
    const c = create(NotifyChannelSchema, { kind: ChannelKind.TELEGRAM, telegram: { chatId: "-100", hasBotToken: true } });
    expect(channelTarget(c)).toBe("会话 -100");
  });
  it("Webhook 显示方法、主机与头名，空方法按 POST", () => {
    const c = create(NotifyChannelSchema, { kind: ChannelKind.WEBHOOK, webhook: { method: "", hasUrl: true, urlHost: "https://hooks.example", headerNames: ["Authorization", "X-Tag"] } });
    expect(channelTarget(c)).toBe("POST https://hooks.example，头 Authorization、X-Tag");
  });
  it("Webhook 没有头时不带头名段", () => {
    const c = create(NotifyChannelSchema, { kind: ChannelKind.WEBHOOK, webhook: { method: "PUT", hasUrl: true, urlHost: "http://10.0.0.2:8080" } });
    expect(channelTarget(c)).toBe("PUT http://10.0.0.2:8080");
  });
});

describe("rateLabel", () => {
  it("0 是不限，正数写每分钟条数", () => {
    expect(rateLabel(create(NotifyChannelSchema, { ratePerMinute: 0 }))).toBe("不限");
    expect(rateLabel(create(NotifyChannelSchema, { ratePerMinute: 20 }))).toBe("每分钟 20 条");
  });
  it("rate_per_minute 由 channelProto 总是写入：缺席不是占位", () => {
    expect(() => rateLabel(create(NotifyChannelSchema, {}))).toThrow(/rate_per_minute/);
  });
});

const tasks = create(ListProbeTasksResponseSchema, { tasks: [{ task: { id: 3n, kind: ProbeKind.TCP, target: "1.1.1.1:443" } }] }).tasks;

describe("ruleCondition", () => {
  it("离线规则", () => {
    expect(ruleCondition(create(AlertRuleSchema, { kind: AlertKind.OFFLINE }), tasks)).toBe("超过宽限期未上报");
  });
  it("丢包规则带百分号", () => {
    const r = create(AlertRuleSchema, { kind: AlertKind.PROBE, taskId: 3n, metric: ProbeMetric.LOSS_PCT, threshold: 50, forMinutes: 3 });
    expect(ruleCondition(r, tasks)).toBe("TCP 1.1.1.1:443 丢包率 ≥ 50%，连续 3 分钟");
  });
  it("到期规则写出提前天数", () => {
    expect(ruleCondition(create(AlertRuleSchema, { kind: AlertKind.EXPIRY, daysBefore: 14 }), tasks)).toBe("到期日距今不超过 14 天（含已过期）");
  });
  it("证书到期规则写出任务与提前天数", () => {
    const r = create(AlertRuleSchema, { kind: AlertKind.CERT_EXPIRY, taskId: 3n, daysBefore: 14 });
    expect(ruleCondition(r, tasks)).toBe("TCP 1.1.1.1:443 的证书到期日距今不超过 14 天（含已过期）");
  });
  it("RTT 规则带 ms，已删除任务用编号", () => {
    const r = create(AlertRuleSchema, { kind: AlertKind.PROBE, taskId: 9n, metric: ProbeMetric.RTT_MS, threshold: 150, forMinutes: 5 });
    expect(ruleCondition(r, tasks)).toBe("任务 #9 RTT 均值 ≥ 150 ms，连续 5 分钟");
  });
});

describe("statesOf", () => {
  it("按规则分组触发与待定，ok 记录不展示", () => {
    const states = [
      create(AlertStateEntrySchema, { ruleId: 1n, nodeId: 1n, state: "firing" }),
      create(AlertStateEntrySchema, { ruleId: 1n, nodeId: 2n, state: "pending" }),
      create(AlertStateEntrySchema, { ruleId: 2n, nodeId: 1n, state: "ok" }),
    ];
    const got = statesOf(states);
    expect(got.get(1n)?.firing.map((s) => s.nodeId)).toEqual([1n]);
    expect(got.get(1n)?.pending.map((s) => s.nodeId)).toEqual([2n]);
    expect(got.has(2n)).toBe(false);
  });
});

describe("deliveryText", () => {
  it("成功、投递中、终止失败三种", () => {
    expect(deliveryText(create(AlertDeliverySchema, { ok: true, done: true, attempts: 1 }), "hook")).toBe("hook：已送达");
    expect(deliveryText(create(AlertDeliverySchema, { ok: false, done: false, attempts: 1, failure: DeliveryFailure.HTTP_STATUS, httpStatus: 503 }), "hook")).toBe("hook：投递中（已尝试 1 次）");
    expect(deliveryText(create(AlertDeliverySchema, { ok: false, done: true, attempts: 3, failure: DeliveryFailure.TRANSPORT }), "hook")).toBe("hook：失败（3 次）连接失败");
  });

  it("终态失败按类别显示文案", () => {
    const text = (failure: DeliveryFailure, httpStatus?: number) =>
      deliveryText(create(AlertDeliverySchema, { ok: false, done: true, attempts: 2, failure, httpStatus }), "hook");
    expect([
      text(DeliveryFailure.HTTP_STATUS, 401),
      text(DeliveryFailure.TRANSPORT),
      text(DeliveryFailure.REQUEST),
      text(DeliveryFailure.CHANNEL_INVALID),
      text(DeliveryFailure.CHANNEL_DELETED),
      text(DeliveryFailure.RESULT_UNRECORDED),
      text(DeliveryFailure.UNCLASSIFIED),
      text(99 as DeliveryFailure),
    ]).toEqual([
      "hook：失败（2 次）HTTP 401",
      "hook：失败（2 次）连接失败",
      "hook：失败（2 次）请求无法构造",
      "hook：失败（2 次）渠道配置无效",
      "hook：失败（2 次）渠道已删除",
      "hook：失败（2 次）结果未记录",
      "hook：失败（2 次）未分类",
      "hook：失败（2 次）类别 99",
    ]);
  });

  it("文案表覆盖协议枚举里的每个失败类别", () => {
    for (const { number, name } of DeliveryFailureSchema.values) {
      const failure: DeliveryFailure = number;
      if (failure === DeliveryFailure.UNSPECIFIED) continue;
      const got = failureText(create(AlertDeliverySchema, { failure, httpStatus: 500 }));
      expect(got, name).not.toMatch(/^类别 /);
    }
  });
});

it("类型表覆盖协议枚举里除未指定之外的每个种类", () => {
  for (const { number, name } of AlertKindSchema.values) {
    const kind: AlertKind = number;
    if (kind === AlertKind.UNSPECIFIED) continue;
    expect(labelOf(ALERT_KINDS, kind), name).not.toMatch(/^未知/);
  }
});

it("transitionLabel 认识规则事件与系统事件的每种变化，未知值原样显示", () => {
  const all = ["firing", "recovered", "login_success", "login_locked", "backup_failed", "backup_recovered", "backup_disabled", "x"];
  expect(all.map(transitionLabel)).toEqual(["触发", "恢复", "登录成功", "登录锁定", "备份失败", "备份恢复", "备份停用", "x"]);
});

it("graceText 缺失即默认", () => {
  expect([graceText(undefined), graceText(90)]).toEqual(["默认", "90 秒"]);
});

describe("eventValueText", () => {
  const ev = (value: number, transition = "firing") => create(AlertEventSchema, { ruleId: 1n, transition, value });
  const rule = (init: Parameters<typeof create<typeof AlertRuleSchema>>[1]) => create(AlertRuleSchema, { id: 1n, ...init });
  it.each([
    ["离线不足一分钟写秒", rule({ kind: AlertKind.OFFLINE }), ev(39.226), "39 s"],
    ["离线超过一分钟写时长", rule({ kind: AlertKind.OFFLINE }), ev(3725), "1h 2m"],
    ["探测丢包是百分比", rule({ kind: AlertKind.PROBE, metric: ProbeMetric.LOSS_PCT }), ev(22.5), "23%"],
    ["探测往返是毫秒", rule({ kind: AlertKind.PROBE, metric: ProbeMetric.RTT_MS }), ev(182.44), "182 ms"],
    ["到期是剩余天数", rule({ kind: AlertKind.EXPIRY }), ev(12), "剩 12 天"],
    ["已过期写过期天数", rule({ kind: AlertKind.EXPIRY }), ev(-3), "已过期 3 天"],
    ["证书到期同到期", rule({ kind: AlertKind.CERT_EXPIRY }), ev(5, "recovered"), "剩 5 天"],
    ["流量是配额百分比", rule({ kind: AlertKind.TRAFFIC }), ev(90), "90%"],
    ["资源百分比", rule({ kind: AlertKind.RESOURCE, resourceMetric: ResourceMetric.CPU_PCT }), ev(91.24), "91%"],
    ["资源每核负载两位小数", rule({ kind: AlertKind.RESOURCE, resourceMetric: ResourceMetric.LOAD1_PER_CORE }), ev(1.5), "1.50"],
    ["资源速率按 Mbps", rule({ kind: AlertKind.RESOURCE, resourceMetric: ResourceMetric.NET_RX_BPS }), ev(1250000), "10.0 Mbps"],
    ["规则未知只写数字", undefined, ev(39.226), "39.23"],
  ])("%s", (_, r, e, want) => {
    expect(eventValueText(e, r)).toBe(want);
  });
  it("没有观测值的事件与清除到期日的恢复写破折号", () => {
    expect(eventValueText(ev(0, "login_success"), undefined)).toBeNull();
    expect(eventValueText(ev(0, "recovered"), rule({ kind: AlertKind.EXPIRY }))).toBeNull();
    expect(eventValueText(ev(0, "firing"), rule({ kind: AlertKind.EXPIRY }))).toBe("剩 0 天");
  });
  it("规则标签按编号回退", () => {
    expect(ruleLabel(1n, [rule({ name: "cpu" })])).toBe("cpu（#1）");
    expect(ruleLabel(2n, undefined)).toBe("规则 #2");
  });
});
