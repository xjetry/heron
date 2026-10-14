import { createBrowserRouter } from "react-router";
import { NotFound, PublicLayout } from "./Layout";
import { NodePage } from "./NodePage";
import { PublicProbeCompare } from "./ProbeCompare";
import { PublicOverview } from "./Overview";

// 公开页挂在 /：/admin 与 RPC 路径由 hub 先行匹配，其余路径都回落到本页的 index.html，由这里路由。
export const router = createBrowserRouter([
  {
    path: "/",
    Component: PublicLayout,
    children: [
      { index: true, Component: PublicOverview },
      { path: "nodes/:id", Component: NodePage },
      { path: "probes/:id", Component: PublicProbeCompare },
      { path: "*", Component: NotFound },
    ],
  },
]);
