import { create } from "@bufbuild/protobuf";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { expect, it } from "vitest";
import { HeartbeatMethod, HeartbeatSchema, type GetHeartbeatStatusResponse, type UpdateSettingsRequest } from "../gen/heron/v1/admin_pb";
import { renderWithAdmin } from "../test/harness";
import { HeartbeatSettingsForm, HeartbeatStatus as HeartbeatStatusView } from "./HeartbeatSettings";

const configured = create(HeartbeatSchema, { intervalS: 120, method: HeartbeatMethod.POST, hasUrl: true, urlHost: "hc.example" });

// 地址是只写设置：已配置时留空不能提交（改间隔不会误清地址），填新地址保存发的是整组，停用按钮才发空地址。
it("心跳表单提交整组、已配置时留空不清地址", async () => {
  const sent: UpdateSettingsRequest[] = [];
  renderWithAdmin(
    { updateSettings: async (r) => { sent.push(r); return { settings: r.settings }; } },
    [{ path: "/hb", Component: () => <HeartbeatSettingsForm current={configured} /> }],
    "/hb",
  );
  const form = within(await screen.findByRole("form", { name: "心跳外推" }));
  expect(form.getByRole("button", { name: "保存" })).toBeDisabled();
  fireEvent.change(form.getByLabelText("地址"), { target: { value: "https://hc.example/ping/abc" } });
  fireEvent.change(form.getByLabelText("间隔（秒）"), { target: { value: "300" } });
  fireEvent.click(form.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  const hb = sent[0].settings?.heartbeat;
  expect(hb?.url).toBe("https://hc.example/ping/abc");
  expect(hb?.intervalS).toBe(300);
  expect(hb?.method).toBe(HeartbeatMethod.POST);
  fireEvent.click(form.getByRole("button", { name: "停用心跳" }));
  await waitFor(() => expect(sent).toHaveLength(2));
  expect(sent[1].settings?.heartbeat?.url).toBe("");
});

// 从未配置过时表单从默认开始：60 秒、POST，地址空也能保存（是显式给出这一组）。
it("未配置过的心跳表单用默认值", async () => {
  const sent: UpdateSettingsRequest[] = [];
  renderWithAdmin(
    { updateSettings: async (r) => { sent.push(r); return { settings: r.settings }; } },
    [{ path: "/hb", Component: () => <HeartbeatSettingsForm current={undefined} /> }],
    "/hb",
  );
  const form = within(await screen.findByRole("form", { name: "心跳外推" }));
  fireEvent.click(form.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  const hb = sent[0].settings?.heartbeat;
  expect(hb?.intervalS).toBe(60);
  expect(hb?.method).toBe(HeartbeatMethod.POST);
  expect(hb?.url).toBe("");
});

it("POST 计数说明包含已出租", async () => {
  renderWithAdmin(
    { updateSettings: async (r) => ({ settings: r.settings }) },
    [{ path: "/hb", Component: () => <HeartbeatSettingsForm current={configured} /> }],
    "/hb",
  );
  const form = within(await screen.findByRole("form", { name: "心跳外推" }));
  expect(form.getByText(/POST 会带上/)).toHaveTextContent("已出租");
});

it("心跳状态区分从未成功与失败类别", async () => {
  const status: GetHeartbeatStatusResponse = {
    $typeName: "heron.v1.GetHeartbeatStatusResponse", enabled: true, lastSuccessAt: 1700000000n, lastFailureAt: 1700000500n,
    failureCategory: "http_status", failureHttpStatus: 503, nextAt: 1700000600n,
  };
  renderWithAdmin(
    { getHeartbeatStatus: async () => status },
    [{ path: "/hb", Component: HeartbeatStatusView }],
    "/hb",
  );
  const section = within(await screen.findByRole("region", { name: "心跳状态" }));
  expect(section.getByText(/上次成功：/)).toBeInTheDocument();
  expect(section.getByText(/http_status（HTTP 503）/)).toBeInTheDocument();
  expect(section.getByText(/连不上目标地址|对方返回了非 2xx/)).toBeInTheDocument();
});
