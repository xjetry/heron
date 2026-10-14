import { Code, ConnectError } from "@connectrpc/connect";
import { screen, within } from "@testing-library/react";
import { expect, it } from "vitest";
import { renderWithAdmin } from "../test/harness";
import { Appearance } from "../pages/Appearance";
import { dateTime } from "../lib/format";

const routes = [{ path: "/appearance", Component: Appearance }];

it("设置页按层显示启用、上次成功和故障，保留从未成功的缺席语义", async () => {
  renderWithAdmin({
    getSettings: async () => ({}),
    getBackupStatus: async () => ({ enabled: true, config: { lastSuccessAt: 0n, failure: { category: "upload/http_status", sinceAt: 60n, statusCode: 503 } }, metrics: {} }),
  }, routes, "/appearance");
  const region = within(await screen.findByRole("region", { name: "备份状态" }));
  expect(region.getByText("自动备份已启用")).toBeVisible();
  const config = within(region.getByRole("article", { name: "配置与凭据" }));
  expect(config.getByText(`上次成功：${dateTime(0)}`)).toBeVisible();
  expect(config.getByText(`当前故障：upload/http_status（HTTP 503）；自 ${dateTime(60)} 起`)).toHaveClass("error");
  const metrics = within(region.getByRole("article", { name: "指标与探测历史" }));
  expect(metrics.getByText("上次成功：从未成功")).toBeVisible();
  expect(metrics.getByText("无当前故障")).toBeVisible();
});

it("设置读不出时 enabled 为 false，仍显示配置层 settings 故障与历史成功", async () => {
  renderWithAdmin({ getSettings: async () => ({}), getBackupStatus: async () => ({ enabled: false, config: { lastSuccessAt: 60n, failure: { category: "settings", sinceAt: 120n } }, metrics: { lastSuccessAt: 60n } }) }, routes, "/appearance");
  const region = within(await screen.findByRole("region", { name: "备份状态" }));
  expect(region.getByText("自动备份未启用")).toBeVisible();
  const config = within(region.getByRole("article", { name: "配置与凭据" }));
  expect(config.getByText(`上次成功：${dateTime(60)}`)).toBeVisible();
  expect(config.getByText(`当前故障：settings；自 ${dateTime(120)} 起`)).toHaveClass("error");
  const metrics = within(region.getByRole("article", { name: "指标与探测历史" }));
  expect(metrics.getByText(`上次成功：${dateTime(60)}`)).toBeVisible();
  expect(metrics.getByText("无当前故障")).toBeVisible();
});

it("状态查询失败不冒充备份正常，也不阻塞设置表单", async () => {
  renderWithAdmin({ getSettings: async () => ({}), getBackupStatus: async () => { throw new ConnectError("backup status failed", Code.Internal); } }, routes, "/appearance");
  expect(await screen.findByText(/backup status failed/)).toBeVisible();
  expect(screen.queryByText("无当前故障")).toBeNull();
  expect(screen.getByRole("form", { name: "公开页外观" })).toBeVisible();
});

it("缺包主题单列提示，主题上传故障给出可操作说明", async () => {
  renderWithAdmin({ getSettings: async () => ({}), getBackupStatus: async () => ({ themesWithoutPackage: ["old"], config: { failure: { category: "theme_upload/http_status", sinceAt: 1n } } }) }, routes, "/appearance");
  expect(await screen.findByText("主题 old 未备份：请重新上传原包")).toBeVisible();
  expect(screen.getByText("主题包上传失败，请检查对象存储连接、凭据和写入权限")).toBeVisible();
});

it("恢复记录按恢复时刻倒序展示，逐表明细放 title，缺席显示未恢复指标层", async () => {
  renderWithAdmin({
    getSettings: async () => ({}),
    getBackupStatus: async () => ({}),
    listRestoreRecords: async () => ({
      records: [
        { id: "latest", restoredAt: 900n, configTakenAt: 800n, metricsTakenAt: 850n,
          orphans: [{ table: "metric_1m", rows: 3n }, { table: "probe_1h", rows: 4n }],
          themes: { restored: ["dark"], missing: ["old"], ignored: ["stray.zip"] } },
        { id: "config-only", restoredAt: 500n, configTakenAt: 400n, orphans: [], themes: {} },
      ],
    }),
  }, routes, "/appearance");
  const section = within(await screen.findByRole("region", { name: "恢复记录" }));
  const table = within(await section.findByRole("region", { name: "恢复记录表" }));
  const rows = table.getAllByRole("row");
  expect(rows).toHaveLength(3);
  const latest = within(rows[1]);
  expect(latest.getByText(dateTime(900n))).toBeVisible();
  expect(latest.getByText(dateTime(800n))).toBeVisible();
  expect(latest.getByText(dateTime(850n))).toBeVisible();
  expect(latest.getByText("7 行")).toBeVisible();
  expect(latest.getByTitle("metric_1m：3 行；probe_1h：4 行")).toBeVisible();
  expect(latest.getByText("恢复 1 / 缺包 1 / 忽略 1")).toBeVisible();
  expect(latest.getByTitle("恢复：dark；缺包：old；忽略：stray.zip")).toBeVisible();
  const configOnly = within(rows[2]);
  expect(configOnly.getByText(dateTime(500n))).toBeVisible();
  // 缺席是那次恢复没带指标层，不是 0 时刻。
  expect(configOnly.getByText("未恢复指标层")).toBeVisible();
  expect(configOnly.getByText("0 行")).toBeVisible();
  expect(configOnly.getByText("恢复 0 / 缺包 0 / 忽略 0")).toBeVisible();
});

it("没有恢复记录时空态一行说明", async () => {
  renderWithAdmin({ getSettings: async () => ({}), getBackupStatus: async () => ({}), listRestoreRecords: async () => ({ records: [] }) }, routes, "/appearance");
  const section = within(await screen.findByRole("region", { name: "恢复记录" }));
  expect(await section.findByText("没有恢复记录")).toBeVisible();
  expect(section.queryByRole("region", { name: "恢复记录表" })).toBeNull();
});
