import { createConnectTransport } from "@connectrpc/connect-web";
import { readDeadline } from "./deadline";

// JSON 编码：开发者工具里能直接读请求与响应，体积差异对面板无意义。
// baseUrl 是同源根路径：会话 cookie 由浏览器自动附带，前端不持有任何凭据。
export const transport = createConnectTransport({ baseUrl: "/", useBinaryFormat: false, interceptors: [readDeadline] });
