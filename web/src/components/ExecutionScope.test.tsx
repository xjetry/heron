import { create } from "@bufbuild/protobuf";
import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { ExecutionScopeSchema, ResourceScope, ScopeKind, ScopeNote } from "../gen/heron/v1/types_pb";
import { ExecutionScope } from "./ExecutionScope";

const updatedAt = 1_790_000_000n;

it("未上报不显示成主机范围", () => {
  render(<ExecutionScope />);
  expect(screen.getByLabelText("执行环境")).toHaveTextContent("未上报");
  expect(screen.queryByText("整台主机")).not.toBeInTheDocument();
});

it("主机、环境与带说明的未知范围按可见上限显示，不认识的说明不出现", () => {
  const { rerender } = render(<ExecutionScope updatedAt={updatedAt} execution={create(ExecutionScopeSchema, {
    kind: ScopeKind.HOST, cpu: ResourceScope.HOST, memory: ResourceScope.HOST, swap: ResourceScope.HOST, load: ResourceScope.HOST,
    cpuEffectiveCores: 4, loadCores: 4,
  })} />);
  expect(screen.getByText("整体范围").nextElementSibling).toHaveTextContent("整台主机");
  expect(screen.getByText("CPU").nextElementSibling).toHaveTextContent(/^整台主机，可见上限 4 核$/);
  expect(screen.getByText("负载").nextElementSibling).toHaveTextContent(/^整台主机，按核负载的分母 4 核$/);
  expect(screen.getByText("保存时间").nextElementSibling?.querySelector("time")).toHaveAttribute("dateTime", new Date(Number(updatedAt) * 1000).toISOString());
  expect(screen.getByText(/采样来源取自最近保存的 Facts/)).toBeInTheDocument();

  rerender(<ExecutionScope execution={create(ExecutionScopeSchema, {
    kind: ScopeKind.CGROUP_NAMESPACE, cpu: ResourceScope.ENVIRONMENT, memory: ResourceScope.ENVIRONMENT,
    swap: ResourceScope.UNKNOWN, load: ResourceScope.HOST,
    cpuEffectiveCores: 1.5, memoryLimitBytes: 512n * 1024n * 1024n, loadCores: 8,
    notes: [ScopeNote.SWAP_ACCOUNTING_MISSING, 99 as ScopeNote],
  })} />);
  expect(screen.getByText("整体范围").nextElementSibling).toHaveTextContent("容器或 guest 的 cgroup");
  expect(screen.getByText("CPU").nextElementSibling).toHaveTextContent(/^所在环境，可见上限 1\.5 核$/);
  expect(screen.getByText("内存").nextElementSibling).toHaveTextContent(/^所在环境，可见上限 512 MiB$/);
  expect(screen.getByText("swap").nextElementSibling).toHaveTextContent(/^无法确定$/);
  expect(screen.getByText("负载").nextElementSibling).toHaveTextContent(/^整台主机，按核负载的分母 8 核$/);
  expect(screen.getByText("说明").nextElementSibling).toHaveTextContent("环境的 swap 记账缺失或读不出");
  expect(screen.getByText("说明").nextElementSibling).not.toHaveTextContent("99");
});

it("范围已知而容量读不出显示读不出，swap 的 0 是已知值照常显示", () => {
  render(<ExecutionScope execution={create(ExecutionScopeSchema, {
    kind: ScopeKind.CGROUP_NAMESPACE, cpu: ResourceScope.ENVIRONMENT, memory: ResourceScope.ENVIRONMENT,
    swap: ResourceScope.ENVIRONMENT, load: ResourceScope.HOST,
    cpuEffectiveCores: 2, swapLimitBytes: 0n,
    notes: [ScopeNote.CPUINFO_NOT_PROCFS],
  })} />);
  expect(screen.getByText("内存").nextElementSibling).toHaveTextContent(/^所在环境，可见上限读不出$/);
  expect(screen.getByText("swap").nextElementSibling).toHaveTextContent(/^所在环境，可见上限 0 B$/);
  expect(screen.getByText("负载").nextElementSibling).toHaveTextContent(/^整台主机，按核负载的分母读不出$/);
});

it("容器里以宿主根采集的节点显示中性说明", () => {
  render(<ExecutionScope execution={create(ExecutionScopeSchema, {
    kind: ScopeKind.HOST, cpu: ResourceScope.HOST, memory: ResourceScope.HOST, swap: ResourceScope.HOST, load: ResourceScope.HOST,
    notes: [ScopeNote.CONTAINER_SIGNAL_ON_HOST_ROOT],
  })} />);
  expect(screen.getByText("整体范围").nextElementSibling).toHaveTextContent("整台主机");
  expect(screen.getByText("说明").nextElementSibling).toHaveTextContent(/^在容器中运行，读数来自整台主机$/);
});
