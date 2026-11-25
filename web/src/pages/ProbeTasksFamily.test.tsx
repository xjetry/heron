import { expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { renderWithAdmin } from "../test/harness";
import { AddressSource, type SaveProbeTaskRequest } from "../gen/heron/v1/admin_pb";
import { AddressDetectionState, DnsRecordType, ProbeIpFamily, ProbeKind } from "../gen/heron/v1/types_pb";
import { ProbeTasks } from "./ProbeTasks";

const routes = [{ path: "/probes", Component: ProbeTasks }];
const available = (address: string) => ({ address, state: AddressDetectionState.AVAILABLE, source: AddressSource.DETECTED });
const nodes = { nodes: [
  { id: 1n, name: "东京", network: { ipv4: available("203.0.113.5"), ipv6: available("2001:db8::5") } },
  { id: 2n, name: "法兰克福", network: { ipv4: { state: AddressDetectionState.FAILED, source: AddressSource.DETECTED } } },
  { id: 3n, name: "首尔", network: { ipv6: available("2001:db8::7") } },
] };

async function openCreate() {
  fireEvent.click(await screen.findByRole("button", { name: "新建探测任务" }));
  return screen.getByRole("form", { name: "新建探测任务" });
}

it("地址族只对 ICMP、TCP、HTTP 出现，记录类型只对 DNS 出现；提交只带适用的那一项", async () => {
  const saved: SaveProbeTaskRequest[] = [];
  renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => ({}),
    saveProbeTask: async (req) => { saved.push(req); return {}; },
  }, routes, "/probes");
  let form = await openCreate();
  const family = within(form).getByRole("radiogroup", { name: "地址族" });
  expect(within(family).getAllByRole("radio").map((radio) => radio.getAttribute("aria-label"))).toEqual(["自动", "IPv4", "IPv6"]);
  expect(within(family).getByRole("radio", { name: "自动" })).toBeChecked();
  expect(within(form).queryByRole("radiogroup", { name: "记录类型" })).toBeNull();
  fireEvent.click(within(family).getByRole("radio", { name: "IPv6" }));
  fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "example.com" } });
  fireEvent.submit(form);
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].task).toMatchObject({ kind: ProbeKind.ICMP, target: "example.com", ipFamily: ProbeIpFamily.IPV6, dnsRecordType: DnsRecordType.UNSPECIFIED });

  // 草稿里残留的地址族不随 DNS 提交，记录类型也不随别的种类提交。
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull(), { timeout: 1000 });
  form = await openCreate();
  fireEvent.click(within(form).getByRole("radio", { name: "IPv6" }));
  fireEvent.click(within(form).getByRole("radio", { name: "DNS" }));
  expect(within(form).queryByRole("radiogroup", { name: "地址族" })).toBeNull();
  const record = within(form).getByRole("radiogroup", { name: "记录类型" });
  expect(within(record).getAllByRole("radio").map((radio) => radio.getAttribute("aria-label"))).toEqual(["A", "AAAA"]);
  expect(within(record).getByRole("radio", { name: "A" })).toBeChecked();
  fireEvent.click(within(record).getByRole("radio", { name: "AAAA" }));
  fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "example.com" } });
  fireEvent.change(within(form).getByLabelText("解析器"), { target: { value: "1.1.1.1:53" } });
  fireEvent.submit(form);
  await waitFor(() => expect(saved).toHaveLength(2));
  expect(saved[1].task).toMatchObject({ kind: ProbeKind.DNS, ipFamily: ProbeIpFamily.UNSPECIFIED, dnsRecordType: DnsRecordType.AAAA });

  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull(), { timeout: 1000 });
  form = await openCreate();
  fireEvent.click(within(form).getByRole("radio", { name: "DNS" }));
  fireEvent.click(within(form).getByRole("radio", { name: "AAAA" }));
  fireEvent.click(within(form).getByRole("radio", { name: "HTTP" }));
  fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "https://example.com/" } });
  fireEvent.submit(form);
  await waitFor(() => expect(saved).toHaveLength(3));
  expect(saved[2].task).toMatchObject({ kind: ProbeKind.HTTP, ipFamily: ProbeIpFamily.UNSPECIFIED, dnsRecordType: DnsRecordType.UNSPECIFIED });
});

it("编辑回填地址族与记录类型，列表在类型旁标出非缺省值", async () => {
  renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => ({ tasks: [
    { task: { id: 1n, kind: ProbeKind.TCP, target: "example.com:443", intervalS: 60, timeoutMs: 1000, ipFamily: ProbeIpFamily.IPV6 } },
    { task: { id: 2n, kind: ProbeKind.DNS, target: "example.com", dnsServer: "1.1.1.1:53", intervalS: 60, timeoutMs: 1000, dnsRecordType: DnsRecordType.AAAA } },
    { task: { id: 3n, kind: ProbeKind.ICMP, target: "example.com", intervalS: 60, timeoutMs: 1000 } },
  ] }) }, routes, "/probes");
  expect(await screen.findByRole("cell", { name: "TCP · IPv6" })).toBeInTheDocument();
  expect(screen.getByRole("cell", { name: "DNS · AAAA" })).toBeInTheDocument();
  expect(screen.getByRole("cell", { name: "ICMP" })).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "更多操作 example.com:443（#1）" }));
  fireEvent.click(screen.getByRole("menuitem", { name: "编辑 example.com:443（#1）" }));
  expect(within(screen.getByRole("dialog")).getByRole("radio", { name: "IPv6" })).toBeChecked();
});

it("从节点出口地址快选目标：只列已就绪的地址，双栈拆成两项；TCP 的 IPv6 带方括号并保留端口", async () => {
  renderWithAdmin({ listNodes: async () => nodes, listProbeTasks: async () => ({}) }, routes, "/probes");
  const form = await openCreate();
  const target = within(form).getByLabelText("目标");
  const picker = () => within(form).getByLabelText("从节点地址填入");
  expect(within(picker()).getAllByRole("option").map((option) => option.textContent)).toEqual([
    "选择节点地址…", "东京 · IPv4 203.0.113.5", "东京 · IPv6 2001:db8::5", "首尔 · IPv6 2001:db8::7",
  ]);
  const pick = (label: string) => {
    const option = within(picker()).getByRole<HTMLOptionElement>("option", { name: label });
    fireEvent.change(picker(), { target: { value: option.value } });
  };

  pick("东京 · IPv6 2001:db8::5");
  expect(target).toHaveValue("2001:db8::5");
  expect(picker()).toHaveValue("");

  fireEvent.click(within(form).getByRole("radio", { name: "TCP" }));
  fireEvent.change(target, { target: { value: "example.com:8443" } });
  pick("东京 · IPv6 2001:db8::5");
  expect(target).toHaveValue("[2001:db8::5]:8443");
  pick("东京 · IPv4 203.0.113.5");
  expect(target).toHaveValue("203.0.113.5:8443");
  // 还没有端口时留出冒号，端口由人补上。
  fireEvent.change(target, { target: { value: "" } });
  pick("首尔 · IPv6 2001:db8::7");
  expect(target).toHaveValue("[2001:db8::7]:");

  // HTTP 只换主机，保留协议、端口与路径。
  fireEvent.click(within(form).getByRole("radio", { name: "HTTP" }));
  fireEvent.change(target, { target: { value: "https://example.com:8443/p?q=1" } });
  pick("东京 · IPv6 2001:db8::5");
  expect(target).toHaveValue("https://[2001:db8::5]:8443/p?q=1");
  fireEvent.change(target, { target: { value: "" } });
  pick("东京 · IPv4 203.0.113.5");
  expect(target).toHaveValue("http://203.0.113.5/");

  // 显式地址族与选中的地址不同族时跟着换成该族，否则保存必被拒绝。
  fireEvent.click(within(form).getByRole("radio", { name: "IPv4" }));
  pick("首尔 · IPv6 2001:db8::7");
  expect(within(form).getByRole("radio", { name: "IPv6" })).toBeChecked();

  // DNS 的目标是名字，不提供地址快选。
  fireEvent.click(within(form).getByRole("radio", { name: "DNS" }));
  expect(within(form).queryByLabelText("从节点地址填入")).toBeNull();
});
