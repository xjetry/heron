import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TransportProvider } from "@connectrpc/connect-query";
import { RouterProvider } from "react-router";
import { queryDefaults } from "../queryDefaults";
import { transport } from "./transport";
import { router } from "./router";
import "../fonts";
import "../styles.css";
import "./public.css";

// 公开页没有会话，不装认证跳转。失败的查询只对网络与反代的瞬时错误至多再试两次（retry.ts 的白名单），其余直接交给页面的
// 错误横幅：节点不公开的 NotFound 重试不会变成功；限流的 ResourceExhausted 在超额持续时重试不增加放行数，一次性突发时
// 重试能成功、不重试的代价是这一次查询显示错误——轮询的查询下一轮就恢复，只取一次的站点设置停到刷新页面（推导见 retry.ts）。
// 只有轮询查询在回前台时立即补取，历史与站点设置不因聚焦重复请求（queryDefaults.ts）。
const queryClient = new QueryClient({ defaultOptions: { queries: queryDefaults } });

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <TransportProvider transport={transport}>
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </TransportProvider>
  </StrictMode>,
);
