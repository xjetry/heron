import { screen } from "@testing-library/react";
import { expect } from "vitest";

// 空列表的页面断言（components/EmptyState.tsx）：空态写在卡片里，原来那张表连同表头都不画；
// 筛选后为空的空态是 status（随输入播报），列表本身为空不是 live region。
export async function expectEmptyState(title: string | RegExp, { region, status = false }: { region: string; status?: boolean }) {
  const card = (await screen.findByText(title)).closest(".empty-state");
  expect(card).not.toBeNull();
  if (status) expect(card).toHaveAttribute("role", "status");
  else expect(card).not.toHaveAttribute("role");
  expect(screen.queryByRole("region", { name: region })).toBeNull();
}
