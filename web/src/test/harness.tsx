import type { DescService } from "@bufbuild/protobuf";
import { createRouterTransport, type ServiceImpl } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClient, QueryClientProvider, type QueryClientConfig } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { createMemoryRouter, RouterProvider, type RouteObject } from "react-router";
import { AdminService } from "../gen/heron/v1/admin_pb";

export type AdminImpl = Partial<ServiceImpl<typeof AdminService>>;

// 页面测试使用内存服务和路由，默认关闭重试，不安装全局认证跳转；生产 client 的跳转由入口测试验证。queries 覆盖查询的默认
// 选项，需要看生产重试策略下页面行为的用例经它传入 retry.ts 的谓词。
type QueryDefaults = NonNullable<NonNullable<QueryClientConfig["defaultOptions"]>["queries"]>;

export function renderWithService<S extends DescService>(service: S, impl: Partial<ServiceImpl<S>>, routes: RouteObject[], initialPath: string, queries: QueryDefaults = { retry: false }) {
  const transport = createRouterTransport(({ service: register }) => {
    register(service, impl);
  });
  const router = createMemoryRouter(routes, { initialEntries: [initialPath] });
  const queryClient = new QueryClient({ defaultOptions: { queries } });
  render(
    <TransportProvider transport={transport}>
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </TransportProvider>,
  );
  return { router, queryClient };
}

export function renderWithAdmin(impl: AdminImpl, routes: RouteObject[], initialPath: string, queries?: QueryDefaults) {
  return renderWithService(AdminService, impl, routes, initialPath, queries);
}

// 时间窗口按钮所在的 .row 头部。级别、“非当前窗口”提示与更新中之间只靠这个头部的 gap 分隔，
// 用例断言它们的 parentElement 就是它：放到头部之外或另包一层，几段文字会连成一句话。
export function rangeHeader(): HTMLElement {
  const header = screen.getByRole("navigation", { name: "时间窗口" }).closest<HTMLElement>("header.row");
  if (header === null) throw new Error("时间窗口按钮不在 .row 头部里");
  return header;
}
