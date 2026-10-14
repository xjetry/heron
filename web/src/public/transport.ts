import { createConnectTransport } from "@connectrpc/connect-web";
import { readDeadline } from "../api/deadline";

// GET：PublicService 的方法都无副作用，GET 的 URL 可被浏览器与中间缓存按 hub 下发的 Cache-Control 复用。
// credentials: "omit"：公开服务不看凭据，同源的会话 cookie 不随公开请求发出，中间缓存也不必按登录与否区分。
// JSON 编码与面板一致，开发者工具里可读。
export const transport = createConnectTransport({
  baseUrl: "/",
  useBinaryFormat: false,
  useHttpGet: true,
  interceptors: [readDeadline],
  fetch: (input, init) => globalThis.fetch(input, { ...init, credentials: "omit" }),
});
