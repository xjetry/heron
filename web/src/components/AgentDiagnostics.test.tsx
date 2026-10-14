import { create } from "@bufbuild/protobuf";
import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { AgentDiagnosticsSchema, CollectionComponent, NetStructureReason } from "../gen/heron/v1/types_pb";
import { AgentDiagnostics } from "./AgentDiagnostics";

const value = (label: string) => screen.getByText(label).nextElementSibling;

it("未提供诊断不显示为采集健康，也不借主机信息时间暗示存在诊断", () => {
  render(<AgentDiagnostics updatedAt={1_790_000_000n} />);
  expect(screen.getByRole("heading", { name: "Agent 运行诊断" })).toBeInTheDocument();
  expect(screen.getByText("Agent 未提供诊断信息，请更新 Agent 后等待上报。")).toBeInTheDocument();
  expect(screen.queryByText("最近采集未报告失败")).not.toBeInTheDocument();
  expect(screen.queryByText("诊断信息更新时间")).not.toBeInTheDocument();
});

it("只读展示生效规则、接口、间隔与保存时间，不暗示实时健康", () => {
  const updatedAt = 1_790_000_000n;
  render(<AgentDiagnostics updatedAt={updatedAt} diagnostics={create(AgentDiagnosticsSchema, {
    netInclude: ["eth*", "en*"], netInterfaces: ["en0", "eth0"], netInterfacesTotal: 2, reportIntervalMs: 1500,
  })} />);
  expect(value("包含规则")).toHaveTextContent("eth* en*");
  expect(value("排除规则")).toHaveTextContent(/^无$/);
  expect(value("实际计入接口")).toHaveTextContent("共 2 个");
  expect(screen.getByText("en0")).toBeInTheDocument();
  expect(screen.getByText("eth0")).toBeInTheDocument();
  expect(value("生效上报间隔")).toHaveTextContent("1500 ms");
  expect(value("采集失败项")).toHaveTextContent(/^最近采集未报告失败$/);
  expect(value("诊断信息更新时间")?.querySelector("time")).toHaveAttribute("dateTime", new Date(Number(updatedAt) * 1000).toISOString());
  expect(screen.getByText(/最近保存的诊断，不保证实时健康/)).toBeInTheDocument();
  expect(screen.getByText(/不含请求耗时和失败退避/)).toBeInTheDocument();
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
  expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
});

it("排除模式、没有计入接口与未知间隔分别如实展示", () => {
  render(<AgentDiagnostics diagnostics={create(AgentDiagnosticsSchema, { netExclude: ["lo", "docker*", "veth*"] })} />);
  expect(value("包含规则")).toHaveTextContent(/^全部接口$/);
  expect(value("排除规则")).toHaveTextContent("lo docker* veth*");
  expect(value("实际计入接口")).toHaveTextContent(/^没有计入的接口$/);
  expect(value("生效上报间隔")).toHaveTextContent(/^未知$/);
  expect(value("诊断信息更新时间")).toHaveTextContent(/^未知$/);
});

it("接口清单被截断时显示实际总数及当前展示数", () => {
  const netInterfaces = Array.from({ length: 128 }, (_, i) => `eth${String(i).padStart(3, "0")}`);
  render(<AgentDiagnostics diagnostics={create(AgentDiagnosticsSchema, { netInterfaces, netInterfacesTotal: 140 })} />);
  expect(value("实际计入接口")).toHaveTextContent("共 140 个，仅展示前 128 个");
  expect(screen.getByText("eth000")).toBeInTheDocument();
  expect(screen.getByText("eth127")).toBeInTheDocument();
});

it("网络失败显示本次采集失败而非没有匹配接口", () => {
  render(<AgentDiagnostics diagnostics={create(AgentDiagnosticsSchema, { failedCollectors: [CollectionComponent.NET] })} />);
  expect(value("实际计入接口")).toHaveTextContent(/^本次网络采集失败，接口清单不可用$/);
  expect(value("采集失败项")).toHaveTextContent(/^网络$/);
  expect(screen.queryByText("没有计入的接口")).not.toBeInTheDocument();
  expect(screen.queryByText("最近采集未报告失败")).not.toBeInTheDocument();
});

it("所有固定采集类别有可读名称", () => {
  render(<AgentDiagnostics diagnostics={create(AgentDiagnosticsSchema, { failedCollectors: [
    CollectionComponent.BOOT_ID, CollectionComponent.CPU, CollectionComponent.MEMORY,
    CollectionComponent.SWAP, CollectionComponent.DISK, CollectionComponent.LOAD,
    CollectionComponent.PROCS, CollectionComponent.UPTIME, CollectionComponent.CONNS, CollectionComponent.NET,
    CollectionComponent.DISK_IO,
  ] })} />);
  expect(value("采集失败项")).toHaveTextContent(/^启动标识、CPU、内存、交换空间、磁盘、负载、进程数、运行时间、连接数、网络、磁盘 I\/O$/);
});

it("磁盘 I/O 计数器失败与磁盘用量失败分开显示", () => {
  render(<AgentDiagnostics diagnostics={create(AgentDiagnosticsSchema, { failedCollectors: [
    CollectionComponent.DISK, CollectionComponent.DISK_IO,
  ] })} />);
  expect(value("采集失败项")).toHaveTextContent(/^磁盘、磁盘 I\/O$/);
});

it("结构规则启用时列出被排除的接口与原因，截断时写明总数", () => {
  const netStructureExcluded = Array.from({ length: 128 }, (_, i) => ({ name: `tap${String(i).padStart(3, "0")}`, reason: NetStructureReason.BRIDGE_PORT }));
  netStructureExcluded[0] = { name: "vmbr0", reason: NetStructureReason.STACKED };
  netStructureExcluded[1] = { name: "wg0", reason: NetStructureReason.TUNNEL };
  render(<AgentDiagnostics diagnostics={create(AgentDiagnosticsSchema, {
    netExclude: ["lo"], netInterfaces: ["eno1"], netInterfacesTotal: 1, netStructureRules: true, netStructureExcluded, netStructureExcludedTotal: 130,
  })} />);
  expect(value("结构规则")).toHaveTextContent("已启用，排除 130 个，仅展示前 128 个");
  expect(screen.getByText("vmbr0 · 叠加设备")).toBeInTheDocument();
  expect(screen.getByText("wg0 · 三层隧道")).toBeInTheDocument();
  expect(screen.getByText("tap002 · 网桥虚拟口")).toBeInTheDocument();
});

it("结构规则退回、未启用、启用但没有排除与网络失败各自如实展示", () => {
  const { rerender } = render(<AgentDiagnostics diagnostics={create(AgentDiagnosticsSchema, { netInterfaces: ["wg0"], netInterfacesTotal: 1, netStructureRules: true, netStructureFallback: true })} />);
  expect(value("结构规则")).toHaveTextContent(/^结构规则会排除全部网卡，本次只按名字规则计入$/);
  rerender(<AgentDiagnostics diagnostics={create(AgentDiagnosticsSchema, { netInclude: ["eth*"] })} />);
  expect(value("结构规则")).toHaveTextContent(/^未启用（显式网卡规则或非 Linux）$/);
  rerender(<AgentDiagnostics diagnostics={create(AgentDiagnosticsSchema, { netInterfaces: ["eth0"], netInterfacesTotal: 1, netStructureRules: true })} />);
  expect(value("结构规则")).toHaveTextContent(/^已启用，没有被排除的接口$/);
  rerender(<AgentDiagnostics diagnostics={create(AgentDiagnosticsSchema, { netStructureRules: true, failedCollectors: [CollectionComponent.NET, CollectionComponent.TCP_STATS] })} />);
  expect(value("结构规则")).toHaveTextContent(/^本次网络采集失败，结构判定不可用$/);
  expect(value("采集失败项")).toHaveTextContent(/^网络、TCP 计数$/);
});
