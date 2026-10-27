import { create } from "@bufbuild/protobuf";
import { render, screen, within } from "@testing-library/react";
import { expect, it } from "vitest";
import { PublicNodeSchema } from "../gen/heron/v1/public_pb";
import { StatusSummary } from "./StatusSummary";

const n = (init: Parameters<typeof create<typeof PublicNodeSchema>>[1]) => create(PublicNodeSchema, init);
const nodes = [
  n({ id: 1n, name: "a", online: true, lastSeenAt: 1n, metrics: { netRxBps: 2048n, netTxBps: 1024n }, traffic: { periodRx: 1024n ** 3n, periodTx: 0n } }),
  n({ id: 2n, name: "b", online: true, lastSeenAt: 1n, maintenance: true }),
  n({ id: 5n, name: "e", online: true, lastSeenAt: 1n, rented: true, metrics: { netRxBps: 9999n, netTxBps: 9999n } }),
  n({ id: 3n, name: "c", online: false, lastSeenAt: 1n }),
  n({ id: 4n, name: "d", online: false }),
];

function summary() {
  render(<StatusSummary nodes={nodes} />);
  return within(screen.getByRole("region", { name: "汇总" }));
}

it("在线 / 总数按五态算，维护中不算在线；已出租不进分母、另计", () => {
  const s = summary();
  expect(s.getByText("1 / 4 在线")).toBeInTheDocument();
  expect(s.getByText("已出租 1")).toBeInTheDocument();
});

it("没有已出租节点时不出已出租计数", () => {
  render(<StatusSummary nodes={nodes.filter((n) => !n.rented)} />);
  expect(screen.getByText("1 / 4 在线")).toBeInTheDocument();
  expect(screen.queryByText(/已出租/)).toBeNull();
});

it("五态分段有可访问计数，按固定顺序与比例显示", () => {
  const bar = summary().getByRole("img", { name: "在线 1、维护中 1、已出租 1、从未上报 1、离线 1" });
  expect([...bar.querySelectorAll("[data-status]")].map((el) => [el.getAttribute("data-status"), (el as HTMLElement).style.width]))
    .toEqual([["online", "20%"], ["maintenance", "20%"], ["rented", "20%"], ["never", "20%"], ["offline", "20%"]]);
});

it("实时合计显示上下行二进制速率", () => {
  expect(summary().getByRole("group", { name: "实时合计" })).toHaveTextContent("↓ 2.0 KiB/s ↑ 1.0 KiB/s");
});

it("本周期显示二进制总流量", () => {
  expect(summary().getByText(/本周期/)).toHaveTextContent("本周期 1.0 GiB");
});

it("总数为 0 时在线计数为零", () => {
  render(<StatusSummary nodes={[]} />);
  expect(screen.getByText("0 / 0 在线")).toBeInTheDocument();
});

it("总数为 0 时分段条为空，不生成 NaN 宽度", () => {
  render(<StatusSummary nodes={[]} />);
  expect(screen.getByRole("img", { name: "在线 0、维护中 0、已出租 0、从未上报 0、离线 0" }).querySelectorAll("[data-status]")).toHaveLength(0);
});

it("非空快照的零计数段也不画", () => {
  render(<StatusSummary nodes={[nodes[0]]} />);
  const bar = screen.getByRole("img", { name: "在线 1、维护中 0、已出租 0、从未上报 0、离线 0" });
  expect([...bar.querySelectorAll("[data-status]")].map((el) => [el.getAttribute("data-status"), (el as HTMLElement).style.width]))
    .toEqual([["online", "100%"]]);
});
