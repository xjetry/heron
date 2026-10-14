import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TransportProvider } from "@connectrpc/connect-query";
import { RouterProvider } from "react-router";
import { transport } from "./api/transport";
import { isUnauthenticated } from "./api/auth";
import { router } from "./App";
import { queryDefaults } from "./queryDefaults";
import "./fonts";
import "./styles.css";
import "./admin.css";

// 任何查询或变更得到 Unauthenticated 都跳登录页：由数据层统一识别，页面不各自判断。
// 查询只对可能自行恢复的错误重试（retry.ts 的白名单）；Unauthenticated 不在其中，重试不会让会话复活。变更不重试。
const onError = (err: unknown) => {
  if (isUnauthenticated(err)) void router.navigate("/login", { replace: true });
};
const queryClient = new QueryClient({
  queryCache: new QueryCache({ onError }),
  mutationCache: new MutationCache({ onError }),
  defaultOptions: {
    queries: queryDefaults,
  },
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <TransportProvider transport={transport}>
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </TransportProvider>
  </StrictMode>,
);
