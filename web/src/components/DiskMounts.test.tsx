import { create } from "@bufbuild/protobuf";
import { render, screen, within } from "@testing-library/react";
import { expect, it } from "vitest";
import { GetNodeDisksResponseSchema } from "../gen/heron/v1/admin_pb";
import { DiskMounts } from "./DiskMounts";

const GIB = 1024n ** 3n;

it("按 hub 给出的顺序列出挂载路径、用量与使用率，截断时说明总数", () => {
  render(<DiskMounts data={create(GetNodeDisksResponseSchema, {
    total: 20, sampledAt: 1_790_000_000n, online: true,
    mounts: [{ path: "/data", total: 100n * GIB, used: 90n * GIB }, { path: "/", total: 20n * GIB, used: 5n * GIB }],
  })} />);
  const card = within(screen.getByRole("region", { name: "本地磁盘" }));
  const rows = card.getAllByRole("row").slice(1);
  expect(rows.map((r) => r.querySelector("code")?.textContent)).toEqual(["/data", "/"]);
  expect(rows[0]).toHaveTextContent("90 GiB / 100 GiB");
  expect(within(rows[0]).getByRole("meter")).toHaveAttribute("aria-valuenow", "90");
  expect(within(rows[1]).getByRole("meter")).toHaveAttribute("aria-valuenow", "25");
  expect(card.getByText(/共 20 块，仅展示最满的 2 块/)).toBeInTheDocument();
  expect(card.getByText(/最近一次上报/)).toBeInTheDocument();
});

it("离线节点标出读数是离线前最后一次上报", () => {
  render(<DiskMounts data={create(GetNodeDisksResponseSchema, { total: 1, sampledAt: 1_790_000_000n, online: false, mounts: [{ path: "/", total: 10n * GIB, used: 1n * GIB }] })} />);
  expect(screen.getByText(/离线前最后一次上报/)).toBeInTheDocument();
  expect(screen.queryByText(/仅展示/)).not.toBeInTheDocument();
});

it("hub 未收到上报与本次没有逐盘读数分开说明，不显示空表", () => {
  const { rerender } = render(<DiskMounts data={create(GetNodeDisksResponseSchema, {})} />);
  expect(screen.getByText("hub 启动后尚未收到该节点的上报。")).toBeInTheDocument();
  expect(screen.queryByRole("table")).not.toBeInTheDocument();
  rerender(<DiskMounts data={create(GetNodeDisksResponseSchema, { sampledAt: 1_790_000_000n, online: true })} />);
  expect(screen.getByText(/最近一次上报没有逐盘读数/)).toBeInTheDocument();
  expect(screen.queryByRole("table")).not.toBeInTheDocument();
});
