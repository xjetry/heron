import { create, createRegistry, fromJsonString, toJsonString, type DescMessage, type DescMethodUnary, type Message, type MessageInitShape, type MessageShape } from "@bufbuild/protobuf";
import { test as base, expect, request, type Locator, type Page, type Request, type Route } from "@playwright/test";
import { AdminService, file_heron_v1_admin } from "../src/gen/heron/v1/admin_pb";
import { file_heron_v1_agent } from "../src/gen/heron/v1/agent_pb";
import { file_heron_v1_public } from "../src/gen/heron/v1/public_pb";

export const ADMIN_PASSWORD = "local-browser-test-password";

// 请求与应答都经 @bufbuild/protobuf 的官方 JSON codec 编解码：int64、枚举、oneof、FieldMask、bytes、Any 都按
// proto3 JSON 映射处理，用例里不手写线上的 JSON 形状。Any（ExecuteChange 的 result）按类型 URL 找消息，注册表要
// 带上三个服务文件（它们各自引入 types、query 等依赖文件）。
const registry = createRegistry(file_heron_v1_admin, file_heron_v1_agent, file_heron_v1_public);

// Connect 协议的错误体（https://connectrpc.com/docs/protocol#error-end-stream）没有 proto 描述：按协议定义的形状核对后
// 读出 code 与 message；不是这个形状（例如反代或静态文件服务回的纯文本）时为 undefined，原文留在 text 里。
export type ConnectErrorBody = { code: string; message: string };

export type RpcResult<O extends DescMessage> =
  | { ok: true; method: string; message: MessageShape<O>; text: string }
  | { ok: false; method: string; status: number; error: ConnectErrorBody | undefined; text: string };

export class RpcError extends Error {
  constructor(readonly method: string, readonly status: number, readonly error: ConnectErrorBody | undefined, text: string) {
    super(`${method}: ${status} ${text}`);
  }
}

// 应答成功却缺了用例预期必有的子消息（hub 漏填，或线上形状变了）。与非 200 的失败同属 RpcError：都是这次调用没给出
// 用例要的东西，报告里同样点名方法并附上应答原文，另外点名缺的是哪个字段。
export class MissingFieldError extends RpcError {
  constructor(method: string, readonly field: string, text: string) {
    super(method, 200, undefined, text);
    this.message = `${method}: 200 response has no ${field}: ${text}`;
  }
}

export const rpcPath = (method: DescMethodUnary): string => `/${method.parent.typeName}/${method.name}`;

function connectError(text: string): ConnectErrorBody | undefined {
  let body: unknown;
  try {
    body = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (typeof body !== "object" || body === null || !("code" in body) || typeof body.code !== "string") return undefined;
  return { code: body.code, message: "message" in body && typeof body.message === "string" ? body.message : "" };
}

// 一次 unary 调用的线路部分：发出编码好的 JSON 字符串，拿回状态码与原文。编解码只在 Node 侧做。
type Send = (path: string, body: string, token: string | undefined) => Promise<{ status: number; text: string }>;

async function invoke<I extends DescMessage, O extends DescMessage>(send: Send, method: DescMethodUnary<I, O>, init: MessageInitShape<I>, token?: string): Promise<RpcResult<O>> {
  const body = toJsonString(method.input, create(method.input, init), { registry });
  const { status, text } = await send(rpcPath(method), body, token);
  if (status === 200) return { ok: true, method: method.name, message: fromJsonString(method.output, text, { registry }), text };
  return { ok: false, method: method.name, status, error: connectError(text), text };
}

// 经页面调用：请求从浏览器发出，带着页面的 cookie 会话（管理端登录态、同源检查看到的 Origin）。给了 token 时按
// agent 或 API token 的方式鉴权，不带 cookie，免得 cookie 会话替它通过鉴权。page.evaluate 只收发字符串与状态码。
export function rpc<I extends DescMessage, O extends DescMessage>(page: Page, method: DescMethodUnary<I, O>, init: MessageInitShape<I>, token?: string): Promise<RpcResult<O>> {
  return invoke((path, body, token) => page.evaluate(async ({ path, body, token }) => {
    const response = await fetch(path, {
      method: "POST",
      credentials: token ? "omit" : "same-origin",
      headers: { "Content-Type": "application/json", ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      body,
    });
    return { status: response.status, text: await response.text() };
  }, { path, body, token }), method, init, token);
}

// 取成功应答；失败时抛出带状态码与原文的错误，用例在调用点失败而不是拿着错误体往下走。
export function must<O extends DescMessage>(result: RpcResult<O>): MessageShape<O> {
  if (result.ok) return result.message;
  throw new RpcError(result.method, result.status, result.error, result.text);
}

// M 里取值为子消息的字段名。proto3 的子消息字段在生成的类型里都是可选的：线上缺席就是 undefined，类型系统替用例
// 担保不了"必有"。标量、repeated、map、oneof 都不是子消息，不在其中。
type SubMessageKey<M> = { [K in keyof M & string]-?: undefined extends M[K] ? (NonNullable<M[K]> extends Message ? K : never) : never }[keyof M & string];

// 取成功应答里用例预期必有的子消息（给两个字段名时取子消息的子消息，如 SaveProbeTask 的 task.task）。返回类型去掉了
// undefined，调用处不需要 `!`；缺席时抛出 MissingFieldError，点名方法与字段，而不是在下游以 TypeError 失败。
export function mustField<O extends DescMessage, K extends SubMessageKey<MessageShape<O>>>(result: RpcResult<O>, key: K): NonNullable<MessageShape<O>[K]>;
export function mustField<O extends DescMessage, K extends SubMessageKey<MessageShape<O>>, K2 extends SubMessageKey<NonNullable<MessageShape<O>[K]>>>(result: RpcResult<O>, key: K, key2: K2): NonNullable<NonNullable<MessageShape<O>[K]>[K2]>;
export function mustField(result: RpcResult<DescMessage>, ...path: string[]): Message {
  let value: Message = must(result);
  for (const [depth, key] of path.entries()) {
    const next = (value as Record<string, unknown>)[key];
    if (next === undefined) throw new MissingFieldError(result.method, path.slice(0, depth + 1).join("."), result.text);
    value = next as Message;
  }
  return value;
}

// 元素的包围盒；元素不存在或不可见时 Playwright 给 null，这里抛错点名定位器，而不是在下游读 null 的坐标。
// page.evaluate 里的代码跑在浏览器里，引用不到这里的工具，那里的 DOM 与几何值各自判空。
export async function boxOf(locator: Locator): Promise<{ x: number; y: number; width: number; height: number }> {
  const box = await locator.boundingBox();
  if (box === null) throw new Error(`${locator.toString()} has no bounding box: not attached or not visible`);
  return box;
}

// 以页面的 cookie 会话登录管理端。
export async function login(page: Page): Promise<void> {
  must(await rpc(page, AdminService.method.login, { password: ADMIN_PASSWORD }));
}

// 页面发出的请求消息：Connect 的 GET 把 JSON 放在 message 查询参数里，POST 放在正文里。
export function requestMessage<I extends DescMessage>(request: Request, method: { name: string; input: I }): MessageShape<I> {
  const json = request.method() === "GET" ? new URL(request.url()).searchParams.get("message") : request.postData();
  if (json === null) throw new Error(`${method.name}: request carries no message`);
  return fromJsonString(method.input, json, { registry });
}

// 拦截某个方法的路由模式与应答：按页面发来的请求消息给出应答消息，编码成 Connect 的 JSON 应答。
export const rpcRoute = (method: DescMethodUnary): string => `**${rpcPath(method)}**`;
export function fulfillRpc<I extends DescMessage, O extends DescMessage>(route: Route, method: DescMethodUnary<I, O>, respond: (request: MessageShape<I>) => MessageInitShape<O>): Promise<void> {
  const message = create(method.output, respond(requestMessage(route.request(), method)));
  return route.fulfill({ contentType: "application/json", body: toJsonString(method.output, message, { registry }) });
}

export type Hub = {
  // 以独立会话调用 AdminService，返回可判别的结果；要成功应答时用 must 取出。
  rpc: <I extends DescMessage, O extends DescMessage>(method: DescMethodUnary<I, O>, init: MessageInitShape<I>) => Promise<RpcResult<O>>;
  // 登记用例结束时要做的事：不论用例成败、是否超时，都在 fixture 拆除时按登记的逆序执行。
  atEnd: (label: string, fn: () => Promise<unknown>) => void;
  // 用例结束时调用一个删除过程（DeleteNode、DeleteProbeTask、DeleteApiToken 等）；用例自己已经删掉的（404）不算泄漏。
  deleteAtEnd: <I extends DescMessage>(method: DescMethodUnary<I, DescMessage>, init: MessageInitShape<I>) => void;
  deleteNodeAtEnd: (id: bigint) => void;
};

// e2e 的所有 spec 共用一个 hub 与一个库，一个用例留下的节点、主题或总闸状态会让后面无关的用例连带失败。收尾因此不放在
// 用例自己的 finally 里：用例超时时 Playwright 先关掉页面，finally 里借页面发的请求全部失败。hub fixture 用自己登录的
// 请求上下文（不带 Origin，管理接口的同源检查放行），收尾在 fixture 拆除阶段执行，有自己的时限，不依赖用例的页面还活着；
// 任何一项收尾失败都让拆除报错，留下的状态在报告里点名，而不是悄悄污染后面的用例。
export const test = base.extend<{ hub: Hub }>({
  hub: [async ({ baseURL }, use) => {
    const context = await request.newContext({ baseURL, ignoreHTTPSErrors: true });
    const send: Send = async (path, body) => {
      const response = await context.post(path, { data: body, headers: { "Content-Type": "application/json" } });
      return { status: response.status(), text: await response.text() };
    };
    const hubRpc: Hub["rpc"] = (method, init) => invoke(send, method, init);
    must(await hubRpc(AdminService.method.login, { password: ADMIN_PASSWORD }));
    const tasks: { label: string; fn: () => Promise<unknown> }[] = [];
    const atEnd = (label: string, fn: () => Promise<unknown>) => { tasks.push({ label, fn }); };
    const deleteAtEnd: Hub["deleteAtEnd"] = (method, init) => atEnd(`${method.name} ${toJsonString(method.input, create(method.input, init))}`, async () => {
      const result = await hubRpc(method, init);
      if (!result.ok && result.status !== 404) throw new RpcError(result.method, result.status, result.error, result.text);
    });
    await use({ rpc: hubRpc, atEnd, deleteAtEnd, deleteNodeAtEnd: (id) => deleteAtEnd(AdminService.method.deleteNode, { id }) });
    const failures: string[] = [];
    for (const task of tasks.reverse()) {
      try {
        await task.fn();
      } catch (error) {
        failures.push(`${task.label}: ${error instanceof Error ? error.message : String(error)}`);
      }
    }
    await context.dispose();
    if (failures.length > 0) throw new Error(`用例收尾失败，状态留在了 hub 里：\n${failures.join("\n")}`);
  }, { timeout: 30_000 }],
});

export { expect };
