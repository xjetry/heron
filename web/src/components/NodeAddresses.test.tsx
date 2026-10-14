import { create } from "@bufbuild/protobuf";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AddressSource as Source, NodeNetworkSchema } from "../gen/heron/v1/admin_pb";
import { AddressDetectionState as State, NetworkInfoSchema } from "../gen/heron/v1/types_pb";
import { dateTime } from "../lib/format";
import { NodeAddresses } from "./NodeAddresses";

const ipv4 = "8.8.8.8";
const ipv6 = "2606:4700:4700:0000:0000:0000:0000:1111";
const network = (address = ipv4) => create(NodeNetworkSchema, {
  ipv4: { state: State.AVAILABLE, address, source: Source.DETECTED },
  ipv6: { state: State.AVAILABLE, address: ipv6, source: Source.DETECTED },
});
afterEach(() => { vi.unstubAllGlobals(); window.getSelection()?.removeAllRanges(); });

it.each([false, true])("双栈地址独立复制完整值，详细模式 %s", async (detailed) => {
  const writeText = vi.fn(async () => {});
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  render(<NodeAddresses network={network()} detailed={detailed} />);
  for (const [family, address] of [["IPv4", ipv4], ["IPv6", ipv6]]) {
    fireEvent.click(screen.getByRole("button", { name: `复制 ${family} ${address}` }));
    await waitFor(() => expect(writeText).toHaveBeenLastCalledWith(address));
    expect(await screen.findByText(`已复制 ${address}`)).toBeInTheDocument();
  }
  expect(writeText.mock.calls).toEqual([[ipv4], [ipv6]]);
});

it("不可用或空地址不提供复制操作，也不复制残留地址", () => {
  for (const state of [State.UNSUPPORTED, State.FAILED, State.DISABLED, State.UNSPECIFIED]) {
    const { unmount } = render(<NodeAddresses network={create(NodeNetworkSchema, { ipv4: { state, address: ipv4, source: Source.DETECTED }, ipv6: { state: State.AVAILABLE } })} />);
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
    expect(screen.queryByText(ipv4)).not.toBeInTheDocument();
    unmount();
  }
});

it.each(["missing", "rejected"])("剪贴板 %s 时显示失败并选中完整 IP", async (reason) => {
  vi.stubGlobal("navigator", reason === "missing" ? {} : { clipboard: { writeText: async () => { throw new Error("denied"); } } });
  render(<NodeAddresses network={network()} />);
  fireEvent.click(screen.getByRole("button", { name: `复制 IPv6 ${ipv6}` }));
  expect(await screen.findByText("复制失败，请手动选择")).toBeInTheDocument();
  expect(window.getSelection()?.toString()).toBe(ipv6);
  expect(screen.queryByText(`已复制 ${ipv6}`)).not.toBeInTheDocument();
});

it("IP 变化后旧复制结果不能污染新地址反馈", async () => {
  let finish!: () => void;
  vi.stubGlobal("navigator", { clipboard: { writeText: () => new Promise<void>((resolve) => { finish = resolve; }) } });
  const { rerender } = render(<NodeAddresses network={network()} />);
  fireEvent.click(screen.getByRole("button", { name: `复制 IPv4 ${ipv4}` }));
  rerender(<NodeAddresses network={network("1.1.1.1")} />);
  await act(async () => finish());
  expect(screen.getByRole("button", { name: "复制 IPv4 1.1.1.1" })).toBeInTheDocument();
  expect(screen.queryByText(/^已复制 /)).not.toBeInTheDocument();
});

// 手填的族显示手填地址并标"手填"；详细视图同时给出 agent 原报（已停用探测）与它的时间，看得出 agent 是否已按手填停用。
it("手填地址带标记，详细视图对照 agent 原报", () => {
  const display = create(NodeNetworkSchema, {
    ipv4: { state: State.AVAILABLE, address: ipv4, source: Source.MANUAL },
    ipv6: { state: State.FAILED, source: Source.DETECTED },
  });
  const reported = create(NetworkInfoSchema, { ipv4: { state: State.DISABLED, checkedAt: 1790679000n }, ipv6: { state: State.FAILED, checkedAt: 1790679000n } });
  const { rerender } = render(<NodeAddresses network={display} reported={reported} />);
  const v4 = screen.getByLabelText("IPv4");
  expect(v4).toHaveTextContent(`${ipv4}手填`);
  expect(v4).not.toHaveTextContent("已停用探测");
  expect(screen.getByLabelText("IPv6")).toHaveTextContent("探测失败");
  expect(screen.getByLabelText("IPv6")).not.toHaveTextContent("手填");
  expect(v4).toHaveAttribute("title", `管理员手填；agent：已停用探测（自 ${dateTime(1790679000n)} 起）`);
  rerender(<NodeAddresses network={display} reported={reported} detailed />);
  expect(screen.getByLabelText("IPv4")).toHaveTextContent("agent：已停用探测");
  expect(screen.getByLabelText("IPv4").querySelector("time")).toHaveAttribute("datetime", "2026-09-29T10:50:00.000Z");
  expect(screen.getByLabelText("IPv4")).toHaveTextContent(`自 ${dateTime(1790679000n)} 起`);
});

// checked_at 是当前结果首次得到的时刻，不是最近一次探测：探测来源的族写"结果自某时起未变"，详细视图的时间同样写成"自某时起"。
it("探测时间按结果起始时刻表述", () => {
  const display = create(NodeNetworkSchema, {
    ipv4: { state: State.AVAILABLE, address: ipv4, source: Source.DETECTED },
    ipv6: { state: State.UNSUPPORTED, source: Source.DETECTED },
  });
  const reported = create(NetworkInfoSchema, { ipv4: { state: State.AVAILABLE, address: ipv4, checkedAt: 1790679000n }, ipv6: { state: State.UNSUPPORTED, checkedAt: 1790679000n } });
  const { rerender } = render(<NodeAddresses network={display} reported={reported} />);
  const at = dateTime(1790679000n);
  expect(screen.getByLabelText("IPv4")).toHaveAttribute("title", `结果自 ${at} 起未变`);
  expect(screen.getByLabelText("IPv6")).toHaveAttribute("title", `结果自 ${at} 起未变；无可用接口地址或路由`);
  expect(screen.getByLabelText("IPv4")).not.toHaveTextContent(at);
  rerender(<NodeAddresses network={display} reported={reported} detailed />);
  for (const family of ["IPv4", "IPv6"]) {
    expect(screen.getByLabelText(family).querySelector(".address-since")).toHaveTextContent(`自 ${at} 起`);
  }
});

// 手填清空后、agent 恢复探测之前，显示值是探测来源的 DISABLED：写"等待探测"，不写"已停用"，也不带手填标记。
it("手填清空而 agent 尚未恢复时显示等待探测", () => {
  render(<NodeAddresses network={create(NodeNetworkSchema, { ipv4: { state: State.DISABLED, source: Source.DETECTED } })} reported={create(NetworkInfoSchema, { ipv4: { state: State.DISABLED, checkedAt: 1790679000n } })} detailed />);
  const v4 = screen.getByLabelText("IPv4");
  expect(v4).toHaveTextContent("等待探测");
  expect(v4).not.toHaveTextContent("手填");
  expect(screen.getByLabelText("IPv6")).toHaveTextContent("等待上报");
});

// hub 来源（HUB_SEEN）：显示来源地址并标"上报来源"；title 与手填同形、点明不是 agent 探测；详细视图像手填一样对照
// agent 原报，但不渲染独立的"自 … 起"——数据是一个 FAILED 很久的节点刚换了上报来源，checked_at 只属于 agent 原报，
// 独立一行会像 hub 来源地址的起始时刻，时间只能留在 agent 原报文案里。
it("hub 来源带标记，不渲染独立的起始时刻", () => {
  const display = create(NodeNetworkSchema, {
    ipv4: { state: State.AVAILABLE, address: ipv4, source: Source.HUB_SEEN },
    ipv6: { state: State.FAILED, source: Source.DETECTED },
  });
  const reported = create(NetworkInfoSchema, { ipv4: { state: State.FAILED, checkedAt: 1790679000n }, ipv6: { state: State.FAILED, checkedAt: 1790679000n } });
  const { rerender } = render(<NodeAddresses network={display} reported={reported} />);
  const v4 = screen.getByLabelText("IPv4");
  expect(v4).toHaveTextContent(`${ipv4}上报来源`);
  expect(v4).toHaveAttribute("title", `hub 在上报连接上看到的来源地址，不是 agent 探测；agent：探测失败（自 ${dateTime(1790679000n)} 起）`);
  expect(screen.getByLabelText("IPv6")).toHaveTextContent("探测失败");
  rerender(<NodeAddresses network={display} reported={reported} detailed />);
  const row = screen.getByLabelText("IPv4");
  expect(row).toHaveTextContent("agent：探测失败");
  expect(row.querySelector(".address-since")).toBeNull();
  expect(row.querySelector("time")).toBeNull();
  expect(screen.getByLabelText("IPv6").querySelector(".address-since")).toHaveTextContent(`自 ${dateTime(1790679000n)} 起`);
});
