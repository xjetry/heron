import { expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { renderWithAdmin } from "../test/harness";
import type { SaveProbeTaskRequest } from "../gen/heron/v1/admin_pb";
import { HttpStatusPolicy, ProbeIpFamily, ProbeKind } from "../gen/heron/v1/types_pb";
import { ProbeTasks } from "./ProbeTasks";

const routes = [{ path: "/probes", Component: ProbeTasks }];

async function openCreate() {
  fireEvent.click(await screen.findByRole("button", { name: "新建探测任务" }));
  return screen.getByRole("form", { name: "新建探测任务" });
}

const closed = () => waitFor(() => expect(screen.queryByRole("dialog")).toBeNull(), { timeout: 1000 });

it("新建 HTTP 任务默认要求 2xx；状态码判据只对 HTTP 出现，只随 HTTP 提交，精确码以外不带状态码", async () => {
  const saved: SaveProbeTaskRequest[] = [];
  renderWithAdmin({ listNodes: async () => ({}), listProbeTasks: async () => ({}),
    saveProbeTask: async (req) => { saved.push(req); return {}; },
  }, routes, "/probes");
  let form = await openCreate();
  expect(within(form).queryByRole("radiogroup", { name: "期望状态码" })).toBeNull();
  fireEvent.click(within(form).getByRole("radio", { name: "HTTP" }));
  const policy = within(form).getByRole("radiogroup", { name: "期望状态码" });
  expect(within(policy).getAllByRole("radio").map((radio) => radio.getAttribute("aria-label"))).toEqual(["2xx", "< 400", "指定状态码"]);
  expect(within(policy).getByRole("radio", { name: "2xx" })).toBeChecked();
  expect(within(form).queryByLabelText("状态码")).toBeNull();
  fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "https://example.com/" } });
  fireEvent.submit(form);
  await waitFor(() => expect(saved).toHaveLength(1));
  expect(saved[0].task).toMatchObject({ kind: ProbeKind.HTTP, httpStatusPolicy: HttpStatusPolicy.SUCCESS, httpExpectedStatus: 0 });

  // 精确码：填了才提交，切回别的判据后草稿里残留的码不随请求发出。
  await closed();
  form = await openCreate();
  fireEvent.click(within(form).getByRole("radio", { name: "HTTP" }));
  fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "https://example.com/health" } });
  fireEvent.click(within(form).getByRole("radio", { name: "指定状态码" }));
  const code = within(form).getByLabelText("状态码");
  // 1xx 是中间响应，探测只看得到最终响应：表单与 hub 同一范围 200–599。
  expect(code).toHaveAttribute("min", "200");
  expect(code).toHaveAttribute("max", "599");
  expect(code).toBeRequired();
  fireEvent.change(code, { target: { value: "204" } });
  fireEvent.submit(form);
  await waitFor(() => expect(saved).toHaveLength(2));
  expect(saved[1].task).toMatchObject({ httpStatusPolicy: HttpStatusPolicy.EXACT, httpExpectedStatus: 204 });

  await closed();
  form = await openCreate();
  fireEvent.click(within(form).getByRole("radio", { name: "HTTP" }));
  fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "https://example.com/" } });
  fireEvent.click(within(form).getByRole("radio", { name: "指定状态码" }));
  fireEvent.change(within(form).getByLabelText("状态码"), { target: { value: "302" } });
  fireEvent.click(within(form).getByRole("radio", { name: "< 400" }));
  fireEvent.submit(form);
  await waitFor(() => expect(saved).toHaveLength(3));
  expect(saved[2].task).toMatchObject({ httpStatusPolicy: HttpStatusPolicy.UNSPECIFIED, httpExpectedStatus: 0 });

  // 切到别的种类后判据整项不提交：非 HTTP 任务带判据 hub 会拒绝整个保存。
  await closed();
  form = await openCreate();
  fireEvent.click(within(form).getByRole("radio", { name: "HTTP" }));
  fireEvent.click(within(form).getByRole("radio", { name: "指定状态码" }));
  fireEvent.change(within(form).getByLabelText("状态码"), { target: { value: "204" } });
  fireEvent.click(within(form).getByRole("radio", { name: "TCP" }));
  expect(within(form).queryByRole("radiogroup", { name: "期望状态码" })).toBeNull();
  fireEvent.change(within(form).getByLabelText("目标"), { target: { value: "example.com:443" } });
  fireEvent.submit(form);
  await waitFor(() => expect(saved).toHaveLength(4));
  expect(saved[3].task).toMatchObject({ kind: ProbeKind.TCP, httpStatusPolicy: HttpStatusPolicy.UNSPECIFIED, httpExpectedStatus: 0 });
});

it("编辑回显任务当前的判据，列表在 HTTP 类型旁标出判据", async () => {
  renderWithAdmin({ listNodes: async () => ({}), listProbeTasks: async () => ({ tasks: [
    { task: { id: 1n, kind: ProbeKind.HTTP, target: "https://legacy.example/", intervalS: 60, timeoutMs: 1000 } },
    { task: { id: 2n, kind: ProbeKind.HTTP, target: "https://strict.example/", intervalS: 60, timeoutMs: 1000, httpStatusPolicy: HttpStatusPolicy.SUCCESS, ipFamily: ProbeIpFamily.IPV6 } },
    { task: { id: 3n, kind: ProbeKind.HTTP, target: "https://exact.example/", intervalS: 60, timeoutMs: 1000, httpStatusPolicy: HttpStatusPolicy.EXACT, httpExpectedStatus: 401 } },
    { task: { id: 4n, kind: ProbeKind.TCP, target: "example.com:443", intervalS: 60, timeoutMs: 1000 } },
  ] }) }, routes, "/probes");
  expect(await screen.findByRole("cell", { name: "HTTP · < 400" })).toBeInTheDocument();
  expect(screen.getByRole("cell", { name: "HTTP · IPv6 · 2xx" })).toBeInTheDocument();
  expect(screen.getByRole("cell", { name: "HTTP · 401" })).toBeInTheDocument();
  expect(screen.getByRole("cell", { name: "TCP" })).toBeInTheDocument();

  const edit = (label: string) => {
    fireEvent.click(screen.getByRole("button", { name: `更多操作 ${label}` }));
    fireEvent.click(screen.getByRole("menuitem", { name: `编辑 ${label}` }));
    return screen.getByRole("dialog");
  };
  let dialog = edit("https://legacy.example/（#1）");
  expect(within(dialog).getByRole("radio", { name: "< 400" })).toBeChecked();
  fireEvent.click(within(dialog).getByRole("button", { name: "取消" }));
  await closed();
  dialog = edit("https://exact.example/（#3）");
  expect(within(dialog).getByRole("radio", { name: "指定状态码" })).toBeChecked();
  expect(within(dialog).getByLabelText("状态码")).toHaveValue(401);
  fireEvent.click(within(dialog).getByRole("button", { name: "取消" }));
  await closed();
  // 非 HTTP 任务改成 HTTP 等于新配一个 HTTP 判据，按新建的默认给 2xx。
  dialog = edit("example.com:443（#4）");
  fireEvent.click(within(dialog).getByRole("radio", { name: "HTTP" }));
  expect(within(dialog).getByRole("radio", { name: "2xx" })).toBeChecked();
});
