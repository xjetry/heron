import type { DescMethodUnary } from "@bufbuild/protobuf";
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AdminService } from "../gen/heron/v1/admin_pb";
import { PublicService } from "../gen/heron/v1/public_pb";
import { transport as admin } from "./transport";
import { transport as publicTransport } from "../public/transport";
import { queryDefaults } from "../queryDefaults";
import { errorBanner, queryGate } from "./queryGate";
import { READ_DEADLINE_MS, READ_DEADLINE_MESSAGE } from "./deadline";

afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); });

const reads = [
  "ListOperations", "ListNotifyChannelRefs", "GetUpdates", "ListNodes", "ListTags", "GetRegisterWindow", "GetSnapshot",
  "QueryMetrics", "GetTraffic", "ListProbeTasks", "QueryProbes", "ListProbeComparisonNodes", "QueryProbeComparison",
  "ListProbeCertificates", "ListAlertRules", "ListSilences", "ListAlertEvents", "GetSettings", "GetBackupStatus",
  "GetHeartbeatStatus", "GetStorageStats", "GetApiReference", "ListRestoreRecords",
];
const others = [
  "ExecuteChange", "StartUpdate", "CancelUpdate", "Login", "BeginPasskeyLogin", "FinishPasskeyLogin", "GetSecurity",
  "SecurityAction", "Logout", "ListSessions", "RevokeSession", "CreateNode", "UpdateNode", "RenewNodeBilling", "BatchUpdateNodeTags",
  "DeleteNode", "RotateNodeToken", "ReorderNodes", "MoveNodes", "DeleteTag", "OpenRegisterWindow", "CloseRegisterWindow",
  "AdjustTraffic", "SaveProbeTask", "DeleteProbeTask", "ReorderProbeTasks", "SaveAlertRule", "DeleteAlertRule",
  "SaveSilence", "DeleteSilence", "GetAlertDeliveryError", "ListNotifyChannels", "SaveNotifyChannel", "DeleteNotifyChannel",
  "TestNotifyChannel", "UpdateSettings", "UploadTheme", "ListThemes", "EnableTheme", "DeleteTheme", "GetThemePreview",
  "DeleteThemeVersion", "ListThemeReleases", "InstallThemeRelease", "PreviewTheme", "GetThemePackage", "ListApiTokens",
  "CreateApiToken", "DeleteApiToken",
];
const publicReads = ["GetSite", "GetSnapshot", "QueryMetrics", "QueryProbes", "ListProbeComparisonNodes", "QueryProbeComparison"];
const json = (value: unknown = {}) => new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });
const surfaces: { name: string; transport: Transport; method: DescMethodUnary }[] = [
  { name: "管理", transport: admin, method: AdminService.method.getSnapshot },
  { name: "公开", transport: publicTransport, method: PublicService.method.getSnapshot },
];

it("超时错误文案由实际等待预算推出", async () => {
  vi.useFakeTimers();
  vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>(() => {})));
  const pending = admin.unary(AdminService.method.getSnapshot, undefined, undefined, undefined, {}).catch((err: unknown) => err);
  await vi.advanceTimersByTimeAsync(READ_DEADLINE_MS);
  expect(await pending).toMatchObject({ code: Code.DeadlineExceeded, rawMessage: `请求超过 ${READ_DEADLINE_MS / 1000} 秒等待预算` });
});

it("全部方法都有显式归类，只有 ACCESS_READ 与公开无副作用声明获得服务端截止头", async () => {
  expect(AdminService.methods.map((m) => m.name).sort()).toEqual([...reads, ...others].sort());
  expect(PublicService.methods.map((m) => m.name).sort()).toEqual([...publicReads].sort());
  for (const [service, transport, expected] of [[AdminService, admin, reads], [PublicService, publicTransport, publicReads]] as const) {
    const bounded: string[] = [];
    vi.stubGlobal("fetch", vi.fn(async (_input, init?: RequestInit) => {
      if (new Headers(init?.headers).get("Connect-Timeout-Ms") === "30000") bounded.push(service.methods[boundedIndex].name);
      return json();
    }));
    let boundedIndex = 0;
    for (const method of service.methods) {
      expect(method.methodKind).toBe("unary");
      await transport.unary(method as DescMethodUnary, undefined, undefined, undefined, {});
      boundedIndex += 1;
    }
    expect(bounded.sort()).toEqual([...expected].sort());
  }
});

it.each(surfaces)("$name 挂住读取在 30 秒以 DeadlineExceeded 失败并中断 fetch", async ({ transport, method }) => {
  vi.useFakeTimers();
  let signal!: AbortSignal;
  vi.stubGlobal("fetch", vi.fn((_input, init?: RequestInit) => {
    signal = init!.signal!;
    return new Promise<Response>(() => {});
  }));
  let error: unknown;
  const pending = transport.unary(method, undefined, undefined, undefined, {}).catch((err) => { error = err; });
  await vi.advanceTimersByTimeAsync(29_999);
  expect(error).toBeUndefined();
  expect(signal.aborted).toBe(false);
  await vi.advanceTimersByTimeAsync(1);
  expect(error).toBeInstanceOf(ConnectError);
  await pending;
  expect(error).toMatchObject({ code: Code.DeadlineExceeded });
  expect(signal.aborted).toBe(true);
});

it("有副作用的调用超过读预算仍可成功，不添加超时头也不取消", async () => {
  vi.useFakeTimers();
  let finish!: (response: Response) => void;
  let signal!: AbortSignal;
  let headers!: Headers;
  vi.stubGlobal("fetch", vi.fn((_input, init?: RequestInit) => {
    signal = init!.signal!;
    headers = new Headers(init?.headers);
    return new Promise<Response>((resolve) => { finish = resolve; });
  }));
  const pending = admin.unary(AdminService.method.createNode, undefined, undefined, undefined, { name: "node" });
  void pending.catch(() => {});
  await vi.advanceTimersByTimeAsync(90_000);
  expect(headers.has("Connect-Timeout-Ms")).toBe(false);
  expect(signal.aborted).toBe(false);
  finish(json());
  await expect(pending).resolves.toMatchObject({ message: { $typeName: "heron.v1.CreateNodeResponse" } });
});

it("主动取消保留 Canceled，成功与取消都清理截止计时器", async () => {
  vi.useFakeTimers();
  vi.stubGlobal("fetch", vi.fn(async () => json()));
  await admin.unary(AdminService.method.getSnapshot, undefined, undefined, undefined, {});
  expect(vi.getTimerCount()).toBe(0);
  vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>(() => {})));
  const ac = new AbortController();
  const pending = admin.unary(AdminService.method.getSnapshot, ac.signal, undefined, undefined, {}).catch((e: unknown) => e);
  ac.abort();
  expect(await pending).toMatchObject({ code: Code.Canceled });
  expect(vi.getTimerCount()).toBe(0);
});

it.each(surfaces)("$name 轮询连续挂住 93 秒进入横幅，下一轮成功后恢复", async ({ transport, method }) => {
  vi.useFakeTimers();
  vi.setSystemTime(1_800_000_000_000);
  let hang = false;
  const starts: number[] = [];
  vi.stubGlobal("fetch", vi.fn(() => {
    starts.push(Date.now());
    return hang ? new Promise<Response>(() => {}) : Promise.resolve(json({ now: "1800000000" }));
  }));
  const client = new QueryClient({ defaultOptions: { queries: queryDefaults } });
  function Poll() {
    const result = useQuery({ queryKey: ["poll"], refetchInterval: 2000,
      queryFn: async ({ signal }) => (await transport.unary(method, signal, undefined, undefined, {})).message });
    const gate = queryGate(result);
    return <>{gate.ready ? <>{gate.banner}<p>已有快照</p></> : gate.loading ?? errorBanner(...gate.errors)}</>;
  }
  const view = render(<QueryClientProvider client={client}><Poll /></QueryClientProvider>);
  await act(async () => { await vi.advanceTimersByTimeAsync(10); });
  expect(screen.getByText("已有快照")).toBeInTheDocument();
  hang = true;
  await act(async () => { await vi.advanceTimersByTimeAsync(1990); });
  const started = starts[1];
  expect(started).toBe(starts[0] + 2000);
  await act(async () => { await vi.advanceTimersByTimeAsync(92_999); });
  expect(screen.queryByRole("alert")).toBeNull();
  await act(async () => { await vi.advanceTimersByTimeAsync(1); });
  expect(client.getQueryState(["poll"])?.errorUpdatedAt).toBe(started + 93_000);
  expect(starts.slice(1)).toEqual([started, started + 31_000, started + 63_000]);
  await act(async () => { await vi.advanceTimersByTimeAsync(1); });
  expect(screen.getByRole("alert")).toHaveTextContent(READ_DEADLINE_MESSAGE);
  expect(screen.getByText("已有快照")).toBeInTheDocument();
  hang = false;
  await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
  expect(screen.queryByRole("alert")).toBeNull();
  expect(starts).toHaveLength(5);
  view.unmount();
  client.clear();
});
