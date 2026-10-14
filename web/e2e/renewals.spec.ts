import { AdminService } from "../src/gen/heron/v1/admin_pb";
import { BillingCycle } from "../src/gen/heron/v1/types_pb";
import { addDays, addMonths } from "../src/lib/ymd";
import { expect, login, must, mustField, test } from "./fixtures";

// 浏览器放在与 hub 不同的时区：日历按 hub 下发的 days_left 定今天，与浏览器时区无关（逐日的断言在 Renewals.test.tsx）。
test.use({ timezoneId: "Pacific/Kiritimati" });

// 冒烟：真实 hub 上一台今天到期、没开自动续期的节点出现在续费日历默认选中的今天里，「已续费」经 RenewNodeBilling
// 推后，页面与 hub 回读的是同一个日期。
test("续费日历：今天到期的节点经「已续费」推后", async ({ page, hub, browserName }) => {
  const name = `${browserName}-renewal`;
  const id = mustField(await hub.rpc(AdminService.method.createNode, { name }), "node").id;
  hub.deleteNodeAtEnd(id);
  const probe = mustField(await hub.rpc(AdminService.method.updateNode, { id, name, trafficResetDay: 1, offlineGraceS: 0, billing: { billingCycle: BillingCycle.MONTHLY, expiresOn: "2030-06-15" } }), "node", "billing");
  if (probe.daysLeft === undefined) throw new Error(`hub returned no days_left for ${probe.expiresOn}`);
  const today = addDays(probe.expiresOn, -probe.daysLeft);
  must(await hub.rpc(AdminService.method.updateNode, { id, name, trafficResetDay: 1, offlineGraceS: 0, billing: { billingCycle: BillingCycle.MONTHLY, expiresOn: today } }));

  await page.goto("/admin/login");
  await login(page);
  await page.goto("/admin/renewals");
  const label = `${name}（#${id}）`;
  await expect(page.getByRole("heading", { name: `${today} 到期` })).toBeVisible();
  await expect(page.locator('[aria-current="date"]')).toHaveAttribute("data-ymd", today);
  await page.getByRole("button", { name: `已续费 ${label}` }).click();
  const renewed = page.waitForResponse((r) => r.url().endsWith("/heron.v1.AdminService/RenewNodeBilling"));
  await page.getByRole("button", { name: "确认已续费" }).click();
  expect((await renewed).status()).toBe(200);
  const nodes = must(await hub.rpc(AdminService.method.listNodes, {})).nodes;
  const expiresOn = nodes.find((n) => n.id === id)?.billing?.expiresOn;
  // 今天到期即未过期：恰好推后一个周期，月末钳制与 hub 一致。
  expect(expiresOn).toBe(addMonths(today, 1));
  await expect(page.getByText(`${label} 已续费，到期日推后到 ${expiresOn}`)).toBeVisible();
  await expect(page.getByRole("button", { name: `已续费 ${label}` })).toHaveCount(0);
});
